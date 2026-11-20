// surfsharktv: self-contained Surfshark WireGuard client for Tizen TVs (or any Linux).
//
//	surfsharktv up   [-key wg.key | -conf wg.conf] [-country IT] [-server it-foo.prod.surfshark.com] [-killswitch]
//	surfsharktv down
//	surfsharktv status
//
// Runs wireguard-go in userspace on /dev/net/tun, then sets up policy routing
// (wg-quick style), DNS redirection and an optional kill switch via ip/iptables.
//
// No account login: the user registers a WireGuard public key on surfshark.com and gives us the matching private
// key (in /opt/surfsharktv/wg.key). The public cluster list at api.surfshark.com is then used to pick the least
// loaded server in the chosen country and build a wgConf (10.14.0.2/16, Surfshark DNS, UDP 51820). A full
// WireGuard .conf downloaded from Surfshark's manual-setup page can be used instead (-conf), pinning one server.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "golang.org/x/crypto/x509roots/fallback" // embedded CA roots (TV may lack a bundle)
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

const (
	ifName   = "surftun"
	fwMark   = 51821
	table    = 51821
	rulePref = 31002
	pidFile  = "/tmp/surfsharktv.pid"
	stateDir = "/opt/surfsharktv"
	chain    = "SURFTV"
	natChain = "SURFTV_DNS"
	apiBase  = "https://api.surfshark.com"
	port     = "51820"
	address  = "10.14.0.2/16"
)

var surfDNS = []string{"162.252.172.57", "149.154.159.92"}

