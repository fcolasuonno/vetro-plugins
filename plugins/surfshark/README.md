# Surfshark

Surfshark (WireGuard) on the TV itself, with no router setup or USB stick. A **VPN** tab to switch it on and off, pick a
country, set the kill switch and keep it connected after a restart or a drop.

You need a Surfshark subscription. There are **two ways to set it up**, both in the VPN tab:

1. **Private key (recommended — country switching works).** In a browser, go to Surfshark → VPN → Manual setup →
   Router → WireGuard, generate a key pair and paste its **public** key into the website. Paste the matching **private**
   key into the **Private key** panel. The Shard stores it on the TV and, on every start, picks the least loaded server
   in the selected country from Surfshark's public cluster list (`/v4/server/clusters/generic`). Changing **Country**
   reconnects to that country. No login is needed.
2. **WireGuard config (fixed server).** Download a `.conf` from the same page and paste its whole contents into the
   **WireGuard config** panel. This pins one server, so the country picker is ignored.

The two methods are alternatives: saving a private key removes a saved full config, and vice versa. There is no
account e-mail/password login — Surfshark's login API is Cloudflare-protected and returns HTTP 403 from TV networks.

Nothing is shown again in either case.

This Shard is a port of [`nordvpn`](../nordvpn). The helper binary is published as a pinned release in
[`fcolasuonno/surfsharktv`](https://github.com/fcolasuonno/surfsharktv).

## Permissions

`exec`: creating a network interface, routes and firewall rules needs root, and so does running the VPN program.

## The VPN program (`surfsharktv`)

Node cannot drive `/dev/net/tun` without native code, so the VPN itself is a separate program, `surfsharktv`, written
in Go on userspace [wireguard-go](https://git.zx2c4.com/wireguard-go). Its full source lives in
[`fcolasuonno/surfsharktv`](https://github.com/fcolasuonno/surfsharktv) (a copy is in [`surfsharktv/`](surfsharktv)),
a direct adaptation of the NordVPN Shard's `nordtv`. Build it with:

```sh
cd surfsharktv && ./build.sh            # reproducible armv7 build, gzip, SHA-256
```

- **Where it comes from:** if `/opt/surfsharktv/surfsharktv` is missing, or was installed from another build, the Shard
  downloads `https://github.com/fcolasuonno/surfsharktv/releases/download/v1.0.0/surfsharktv.gz` and installs it **only
  if its SHA-256 is `4f484e4c7af9064aca6f10db46376a85c76d73faeeb589023cea6d6f333fa09a`**, pinned in `index.js`. A file
  with any other hash is deleted and nothing is installed.
- **How it runs:** the Shard keeps it in `/opt/surfsharktv` and runs it from `/opt/surfsharktv/ro/surfsharktv`, a
  read-only bind mount of `/opt/surfsharktv/ro` that `start()` (re)creates on every boot. Newer Tizen firmware has a
  kernel UEP check that refuses unsigned ELFs on writable filesystems but skips read-only VDFS mounts; running from
  the read-only mount is what lets the unsigned `surfsharktv` binary execute on those TVs.
- Any change to the binary gets a new release, a new hash and a new version of this Shard.

## Commands it runs

All through `ctx.execute`, as root.

| When | Command | Why |
| --- | --- | --- |
| before every command below | `chmod 700 /opt/surfsharktv`, `chmod 600 /opt/surfsharktv/*`, `chmod 700 /opt/surfsharktv/surfsharktv`; if `/opt/surfsharktv/ro` is mounted and its copy differs (`md5sum`), `umount` it; `mkdir -p` and `chmod 700` the folder; if the running copy is missing, `cp` the binary, `chmod 700`, `mount --bind` the folder onto itself and `mount -o remount,ro,bind` | keep the folder root-only and stage the program on a read-only VDFS mount where UEP accepts it |
| 15 s after the Shard starts, and on **Download and install** | `mkdir -p /opt/surfsharktv`; if the binary or its pinned hash is missing: `curl -fsSL --max-time 90` the URL above, `sha256sum` check, `gunzip`, `chmod 700`, `mv`, write the hash to `/opt/surfsharktv/.source` | install the pinned binary (nothing is downloaded when it is already there) |
| page opens, **Refresh**, every 60 s | `surfsharktv state` (page: `surfsharktv status -json`) | read the state |
| country list | `surfsharktv countries` | names for the country picker |
| a switch or the country changes | `surfsharktv set country CC` / `set killswitch on\|off` / `set autostart on\|off`, then `surfsharktv start` when the VPN is on | apply the setting; country and kill switch reconnect |
| **VPN on** / **VPN off** | `surfsharktv start` / `surfsharktv stop` | `stop` also saves "VPN off" |
| **Save private key** | `base64 -d > /opt/surfsharktv/wg.key.new`, `chmod 600`, `mv` to `/opt/surfsharktv/wg.key`, remove any `wg.conf`, `surfsharktv verify` | store the registered private key (never in clear on a command line) and check it with the public server list |
| **Save config** | `base64 -d > /opt/surfsharktv/wg.conf.new`, `chmod 600`, `mv` to `/opt/surfsharktv/wg.conf`, remove any `wg.key`, `surfsharktv verify` | store the pasted WireGuard config (never in clear on a command line) and check that it parses |
| every 60 s, only when "VPN on" and "Keep connected" are saved and the tunnel is down | `surfsharktv start` | reconnect after a restart or a drop; a failed start waits 2 minutes |
| the Shard is switched off | `surfsharktv down` | stop the VPN and remove everything it created, keeping "VPN on" saved so it comes back with the Shard |

Inside `surfsharktv`, the commands that change the system are only `ip addr/link/route/rule`, `iptables` and
`ip6tables` (next section).

## What it changes on the TV

While the VPN runs:

- a `surftun` interface and policy routing like `wg-quick`: `ip rule` entries at priority 31002/31003, a default
  route in table 51821, fwmark 51821. The home network (LAN) stays reachable, Vetro included.
- **DNS is forced** to Surfshark's resolver (162.252.172.57): port 53 is redirected (`iptables -t nat`, chain `SURFTV_DNS`).
- **IPv6 egress is blocked** (`ip6tables`, chain `SURFTV`): the tunnel is IPv4 only.
- **Kill switch, on by default** (`iptables`, chain `SURFTV`): internet traffic outside the tunnel is rejected.
  **If the VPN drops, the TV has no internet until it reconnects**; the LAN keeps working. Switch it off in the tab.

`surfsharktv down` / `stop` (and switching the Shard off) remove all of it.

## Network

- `api.surfshark.com`: `GET /v4/server/clusters/generic` — the public server and country list, used to pick a server
  for the selected country. No token, no account data.
- The chosen Surfshark server, UDP 51820 (WireGuard).
- GitHub (`github.com` and the asset servers its release downloads redirect to): only to download the pinned
  binary when it is missing.

Nothing is sent anywhere else, and nothing about the TV, the user or the network is collected.

## Data it keeps

`/opt/surfsharktv`, root only (folder `0700`, files `0600`, binary `0700`): `config` (country, kill switch, "VPN on",
"Keep connected"), `wg.key` (private key you registered on the website, if used), `wg.conf` (pasted WireGuard
config, if used), `log`, `.source` (hash of the installed binary), `surfsharktv`.
`/opt/surfsharktv/ro/surfsharktv` (the running copy, on a read-only bind mount). The Shard does not use `ctx.config`. Removing the Shard leaves
`/opt/surfsharktv`; `rm -rf /opt/surfsharktv` removes it and the private key.

## Tested

- Samsung QE77S90CATXXU (S90C, Tizen 9.0, kernel 5.4, ARMv7, Vetro 0.5.0): install and start, the `state` and
  `countries` routes (real Surfshark cluster list), and execution of the unsigned `surfsharktv` binary from the
  read-only VDFS bind mount. UEP accepts that mount; writable copies (`/dev/shm`, `/opt/surfsharktv`) are refused.
- With a registered WireGuard key on that TV: tunnel up to `it-rom.prod.surfshark.com`, country changes (IT → JP) and
  kill switch toggles applied from the tab and from the Tizen app.
- The pinned v1.0.0 release was downloaded from GitHub by the TV and accepted only with the pinned SHA-256.
- Ported from [`nordvpn`](../nordvpn), which its author tested on a Samsung QE43Q60BAUXXN (Q60B, 2022, firmware
  T-NKLBDEUC-1720.7), Tizen 6.5. Older firmware like that one apparently allows the unsigned binary on writable
  storage directly, so `nordvpn` does not need the read-only mount.
