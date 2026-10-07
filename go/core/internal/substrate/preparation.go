package substrate

import "errors"

// ErrGoldenSnapshotFailed marks an immutable preparation failure. Recreating
// the same ActorTemplate cannot repair it; callers must select corrected inputs.
var ErrGoldenSnapshotFailed = errors.New("golden snapshot preparation failed")
