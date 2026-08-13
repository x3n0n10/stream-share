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
# API (WATCHDOG_STREAM_SHARE_URL) and the VPN control server (WATCHDOG_GLUETUN_URL).
#
# All of this watchdog's own environment variables are namespaced with a
# WATCHDOG_ prefix so they cannot collide with variables belonging to
# stream-share or the VPN when several services share a network namespace.
#
# NOTE: this file must use LF line endings. CRLF (Windows) endings make a POSIX
# shell fail with errors like `: not found` on blank lines and `elif unexpected`.
# The repo enforces LF via .gitattributes; if you edited it on Windows, run
# `sed -i 's/\r$//' watchdog.sh` (or `dos2unix watchdog.sh`).

# --- stream-share ------------------------------------------------------------
STREAM_SHARE_URL="${WATCHDOG_STREAM_SHARE_URL:-http://localhost:8080}"
INTERNAL_API_KEY="${WATCHDOG_INTERNAL_API_KEY:?set WATCHDOG_INTERNAL_API_KEY to the stream-share internal API key}"

# --- VPN control (gluetun example) -------------------------------------------
GLUETUN_URL="${WATCHDOG_GLUETUN_URL:-http://localhost:8000}"
# Auth: the gluetun control server supports HTTP Basic Auth or an API key,
# depending on its roles config. Set whichever matches; if both are set, Basic
# Auth wins (a client maps to exactly one auth method).
GLUETUN_USER="${WATCHDOG_GLUETUN_USER:-}"
GLUETUN_PASSWORD="${WATCHDOG_GLUETUN_PASSWORD:-}"
GLUETUN_API_KEY="${WATCHDOG_GLUETUN_API_KEY:-}"
# /v1/vpn/status is the current unified gluetun status/start/stop endpoint (both
# OpenVPN and WireGuard). Override to /v1/openvpn/status for older gluetun.
GLUETUN_STATUS_PATH="${WATCHDOG_GLUETUN_STATUS_PATH:-/v1/vpn/status}"

