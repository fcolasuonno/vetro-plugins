# Surf Control

A minimal Tizen web app for the TV that controls the [`surfshark`](../../plugins/surfshark) Shard: VPN on/off,
country, kill switch and keep connected. It is needed because Vetro's dashboard API rejects cross-origin requests
from a normal Tizen web app.

## How it works

The Shard opens a localhost-only HTTP bridge on `127.0.0.1:18765` with permissive CORS. The app calls:

- `GET /status` — cached Shard state (refreshed by the Shard every 60 s and after any action);
- `GET /countries` — Surfshark's country list;
- `POST /on`, `/off`, `/country`, `/killswitch`, `/autostart` — commands.

Requests are answered immediately; the Shard executes the command from its own timer loop (calling `ctx.execute`
from inside the HTTP callback does not resolve in Vetro's JS host). The app refreshes after 2.5 s.

## Install

`apps2samsung` signs and installs the widget:

```sh
apps2samsung-cli install helpers/surf-control/surf-control.wgt --tv <TV-IP> --partner --yes --new-id
```

`--new-id` is needed: this TV's package manager refuses a changed `tizen:application` identity, so the widget keeps
the known-good identity (`A2STVStore.Store`) and apps2samsung assigns a fresh package id on install. The signed
`surf-control-signed.wgt` is included for convenience; the CLI re-signs it for the target TV anyway.

## Build

```sh
cd helpers/surf-control
./build.sh                                              # -> surf-control.wgt
A2S=apps2samsung-cli DUID=<TV-DUID> ./build.sh          # -> also surf-control-signed.wgt
```

`build.sh` just zips `config.xml`, `index.html` and `icon.png` (a widget is a ZIP with a `.wgt` extension). Signing
uses apps2samsung-cli's Partner certificate; `apps2samsung-cli cert` prints the DUID if you don't know it.

Two identities are kept side by side:

- `config.xml` — the fresh `SurfControl.main` identity (rejected by this TV's CLI install; kept for trying via the
  apps2samsung GUI). Produces `surf-control.wgt` / `surf-control-alt.wgt`.
- `config.xml.known-good` — the `A2STVStore.Store` identity that installs here with `--new-id`. Produces
  `surf-control-known-good.wgt` / `surf-control-known-good-signed.wgt`.

## Build the Shard

The bridge lives in the Shard, so after editing `plugins/surfshark/index.js`, `ui.html`, the Go source or the README:

```sh
cd ../../                        # repository root
node scripts/build.cjs           # rebuilds dist/ and index.json
node scripts/build.cjs --check   # CI check
```

To rebuild the `surfsharktv` binary, see `plugins/surfshark/surfsharktv/build.sh` (needs Go 1.26 and the module cache).

The Shard must be switched on in Vetro for the bridge to be available.
