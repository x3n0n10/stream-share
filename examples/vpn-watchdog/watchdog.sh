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
# VPN. Everything VPN-specific is here. This example targets the gluetun control
# server, but gluetun is just one option — swap cycle_vpn()/provider-independent
# bits for whatever VPN you run.
#
# The gluetun control sequence is: stop, confirm stopped, start, confirm running.
# Once gluetun reports "running" we assume the tunnel is back and re-run the
# health check to decide whether the new server is unblocked — rather than
# polling gluetun's public-IP endpoint.
#
# Requirements: a POSIX shell and `curl` (both in the alpine/curl image used by
# docker-compose.snippet.yml). It must be able to reach the stream-share internal
# API (STREAM_SHARE_URL) and the VPN control server (GLUETUN_URL).
#
# NOTE: this file must use LF line endings. CRLF (Windows) endings make a POSIX
# shell fail with errors like `: not found` on blank lines and `elif unexpected`.
# The repo enforces LF via .gitattributes; if you edited it on Windows, run
# `sed -i 's/\r$//' watchdog.sh` (or `dos2unix watchdog.sh`).

# --- stream-share ------------------------------------------------------------
STREAM_SHARE_URL="${STREAM_SHARE_URL:-http://localhost:8080}"
INTERNAL_API_KEY="${INTERNAL_API_KEY:?set INTERNAL_API_KEY to the stream-share internal API key}"

# --- VPN control (gluetun example) -------------------------------------------
GLUETUN_URL="${GLUETUN_URL:-http://localhost:8000}"
# Auth: the gluetun control server supports HTTP Basic Auth or an API key,
# depending on its roles config. Set whichever matches; if both are set, Basic
# Auth wins (a client maps to exactly one auth method).
GLUETUN_USER="${GLUETUN_USER:-}"
GLUETUN_PASSWORD="${GLUETUN_PASSWORD:-}"
GLUETUN_API_KEY="${GLUETUN_API_KEY:-}"
# /v1/vpn/status is the current unified gluetun status/start/stop endpoint (both
# OpenVPN and WireGuard). Override to /v1/openvpn/status for older gluetun.
GLUETUN_STATUS_PATH="${GLUETUN_STATUS_PATH:-/v1/vpn/status}"

# --- behaviour ---------------------------------------------------------------
# CHECK_TIMES is the single schedule for hitting the provider: each run forces a
# fresh probe (which also refreshes the stream-share /healthz). When this watchdog
# is running, leave the stream-share HEALTHCHECK_TIMES empty so the provider is
# not probed twice on two schedules for the same information.
CHECK_TIMES="${CHECK_TIMES:-04:00,16:00}"       # local times to run, comma-separated HH:MM
MAX_RECONNECTS="${MAX_RECONNECTS:-5}"           # give up after this many server switches
RECONNECT_TIMEOUT="${RECONNECT_TIMEOUT:-45}"    # per-cycle budget (seconds) to confirm stopped then running

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

# fetch_health forces a fresh probe and sets HEALTH_STATUS / HEALTH_DETAIL.
# Verdicts: healthy | blocked | error | unknown | disabled.
#
# /api/internal/health returns HTTP 503 when the provider is blocked or errored
# (that 503 is the intended "unhealthy" signal for Docker's HEALTHCHECK, and the
# real verdict is in the body), so the body is read on both 200 and 503 — never
# pass curl -f, which would discard the body exactly when it says "blocked".
#
# An EMPTY HEALTH_STATUS is never a provider verdict: it means the watchdog could
# not get an answer from stream-share. HEALTH_DETAIL then explains why — a
# connection failure, an HTTP 401 from a wrong INTERNAL_API_KEY, a wrong URL, and
# so on — rather than the old opaque "<unreachable>". heal() never cycles the VPN
# on an empty status.
fetch_health() {
  HEALTH_STATUS=""
  HEALTH_DETAIL=""

  errf="$(mktemp 2>/dev/null || echo /tmp/wd_curl_err)"
  # -w appends the HTTP status on its own final line so we can read body and code
  # from one request without curl -f swallowing error bodies.
  resp="$(curl -sS -w '\n%{http_code}' -H "X-API-Key: $INTERNAL_API_KEY" \
    "$STREAM_SHARE_URL/api/internal/health" 2>"$errf")"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    HEALTH_DETAIL="cannot reach $STREAM_SHARE_URL: $(tr '\n' ' ' < "$errf" | sed 's/  */ /g;s/ *$//')"
    rm -f "$errf"
    return
  fi
  rm -f "$errf"

  code="$(printf '%s' "$resp" | tail -n1)"
  body="$(printf '%s' "$resp" | sed '$d')"
  case "$code" in
    200|503)
      HEALTH_STATUS="$(json_str "$body" status)"
      HEALTH_DETAIL="$(json_str "$body" detail)"
      [ -z "$HEALTH_STATUS" ] && HEALTH_DETAIL="unexpected response body from stream-share (HTTP $code)"
      ;;
    401) HEALTH_DETAIL="HTTP 401 unauthorized — INTERNAL_API_KEY does not match the stream-share key" ;;
    404) HEALTH_DETAIL="HTTP 404 — check STREAM_SHARE_URL points at stream-share" ;;
    *)   HEALTH_DETAIL="unexpected HTTP $code from stream-share" ;;
  esac
}

