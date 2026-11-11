package taskstore

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	runtimev1 "github.com/kagent-dev/kagent/go/api/gen/kagent/runtime/v1"
)

// workspaceServer answers GetWorkspace with a fixed fresh-conversation flag and
// records the Session the runtime asked about.
type workspaceServer struct {
	readinessServer
	fresh     bool
	requested atomic.Value
}

func (s *workspaceServer) GetWorkspace(_ context.Context, request *runtimev1.TaskStoreServiceGetWorkspaceRequest) (*runtimev1.TaskStoreServiceGetWorkspaceResponse, error) {
	s.requested.Store(request.GetSessionId())
	return &runtimev1.TaskStoreServiceGetWorkspaceResponse{FreshConversation: s.fresh}, nil
}

func TestFreshConversationAsksForItsOwnSession(t *testing.T) {
	for _, fresh := range []bool{true, false} {
		api := &workspaceServer{fresh: fresh}
		var available atomic.Bool
		available.Store(true)
		store := newTaskStoreServer(t, api, &api.readinessServer, &available, nil)

		got, err := store.FreshConversation(t.Context())
		require.NoError(t, err)
		require.Equal(t, fresh, got)
		id, err := store.SessionID()
		require.NoError(t, err)
		require.Equal(t, id, api.requested.Load(), "the flag must be read for this runtime's Session")
	}
}
