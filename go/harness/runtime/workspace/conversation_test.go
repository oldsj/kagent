package workspace

import (
	"testing"

	"github.com/kagent-dev/kagent/go/adk/pkg/taskstore"
	"github.com/stretchr/testify/require"
)

// The ADK task store is what a serving runtime hands to FromTaskStore. If it
// stopped satisfying conversationStore, every Session would fail its first turn.
var _ conversationStore = (*taskstore.Store)(nil)

// A serving runtime's Source must report the fresh-conversation flag. A nil
// answer from ConversationOf silently disables the reset for fresh forks.
func TestServingSourceReportsConversation(t *testing.T) {
	t.Setenv("KAGENT_API_URL", "http://127.0.0.1:1")
	controller, source, err := OpenController("agent")
	require.NoError(t, err)
	t.Cleanup(func() { controller.Close() })
	require.NotNil(t, ConversationOf(source))
	require.Nil(t, ConversationOf(Unavailable()), "a configuration check has no control plane")
}
