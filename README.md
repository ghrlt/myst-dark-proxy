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

The session follows the clients rather than the clock: none is opened until
something connects to a proxy port, and one nothing has used for 15 minutes is
closed again, because providers charge by the hour as well as by the gibibyte.
See [Sessions and what they cost](#sessions-and-what-they-cost).

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
mystrelay.exe update    # check for a newer release and install it over itself

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
make update               # pull a newer node image and rebuild the relay
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
  "sessions":    { "mode": "on demand", "idle_timeout": "15m0s",
                   "idle_for": "2m5s", "opened_total": 3 },
  "healthy": true, "ready": true
}
```

`healthy` is the strict reading: a session is up and the gate is open, so
traffic is moving this instant. `ready` also covers resting - no session,
because nothing has asked for one (see [Sessions and what they
cost](#sessions-and-what-they-cost)). `/healthz` reports **readiness**, so a
supervisor does not restart a relay for waiting: `200` while carrying or
resting, `503` when something is actually wrong. `/metrics` serves the same
counters in Prometheus format.

## Configuration

Environment variables, or `config.env` on a native install (the environment
wins). Full annotated list in [`.env.example`](.env.example).

| | |
|---|---|
| `MYST_IDENTITY_FILE` / `MYST_IDENTITY_PASSPHRASE` | the keystore and its passphrase (`_FILE` variants supported) |
| `RELAY_SOCKS_LISTEN` / `RELAY_HTTP_LISTEN` / `RELAY_RAW_LISTEN` | fixed ports |
| `RELAY_PROXY_USER` / `RELAY_PROXY_PASS` | optional proxy auth, both frontends |
| `RELAY_KILLSWITCH_STRICT` | `true` also tears down streams already open |
| `RELAY_ON_DEMAND` | open the session at the first client, not at startup |
| `RELAY_SESSION_IDLE_TIMEOUT` | close a session that has carried nothing this long (`0` keeps it) |
| `RELAY_ACTIVATION_TIMEOUT` | how long a client waits while its session is built |
| `EXIT_COUNTRY`, `EXIT_IP_TYPE`, `EXIT_MIN_QUALITY`, `EXIT_MAX_LATENCY` | exit policy allow-lists |
| `EXIT_PROVIDER_ID` | pin one provider, ignoring the filters |
| `RELAY_ROTATE_EVERY` | change exit on a timer |
| `RELAY_EXIT_CHECK_URL` | the egress proof; empty disables it (see caveats) |
| `MYST_IMAGE_TAG` / `MYST_NODE_VERSION` | pin the node - an image tag under Docker, a release version natively (see [Keeping it current](#keeping-it-current)) |

## Keeping it current

Two things move independently: this relay, and the Mysterium node it drives.
Neither updates itself. Nothing here contacts anything on its own - a relay
carrying traffic makes no request of its own making - so being out of date is
never noticed for you either. The commands below are the whole mechanism.

### The executable updates itself

```powershell
mystrelay.exe update -check   # is there a newer release? exit status 10 if so
mystrelay.exe update          # download, verify, install over this binary
```

In order: read the newest release, download the asset for this platform, check
it against the `SHA256SUMS` published beside it, **run it once** to confirm it
works, and only then move it over the running executable. A failure at any step
leaves the working binary exactly where it was. That checksum catches a
truncated or corrupted transfer, not a compromised release - the digest ships
with the file it describes - so TLS to GitHub is what says the release is this
project's.

Windows cannot delete a running program, so its predecessor is renamed to
`mystrelay.exe.old` and removed at the next start. Either way the relay
*already running* is still the old version until it is restarted; the command
prints how.

The node version follows the relay: each build carries the checksum of the node
release it was tested against and fetches it at the next start. A
`MYST_NODE_VERSION` line in `config.env` overrides that and holds the node where
it is - `update` says so when it finds one.

`-to v0.3.0` installs a named release, `-force` reinstalls the current one, and
`-via-relay` fetches through the running relay, so the request leaves from the
exit node instead of from this machine.

### Docker updates by tag

The relay image is built from this checkout, so it moves when you `git pull`.
The node image comes from upstream, and `MYST_IMAGE_TAG` is the whole policy:

| | |
|---|---|
| unset, or `latest` | follows upstream; `make update` pulls whatever is newest |
| `1.39.5-alpine` | pinned; nothing moves until you edit `.env` |

```bash
make update-check   # what has moved: the node image, this checkout vs releases
make update         # pull, rebuild, recreate what changed
```

Upstream tags releases with an `-alpine` suffix and publishes no bare number, so
`1.39.5-alpine` is a tag and `1.39.5` is not. Pinning is the safer setting for a
relay whose behaviour you have verified: proxy mode is undocumented, and an
upstream release can change it without saying so. Run `make verify` after an
update - that is the check that it did not.

## Kill switch

Three independent layers:

1. **No completable path.** The relay's networks have no default route (Docker),
   and the process has no direct-dial code path at all.
2. **No fallback in code.** Client traffic is dialled only through the tunnel
   abstraction, which returns *tunnel down* rather than dialling directly.
3. **Fail-closed policy.** A gate mirrors tunnel state: SOCKS5 answers
   `0x02 not allowed`, HTTP answers `503`, the raw port closes. In strict mode
   established streams are torn down too, so no stream outlives its exit.

While sessions are opened on demand the shut gate has two flavours. *Armed* is
shut with a session on its way, and a client that arrives waits for the one its
own arrival started. *Closed* is a tunnel that failed, and refuses at once. The
difference is decided when the client arrives, so a failure never turns into a
minute of hope - and nothing is carried in either state.

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

## Sessions and what they cost

A Mysterium session is billed **per hour as well as per gibibyte**, so one that
nobody is using still spends the identity's balance. By default the relay
therefore holds a session only while something is using it: none is opened
until a client connects to a proxy port, and one that has carried nothing for
`RELAY_SESSION_IDLE_TIMEOUT` (15m) is closed again. A relay started at boot and
first used at noon is billed from noon.

Measured against node 1.39.5 on 2026-08-22, at the price every provider was
advertising that day - 0.000445 MYST/hour and 3.56 MYST/GiB, MYST at $0.11:

| | |
|---|---|
| holding an idle session | 0.000445 MYST/h, plus ~13 KiB/h of keepalives (0.000045) |
| | = **0.00049 MYST/h**, about 0.35 MYST (~$0.04) per month held open |
| opening one | ~8 KiB and ~12 s - WireGuard handshake plus the egress proof |
| | = **0.000027 MYST**, what ~3.3 minutes of idling costs |

So dropping an idle session pays for its own reconnect within minutes. The
absolute numbers are small - a month of idling costs about what 100 MiB of
traffic does - but so is light use: for a relay that moves a few hundred MiB a
month, holding the tunnel open around the clock is most of the bill.

What it costs instead is latency: the first request after a lull waits while a
session is built - ~12 s on the reference setup, more when the first provider
tried turns out to be unusable and another has to be picked, bounded by
`RELAY_ACTIVATION_TIMEOUT` (60s). Only a session that is actually coming is
worth waiting for: **a tunnel that has failed refuses immediately**, in 4 ms
measured, so the kill switch reacts exactly as fast as it did before.

```bash
RELAY_ON_DEMAND=false            # connect at startup, as earlier versions did
RELAY_SESSION_IDLE_TIMEOUT=0     # and never close it
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
  provider, which is why sessions follow clients by default - see [Sessions and
  what they cost](#sessions-and-what-they-cost). An empty identity means no
  tunnel, and the relay refuses to start.
- **The Windows binary is unsigned** - SmartScreen will warn. Verify the SHA-256
  published with the release; `mystrelay.exe update` makes that check itself,
  every time.
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
switch, `internal/node` the managed-node fetcher and supervisor,
`internal/selfupdate` the release check and the in-place install. Adding an
egress kind or a frontend protocol touches nothing else.

## Licence

[GPL-3.0](LICENSE). The Mysterium node it drives is GPL-3.0 too, but is
downloaded at runtime rather than bundled, so no node code lives in this
repository.
