#!/bin/sh
# Entrypoint for the kagent-controller-dev image (see docs/mainloop-dev-target.md).
#
# Builds $BUILD_PACKAGE out of $SRC_DIR into $OUT_BIN and runs it with no
# arguments, exactly like the production image's `ENTRYPOINT ["/app"]` (the
# controller takes all of its configuration from the environment, not CLI
# flags, so there is nothing else to replicate). After the initial build it
# polls $SRC_DIR for *.go files newer than the running binary and, on a
# change, rebuilds and restarts the controller. Sending SIGHUP forces an
# immediate rebuild check instead of waiting for the next poll.
set -eu

SRC_DIR="${SRC_DIR:-/src/go}"
BUILD_PACKAGE="${BUILD_PACKAGE:-core/cmd/controller/main.go}"
OUT_BIN="${OUT_BIN:-/home/nonroot/bin/controller}"
POLL_INTERVAL="${DEV_POLL_INTERVAL:-2}"

CONTROLLER_PID=""
FORCE_REBUILD=0

log() {
  echo "[controller-dev-entrypoint] $*" >&2
}

build() {
  log "building $BUILD_PACKAGE"
  mkdir -p "$(dirname "$OUT_BIN")"
  ( cd "$SRC_DIR" && go build -o "$OUT_BIN.tmp" "$BUILD_PACKAGE" )
  mv "$OUT_BIN.tmp" "$OUT_BIN"
}

start() {
  "$OUT_BIN" &
  CONTROLLER_PID=$!
  log "controller started pid=$CONTROLLER_PID"
}

stop() {
  if [ -n "$CONTROLLER_PID" ] && kill -0 "$CONTROLLER_PID" 2>/dev/null; then
    kill "$CONTROLLER_PID" 2>/dev/null || true
    wait "$CONTROLLER_PID" 2>/dev/null || true
  fi
  CONTROLLER_PID=""
}

on_term() {
  log "received termination signal, stopping"
  stop
  exit 0
}
trap on_term TERM INT

on_hup() {
  FORCE_REBUILD=1
}
trap on_hup HUP

# Fail fast if the baked-in source does not even build; a broken sync should
# not silently fall back to looping forever with nothing running.
build
start

while true; do
  sleep "$POLL_INTERVAL" || true
  changed=$(find "$SRC_DIR" -name '*.go' -newer "$OUT_BIN" -print -quit 2>/dev/null || true)
  if [ "$FORCE_REBUILD" = 1 ] || [ -n "$changed" ]; then
    FORCE_REBUILD=0
    log "source changed, rebuilding"
    if build; then
      stop
      start
    else
      log "build failed, keeping previous binary running"
    fi
  fi
  if [ -n "$CONTROLLER_PID" ] && ! kill -0 "$CONTROLLER_PID" 2>/dev/null; then
    log "controller exited unexpectedly, restarting"
    start
  fi
done
