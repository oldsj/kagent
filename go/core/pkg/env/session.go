package env

import "time"

var QuiesceDelay = RegisterDurationVar("KAGENT_QUIESCE_DELAY", 0, "Delay after native cleanup publishes a turn before automatic workspace pause or suspension. Clamped to 0–30m; zero keeps immediate eligibility. Stored deadlines survive restarts and are superseded by new turns.", ComponentController)

var SessionIdleTTL = RegisterDurationVar("KAGENT_SESSION_IDLE_TTL", 7*24*time.Hour, "Default idle duration before deletion. Harness sessionIdleTTL overrides it for existing Sessions; zero disables default admission. Admitted deletions still retry; running and waiting tasks are retained.", ComponentController)

var SessionExpirationPollInterval = RegisterDurationVar("KAGENT_SESSION_EXPIRATION_POLL_INTERVAL", time.Minute, "Interval between idle session expiration sweeps. Must be positive.", ComponentController)

var SessionShareMaxTTL = RegisterDurationVar("KAGENT_SESSION_SHARE_MAX_TTL", 0, "Longest lifetime a session share may request. Shares created without a ttl receive it. Zero leaves shares unbounded.", ComponentController)
