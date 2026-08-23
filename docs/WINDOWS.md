# myst-dark-proxy on Windows 11

One executable. It downloads and supervises the Mysterium node itself, so there
is no Docker, no service to install by hand, and nothing to keep in sync.

```
mystrelay.exe setup     configure, fetch the node, register autostart
mystrelay.exe run       run it
mystrelay.exe status    what it is doing right now
mystrelay.exe verify    prove traffic leaves through the exit, not through you
mystrelay.exe update    check for a newer release and install it over itself
```

## 1. Install

Copy `mystrelay.exe` anywhere - `C:\Tools\myst-relay\` is fine - along with
your **Mysterium keystore file**: the `UTC--<date>--<address>` JSON your
Mysterium app wrote, normally under `%USERPROFILE%\.mysterium\keystore\`.

Open a terminal in that folder and run:

```
mystrelay.exe setup
```

It asks for the keystore path, its passphrase (the desktop app normally uses an
empty one - just press Enter), the ports, an optional country restriction, and
whether to start automatically. Then it downloads the node, checks its SHA-256
against a digest recorded in the binary, and unpacks it.

Everything lands in `%LOCALAPPDATA%\myst-relay\`:

```
config.env      settings - plain text, edit freely
identity.json   a copy of your keystore; the original is untouched
node\           the Mysterium node binary
node-data\      the node's database, keystore and logs
logs\relay.log  the relay's log
```

> **SmartScreen** will warn that the publisher is unknown, because the binary is
> unsigned. *More info -> Run anyway*.

To script it instead of answering prompts:

```
mystrelay.exe setup -yes -identity C:\path\to\UTC--...--1234567890abcdef1234567890abcdef12345678 -passphrase "" -autostart logon
```

## 2. Autostart

You choose during setup, and can change it later:

```
mystrelay.exe autostart install -scope logon    at sign-in, as you, no admin
mystrelay.exe autostart install -scope boot     before sign-in, as SYSTEM, needs admin
mystrelay.exe autostart status
mystrelay.exe autostart remove
```

Both register a Scheduled Task named `myst-relay`.

The **logon** task is registered against your account alone: it triggers at your
sign-in and runs as you, so an ordinary terminal is all it takes. It launches
the relay hidden, so no console window sits on your desktop; check
`logs\relay.log` to see what it is doing.

The **boot** task runs as the system account before anyone signs in. Only an
administrator can register that, so open the terminal with *Run as
administrator* for it - and for removing it again later.

Pick **logon** unless something that runs before you sign in needs the proxy.

## 3. Point your apps at it

| | |
|---|---|
| SOCKS5 | `127.0.0.1:7362` - prefer this, DNS is resolved at the exit node |
| HTTP | `127.0.0.1:7360` |
| Status | `http://127.0.0.1:7348/status` |

Both listen on loopback only: nothing else on your network can use them.

### Windows system proxy - covers Edge, Chrome and most apps

*Settings -> Network & Internet -> Proxy -> Manual proxy setup -> Use a proxy
server*, address `127.0.0.1`, port `7360`.

Anything using the Windows HTTP stack follows this, which is the simplest way to
catch "some apps". When the relay is down these apps fail to connect rather than
falling back to your own connection - that is the kill switch reaching them.

### Firefox - has its own proxy settings

*Settings -> Network Settings -> Manual proxy configuration*:

- SOCKS Host `127.0.0.1`, Port `7362`, **SOCKS v5**
- Tick **Proxy DNS when using SOCKS v5**

### Chrome / Edge - follow the system proxy

Either use the system proxy above, or start them with an explicit one:

```
chrome.exe --proxy-server="socks5://127.0.0.1:7362"
msedge.exe --proxy-server="socks5://127.0.0.1:7362"
```

Put that on a desktop shortcut if only some browsing should go through the
relay.

## 4. The settings that actually stop leaks

**This relay carries TCP only.** UDP is refused rather than passed through, so it
can never leak *through* the relay - but a browser can still send UDP directly,
around it, and that is what would expose your real address. Two features do
this, and both need turning off.

### Firefox

`about:config`, then set:

| Setting | Value | Why |
|---|---|---|
| `media.peerconnection.ice.proxy_only` | `true` | WebRTC may only use the proxy - no direct UDP |
| `network.http.http3.enable` | `false` | HTTP/3 is QUIC, which is UDP |
| `network.dns.disablePrefetch` | `true` | stops name lookups made outside the proxy |
| `network.predictor.enabled` | `false` | same, for its preconnect guesses |

Setting `media.peerconnection.enabled` to `false` disables WebRTC outright, if
you never use video calls in the browser.

### Chrome / Edge

Add to the shortcut alongside `--proxy-server`:

```
--disable-quic --force-webrtc-ip-handling-policy=disable_non_proxied_udp
```

To apply it to every launch instead of one shortcut, set a policy (needs admin -
`HKLM\SOFTWARE\Policies\Google\Chrome`, or `...\Microsoft\Edge` for Edge):

| Value | Type | Data |
|---|---|---|
| `QuicAllowed` | DWORD | `0` |
| `WebRtcIPHandling` | String | `disable_non_proxied_udp` |

### Check it

