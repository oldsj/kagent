package substrate

import (
	"fmt"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
)

// QuiesceFidelity returns the snapshot fidelity Substrate should prefer when an
// idle runtime suspends. Empty means Data.
func QuiesceFidelity(scope v1alpha3.RuntimeSnapshotScope) (ateapipb.SnapshotFidelity, error) {
	switch scope {
	case "", v1alpha3.RuntimeSnapshotScopeData:
		return ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES, nil
	case v1alpha3.RuntimeSnapshotScopeFull:
		return ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, nil
	default:
		return 0, fmt.Errorf("unsupported quiesce snapshot scope %q", scope)
	}
}

// SuspendFidelity reports whether a suspend snapshot can wake the runtime.
// PreferredFidelity is a ceiling, so a Full template may report any fidelity.
func SuspendFidelity(fidelity ateapipb.SnapshotFidelity) bool {
	switch fidelity {
	case ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES,
		ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS,
		ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY:
		return true
	default:
		return false
	}
}