# --- behaviour ---------------------------------------------------------------
# WATCHDOG_CHECK_TIMES is the single schedule for hitting the provider: each run forces a
# fresh probe (which also refreshes the stream-share /healthz). When this watchdog
# is running, leave the stream-share HEALTHCHECK_TIMES empty so the provider is
# not probed twice on two schedules for the same information.
CHECK_TIMES="${WATCHDOG_CHECK_TIMES:-04:00,16:00}"       # local times to run, comma-separated HH:MM
MAX_RECONNECTS="${WATCHDOG_MAX_RECONNECTS:-5}"           # give up after this many server switches
RECONNECT_TIMEOUT="${WATCHDOG_RECONNECT_TIMEOUT:-45}"    # seconds to wait for a usable public IP after restart
STATUS_TIMEOUT="${WATCHDOG_STATUS_TIMEOUT:-20}"         # seconds to wait for gluetun to report stopped / running
DISCONNECT_TIMEOUT="${WATCHDOG_DISCONNECT_TIMEOUT:-15}" # seconds to wait for the tunnel to actually drop after stop
RECONNECT_SETTLE="${WATCHDOG_RECONNECT_SETTLE:-8}"      # seconds to pause between reconnect attempts
CONNECT_RETRIES="${WATCHDOG_CONNECT_RETRIES:-5}"         # retries when stream-share is unreachable (e.g. still starting after a restart)
CONNECT_RETRY_WAIT="${WATCHDOG_CONNECT_RETRY_WAIT:-5}"   # seconds to wait between those retries
FRESH_WAIT="${WATCHDOG_FRESH_WAIT:-3}"                  # seconds between retries while stream-share serves a cached (throttled) verdict
FRESH_MAX_WAIT="${WATCHDOG_FRESH_MAX_WAIT:-15}"        # give up waiting for a fresh verdict after this long (then use the cached one)
SETTLE_WAIT="${WATCHDOG_SETTLE_WAIT:-5}"                # seconds between re-checks while a post-reconnect error settles
SETTLE_MAX="${WATCHDOG_SETTLE_MAX:-20}"                # max seconds to let a transient post-reconnect error resolve before moving on

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
# connection failure, an HTTP 401 from a wrong WATCHDOG_INTERNAL_API_KEY, a wrong URL, and
# so on — rather than the old opaque "<unreachable>". heal() never cycles the VPN
# on an empty status.
fetch_health() {
  HEALTH_STATUS=""
  HEALTH_DETAIL=""
  HEALTH_THROTTLED=0

  errf="$(mktemp 2>/dev/null || echo /tmp/wd_curl_err)"
  hdrf="$(mktemp 2>/dev/null || echo /tmp/wd_hdr)"
  # -w appends the HTTP status on its own final line so we can read body and code
  # from one request without curl -f swallowing error bodies; -D captures the
  # response headers so we can tell whether the verdict was freshly probed or a
  # cached one stream-share served under its rate limit (X-Health-Throttled).
  #
  # A connection failure (curl rc != 0) usually means stream-share is not up yet
  # — common right after a restart — so retry a few times before giving up. Note
  # this only retries genuine unreachability; an HTTP 401/404 is a reachable
  # server answering, so it is handled below without retrying.
  attempt=0
  while :; do
    resp="$(curl -sS -D "$hdrf" -w '\n%{http_code}' -H "X-API-Key: $INTERNAL_API_KEY" \
      "$STREAM_SHARE_URL/api/internal/health" 2>"$errf")"
    rc=$?
    [ "$rc" -eq 0 ] && break

    curlerr="$(tr '\n' ' ' < "$errf" | sed 's/  */ /g;s/ *$//')"
    if [ "$attempt" -lt "$CONNECT_RETRIES" ]; then
      attempt=$((attempt + 1))
      log "stream-share unreachable (try $attempt/$CONNECT_RETRIES): ${curlerr:-connection failed}; retrying in ${CONNECT_RETRY_WAIT}s"
      sleep "$CONNECT_RETRY_WAIT"
      continue
    fi
    HEALTH_DETAIL="cannot reach $STREAM_SHARE_URL after $((CONNECT_RETRIES + 1)) attempt(s): ${curlerr:-connection failed}"
    rm -f "$errf" "$hdrf"
    return
  done
  rm -f "$errf"

  grep -iq '^x-health-throttled:[[:space:]]*true' "$hdrf" 2>/dev/null && HEALTH_THROTTLED=1
  rm -f "$hdrf"

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

# probe_fresh calls fetch_health and, if stream-share served a throttled (cached)
# verdict, waits and re-requests so decisions use a freshly-probed reading rather
# than a stale one from a previous IP. Bounded by FRESH_MAX_WAIT so a high
# HEALTHCHECK_MIN_INTERVAL_SECONDS on stream-share cannot stall the watchdog.
probe_fresh() {
  waited=0
  fetch_health
  while [ "${HEALTH_THROTTLED:-0}" = "1" ] && [ "$waited" -lt "$FRESH_MAX_WAIT" ]; do
    log "provider verdict was cached (stream-share throttled the probe); waiting ${FRESH_WAIT}s for a fresh reading — lower HEALTHCHECK_MIN_INTERVAL_SECONDS on stream-share to avoid this"
    sleep "$FRESH_WAIT"
    waited=$((waited + FRESH_WAIT))
    fetch_health
  done
}

# settle_and_probe reads a fresh verdict after a reconnect, tolerating a transient
# error/unknown/unreachable (DNS or connectivity still settling while gluetun
# finishes its own restart) by waiting and re-probing instead of burning another
# VPN cycle on it. Returns as soon as the verdict is definitive (healthy/blocked),
# or after SETTLE_MAX seconds.
settle_and_probe() {
  waited=0
  while :; do
    probe_fresh
    case "$HEALTH_STATUS" in
      healthy|blocked) return 0 ;;
    esac
    [ "$waited" -ge "$SETTLE_MAX" ] && return 0
    log "provider ${HEALTH_STATUS:-unreachable} right after reconnect (likely still settling); re-checking in ${SETTLE_WAIT}s"
    sleep "$SETTLE_WAIT"
    waited=$((waited + SETTLE_WAIT))
  done
}

# public_ip reads the gluetun control server. (VPN status is read inline by
# wait_status so it can surface the raw body/error on failure.)
public_ip() {
  body="$(gluetun_curl "$GLUETUN_URL/v1/publicip/ip" 2>/dev/null)" || return 1
  ip="$(json_str "$body" public_ip)"; [ -z "$ip" ] && ip="$(json_str "$body" ip)"
  [ -n "$ip" ] || return 1
  echo "$ip"
}

set_vpn() { gluetun_curl -X PUT -H "Content-Type: application/json" -d "{\"status\":\"$1\"}" "$GLUETUN_URL$GLUETUN_STATUS_PATH" >/dev/null; }

