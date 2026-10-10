package driver

import "time"

// activeBudget counts execution time across segments of one native turn.
// Its sole owner is the running consumer or the parked pending-turn handle.
type activeBudget struct {
	remaining time.Duration
	started   time.Time
	timer     *time.Timer
}

func newActiveBudget(limit time.Duration) *activeBudget {
	b := &activeBudget{remaining: limit}
	b.resume()
	return b
}

func (b *activeBudget) resume() {
	if b != nil && b.timer == nil {
		b.started = time.Now()
		b.timer = time.NewTimer(max(b.remaining, 0))
	}
}

func (b *activeBudget) pause() {
	if b != nil && b.timer != nil {
		b.timer.Stop()
		b.remaining -= time.Since(b.started)
		b.timer = nil
	}
}

func (b *activeBudget) done() <-chan time.Time {
	if b == nil || b.timer == nil {
		return nil
	}
	return b.timer.C
}

func (b *activeBudget) expired() bool {
	return b != nil && b.timer != nil && time.Since(b.started) >= b.remaining
}
