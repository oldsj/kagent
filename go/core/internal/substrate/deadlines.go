package substrate

import (
	"cmp"
	"fmt"
	"time"
)

// Default Substrate deadlines. Lifecycle calls move snapshots, so they get a
// longer bound than ordinary RPCs, and an attempt must outlast its calls.
const (
	DefaultCallTimeout          = 30 * time.Second
	DefaultLifecycleCallTimeout = 5 * time.Minute
	DefaultOperationTimeout     = 10 * time.Minute
)

// Deadlines are the resolved Substrate call and attempt bounds.
type Deadlines struct {
	Call      time.Duration
	Lifecycle time.Duration
	Operation time.Duration
}

// ResolveDeadlines applies defaults to unset (zero) values and rejects an
// attempt bound that does not exceed the lifecycle call bound: such an attempt
// would expire its execution claim while a suspend or resume is still running.
func ResolveDeadlines(call, lifecycle, operation time.Duration) (Deadlines, error) {
	d := Deadlines{
		Call:      cmp.Or(call, DefaultCallTimeout),
		Lifecycle: cmp.Or(lifecycle, DefaultLifecycleCallTimeout),
		Operation: cmp.Or(operation, DefaultOperationTimeout),
	}
	if d.Call <= 0 || d.Lifecycle <= 0 || d.Operation <= 0 {
		return Deadlines{}, fmt.Errorf("substrate deadlines must be positive")
	}
	if d.Operation <= d.Lifecycle {
		return Deadlines{}, fmt.Errorf("substrate.operationTimeout (%s) must exceed substrate.lifecycleCallTimeout (%s)", d.Operation, d.Lifecycle)
	}
	return d, nil
}
