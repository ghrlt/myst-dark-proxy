#!/usr/bin/env bash
# End-to-end proof that the relay works and that the kill switch bites.
#
#   scripts/verify.sh          checks the relay carries traffic through the exit
#   scripts/verify.sh --kill   also drops the tunnel and proves traffic stops
#
# The kill-switch test interrupts the tunnel, so it is opt-in.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
[ -f .env ] && { set -a; . ./.env; set +a; }

BIND=${RELAY_BIND:-127.0.0.1}
SOCKS=${RELAY_SOCKS_PORT:-7362}
HTTP=${RELAY_HTTP_PORT:-7360}
RAW=${RELAY_RAW_PORT:-7339}
HEALTH=${RELAY_HEALTH_PORT:-7348}
AUTH=""
[ -n "${RELAY_PROXY_USER:-}" ] && AUTH="--proxy-user ${RELAY_PROXY_USER}:${RELAY_PROXY_PASS}"

FAIL=0
ok()  { printf '  \033[32m✓\033[0m %s\n' "$1"; }
bad() { printf '  \033[31m✗\033[0m %s\n' "$1"; FAIL=$((FAIL+1)); }
head_() { printf '\n\033[1m%s\033[0m\n' "$1"; }

IPSVC=${IPSVC:-https://api.ipify.org}

head_ "Baseline"
DIRECT=$(curl -s --max-time 15 "$IPSVC")
if [ -n "$DIRECT" ]; then
  ok "this host's public address is $DIRECT"
else
  bad "could not determine this host's public address; comparisons below are weakened"
fi

head_ "Health"
STATUS=$(curl -s --max-time 5 "http://${BIND}:${HEALTH}/status")
if [ -z "$STATUS" ]; then
  bad "health endpoint is not answering on ${BIND}:${HEALTH}"
else
  python3 - "$STATUS" <<'PY'
import json,sys
s=json.loads(sys.argv[1])
t=s["tunnel"]; k=s["kill_switch"]; sess=s.get("sessions",{})
# Three states, not two: carrying traffic, resting between clients with no
# session (which is what an on-demand relay does most of the day), or broken.
if s["healthy"]:
    mark = "\033[32m✓\033[0m"
elif s.get("ready"):
    mark = "-"
else:
    mark = "\033[31m✗\033[0m"
print(f"  {mark} healthy={s['healthy']} ready={s.get('ready', s['healthy'])} tunnel={t['state']} "
      f"exit={t.get('exit_ip','?')} country={t.get('country','?')} ip_type={t.get('ip_type','?')}")
if t.get("provider_id"):
    print(f"      provider {t['provider_id']}")
if sess:
    print(f"      sessions: {sess.get('mode','?')} idle_timeout={sess.get('idle_timeout','-')} "
          f"opened={sess.get('opened_total',0)} idle_for={sess.get('idle_for','-')}")
print(f"      kill switch: open={k['open']} strict={k['strict']} active={k['active_streams']} "
      f"admitted={k['admitted_total']} refused={k['refused_total']}")
PY
fi

head_ "SOCKS5 on ${BIND}:${SOCKS}"
# socks5h keeps name resolution at the exit node: a DNS leak would show up as a
# resolution happening here instead. The timeout allows for a session being
# built on the spot: with RELAY_ON_DEMAND this request is what opens one.
S_IP=$(curl -s --max-time 90 $AUTH --proxy "socks5h://${BIND}:${SOCKS}" "$IPSVC")
if ! printf '%s' "$S_IP" | grep -qE '^[0-9a-fA-F:.]+$'; then
  bad "no usable response through SOCKS5 (got '${S_IP:-empty}')"
elif [ "$S_IP" = "$DIRECT" ]; then
  bad "SOCKS5 egressed from this host's own address ($S_IP) - that is a leak"
else
  ok "SOCKS5 egressed from $S_IP (not $DIRECT)"
fi

head_ "HTTP proxy on ${BIND}:${HTTP}"
H_IP=$(curl -s --max-time 90 $AUTH --proxy "http://${BIND}:${HTTP}" "$IPSVC")
if ! printf '%s' "$H_IP" | grep -qE '^[0-9a-fA-F:.]+$'; then
  bad "no usable response through the HTTP proxy (got '${H_IP:-empty}')"
elif [ "$H_IP" = "$DIRECT" ]; then
  bad "HTTP proxy egressed from this host's own address ($H_IP) - that is a leak"
else
  ok "HTTP CONNECT egressed from $H_IP"
fi

H_PLAIN=$(curl -s --max-time 90 $AUTH --proxy "http://${BIND}:${HTTP}" "http://api.ipify.org")
# Must look like an address: the proxy reports its own errors in the body, and
# an error page is not a successful fetch.
if ! printf '%s' "$H_PLAIN" | grep -qE '^[0-9a-fA-F:.]+$'; then
  bad "plain HTTP through the proxy failed (got '${H_PLAIN:-empty}')"
elif [ "$H_PLAIN" = "$DIRECT" ]; then
  bad "plain HTTP egressed from this host's own address ($H_PLAIN) - that is a leak"
else
  ok "HTTP absolute-URI (non-CONNECT) egressed from $H_PLAIN"
fi

if [ -n "${RELAY_RAW_TARGET:-}" ]; then
  head_ "Raw forward ${BIND}:${RAW} -> ${RELAY_RAW_TARGET}"
  # Accepting a connection proves nothing: the relay accepts and then closes
  # when the kill switch is shut. A connection that stays open is one the exit
  # node actually completed to the target.
  if python3 - "$BIND" "$RAW" <<'PYEOF'
import socket, sys
try:
    s = socket.create_connection((sys.argv[1], int(sys.argv[2])), 90)
except Exception as e:
    print(f"connect failed: {e}"); sys.exit(1)
s.settimeout(4)
try:
    if s.recv(1) == b"":
        print("relay closed the connection: the target was never reached")
        sys.exit(1)
except socket.timeout:
    pass          # open and idle: the tunnel completed to the target
except Exception as e:
    print(f"stream error: {e}"); sys.exit(1)
sys.exit(0)
PYEOF
  then
    ok "raw port holds a tunnelled connection to ${RELAY_RAW_TARGET}"
  else
    bad "raw port did not open a tunnelled connection"
  fi
else
  head_ "Raw forward"
  printf '  - skipped: RELAY_RAW_TARGET is not set\n'
fi

if [ "${1:-}" = "--kill" ]; then
  head_ "Kill switch"
  echo "  stopping the myst node..."
  docker compose stop myst >/dev/null 2>&1
  sleep 12

  OUT=$(curl -s --max-time 20 $AUTH --proxy "socks5h://${BIND}:${SOCKS}" "$IPSVC" 2>&1)
  RC=$?
  if [ $RC -eq 0 ] && [ -n "$OUT" ]; then
    bad "traffic still flowed with the tunnel down (got '$OUT') - THE KILL SWITCH FAILED"
  else
    ok "SOCKS5 refused traffic with the tunnel down (curl exit $RC)"
  fi

  HOUT=$(curl -s -o /dev/null -w '%{http_code}' --max-time 20 $AUTH --proxy "http://${BIND}:${HTTP}" "$IPSVC" 2>&1)
  if [ "$HOUT" = "503" ] || [ -z "$HOUT" ] || [ "$HOUT" = "000" ]; then
    ok "HTTP proxy refused traffic with the tunnel down (status ${HOUT:-none})"
  else
    bad "HTTP proxy answered $HOUT with the tunnel down - THE KILL SWITCH FAILED"
  fi

  HEALTHRC=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://${BIND}:${HEALTH}/healthz")
  if [ "$HEALTHRC" = "503" ]; then
    ok "/healthz reports unhealthy while the tunnel is down"
  else
    bad "/healthz returned $HEALTHRC while the tunnel was down"
  fi

  echo "  restarting the myst node..."
  docker compose start myst >/dev/null 2>&1
  echo "  (the relay will reconnect on its own; watch: make logs)"
fi

head_ "Result"
if [ "$FAIL" -eq 0 ]; then
  printf '  \033[32mall checks passed\033[0m\n\n'
else
  printf '  \033[31m%d check(s) failed\033[0m\n\n' "$FAIL"
fi
exit "$FAIL"
