#!/bin/sh
# stream-share VPN watchdog (EXAMPLE)
#
# stream-share only *reports* provider health. This script is the external half
# that acts on it: on a schedule it asks stream-share whether the IPTV provider
# is blocking our current VPN egress IP, and if so it reconnects the VPN onto a
# new server and re-checks — repeating until the new IP is accepted or the
# attempt budget runs out.
#
# It lives OUTSIDE stream-share on purpose, so that app has no dependency on any
# VPN. Everything VPN-specific is here. This example targets gluetun's control
# server, but gluetun is just one option — swap cycle_vpn()/provider-independent
# bits for whatever VPN you run.
#
# The gluetun control sequence here mirrors the stream-share-dashboard project's
# reconnect logic: stop, confirm stopped, confirm traffic actually stopped
# routing, start, confirm running, then poll until a usable public IP comes back
# (gluetun can report "running" before it has re-resolved its IP).
#
# Requirements: a POSIX shell and `curl` (both in the alpine/curl image used by
# docker-compose.snippet.yml). It must be able to reach stream-share's internal
# API (STREAM_SHARE_URL) and the VPN's control server (GLUETUN_URL).

# --- stream-share ------------------------------------------------------------
STREAM_SHARE_URL="${STREAM_SHARE_URL:-http://localhost:8080}"
INTERNAL_API_KEY="${INTERNAL_API_KEY:?set INTERNAL_API_KEY to stream-share's key}"

# --- VPN control (gluetun example) -------------------------------------------
GLUETUN_URL="${GLUETUN_URL:-http://localhost:8000}"
# Auth: gluetun's control server supports HTTP Basic Auth or an API key,
# depending on its roles config. Set whichever matches; if both are set, Basic
# Auth wins (a client maps to exactly one auth method).
GLUETUN_USER="${GLUETUN_USER:-}"
GLUETUN_PASSWORD="${GLUETUN_PASSWORD:-}"
GLUETUN_API_KEY="${GLUETUN_API_KEY:-}"
# /v1/vpn/status is gluetun's current unified status/start/stop endpoint (both
# OpenVPN and WireGuard). Override to /v1/openvpn/status for older gluetun.
GLUETUN_STATUS_PATH="${GLUETUN_STATUS_PATH:-/v1/vpn/status}"

# --- behaviour ---------------------------------------------------------------
CHECK_TIMES="${CHECK_TIMES:-04:00,16:00}"       # local times to run, comma-separated HH:MM
MAX_RECONNECTS="${MAX_RECONNECTS:-5}"           # give up after this many server switches
RECONNECT_TIMEOUT="${RECONNECT_TIMEOUT:-45}"    # per-cycle budget (seconds) to reach a usable IP
DISCONNECT_TIMEOUT="${DISCONNECT_TIMEOUT:-10}"  # max seconds to confirm the old tunnel actually dropped

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"; }

# gluetun_curl applies whichever auth is configured (Basic wins over API key).
gluetun_curl() {
  if [ -n "$GLUETUN_USER" ] && [ -n "$GLUETUN_PASSWORD" ]; then
    curl -fsS -u "$GLUETUN_USER:$GLUETUN_PASSWORD" "$@"
  elif [ -n "$GLUETUN_API_KEY" ]; then
    curl -fsS -H "X-Api-Key: $GLUETUN_API_KEY" "$@"
  else
    curl -fsS "$@"
  fi
}

# json_str extracts a string field value from a small JSON body.
json_str() { sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" <<EOF
$1
EOF
}

# provider_status forces a fresh probe and prints stream-share's verdict:
# healthy | blocked | error | unknown | disabled.
provider_status() {
  body="$(curl -fsS -H "X-API-Key: $INTERNAL_API_KEY" "$STREAM_SHARE_URL/api/internal/health" 2>/dev/null)"
  json_str "$body" status
}

# vpn_status / public_ip read gluetun's control server.
vpn_status() { json_str "$(gluetun_curl "$GLUETUN_URL$GLUETUN_STATUS_PATH" 2>/dev/null)" status; }
public_ip() {
  body="$(gluetun_curl "$GLUETUN_URL/v1/publicip/ip" 2>/dev/null)" || return 1
  ip="$(json_str "$body" public_ip)"; [ -z "$ip" ] && ip="$(json_str "$body" ip)"
  [ -n "$ip" ] || return 1
  echo "$ip"
}

set_vpn() { gluetun_curl -X PUT -H "Content-Type: application/json" -d "{\"status\":\"$1\"}" "$GLUETUN_URL$GLUETUN_STATUS_PATH" >/dev/null; }

# wait_status polls until gluetun reports the desired status or the deadline.
wait_status() { # $1=desired $2=deadline_epoch
  while [ "$(date +%s)" -lt "$2" ]; do
    [ "$(vpn_status)" = "$1" ] && return 0
    sleep 1
  done
  return 1
}

# wait_disconnected waits until the public-IP probe starts failing, i.e. traffic
# genuinely stopped routing through the tunnel. Bounded on its own budget: some
# setups don't firewall non-VPN traffic, so this signal may never come — proceed
# anyway rather than blocking the whole reconnect on it.
wait_disconnected() {
  sub_deadline=$(( $(date +%s) + DISCONNECT_TIMEOUT ))
  while [ "$(date +%s)" -lt "$sub_deadline" ]; do
    public_ip >/dev/null 2>&1 || return 0
    sleep 1
  done
}

# cycle_vpn reconnects the VPN and blocks until a usable public IP returns (or
# the per-cycle budget expires). Reselecting a *different* server depends on the
# VPN's own config allowing more than one — e.g. gluetun's SERVER_*/VPN_* vars.
cycle_vpn() {
  deadline=$(( $(date +%s) + RECONNECT_TIMEOUT ))
  log "Cycling VPN (old IP: $(public_ip 2>/dev/null || echo '?'))"

  set_vpn stopped || log "warning: stop request failed"
  wait_status stopped "$deadline" || log "warning: never confirmed 'stopped'"
  wait_disconnected

  set_vpn running || log "warning: start request failed"
  wait_status running "$deadline" || log "warning: never confirmed 'running'"

  while [ "$(date +%s)" -lt "$deadline" ]; do
    if new_ip="$(public_ip)"; then
      log "VPN back up (new IP: $new_ip)"
      return 0
    fi
    sleep 1
  done
  log "warning: no usable public IP within ${RECONNECT_TIMEOUT}s"
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
    status="$(provider_status)"
    if [ "$status" = "healthy" ]; then
      log "Recovered after $i reconnect(s)."
      return 0
    fi
    log "Still ${status:-<unreachable>} after attempt $i."
    i=$((i + 1))
  done
  log "Gave up after $MAX_RECONNECTS reconnects; provider still ${status:-<unreachable>}."
  return 1
}

# seconds_until_next returns the seconds to wait until the next HH:MM in CHECK_TIMES.
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
heal || true                                # run once at startup
while true; do
  wait_s="$(seconds_until_next)"
  log "Next check in ${wait_s}s."
  sleep "$wait_s"
  heal || true
done
