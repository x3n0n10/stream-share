#!/bin/sh
# stream-share VPN watchdog (example)
#
# stream-share only *reports* provider health. This script is the external half
# that acts on it: on a schedule it asks stream-share whether the IPTV provider
# is blocking our current VPN egress IP (the provider returns HTTP 456), and if
# so it cycles gluetun onto a new server and re-checks — repeating until the new
# IP is accepted or the attempt budget runs out.
#
# It deliberately lives OUTSIDE stream-share so that app has no dependency on
# gluetun or any particular VPN. Everything VPN-specific is here.
#
# Requirements in the container running this script: a POSIX shell, `curl`, and
# either `wget` or `curl` (both provided by the alpine/curl image below). It must
# be able to reach:
#   - stream-share's internal API  (STREAM_SHARE_URL)
#   - gluetun's control server      (GLUETUN_URL)
#
# Configure via environment variables (see docker-compose.snippet.yml):
STREAM_SHARE_URL="${STREAM_SHARE_URL:-http://localhost:8080}"
INTERNAL_API_KEY="${INTERNAL_API_KEY:?set INTERNAL_API_KEY to stream-share's key}"
GLUETUN_URL="${GLUETUN_URL:-http://localhost:8000}"
GLUETUN_APIKEY="${GLUETUN_APIKEY:-}"          # only if gluetun's control server has auth
CHECK_TIMES="${CHECK_TIMES:-04:00,16:00}"     # local times to run, comma-separated HH:MM
MAX_RECONNECTS="${MAX_RECONNECTS:-5}"         # give up after this many server switches
SETTLE_SECONDS="${SETTLE_SECONDS:-20}"        # wait for the tunnel to come up after a cycle

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"; }

# gluetun_auth_header echoes a curl -H argument when an API key is configured.
gluetun_curl() {
  if [ -n "$GLUETUN_APIKEY" ]; then
    curl -fsS -H "X-API-Key: $GLUETUN_APIKEY" "$@"
  else
    curl -fsS "$@"
  fi
}

# provider_status forces a fresh probe and prints stream-share's verdict:
# "healthy", "blocked", "error", "unknown", or "disabled".
provider_status() {
  curl -fsS -H "X-API-Key: $INTERNAL_API_KEY" \
    "$STREAM_SHARE_URL/api/internal/health" 2>/dev/null \
    | sed -n 's/.*"status" *: *"\([a-z]*\)".*/\1/p'
}

# public_ip prints gluetun's current public IP, for logging that the exit changed.
public_ip() {
  gluetun_curl "$GLUETUN_URL/v1/publicip/ip" 2>/dev/null \
    | sed -n 's/.*"public_ip" *: *"\([^"]*\)".*/\1/p'
}

# cycle_vpn stops and restarts gluetun's tunnel, which reselects a server per
# gluetun's own SERVER_* / VPN_* configuration (so those must allow more than one
# server for the IP to actually change).
cycle_vpn() {
  log "Cycling gluetun (old IP: $(public_ip))"
  gluetun_curl -X PUT -d '{"status":"stopped"}' "$GLUETUN_URL/v1/openvpn/status" >/dev/null || \
    log "warning: gluetun stop request failed"
  sleep 3
  gluetun_curl -X PUT -d '{"status":"running"}' "$GLUETUN_URL/v1/openvpn/status" >/dev/null || \
    log "warning: gluetun start request failed"
}

# heal probes once and, while blocked, cycles the VPN up to MAX_RECONNECTS times.
heal() {
  status="$(provider_status)"
  log "Provider status: ${status:-<unreachable>}"

  case "$status" in
    healthy|disabled) return 0 ;;
    blocked) ;;                         # the only case we act on
    *)
      # error / unknown / unreachable: reconnecting won't reliably help
      # (provider outage, bad probe channel, stream-share still starting).
      log "Not a block; leaving the VPN alone."
      return 0
      ;;
  esac

  i=1
  while [ "$i" -le "$MAX_RECONNECTS" ]; do
    log "Blocked — reconnect attempt $i/$MAX_RECONNECTS"
    cycle_vpn
    sleep "$SETTLE_SECONDS"
    log "New IP: $(public_ip)"
    status="$(provider_status)"
    if [ "$status" = "healthy" ]; then
      log "Recovered after $i reconnect(s)."
      return 0
    fi
    log "Still $status after attempt $i."
    i=$((i + 1))
  done
  log "Gave up after $MAX_RECONNECTS reconnects; provider still $status."
  return 1
}

# seconds_until returns the seconds to wait until the next HH:MM in CHECK_TIMES.
seconds_until_next() {
  now=$(date +%s)
  best=""
  IFS=','
  for t in $CHECK_TIMES; do
    t=$(echo "$t" | tr -d ' ')
    [ -z "$t" ] && continue
    target=$(date -d "today $t" +%s 2>/dev/null) || continue
    [ "$target" -le "$now" ] && target=$((target + 86400))
    if [ -z "$best" ] || [ "$target" -lt "$best" ]; then best=$target; fi
  done
  unset IFS
  [ -z "$best" ] && best=$((now + 43200))   # fallback: 12h
  echo $((best - now))
}

log "VPN watchdog starting. Checking at: $CHECK_TIMES"
# Run once at startup, then on the schedule.
heal || true
while true; do
  wait_s="$(seconds_until_next)"
  log "Next check in ${wait_s}s."
  sleep "$wait_s"
  heal || true
done
