package workspace

import (
	"context"
	"errors"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/harness/runtime/continuation"
)

// TaskStore is the control-plane client that reports a Session's workspace.
type TaskStore interface {
	GetWorkspace(context.Context) (*apiv1.Workspace, error)
}

type controllerSource struct{ store TaskStore }

// ConversationOf returns the Source's view of whether the Session starts a
// fresh native conversation, or nil when the Source has none (a configuration
// check, or a control plane that cannot say).
func ConversationOf(source Source) continuation.FreshSource {
	fresh, _ := source.(continuation.FreshSource)
	return fresh
}

type conversationStore interface {
	SessionID() (string, error)
	FreshConversation(context.Context) (bool, error)
}

func (s controllerSource) SessionID() (string, error) {
	store, ok := s.store.(conversationStore)
	if !ok {
		return "", errors.New("session identity is unavailable")
	}
	return store.SessionID()
}

func (s controllerSource) FreshConversation(ctx context.Context) (bool, error) {
	store, ok := s.store.(conversationStore)
	if !ok {
		return false, errors.New("fresh conversation flag is unavailable")
	}
	return store.FreshConversation(ctx)
}

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
