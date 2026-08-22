#!/usr/bin/env bash
# Preflight checks. Everything here is read-only; nothing is changed for you,
# but every failure prints the exact command that fixes it.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

PASS=0
FAIL=0
WARN=0

ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; PASS=$((PASS+1)); }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$1"; [ $# -gt 1 ] && printf '      fix: %s\n' "$2"; FAIL=$((FAIL+1)); }
warn() { printf '  \033[33m!\033[0m %s\n' "$1"; [ $# -gt 1 ] && printf '      %s\n' "$2"; WARN=$((WARN+1)); }
head_() { printf '\n\033[1m%s\033[0m\n' "$1"; }

head_ "Container runtime"
if command -v docker >/dev/null 2>&1; then
  ok "docker present ($(docker --version | cut -d, -f1))"
  if docker info >/dev/null 2>&1; then
    ok "docker daemon reachable"
  else
    bad "docker daemon not reachable" "sudo systemctl start docker  (and: sudo usermod -aG docker $USER)"
  fi
else
  bad "docker not installed" "install docker and docker-compose-plugin"
fi

if docker compose version >/dev/null 2>&1; then
  ok "docker compose v2 present ($(docker compose version --short 2>/dev/null))"
else
  bad "docker compose v2 missing" "install the docker-compose-plugin package"
fi

head_ "Host networking"
FORWARD=$(cat /proc/sys/net/ipv4/ip_forward 2>/dev/null || echo 0)
if [ "$FORWARD" = "1" ]; then
  ok "net.ipv4.ip_forward=1"
else
  bad "net.ipv4.ip_forward=0 - containers have no outbound connectivity" \
      "sudo sysctl -w net.ipv4.ip_forward=1   (persist: echo 'net.ipv4.ip_forward=1' | sudo tee /etc/sysctl.d/99-docker-forward.conf)"
fi

if docker info >/dev/null 2>&1; then
  # --entrypoint is required: the image's entrypoint hands its arguments to
  # myst, so without it this runs the node instead of a shell and always fails.
  if docker run --rm --network bridge --entrypoint sh mysteriumnetwork/myst:latest \
       -c 'wget -q -T 10 -O /dev/null "https://discovery.mysterium.network/api/v4/proposals?service_type=wireguard"' >/dev/null 2>&1; then
    ok "containers can reach the Mysterium discovery API"
  else
    warn "a container could not reach discovery.mysterium.network" \
         "usually the ip_forward problem above; also check any host firewall on the docker bridge"
  fi
fi

head_ "Configuration"
if [ -f .env ]; then
  ok ".env present"
  set -a; . ./.env 2>/dev/null; set +a
else
  bad ".env missing" "cp .env.example .env && \$EDITOR .env"
fi

if [ -n "${MYST_IDENTITY_FILE:-}" ]; then
  if [ -r "$MYST_IDENTITY_FILE" ]; then
    ADDR=$(python3 -c "import json,sys;print(json.load(open(sys.argv[1])).get('address',''))" "$MYST_IDENTITY_FILE" 2>/dev/null)
    if [ -n "$ADDR" ]; then
      ok "identity file readable, address 0x$ADDR"
      OWNER=$(stat -c '%u:%g' "$MYST_IDENTITY_FILE")
      WANT="${RELAY_UID:-1000}:${RELAY_GID:-1000}"
      MODE=$(stat -c '%a' "$MYST_IDENTITY_FILE")
      if [ "$OWNER" = "$WANT" ] || [ "${MODE: -1}" -ge 4 ] 2>/dev/null; then
        ok "the relay's uid ($WANT) can read it (owner $OWNER, mode $MODE)"
      else
        bad "the relay runs as $WANT but the identity file is owned by $OWNER mode $MODE" \
            "set RELAY_UID=${OWNER%%:*} and RELAY_GID=${OWNER##*:} in .env"
      fi
    else
      bad "identity file is not a keystore JSON (no 'address' field)" "point MYST_IDENTITY_FILE at a UTC--...--<address> keystore file"
    fi
  else
    bad "identity file not readable: $MYST_IDENTITY_FILE" "check the path and permissions"
  fi
else
  bad "MYST_IDENTITY_FILE is not set" "set it in .env"
fi

if [ -z "${MYST_IDENTITY_PASSPHRASE+x}" ] && [ -z "${MYST_IDENTITY_PASSPHRASE_FILE:-}" ]; then
  bad "MYST_IDENTITY_PASSPHRASE is not set" "set it in .env (an empty value is valid: MYST_IDENTITY_PASSPHRASE=)"
else
  ok "identity passphrase configured"
fi

head_ "Fixed ports"
for spec in \
  "SOCKS5:${RELAY_SOCKS_PORT:-7362}" \
  "HTTP:${RELAY_HTTP_PORT:-7360}" \
  "raw:${RELAY_RAW_PORT:-7339}" \
  "health:${RELAY_HEALTH_PORT:-9090}"; do
  name=${spec%%:*}; port=${spec##*:}
  holder=$(ss -ltnp 2>/dev/null | awk -v p=":$port\$" '$4 ~ p {print $NF; exit}')
  if [ -z "$holder" ]; then
    ok "$name port $port free"
  elif docker ps --format '{{.Ports}}' 2>/dev/null | grep -q ":$port->"; then
    ok "$name port $port held by this stack"
  else
    bad "$name port $port already in use by $holder" "change RELAY_${name}_PORT in .env or stop the other process"
  fi
done

head_ "Result"
printf '  %d passed, %d warnings, %d failed\n\n' "$PASS" "$WARN" "$FAIL"
[ "$FAIL" -eq 0 ]
