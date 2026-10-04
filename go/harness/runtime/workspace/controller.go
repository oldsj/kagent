package workspace

import (
	"context"
	"errors"

	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
)

// TaskStore is the control-plane client that reports a Session's workspace.
type TaskStore interface {
	GetWorkspace(context.Context) (*apiv1alpha1.Workspace, error)
}

type controllerSource struct{ store TaskStore }

// FromTaskStore reads the workspace request through the runtime's authenticated
// control-plane channel.
func FromTaskStore(store TaskStore) Source { return controllerSource{store: store} }

func (s controllerSource) Workspace(ctx context.Context) (*Request, error) {
	wire, err := s.store.GetWorkspace(ctx)
	if err != nil || wire == nil {
		return nil, err
	}
	return &Request{Repo: wire.GetRepo(), Ref: wire.GetRef(), Branch: wire.GetBranch(), Depth: int(wire.GetDepth())}, nil
}

type unavailableSource struct{}

// Unavailable is a Source that always fails. A configuration check, which never
// serves turns, uses it to validate a Git-enabled runtime without a control plane.
func Unavailable() Source { return unavailableSource{} }

func (unavailableSource) Workspace(context.Context) (*Request, error) {
	return nil, errors.New("workspace source is unavailable")
}