With the relay running, visit a leak-test page and confirm the address shown
matches `mystrelay.exe status`, and that no WebRTC address is your own.

## 5. Prove it works

```
mystrelay.exe verify
```

It fetches your real address directly, then again through each port, and fails
loudly if they match. `status` shows the live exit and the kill-switch counters.

## 6. What happens when the tunnel drops

Nothing gets through. New connections are refused on every port - SOCKS5 answers
*not allowed*, the HTTP proxy answers `503`, the raw port closes - and streams
that were already open are torn down. There is no code path in the relay that
dials the internet directly, so there is nothing to fall back to.

The relay picks a new exit and reconnects on its own - straight away if
something is still using it, otherwise the next time something connects - and
only reports itself healthy once it has completed a real request through the
new one.

## 7. It holds a session only while you are using it

Exit providers charge by the hour as well as by the gigabyte, so a session
nobody is using still spends your balance. The relay therefore opens one when
something first connects to a proxy port, and closes it again after 15 minutes
with nothing carried. Started at sign-in and first used at noon, it is billed
from noon.

Two things follow from that. The first page load after a break takes about
twelve seconds while a session is built - everything after it is as fast as the
exit allows. And `mystrelay.exe status` says `idle` most of the time, which is
the resting state, not a fault:

```
myst-relay  - idle: no session, the next client opens one

  tunnel       idle
  sessions     on demand, closed after 15m0s idle; 3 opened, nothing carried for 6m2s
```

To keep one session open permanently instead, put this in `config.env`:

```
RELAY_ON_DEMAND=false
RELAY_SESSION_IDLE_TIMEOUT=0
```

## 8. Keeping it up to date

```
mystrelay.exe update -check    is there a newer release?
mystrelay.exe update           download it, check it, install it over this one
```

Nothing checks on its own. A relay that is carrying traffic makes no request of
its own making, so the only thing that ever tells GitHub this machine exists is
you running the command above. The cost of that is that being out of date is
never noticed for you; if you would rather it were, `update -check` exits with
status 10 when a release is newer, which is enough for a scheduled task:

```
schtasks /create /tn myst-relay-update-check /sc weekly ^
  /tr "\"%LOCALAPPDATA%\myst-relay\mystrelay.exe\" update -check"
```

Task Scheduler keeps that code as the task's *Last Run Result*, where `0xA` is
"a newer release is published" and `0x0` is "nothing to do".

What `update` does, in order: read the newest release, download the binary for
this platform, check it against the `SHA256SUMS` published with the release, run
it once to confirm it works, and only then put it in place of the running
executable. If any step fails, nothing is replaced and the old binary is still
the one on disk.

Windows will not delete a running program, so the old one is renamed to
`mystrelay.exe.old` and deleted at the next start. That also means **the relay
already running is still the old version** - the command prints the two lines
that restart the scheduled task.

The Mysterium node follows the relay: each build carries the checksum of the
node release it was tested against and installs that at the next start. A
`MYST_NODE_VERSION` line in `config.env` overrides this and holds the node where
it is; `update` says so if it finds one.

Other flags: `-to v0.3.0` installs a named release, `-force` reinstalls the
current one, and `-via-relay` fetches through the running relay, so the download
leaves from the exit node rather than from your own address.

## 9. Troubleshooting

**Nothing loads and `status` says the relay is not running** - check
`%LOCALAPPDATA%\myst-relay\logs\relay.log`. Run `mystrelay.exe run` in a
terminal to watch it live.

**"port 127.0.0.1:4050 is already in use"** - another Mysterium app is running,
or a node from a previous run survived. The relay clears its own leftovers
automatically; if something else holds the port, close it or set
`MYST_TEQUILAPI_PORT` in `config.env` to a free one.

**`identity ... is "Unregistered"`** - register and fund the identity in the
Mysterium app first. This service uses an identity, it does not create one.

**Everything is refused and the log says "kill switch closed"** - that is the
kill switch working; the tunnel is down. The reason is on the same line. "Kill
switch armed" is different: there is simply no session yet, and the next
connection opens one.

**The first page after a pause takes ten seconds or so** - that is a session
being opened for it; see section 7. Raise `RELAY_SESSION_IDLE_TIMEOUT` if the
pauses in your day are longer than the timeout, or turn the behaviour off.

**Autostart is refused with "access is denied"** (in whatever language Windows
was installed in) - the boot scope needs a terminal opened with *Run as
administrator*; the logon scope does not. If the logon scope is refused, a
`myst-relay` task left by an earlier build is in the way: remove it from an
administrator prompt with `mystrelay.exe autostart remove`, then register it
again as yourself. Those builds registered the logon task without naming an
account, which Windows reads as "at the logon of anyone" and will not create
for a non-administrator. If you worked around that with an administrator
prompt, re-registering once gives you a task that is yours alone.

**It picked a slow country** - set `EXIT_COUNTRY=NL,DE` (or whatever you prefer)
in `config.env` and restart. `EXIT_IP_TYPE=residential` also helps against sites
that block hosting ranges.

## 10. Uninstall

```
mystrelay.exe autostart remove
```

Then delete `%LOCALAPPDATA%\myst-relay\` and the executable. Nothing is written
anywhere else: no services, no registry keys, no firewall rules.
