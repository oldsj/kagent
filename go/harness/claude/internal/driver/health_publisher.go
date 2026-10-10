package driver

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"
	"github.com/kagent-dev/kagent/go/pkg/logging"
)

const healthQueueSize = 256

// healthFlushTimeout bounds how long a finished turn waits for queued health
// observations. A canceled turn does not wait at all.
var healthFlushTimeout = time.Second

// healthPublisher keeps health observations off the turn's control path. The
// turn enqueues without blocking; one goroutine delivers in sequence order.
// Overflow and sink errors drop the observation and are counted: the consumer
// sees the sequence gap and reports incomplete coverage.
type healthPublisher struct {
	events    chan runtime.HealthEvent
	done      chan struct{}
	abandoned atomic.Bool
	dropped   atomic.Int64
	failed    atomic.Int64
}

func startHealthPublisher(sink runtime.EventSink) *healthPublisher {
	p := &healthPublisher{events: make(chan runtime.HealthEvent, healthQueueSize), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for event := range p.events {
			if p.abandoned.Load() {
				p.dropped.Add(1)
				continue
			}
			if err := sink.Health(event); err != nil {
				p.failed.Add(1)
			}
		}
	}()
	return p
}

func (p *healthPublisher) publish(event runtime.HealthEvent) {
	select {
	case p.events <- event:
	default:
		p.dropped.Add(1)
	}
}

// close flushes queued observations within a bound, then abandons the rest so
// the sink is not called once the turn has moved on.
func (p *healthPublisher) close(ctx context.Context) {
	close(p.events)
	timer := time.NewTimer(healthFlushTimeout)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-ctx.Done():
	case <-timer.C:
	}
	p.abandoned.Store(true)
	select {
	case <-p.done:
	default:
		logging.FromContext(ctx).WarnContext(ctx, "claude health sink did not drain; abandoning queued observations")
	}
	if dropped, failed := p.dropped.Load(), p.failed.Load(); dropped != 0 || failed != 0 {
		logging.FromContext(ctx).WarnContext(ctx, "claude health observations were not delivered", "dropped", dropped, "failed", failed)
	}
}
