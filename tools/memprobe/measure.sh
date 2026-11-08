#!/bin/bash
# usage: tools/memprobe/measure.sh <label>
# Builds the binary (with pprof) and a fake provider, then prints RSS samples and the heap top.
# Env: GO, CATALOG_STORAGE, DB_HOST, DB_PORT, DB_NAME, DB_USER.
set -u
LABEL=${1:?label}
GO=${GO:-/home/linuxbrew/.linuxbrew/opt/go@1.23/bin/go}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
OUT=$(mktemp -d)
cd "$ROOT" || exit 1
$GO build -mod=vendor -tags memprobe -o "$OUT/ss-bin" . || exit 1
$GO build -mod=vendor -o "$OUT/fakeprov" ./tools/memprobe/fakeprov || exit 1
FPID=""; PID=""
trap 'kill $PID $FPID 2>/dev/null' EXIT
ADDR=:18080 "$OUT/fakeprov" > "$OUT/fake.log" 2>&1 &
FPID=$!
sleep 3
mkdir -p "$OUT/cache"
env DB_HOST="${DB_HOST:-}" DB_PORT="${DB_PORT:-5432}" DB_NAME="${DB_NAME:-streamshare}" DB_USER="${DB_USER:-postgres}" \
  CATALOG_STORAGE="${CATALOG_STORAGE:-}" CATALOG_CACHE_TTL_MINUTES=1 \
  M3U_URL="http://127.0.0.1:18080/get.php?username=u&password=p&type=m3u_plus" \
  PORT=18090 AUTH_USER=a AUTH_PASSWORD=b CACHE_FOLDER="$OUT/cache" \
  "$OUT/ss-bin" > "$OUT/ss.log" 2>&1 &
PID=$!
rss() { awk '/VmRSS|VmHWM/{printf "%s %d MB  ", $1, $2/1024}' /proc/$PID/status; echo; }
B="http://127.0.0.1:18090/player_api.php?username=a&password=b"
lists() {
  for a in get_live_categories get_vod_categories get_series_categories get_live_streams get_vod_streams get_series; do
    curl -s -o /dev/null -H 'Accept-Encoding: gzip' "$B&action=$a"
  done
}
sleep 20; echo -n "idle after warmup: "; rss
curl -s -o /dev/null "$B"
lists
echo -n "after full lists: "; rss
for c in 1 2 3 4 5 6 7 8 9 10; do curl -s -o /dev/null "$B&action=get_vod_streams&category_id=$c"; curl -s -o /dev/null "$B&action=get_live_streams&category_id=$c"; done
curl -s -o /dev/null "http://127.0.0.1:18090/get.php?username=a&password=b"
echo -n "after slices+m3u: "; rss
sleep 30; echo -n "30s later: "; rss
curl -s http://127.0.0.1:18080/bump > /dev/null
sleep 65 # TTL is 1 min: the next request serves stale and refreshes in the background
lists
sleep 30; echo -n "after forced refresh +30s: "; rss
echo "error lines: $(grep -ciE 'error|panic' "$OUT/ss.log")"
curl -s -o "$OUT/heap.pb.gz" "http://127.0.0.1:16060/debug/pprof/heap?gc=1"
$GO tool pprof -sample_index=inuse_space -top -nodecount=15 "$OUT/ss-bin" "$OUT/heap.pb.gz" 2>/dev/null | head -22
rm -rf "$OUT"