type wgConf struct {
	PrivateKey string
	Address    string // CIDR
	DNS        []string
	MTU        int
	PeerPub    string
	Endpoint   string // host:port
	Keepalive  int
	Label      string
}

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
	}
	secureState()
	switch os.Args[1] {
	case "up":
		cmdUp(os.Args[2:])
	case "down":
		cmdDown()
	case "status":
		if len(os.Args) > 2 && os.Args[2] == "-json" {
			cmdStateJSON()
		} else {
			cmdStatus()
		}
	case "state":
		cmdStateJSON()
	case "verify":
		cmdVerify()
	case "countries":
		cmdCountries()
	case "start", "restart":
		cmdStart(os.Args[2:])
	case "stop":
		cfgSet("ENABLED", "0")
		cmdDown()
	case "set":
		cmdSet(os.Args[2:])
	case "log":
		cmdLog(os.Args[2:])
	case "uninstall":
		cmdDown()
		os.RemoveAll(stateDir)
		fmt.Println("removed", stateDir)
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  surfsharktv up [-key keyfile | -conf file.conf] [-country CODE] [-server HOST] [-killswitch]
  surfsharktv down
  surfsharktv status
  surfsharktv countries

managed mode (settings in /opt/surfsharktv/config):
  surfsharktv start [CC] [--no-ks|--ks]   start in background (optionally change country/kill switch)
  surfsharktv stop | restart
  surfsharktv set country CC | set killswitch on|off
  surfsharktv log [N]
  surfsharktv state | status -json        machine-readable state
  surfsharktv verify                      check the saved private key or WireGuard config
  surfsharktv uninstall`)
	os.Exit(2)
}

// ---------------- Surfshark API ----------------

var httpc = &http.Client{Timeout: 20 * time.Second}

type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, truncate(e.body, 200))
}

// apiGet fetches a public Surfshark endpoint. No credentials or tokens are used anywhere in this program.
func apiGet(u string, out any) error {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "surfsharktv/1.0")
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{status: resp.StatusCode, body: truncate(string(data), 200)}
	}
	return json.Unmarshal(data, out)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

type country struct {
	Code string `json:"countryCode"`
	Name string `json:"country"`
}

type server struct {
	Hostname    string `json:"connectionName"`
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
	Region      string `json:"region"`
	Location    string `json:"location"`
	PubKey      string `json:"pubKey"`
	Load        int    `json:"load"`
}

func countries() ([]country, error) {
	var list []server
	if err := apiGet(apiBase+"/v4/server/clusters/generic", &list); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var cs []country
	for _, s := range list {
		code := strings.ToUpper(strings.TrimSpace(s.CountryCode))
		if len(code) != 2 || seen[code] || s.PubKey == "" {
			continue
		}
		seen[code] = true
		cs = append(cs, country{Code: code, Name: strings.TrimSpace(s.Country)})
	}
	if len(cs) == 0 {
		return nil, errors.New("empty country list")
	}
	return cs, nil
}

func cmdCountries() {
	cs, err := countries()
	if err != nil {
		log.Fatal(err)
	}
	for _, c := range cs {
		fmt.Printf("%s\t%s\n", c.Code, c.Name)
	}
}

// surfConfFromKey builds a config for the selected country from Surfshark's public cluster list, using a private key
// the user registered on the website. No account login is needed, so Cloudflare-blocking of the login API is bypassed
// and switching countries works: every start picks the least loaded server in that country.
func surfConfFromKey(priv, countryCode, host string) (*wgConf, error) {
	if _, err := b64hex(priv); err != nil {
		return nil, fmt.Errorf("invalid WireGuard private key: %w", err)
	}
	var list []server
	if err := apiGet(apiBase+"/v4/server/clusters/generic", &list); err != nil {
		return nil, err
	}
	var chosen server
	if host != "" {
		for _, s := range list {
			if strings.EqualFold(s.Hostname, host) {
				chosen = s
				break
			}
		}
		if chosen.Hostname == "" {
			return nil, fmt.Errorf("server %s not found", host)
		}
	} else {
		best := 1 << 30
		for _, s := range list {
			if s.PubKey == "" || len(s.CountryCode) != 2 {
				continue
			}
			if countryCode != "" && !strings.EqualFold(s.CountryCode, countryCode) {
				continue
			}
			if s.Load < best {
				best, chosen = s.Load, s
			}
		}
		if chosen.Hostname == "" {
			return nil, errors.New("no WireGuard server available for this country")
		}
	}
	if chosen.PubKey == "" {
		return nil, fmt.Errorf("server %s has no WireGuard key", chosen.Hostname)
	}
	return &wgConf{
		PrivateKey: priv,
		Address:    address,
		DNS:        surfDNS,
		MTU:        1350,
		PeerPub:    chosen.PubKey,
		Endpoint:   net.JoinHostPort(chosen.Hostname, port),
		Keepalive:  25,
		Label:      fmt.Sprintf("%s · %s (load %d%%)", chosen.Location, chosen.Hostname, chosen.Load),
	}, nil
}

// ---------------- wg-quick style .conf ----------------

func parseConf(path string) (*wgConf, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c := &wgConf{MTU: 1350, Keepalive: 25, Label: path, DNS: surfDNS}
	sc := bufio.NewScanner(f)
	inPeer := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inPeer = strings.EqualFold(line, "[Peer]")
			continue
		}
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if inPeer {
			switch k {
			case "publickey":
				c.PeerPub = v
			case "endpoint":
				c.Endpoint = v
			case "persistentkeepalive":
				c.Keepalive, _ = strconv.Atoi(v)
			}
			continue
		}
		switch k {
		case "privatekey":
			c.PrivateKey = v
		case "address":
			for _, a := range strings.Split(v, ",") {
				a = strings.TrimSpace(a)
				if strings.Contains(a, ".") && c.Address == "" {
					if !strings.Contains(a, "/") {
						a += "/32"
					}
					c.Address = a
				}
			}
		case "dns":
			c.DNS = nil
			for _, d := range strings.Split(v, ",") {
				if ip := net.ParseIP(strings.TrimSpace(d)); ip != nil && ip.To4() != nil {
					c.DNS = append(c.DNS, ip.String())
				}
			}
		case "mtu":
			c.MTU, _ = strconv.Atoi(v)
		}
	}
	if c.PrivateKey == "" || c.PeerPub == "" || c.Endpoint == "" || c.Address == "" {
		return nil, errors.New("conf needs PrivateKey, Address, PublicKey, Endpoint")
	}
	return c, nil
}

// ---------------- up ----------------

func b64hex(k string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(k)
	if err != nil || len(b) != 32 {
		return "", fmt.Errorf("bad key")
	}
	return hex.EncodeToString(b), nil
}

func sh(args ...string) error {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func shq(args ...string) { _ = sh(args...) } // best effort

func cmdUp(args []string) {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	confPath := fs.String("conf", "", "WireGuard .conf instead of the Surfshark API")
	keyPath := fs.String("key", "", "file with a WireGuard private key registered on surfshark.com")
	cc := fs.String("country", "", "country code, e.g. US, GB, IT")
	host := fs.String("server", "", "specific server hostname, e.g. it-foo.prod.surfshark.com")
	ks := fs.Bool("killswitch", false, "block non-VPN internet traffic while running")
	fs.Parse(args)

	if pid := readPid(); pid > 0 && syscall.Kill(pid, 0) == nil {
		log.Fatalf("already running (pid %d); run 'surfsharktv down' first", pid)
	}

	var c *wgConf
	var err error
	if *confPath != "" {
		c, err = parseConf(*confPath)
	} else if *keyPath != "" {
		kb, readErr := os.ReadFile(*keyPath)
		if readErr != nil {
			log.Fatal("private key file: ", readErr)
		}
		c, err = surfConfFromKey(strings.TrimSpace(string(kb)), *cc, *host)
	} else {
		log.Fatal("need -key /opt/surfsharktv/wg.key or -conf /opt/surfsharktv/wg.conf")
	}
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("server: %s endpoint %s", c.Label, c.Endpoint)

	priv, err := b64hex(c.PrivateKey)
	if err != nil {
		log.Fatal("private key: ", err)
	}
	pub, err := b64hex(c.PeerPub)
	if err != nil {
		log.Fatal("peer key: ", err)
	}
	ep, err := net.ResolveUDPAddr("udp4", c.Endpoint)
	if err != nil {
		log.Fatal("endpoint: ", err)
	}

	cleanup(false) // remove leftovers from a crash

	tdev, err := tun.CreateTUN(ifName, c.MTU)
	if err != nil {
		log.Fatal("create tun: ", err)
	}
	logger := device.NewLogger(device.LogLevelError, "wg: ")
	dev := device.NewDevice(tdev, conn.NewDefaultBind(), logger)
	uapi := fmt.Sprintf("private_key=%s\nfwmark=%d\nreplace_peers=true\npublic_key=%s\nendpoint=%s\npersistent_keepalive_interval=%d\nreplace_allowed_ips=true\nallowed_ip=0.0.0.0/0\n",
		priv, fwMark, pub, ep.String(), c.Keepalive)
	if err := dev.IpcSet(uapi); err != nil {
		log.Fatal("configure: ", err)
	}
	if err := dev.Up(); err != nil {
		log.Fatal("up: ", err)
	}

	steps := [][]string{
		{"ip", "addr", "add", c.Address, "dev", ifName},
		{"ip", "link", "set", ifName, "up", "mtu", strconv.Itoa(c.MTU)},
		{"ip", "route", "add", "default", "dev", ifName, "table", strconv.Itoa(table)},
		{"ip", "rule", "add", "not", "fwmark", strconv.Itoa(fwMark), "table", strconv.Itoa(table), "pref", strconv.Itoa(rulePref + 1)},
		{"ip", "rule", "add", "table", "main", "suppress_prefixlength", "0", "pref", strconv.Itoa(rulePref)},
	}
	for _, s := range steps {
		if err := sh(s...); err != nil {
			dev.Close()
			cleanup(false)
			log.Fatal(err)
		}
	}
	// IPv6 is not tunnelled: drop global v6 egress to avoid leaks
	shq("ip6tables", "-N", chain)
	shq("ip6tables", "-A", chain, "-o", "lo", "-j", "RETURN")
	shq("ip6tables", "-A", chain, "-d", "fe80::/10", "-j", "RETURN")
	shq("ip6tables", "-A", chain, "-d", "ff00::/8", "-j", "RETURN")
	shq("ip6tables", "-A", chain, "-j", "REJECT")
	shq("ip6tables", "-I", "OUTPUT", "1", "-j", chain)

	// Force all DNS (the router from DHCP and resolvers hardcoded by apps) to the VPN resolver
	dns := c.DNS
	if len(dns) == 0 {
		dns = surfDNS
	}
	shq("iptables", "-t", "nat", "-N", natChain)
	for _, p := range []string{"udp", "tcp"} {
		shq("iptables", "-t", "nat", "-A", natChain, "-o", "lo", "-j", "RETURN")
		shq("iptables", "-t", "nat", "-A", natChain, "-p", p, "--dport", "53", "-j", "DNAT", "--to-destination", dns[0]+":53")
	}
	shq("iptables", "-t", "nat", "-I", "OUTPUT", "1", "-j", natChain)
	shq("ip", "route", "flush", "cache")

	if *ks {
		// allow: loopback, tunnel, LAN, the WG endpoint (marked packets); drop everything else
		shq("iptables", "-N", chain)
		shq("iptables", "-A", chain, "-o", "lo", "-j", "RETURN")
		shq("iptables", "-A", chain, "-o", ifName, "-j", "RETURN")
		shq("iptables", "-A", chain, "-p", "udp", "-d", ep.IP.String(), "--dport", strconv.Itoa(ep.Port), "-j", "RETURN")
		shq("iptables", "-A", chain, "-m", "mark", "--mark", strconv.Itoa(fwMark), "-j", "RETURN")
		for _, n := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "224.0.0.0/4", "255.255.255.255/32"} {
			shq("iptables", "-A", chain, "-d", n, "-j", "RETURN")
		}
		shq("iptables", "-A", chain, "-j", "REJECT")
		shq("iptables", "-I", "OUTPUT", "1", "-j", chain)
		log.Print("kill switch enabled")
	}

	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0644)
	_ = os.WriteFile("/tmp/surfsharktv.server", []byte(c.Label+"\n"), 0644)
	log.Printf("tunnel %s up", ifName)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	tick := time.NewTicker(30 * time.Second)
	var lastOK time.Time = time.Now()
	for {
		select {
		case s := <-sig:
			log.Printf("got %v, shutting down", s)
			dev.Close()
			cleanup(true)
			os.Remove(pidFile)
			os.Remove("/tmp/surfsharktv.server")
			return
		case <-dev.Wait():
			log.Print("device closed")
			cleanup(true)
			os.Exit(1)
		case <-tick.C:
			hs := lastHandshake(dev)
			if !hs.IsZero() {
				lastOK = hs
			}
			// Wi-Fi reconnect / DHCP changes can wipe our rules; re-assert them.
			if !hasRule() {
				log.Print("routing rules missing, re-adding")
				for _, s := range steps[2:] {
					shq(s...)
				}
			}
			if time.Since(lastOK) > 5*time.Minute {
				log.Print("no handshake for 5 min, re-setting endpoint")
				dev.IpcSet(fmt.Sprintf("public_key=%s\nendpoint=%s\n", pub, ep.String()))
				lastOK = time.Now()
			}
		}
	}
}

func lastHandshake(dev *device.Device) time.Time {
	s, err := dev.IpcGet()
	if err != nil {
		return time.Time{}
	}
	var sec int64
	for _, l := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(l, "last_handshake_time_sec="); ok {
			sec, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

func hasRule() bool {
	out, _ := exec.Command("ip", "rule").Output()
	return strings.Contains(string(out), "lookup "+strconv.Itoa(table)) ||
		strings.Contains(string(out), "lookup 0xca6d")
}

func cleanup(verbose bool) {
	for i := 0; i < 3; i++ {
		shq("ip", "rule", "del", "pref", strconv.Itoa(rulePref))
		shq("ip", "rule", "del", "pref", strconv.Itoa(rulePref+1))
	}
	shq("ip", "route", "flush", "table", strconv.Itoa(table))
	shq("ip", "link", "del", ifName)
	for i := 0; i < 3; i++ {
		shq("iptables", "-D", "OUTPUT", "-j", chain)
		shq("ip6tables", "-D", "OUTPUT", "-j", chain)
		shq("iptables", "-t", "nat", "-D", "OUTPUT", "-j", natChain)
	}
	shq("iptables", "-F", chain)
	shq("iptables", "-X", chain)
	shq("ip6tables", "-F", chain)
	shq("ip6tables", "-X", chain)
	shq("iptables", "-t", "nat", "-F", natChain)
	shq("iptables", "-t", "nat", "-X", natChain)
	shq("ip", "route", "flush", "cache")
	if verbose {
		log.Print("routes and firewall restored")
	}
}

func readPid() int {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return p
}

func cmdDown() {
	pid := readPid()
	if pid > 0 && syscall.Kill(pid, syscall.SIGTERM) == nil {
		for i := 0; i < 50 && syscall.Kill(pid, 0) == nil; i++ {
			time.Sleep(100 * time.Millisecond)
		}
		fmt.Println("stopped")
	} else {
		fmt.Println("not running; cleaning up leftovers")
	}
	cleanup(false)
	os.Remove(pidFile)
}

func cmdStatus() {
	pid := readPid()
	if pid == 0 || syscall.Kill(pid, 0) != nil {
		fmt.Println("surfsharktv: down")
		return
	}
	srv, _ := os.ReadFile("/tmp/surfsharktv.server")
	fmt.Printf("surfsharktv: up (pid %d) server %s", pid, srv)
	c := cfgLoad()
	fmt.Printf("settings: country=%s killswitch=%s\n", c["COUNTRY"], c["KILLSWITCH"])
}