# vpn_status / public_ip read the gluetun control server.
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

# cycle_vpn reconnects the VPN: stop, confirm stopped, start, confirm running.
# Once gluetun reports "running" we treat the tunnel as back and return — the
# caller re-runs the health check to judge whether the new server is unblocked,
# so we do not poll the public-IP endpoint. Reselecting a *different* server
# depends on the VPN config allowing more than one — e.g. the gluetun
# SERVER_*/VPN_* vars. The public IP is fetched once, best-effort, only to log
# which exit we landed on.
cycle_vpn() {
  deadline=$(( $(date +%s) + RECONNECT_TIMEOUT ))
  log "Cycling VPN..."

  set_vpn stopped || log "warning: stop request failed"
  wait_status stopped "$deadline" || log "warning: never confirmed stopped"

  set_vpn running || log "warning: start request failed"
  if wait_status running "$deadline"; then
    log "VPN reports running (IP: $(public_ip 2>/dev/null || echo '?'))."
  else
    log "warning: never confirmed running within ${RECONNECT_TIMEOUT}s."
  fi
}

# heal probes once and, while blocked, cycles the VPN up to MAX_RECONNECTS times.
heal() {
  fetch_health
  log "Provider status: ${HEALTH_STATUS:-<unreachable>}${HEALTH_DETAIL:+ ($HEALTH_DETAIL)}"

  case "$HEALTH_STATUS" in
    healthy|disabled) return 0 ;;
    blocked) ;;                         # the only case we act on
    error|unknown)
      # A real provider verdict that reconnecting will not reliably fix (provider
      # outage, bad probe channel, stream-share still starting). Detail says which.
      log "Not a block (provider $HEALTH_STATUS); leaving the VPN alone."
      return 0
      ;;
    *)
      # Empty status: could not talk to stream-share (see detail). A watchdog
      # <-> stream-share problem is never a reason to cycle the VPN.
      log "Could not determine provider status; leaving the VPN alone."
      return 0
      ;;
  esac

  i=1
  while [ "$i" -le "$MAX_RECONNECTS" ]; do
    log "Blocked — reconnect attempt $i/$MAX_RECONNECTS"
    cycle_vpn
    fetch_health
    if [ "$HEALTH_STATUS" = "healthy" ]; then
      log "Recovered after $i reconnect(s)."
      return 0
    fi
    log "Still ${HEALTH_STATUS:-<unreachable>} after attempt $i."
    i=$((i + 1))
  done
  log "Gave up after $MAX_RECONNECTS reconnects; provider still ${HEALTH_STATUS:-<unreachable>}."
  return 1
}

# undec strips leading zeros so clock values like 08/09 are not misread as octal
# in shell arithmetic. Empty or all-zero input becomes 0.
undec() {
  v=$1
  while [ "${v#0}" != "$v" ]; do v=${v#0}; done
  [ -z "$v" ] && v=0
  echo "$v"
}

# time_to_min converts "H:M" / "HH:MM" to minutes since midnight (0..1439), or
# prints -1 when it is not a valid time.
time_to_min() {
  case "$1" in *:*) ;; *) echo -1; return ;; esac
  h=$(undec "${1%%:*}")
  m=$(undec "${1#*:}")
  case "$h$m" in *[!0-9]*) echo -1; return ;; esac
  if [ "$h" -ge 0 ] && [ "$h" -le 23 ] && [ "$m" -ge 0 ] && [ "$m" -le 59 ]; then
    echo $(( h * 60 + m ))
  else
    echo -1
  fi
}

# Parse CHECK_TIMES once into a list of minutes-of-day. BusyBox date (alpine) has
# no GNU `date -d` parsing, so the schedule is matched against the wall clock
# directly instead of computing a sleep duration — which is what made the old
# version fall back to a fixed 12h wait and ignore the configured times.
TARGET_MINS=""
oldIFS=$IFS
IFS=','
for t in $CHECK_TIMES; do
  t=$(echo "$t" | tr -d '[:space:]')
  [ -z "$t" ] && continue
  mm=$(time_to_min "$t")
  if [ "$mm" -lt 0 ]; then
    log "Ignoring invalid CHECK_TIMES entry '$t' (want HH:MM)"
    continue
  fi
  TARGET_MINS="$TARGET_MINS $mm"
done
IFS=$oldIFS

if [ -z "$TARGET_MINS" ]; then
  log "No valid CHECK_TIMES configured; defaulting to 04:00,16:00."
  TARGET_MINS="240 960"
fi

log "VPN watchdog starting. Checking at: ${CHECK_TIMES:-04:00,16:00} (local time)"
heal || true                                  # run once at startup
# Remember the minute the startup check ran in, so the poll loop does not
# immediately fire again within that same minute.
last_stamp=$(date +%Y%m%d%H%M)

# Poll the clock every 30s and run heal when the current minute matches one of
# the target minutes. The stamp (date + minute) guards against firing twice in
# the same minute while still firing again on the next day.
while true; do
  sleep 30
  set -- $(date '+%H %M %Y%m%d%H%M')
  cur=$(( $(undec "$1") * 60 + $(undec "$2") ))
  stamp=$3
  [ "$stamp" = "$last_stamp" ] && continue
  for tm in $TARGET_MINS; do
    if [ "$cur" -eq "$tm" ]; then
      last_stamp=$stamp
      heal || true
      break
    fi
  done
done
