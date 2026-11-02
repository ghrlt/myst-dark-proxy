#!/usr/bin/env bash
# Update the Docker deployment.
#
#   scripts/update.sh           pull a newer node image, rebuild the relay,
#                               and recreate whatever changed
#   scripts/update.sh --check   report what is available and change nothing
#
# Two halves move independently:
#
#   the node    mysteriumnetwork/myst:$MYST_IMAGE_TAG. The default, "latest",
#               follows upstream and moves whenever this pulls. A version tag -
#               1.39.9-alpine - never moves until you edit .env, which is the
#               point of pinning: an update happens when you say so.
#   the relay   built from this checkout, so it moves with git, not with this
#               script. The native binary updates itself instead:
#               "mystrelay update".
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
[ -f .env ] && { set -a; . ./.env; set +a; }

COMPOSE=(docker compose)
REPO=${RELAY_REPO:-ghrlt/myst-dark-proxy}
TAG=${MYST_IMAGE_TAG:-latest}
IMAGE="mysteriumnetwork/myst:${TAG}"
HUB="https://hub.docker.com/v2/repositories/mysteriumnetwork/myst"

CHECK=0
case "${1:-}" in
  --check) CHECK=1 ;;
  "")      ;;
  *)       echo "usage: $0 [--check]" >&2; exit 2 ;;
esac

ok()    { printf '  \033[32m✓\033[0m %s\n' "$1"; }
info()  { printf '  - %s\n' "$1"; }
warn()  { printf '  \033[33m!\033[0m %s\n' "$1"; [ $# -gt 1 ] && printf '      %s\n' "$2"; }
head_() { printf '\n\033[1m%s\033[0m\n' "$1"; }
UPDATES=0

command -v docker >/dev/null 2>&1 || { echo "docker is not installed" >&2; exit 1; }

# The digest of the image this machine holds for a tag. Multi-arch images
# report the digest of the manifest list, which is what the registry indexes.
local_digest() {
  docker image inspect --format '{{if .RepoDigests}}{{index .RepoDigests 0}}{{end}}' "$1" 2>/dev/null | cut -d@ -f2
}

# The digest the registry currently serves for that tag.
hub_digest() {
  command -v curl >/dev/null 2>&1 || return 0
  curl -fsS --max-time 15 "$HUB/tags/$TAG" 2>/dev/null |
    python3 -c 'import json,sys; print(json.load(sys.stdin).get("digest",""))' 2>/dev/null
}

# The newest version tag published, for moving a pin forward. Upstream tags
# releases as <version>-alpine; there is no bare <version> tag.
newest_tag() {
  command -v curl >/dev/null 2>&1 || return 0
  curl -fsS --max-time 15 "$HUB/tags?page_size=50&ordering=last_updated" 2>/dev/null |
    python3 -c '
import json,re,sys
tags=[t["name"] for t in json.load(sys.stdin).get("results",[])]
print(next((t for t in tags if re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+-alpine", t)), ""))' 2>/dev/null
}

short() { [ -n "${1:-}" ] && printf '%s' "${1:0:19}..." || printf '%s' "unknown"; }

# ---- the relay ------------------------------------------------------------
head_ "The relay (built from this checkout)"
HAVE=$(git describe --tags --always --dirty 2>/dev/null)
[ -n "$HAVE" ] && info "this checkout is $HAVE" || warn "not a git checkout, so there is nothing to compare"
LATEST=""
if command -v curl >/dev/null 2>&1; then
  LATEST=$(curl -fsS --max-time 15 -H 'Accept: application/vnd.github+json' \
      "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null |
    python3 -c 'import json,sys; print(json.load(sys.stdin).get("tag_name",""))' 2>/dev/null)
fi
if [ -z "$LATEST" ]; then
  warn "could not ask GitHub what the latest release is"
elif [ -z "$HAVE" ]; then
  info "the latest release is $LATEST"
elif [ "$HAVE" = "$LATEST" ]; then
  ok "on the latest release ($LATEST)"
elif [ "${HAVE%%-*}" = "$LATEST" ]; then
  ok "$LATEST plus local commits ($HAVE)"
else
  UPDATES=$((UPDATES+1))
  warn "this checkout is ${HAVE%%-*}, the latest release is $LATEST" \
       "git pull to move onto it, then run this again to rebuild"
fi

# ---- the node image -------------------------------------------------------
head_ "The node image"
HELD=$(local_digest "$IMAGE")
SERVED=$(hub_digest)
case "$TAG" in
  latest|latest-*)
    info "$IMAGE - floating, so it moves whenever this pulls" ;;
  *)
    info "$IMAGE - pinned, so it moves only when you edit MYST_IMAGE_TAG in .env" ;;
esac
info "held here       $(short "$HELD")"
info "in the registry $(short "$SERVED")"
if [ -n "$HELD" ] && [ -n "$SERVED" ] && [ "$HELD" != "$SERVED" ]; then
  UPDATES=$((UPDATES+1))
  warn "the registry serves a different image for :$TAG than this machine holds"
elif [ -n "$HELD" ] && [ "$HELD" = "$SERVED" ]; then
  ok "up to date with the registry"
fi
NEWEST=$(newest_tag)
if [ -n "$NEWEST" ]; then
  case "$TAG" in
    latest|latest-*) info "the newest version tag is $NEWEST - put it in MYST_IMAGE_TAG to pin it" ;;
    "$NEWEST")       ok "pinned to the newest version tag" ;;
    *)               UPDATES=$((UPDATES+1))
                     warn "pinned to $TAG; the newest version tag is $NEWEST" \
                          "move the pin with: MYST_IMAGE_TAG=$NEWEST in .env" ;;
  esac
fi

if [ "$CHECK" = "1" ]; then
  head_ "Result"
  if [ "$UPDATES" -eq 0 ]; then
    printf '  nothing to update\n\n'
  else
    printf '  %d thing(s) can be updated; apply with: make update\n\n' "$UPDATES"
  fi
  exit 0
fi

# ---- apply ----------------------------------------------------------------
head_ "Updating"
printf '  containers that change are recreated, which ends any session in flight\n\n'

"${COMPOSE[@]}" pull myst || exit 1
# --pull also refreshes the Go image the relay is compiled with, so a rebuild
# is not stuck on a base image cached months ago.
"${COMPOSE[@]}" build --pull relay || exit 1
"${COMPOSE[@]}" up -d || exit 1

head_ "Now running"
AFTER=$(local_digest "$IMAGE")
if [ -n "$HELD" ] && [ -n "$AFTER" ] && [ "$HELD" != "$AFTER" ]; then
  ok "node image moved to $(short "$AFTER")"
else
  info "node image unchanged $(short "$AFTER")"
fi
"${COMPOSE[@]}" ps
printf '\n  check it carries traffic again with: make verify\n\n'
