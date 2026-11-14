package activity

import (
	"context"
	"os"
	"time"
)

// ReportAfterRestores re-sends the runtime's activity after every memory
// restore, so a woken runtime is suspended again once nothing uses it.
//
// A runtime restored from a memory snapshot continues the process that was
// suspended: no startup code runs again, a one-shot idle report has already
// been sent, and the monotonic clock does not advance across the pause, so no
// timer notices it either. Substrate projects the runtime identity file anew at
// each restore, so a new file identity (inode) at path marks one. A file that
// is absent and then appears is not a restore of this process's Session: a
// golden-snapshot restore gains its identity that way, and IdleWhenServing
// reports it. Polling continues until ctx ends.
func (r *Reporter) ReportAfterRestores(ctx context.Context, path string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var seen os.FileInfo
	for {
		if info, err := os.Stat(path); err == nil {
			if seen != nil && !os.SameFile(seen, info) {
				r.logger.InfoContext(ctx, "runtime restored; reporting activity again")
				r.Resend()
			}
			seen = info
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Resend reports the current activity again, or idle if none was recorded.
func (r *Reporter) Resend() {
	r.mu.Lock()
	if !r.set {
		r.mu.Unlock()
		r.SetActive(false)
		return
	}
	r.version++
	r.mu.Unlock()
	select {
	case r.changed <- struct{}{}:
	default:
	}
}