# wait_status polls the gluetun control server until it reports the desired
# status ("running"/"stopped") or the deadline passes. On timeout it logs the
# last status it actually saw plus any transport error and a snippet of the raw
# body, so a wrong GLUETUN_STATUS_PATH (e.g. a 404 on /v1/vpn/status for older
# gluetun) or an auth problem is visible instead of a silent per-second spin.
wait_status() { # $1=desired $2=timeout_seconds
  deadline=$(( $(date +%s) + $2 ))
  errf="$(mktemp 2>/dev/null || echo /tmp/wd_vpn_err)"
  seen=""; raw=""; err=""
  while [ "$(date +%s)" -lt "$deadline" ]; do
    raw="$(gluetun_curl "$GLUETUN_URL$GLUETUN_STATUS_PATH" 2>"$errf")"
    if [ $? -ne 0 ]; then err="$(tr '\n' ' ' < "$errf" | sed 's/  */ /g;s/ *$//')"; else err=""; fi
    seen="$(json_str "$raw" status)"
    [ "$seen" = "$1" ] && { rm -f "$errf"; return 0; }
    sleep 2
  done
  rm -f "$errf"
  snip="$(printf '%s' "$raw" | tr '\n' ' ' | sed 's/  */ /g' | cut -c1-120)"
  log "gluetun did not report '$1' within ${2}s (last status: '${seen:-<none>}'${err:+; error: $err}${snip:+; body: $snip}). Check WATCHDOG_GLUETUN_URL / WATCHDOG_GLUETUN_STATUS_PATH / auth."
  return 1
}

# wait_disconnected returns once the public-IP probe fails — i.e. the tunnel has
# really gone down — or the budget runs out. Some setups do not firewall non-VPN
# traffic, so the probe may keep succeeding; it is bounded so we proceed anyway.
wait_disconnected() { # $1=timeout_seconds
  deadline=$(( $(date +%s) + $1 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    public_ip >/dev/null 2>&1 || return 0
    sleep 1
  done
  return 1
}

# wait_public_ip prints the first usable public IP within the budget, else empty.
# gluetun reports "running" before it has re-resolved its exit IP, so a usable IP
# is the real proof the new tunnel is up — and yields the new IP for logging.
wait_public_ip() { # $1=timeout_seconds
  deadline=$(( $(date +%s) + $1 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    ip="$(public_ip)" && { echo "$ip"; return 0; }
    sleep 2
  done
  return 1
}

# cycle_vpn reconnects the VPN, mirroring the stream-share-dashboard sequence
# (which is known to work): stop, confirm stopped, wait until traffic actually
# stops routing, start, confirm running, then wait for a usable public IP.
# gluetun reports "running" as its TARGET state before the tunnel is really back,
# so trusting that flag alone reconnects far too fast (and hammers gluetun on the
# next attempt). Each phase has its own timeout, and the old vs new IP is logged
# so an unchanged IP (single-server gluetun) is obvious.
cycle_vpn() {
  oldip="$(public_ip 2>/dev/null || echo '?')"
  log "Cycling VPN (current IP: $oldip)..."

  set_vpn stopped || log "warning: stop request failed"
  wait_status stopped "$STATUS_TIMEOUT" || :
  wait_disconnected "$DISCONNECT_TIMEOUT" || \
    log "note: traffic still routing after stop (VPN kill-switch may be off); proceeding"

  set_vpn running || log "warning: start request failed"
  wait_status running "$STATUS_TIMEOUT" || :
  newip="$(wait_public_ip "$RECONNECT_TIMEOUT" || true)"
  LAST_IP="${newip:-?}"
  if [ -z "$newip" ]; then
    log "warning: no usable public IP within ${RECONNECT_TIMEOUT}s after restart"
  elif [ "$newip" = "$oldip" ] && [ "$oldip" != "?" ]; then
    log "VPN back up but exit IP is UNCHANGED ($newip) — gluetun likely has only one server to pick from, so a reconnect cannot rotate the IP and the provider stays blocked. Give gluetun multiple servers (e.g. SERVER_COUNTRIES / SERVER_CITIES / SERVER_HOSTNAMES)."
  else
    log "VPN back up (new IP: $newip)"
  fi
}

# heal probes once and, while blocked, cycles the VPN up to MAX_RECONNECTS times.
heal() {
  probe_fresh
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

  heal_start="$(date +%s)"
  tried_ips=""
  i=1
  while [ "$i" -le "$MAX_RECONNECTS" ]; do
    log "Blocked — reconnect attempt $i/$MAX_RECONNECTS"
    cycle_vpn
    tried_ips="$tried_ips ${LAST_IP:-?}"

    # Read a fresh verdict, giving a transient post-reconnect error time to settle
    # rather than immediately spending another reconnect on it.
    settle_and_probe
    if [ "$HEALTH_STATUS" = "healthy" ]; then
      log "Recovered after $i reconnect(s) in $(( $(date +%s) - heal_start ))s. IPs tried:$tried_ips"
      return 0
    fi
    log "Still ${HEALTH_STATUS:-<unreachable>} after attempt $i (IP: ${LAST_IP:-?})."
    i=$((i + 1))
    # Let gluetun settle before hammering it with another stop/start.
    [ "$i" -le "$MAX_RECONNECTS" ] && sleep "$RECONNECT_SETTLE"
  done
  log "Gave up after $MAX_RECONNECTS reconnects in $(( $(date +%s) - heal_start ))s; provider still ${HEALTH_STATUS:-<unreachable>}. IPs tried:$tried_ips"
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
