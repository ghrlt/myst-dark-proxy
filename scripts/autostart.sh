#!/usr/bin/env bash
# Auto-start on boot, via systemd.
#
#   scripts/autostart.sh enable [--user]   install + enable the unit
#   scripts/autostart.sh disable [--user]  disable + remove it
#   scripts/autostart.sh status [--user]   show what is installed
#
# The system unit (default) starts the relay at boot, before anyone logs in.
# The --user unit starts at login unless you also enable lingering:
#   sudo loginctl enable-linger $USER
set -euo pipefail

cd "$(dirname "$0")/.." || exit 1
WORKDIR=$(pwd -P)
UNIT=myst-relay.service
TEMPLATE=deploy/myst-relay.service.in
DOCKER=$(command -v docker || echo /usr/bin/docker)

ACTION=${1:-status}
SCOPE=system
for arg in "$@"; do
  [ "$arg" = "--user" ] && SCOPE=user
done

if [ "$SCOPE" = "user" ]; then
  UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  SYSTEMCTL=(systemctl --user)
  SUDO=()
else
  UNIT_DIR=/etc/systemd/system
  SYSTEMCTL=(systemctl)
  SUDO=(sudo)
fi

render() {
  sed -e "s|__WORKDIR__|$WORKDIR|g" -e "s|__DOCKER__|$DOCKER|g" "$TEMPLATE"
}

case "$ACTION" in
  enable)
    [ -f .env ] || { echo "error: .env is missing — cp .env.example .env first" >&2; exit 1; }
    echo "installing $UNIT ($SCOPE scope) for $WORKDIR"
    if [ "$SCOPE" = "user" ]; then
      mkdir -p "$UNIT_DIR"
      render > "$UNIT_DIR/$UNIT"
      # A user unit has no multi-user.target.
      sed -i 's|^WantedBy=multi-user.target|WantedBy=default.target|' "$UNIT_DIR/$UNIT"
      sed -i '/^Requires=docker.service/d;/^After=docker.service/s|.*|After=network-online.target|' "$UNIT_DIR/$UNIT"
    else
      render | "${SUDO[@]}" tee "$UNIT_DIR/$UNIT" >/dev/null
    fi
    "${SUDO[@]}" "${SYSTEMCTL[@]}" daemon-reload
    "${SUDO[@]}" "${SYSTEMCTL[@]}" enable --now "$UNIT"
    echo
    "${SUDO[@]}" "${SYSTEMCTL[@]}" --no-pager status "$UNIT" || true
    if [ "$SCOPE" = "user" ]; then
      echo
      echo "note: user units only start at login. For boot-time start, run:"
      echo "  sudo loginctl enable-linger $USER"
    fi
    ;;

  disable)
    "${SUDO[@]}" "${SYSTEMCTL[@]}" disable --now "$UNIT" 2>/dev/null || true
    "${SUDO[@]}" rm -f "$UNIT_DIR/$UNIT"
    "${SUDO[@]}" "${SYSTEMCTL[@]}" daemon-reload
    echo "$UNIT removed from $UNIT_DIR"
    ;;

  status)
    if [ -f "$UNIT_DIR/$UNIT" ]; then
      echo "installed: $UNIT_DIR/$UNIT"
      "${SUDO[@]}" "${SYSTEMCTL[@]}" --no-pager status "$UNIT" || true
    else
      HINT="scripts/autostart.sh enable"
      [ "$SCOPE" = "user" ] && HINT="$HINT --user"
      echo "not installed ($SCOPE scope). Enable with: $HINT"
    fi
    ;;

  *)
    echo "usage: $0 {enable|disable|status} [--user]" >&2
    exit 2
    ;;
esac
