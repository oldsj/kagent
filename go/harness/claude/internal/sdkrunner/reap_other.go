//go:build !linux

package sdkrunner

import (
	"context"
	"time"
)

// ReapOrphans is a no-op off Linux, where the harness is never PID 1.
func (s *Session) ReapOrphans(context.Context, time.Duration) {}
