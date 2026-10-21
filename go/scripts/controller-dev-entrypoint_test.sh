#!/bin/sh
# Exercises controller-dev-entrypoint.sh against a throwaway Go package
# instead of the real controller: verifies the initial build+run, and that
# editing a source file triggers a rebuild and restart within one poll
# interval. Requires only `go` and a POSIX shell (no Docker), so it runs
# directly in CI or locally.
set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ENTRYPOINT="$SCRIPT_DIR/controller-dev-entrypoint.sh"

WORKDIR="$(mktemp -d)"
ENTRYPOINT_PID=""

cleanup() {
  if [ -n "$ENTRYPOINT_PID" ] && kill -0 "$ENTRYPOINT_PID" 2>/dev/null; then
    kill "$ENTRYPOINT_PID" 2>/dev/null || true
    wait "$ENTRYPOINT_PID" 2>/dev/null || true
  fi
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

mkdir -p "$WORKDIR/src/cmd/fake"
cat > "$WORKDIR/src/go.mod" <<'EOF'
module fake

go 1.21
EOF

cat > "$WORKDIR/src/cmd/fake/main.go" <<'EOF'
package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	f, err := os.OpenFile(os.Getenv("MARK_FILE"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		panic(err)
	}
	fmt.Fprintln(f, "v1")
	f.Close()
	time.Sleep(time.Hour)
}
EOF

MARK_FILE="$WORKDIR/marks.txt"
: > "$MARK_FILE"

MARK_FILE="$MARK_FILE" \
SRC_DIR="$WORKDIR/src" \
BUILD_PACKAGE="./cmd/fake" \
OUT_BIN="$WORKDIR/bin/fake" \
DEV_POLL_INTERVAL=1 \
  "$ENTRYPOINT" &
ENTRYPOINT_PID=$!

wait_for_lines() {
  want="$1" budget="$2"
  i=0
  while [ "$(wc -l < "$MARK_FILE")" -lt "$want" ]; do
    i=$((i + 1))
    if [ "$i" -gt "$budget" ]; then
      echo "timed out waiting for $want run(s); got $(cat "$MARK_FILE")" >&2
      exit 1
    fi
    sleep 1
  done
}

wait_for_lines 1 30
first_count="$(wc -l < "$MARK_FILE")"
if [ "$first_count" -ne 1 ]; then
  echo "expected exactly one run before any edit, got $first_count" >&2
  exit 1
fi

# Bump the marker and touch the file so its mtime is unambiguously newer than
# the already-built binary, then confirm the entrypoint notices and rebuilds.
sed -i 's/v1/v2/' "$WORKDIR/src/cmd/fake/main.go"
touch "$WORKDIR/src/cmd/fake/main.go"

wait_for_lines 2 30
last_line="$(tail -n1 "$MARK_FILE")"
if [ "$last_line" != "v2" ]; then
  echo "expected rebuilt binary to print v2, got $last_line" >&2
  exit 1
fi

echo "controller-dev-entrypoint.sh rebuild-on-change: OK"
