package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const cfgFile = stateDir + "/config"

func cfgLoad() map[string]string {
	m := map[string]string{"COUNTRY": "", "KILLSWITCH": "1"}
	b, _ := os.ReadFile(cfgFile)
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			m[k] = v
		}
	}
	return m
}

func cfgSet(k, v string) {
	m := cfgLoad()
	m[k] = v
	var sb strings.Builder
	for _, key := range []string{"COUNTRY", "KILLSWITCH", "ENABLED", "AUTOSTART"} {
		if val, ok := m[key]; ok {
			fmt.Fprintf(&sb, "%s=%s\n", key, val)
		}
	}
	_ = os.MkdirAll(stateDir, 0700)
	_ = os.WriteFile(cfgFile, []byte(sb.String()), 0600)
}

func running() bool {
	pid := readPid()
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func cmdStart(args []string) {
	for _, a := range args {
		switch {
		case a == "--no-ks":
			cfgSet("KILLSWITCH", "0")
		case a == "--ks":
			cfgSet("KILLSWITCH", "1")
		case len(a) == 2 && strings.IndexFunc(a, func(r rune) bool { return r < 'A' || r > 'Z' }) < 0:
			cfgSet("COUNTRY", a)
		default:
			fmt.Fprintln(os.Stderr, "unknown arg", a)
			os.Exit(2)
		}
	}
	if running() {
		cmdDown()
	}
	c := cfgLoad()
	upArgs := []string{"up"}
	// A pasted private key (registered on surfshark.com) picks a server per country from the public list.
	// A full pasted .conf pins one server and ignores the country picker.
	if _, err := os.Stat(stateDir + "/wg.conf"); err == nil {
		upArgs = append(upArgs, "-conf", stateDir+"/wg.conf")
	} else if _, err := os.Stat(stateDir + "/wg.key"); err == nil {
		upArgs = append(upArgs, "-key", stateDir+"/wg.key")
		if c["COUNTRY"] != "" {
			upArgs = append(upArgs, "-country", c["COUNTRY"])
		}
	}
	if c["KILLSWITCH"] == "1" {
		upArgs = append(upArgs, "-killswitch")
	}
	self, _ := os.Executable()
	logf, err := os.OpenFile(stateDir+"/log", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cmd := exec.Command(self, upArgs...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}
	_ = cmd.Process.Release()
	cfgSet("ENABLED", "1")
	for i := 0; i < 30 && !running(); i++ {
		time.Sleep(500 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	cmdStatus()
	if !running() {
		cmdLog([]string{"10"})
		os.Exit(1)
	}
}

func cmdSet(a []string) {
	if len(a) < 2 {
		fmt.Fprintln(os.Stderr, "set country CC | set killswitch on|off")
		os.Exit(2)
	}
	switch a[0] {
	case "country":
		cfgSet("COUNTRY", strings.ToUpper(a[1]))
	case "killswitch", "autostart":
		v, ok := map[string]string{"on": "1", "off": "0"}[a[1]]
		if !ok {
			fmt.Fprintln(os.Stderr, "on|off")
			os.Exit(2)
		}
		cfgSet(strings.ToUpper(a[0]), v)
	default:
		fmt.Fprintln(os.Stderr, "unknown setting", a[0])
		os.Exit(2)
	}
	fmt.Println("saved; apply with: surfsharktv restart")
}

func cmdLog(a []string) {
	n := 30
	if len(a) > 0 {
		n, _ = strconv.Atoi(a[0])
	}
	b, _ := os.ReadFile(stateDir + "/log")
	var keep []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if !strings.Contains(l, "sendmmsg") {
			keep = append(keep, l)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	fmt.Println(strings.Join(keep, "\n"))
}

type stateOut struct {
	Running       bool   `json:"running"`
	Pid           int    `json:"pid,omitempty"`
	Server        string `json:"server,omitempty"`
	Country       string `json:"country"`
	KillSwitch    bool   `json:"killswitch"`
	Enabled       bool   `json:"enabled"`
	Autostart     bool   `json:"autostart"`
	HasConfig     bool   `json:"hasConfig"`
	HasPrivateKey bool   `json:"hasPrivateKey"`
	Since         int64  `json:"since,omitempty"`
	LastError     string `json:"lastError,omitempty"`
}

func cmdStateJSON() {
	c := cfgLoad()
	o := stateOut{Country: c["COUNTRY"], KillSwitch: c["KILLSWITCH"] == "1", Enabled: c["ENABLED"] == "1", Autostart: c["AUTOSTART"] != "0"}
	if _, err := os.Stat(stateDir + "/wg.conf"); err == nil {
		o.HasConfig = true
	}
	if _, err := os.Stat(stateDir + "/wg.key"); err == nil {
		o.HasPrivateKey = true
	}
	if running() {
		o.Running, o.Pid = true, readPid()
		b, _ := os.ReadFile("/tmp/surfsharktv.server")
		o.Server = strings.TrimSpace(string(b))
		if fi, err := os.Stat(pidFile); err == nil {
			o.Since = fi.ModTime().Unix()
		}
	} else {
		// last fatal line of the log, if the tunnel stopped on its own
		b, _ := os.ReadFile(stateDir + "/log")
		if strings.Contains(string(b), "shutting down") { // stopped on request: nothing to report
			b = nil
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		for i := len(lines) - 1; i >= 0 && i >= len(lines)-5; i-- {
			l := lines[i]
			if l != "" && !strings.Contains(l, "sendmmsg") && !strings.Contains(l, "shutting down") && !strings.Contains(l, "restored") && !strings.Contains(l, "tunnel ") {
				o.LastError = l
				break
			}
		}
	}
	_ = json.NewEncoder(os.Stdout).Encode(o)
}

func cmdVerify() {
	out := map[string]any{"ok": false}
	if _, err := os.Stat(stateDir + "/wg.conf"); err == nil {
		if _, err := parseConf(stateDir + "/wg.conf"); err != nil {
			out["error"] = "invalid WireGuard config: " + err.Error()
		} else {
			out["ok"] = true
		}
		_ = json.NewEncoder(os.Stdout).Encode(out)
		return
	}
	if kb, err := os.ReadFile(stateDir + "/wg.key"); err == nil {
		priv := strings.TrimSpace(string(kb))
		if _, err := b64hex(priv); err != nil {
			out["error"] = "invalid WireGuard private key (need 32 base64-encoded bytes)"
		} else if _, err := countries(); err != nil {
			out["error"] = "key saved, but the Surfshark server list is unavailable: " + err.Error()
		} else {
			out["ok"] = true
		}
		_ = json.NewEncoder(os.Stdout).Encode(out)
		return
	}
	out["error"] = "no private key or WireGuard config saved"
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

// secureState keeps /opt/surfsharktv root-only: root runs the binary from here and the WireGuard key is a secret.
// Folder 0700, everything in it 0600, the binary 0700. Files created later already get these modes.
func secureState() {
	if os.Geteuid() != 0 {
		return
	}
	if fi, err := os.Stat(stateDir); err != nil || !fi.IsDir() {
		return
	}
	_ = os.Chmod(stateDir, 0700)
	entries, _ := os.ReadDir(stateDir)
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		p := stateDir + "/" + e.Name()
		_ = os.Lchown(p, 0, 0)
		if e.Name() == "surfsharktv" || strings.HasSuffix(e.Name(), ".new") {
			_ = os.Chmod(p, 0700)
		} else {
			_ = os.Chmod(p, 0600)
		}
	}
}
