# myst-dark-proxy

A fail-closed local proxy that carries traffic out through the
[Mysterium Network](https://mysterium.network) using a Mysterium identity you
already own.

Point a browser, a scraper, or a device that only knows how to open a TCP socket
at a fixed local port, and it leaves from a Mysterium exit node instead of your
own address. **If the tunnel is not up, nothing is carried** - the relay refuses,
it never falls back to your connection.

```
   clients                     myst-dark-proxy                    Mysterium
+--------------+   +----------------------------------+    +------------------+
| socks5 :7362 |-->|  SOCKS5 \                        |    |                  |
| http   :7360 |-->|  HTTP    >  kill switch -> node -+--->|  exit provider --+---> internet
| raw    :7339 |-->|  raw    /                        |    |                  |
+--------------+   +----------------------------------+    +------------------+
                          fail closed          myst --proxymode
```

## How it works

The Mysterium node runs with `--consumer --proxymode`: no TUN device, no route
changes, no privileges. It builds a userspace WireGuard stack and exposes it as
a local HTTP `CONNECT` listener. Egress is therefore a *dialer*, not a
system-wide route - nothing on the machine inherits the tunnel by accident, and
nothing can silently bypass it.

The relay sits in front of that, translating SOCKS5 / HTTP / raw TCP onto it and
enforcing the kill switch. Target hostnames are passed to the exit node
verbatim, so **DNS resolves at the exit** and never locally.

### Node behaviour worth knowing

Four things about `--proxymode` are undocumented and were established by testing
against node **1.39.5**. Each one breaks an implementation built on the obvious
assumption:

| | |
|---|---|
| The listener speaks **HTTP CONNECT, not SOCKS5**, despite the flag name | a SOCKS5 greeting gets no reply at all |
| Sessions are **keyed by proxy port**; `/connection*` needs `?id=<port>` | without it the node reports on connection 0 and answers `NotConnected` forever, and `DELETE` never disconnects |
| `/connection/ip` reports the **node's own** address, not the session's | the exit address can only be learned by fetching *through* the tunnel |
| A node restart **re-locks the keystore** | unlocking only at startup makes a restart permanently unrecoverable |

`connect_options.proxy_port` pins the session listener exactly, which is what
makes the fixed ports real rather than best-effort.

## Requirements

- A **registered, funded** Mysterium identity - the `UTC--<date>--<address>`
  keystore JSON, normally in `~/.mysterium/keystore/`. This tool consumes an
  identity, it does not create or fund one.
- Windows 10/11 or Linux (amd64). Docker only for the container deployment.

## Setup - Windows

Download `mystrelay.exe` from [Releases](../../releases), put it in a folder
with your keystore JSON, and run:

```powershell
mystrelay.exe setup
```

It asks for the keystore, its passphrase (the Mysterium desktop app normally
uses an empty one - press Enter), the ports, an optional country filter, and
whether to start automatically. Then it downloads the Mysterium node, verifies
its SHA-256 against a digest baked into the binary, and unpacks it.

Everything lives in `%LOCALAPPDATA%\myst-relay\`. Uninstall is
`mystrelay.exe autostart remove` plus deleting that folder - no services, no
registry keys, no firewall rules.

```powershell
mystrelay.exe run       # run it
mystrelay.exe status    # what it is doing right now
mystrelay.exe verify    # prove traffic leaves through the exit, not through you

mystrelay.exe autostart install -scope logon   # at sign-in, as you (no admin)
mystrelay.exe autostart install -scope boot    # before sign-in, as SYSTEM (admin)
```

Scripted, without prompts:

```powershell
mystrelay.exe setup -yes -identity C:\path\to\UTC--...--<address> -passphrase "" -autostart logon
```

See [docs/WINDOWS.md](docs/WINDOWS.md) for browser configuration and
troubleshooting.

## Setup - Docker

```bash
cp .env.example .env      # set MYST_IDENTITY_FILE and MYST_IDENTITY_PASSPHRASE
make doctor               # preflight; prints the fix for anything that fails
make up
make verify               # proves traffic egresses somewhere that is not you
make autostart            # start at boot (systemd)
```

The node and the relay run as separate containers. The relay reaches the node
over an `internal: true` network with no default route, and is published through
a second bridge whose subnet is deliberately **not** NAT'd - so any packet it
tried to send to the internet directly would leave with an unroutable RFC1918
source and no reply could return.

> Requires `net.ipv4.ip_forward=1` on the host, or no container can reach the
> Mysterium network. `make doctor` checks it.

## Usage

```bash
curl --proxy socks5h://127.0.0.1:7362 https://api.ipify.org   # prefer socks5h
curl --proxy http://127.0.0.1:7360    https://api.ipify.org
curl -s 127.0.0.1:7348/status | jq
```

Firefox -> *Settings -> Network Settings -> Manual*: SOCKS5 `127.0.0.1:7362`, tick
**Proxy DNS when using SOCKS v5**.

Chrome / Edge follow the Windows system proxy (*Settings -> Network -> Proxy ->
`127.0.0.1:7360`*), or take `--proxy-server="socks5://127.0.0.1:7362"`.

The raw port is a straight pipe to one fixed destination, for clients that speak
no proxy protocol at all:

```ini
RELAY_RAW_LISTEN=127.0.0.1:7339
RELAY_RAW_TARGET=example.com:443
```

### Status

```jsonc
{
  "tunnel":      { "state": "up", "provider_id": "0x...", "country": "SG",
                   "ip_type": "residential", "exit_ip": "160.191.77.197" },
  "kill_switch": { "open": true, "strict": true, "active_streams": 3,
                   "admitted_total": 128, "refused_total": 4 },
  "healthy": true
}
```

`/healthz` returns `200` only while traffic is actually being carried, `503`
otherwise. `/metrics` serves the same counters in Prometheus format.

## Configuration

Environment variables, or `config.env` on a native install (the environment
wins). Full annotated list in [`.env.example`](.env.example).

| | |
|---|---|
| `MYST_IDENTITY_FILE` / `MYST_IDENTITY_PASSPHRASE` | the keystore and its passphrase (`_FILE` variants supported) |
| `RELAY_SOCKS_LISTEN` / `RELAY_HTTP_LISTEN` / `RELAY_RAW_LISTEN` | fixed ports |
| `RELAY_PROXY_USER` / `RELAY_PROXY_PASS` | optional proxy auth, both frontends |
| `RELAY_KILLSWITCH_STRICT` | `true` also tears down streams already open |
| `EXIT_COUNTRY`, `EXIT_IP_TYPE`, `EXIT_MIN_QUALITY`, `EXIT_MAX_LATENCY` | exit policy allow-lists |
| `EXIT_PROVIDER_ID` | pin one provider, ignoring the filters |
| `RELAY_ROTATE_EVERY` | change exit on a timer |
| `RELAY_EXIT_CHECK_URL` | the egress proof; empty disables it (see caveats) |

## Kill switch

Three independent layers:

1. **No completable path.** The relay's networks have no default route (Docker),
   and the process has no direct-dial code path at all.
2. **No fallback in code.** Client traffic is dialled only through the tunnel
   abstraction, which returns *tunnel down* rather than dialling directly.
3. **Fail-closed policy.** A gate mirrors tunnel state: SOCKS5 answers
   `0x02 not allowed`, HTTP answers `503`, the raw port closes. In strict mode
   established streams are torn down too, so no stream outlives its exit.

The tunnel is not announced as usable until it has **completed a real request**
through the exit - a provider can establish a session cleanly and still carry
nothing (a dead DNS resolver answers every CONNECT with 502). Such a provider is
put on cooldown and another is chosen.

Measured on the reference setup: tunnel loss detected in **~1.3 s**, all ports
refusing, established streams torn down, reconnect to a fresh exit within ~15 s.

```bash
make verify-kill   # stops the node, checks every port refuses, restarts it
make test          # unit tests for the gate and the SOCKS5 frontend
```

## Caveats

- **UDP is not carried.** The node's listener is HTTP CONNECT, which tunnels TCP
  only. SOCKS5 `UDP ASSOCIATE` is answered *command not supported* rather than
  passed through - deliberately, since a client that silently fell back to
  direct UDP would be the exact leak this exists to prevent.
- **WebRTC and QUIC can leak around it.** They leave a browser over UDP,
  bypassing the proxy entirely. This is the most important thing to fix after
  install: `media.peerconnection.ice.proxy_only=true` and
  `network.http.http3.enable=false` in Firefox;
  `--disable-quic --force-webrtc-ip-handling-policy=disable_non_proxied_udp` in
  Chrome/Edge. Details in [docs/WINDOWS.md](docs/WINDOWS.md).
- **IPv6 is not carried.** The session is IPv4. Disable IPv6 on the client if
  its presence would produce a direct route.
- **Exit nodes are untrusted third parties.** They see your traffic exactly as
  any VPN provider would. Use TLS. Quality varies wildly; `EXIT_MIN_QUALITY` and
  `EXIT_IP_TYPE=residential` help.
- **The egress proof makes one request of its own** per reconnect, to
  `RELAY_EXIT_CHECK_URL` (default `https://api.ipify.org`), through the tunnel.
  Setting it empty removes that request and the guarantee it provides.
- **Sessions cost MYST.** The identity is charged per hour and per GiB by the
  provider. An empty identity means no tunnel, and the relay refuses to start.
- **The Windows binary is unsigned** - SmartScreen will warn. Verify the SHA-256
  published with the release.
- **One session at a time.** The `tunnel.Tunnel` interface admits a pool of
  several, but the shipped configuration runs one.

## Build from source

No third-party Go dependencies; the build needs no network.

```bash
make dist          # dist/mystrelay.exe and dist/mystrelay
make test vet
```

Layout: `internal/tunnel` is the egress interface (`internal/tunnel/myst`
implements it), `internal/frontend` the listeners, `internal/gate` the kill
switch, `internal/node` the managed-node fetcher and supervisor. Adding an
egress kind or a frontend protocol touches nothing else.

## Licence

[GPL-3.0](LICENSE). The Mysterium node it drives is GPL-3.0 too, but is
downloaded at runtime rather than bundled, so no node code lives in this
repository.
