package substrate

import (
	"fmt"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
)

// snapshotConfig returns the Actor snapshot policy. Pausing for input always
// keeps memory so the waiting turn resumes in place. onQuiesce selects what an
// idle or explicit suspend captures; empty means Data.
func snapshotConfig(location string, onQuiesce v1alpha3.RuntimeSnapshotScope) (*ateapipb.SnapshotConfig, error) {
	var commit ateapipb.SnapshotContentScope
	switch onQuiesce {
	case "", v1alpha3.RuntimeSnapshotScopeData:
		commit = ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA
	case v1alpha3.RuntimeSnapshotScopeFull:
		commit = ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL
	default:
		return nil, fmt.Errorf("unsupported quiesce snapshot scope %q", onQuiesce)
	}
	return &ateapipb.SnapshotConfig{
		StorageLocation: location,
		OnPause:         ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL,
		OnCommit:        commit,
	}, nil
}
