package continuation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kagent-dev/kagent/go/harness/internal/utils"
)

// FreshSource tells a runtime whether its Session is a fork that starts without
// the native conversation held in the data it was restored from.
type FreshSource interface {
	// SessionID is read locally, so a settled runtime never calls the control plane.
	SessionID() (string, error)
	FreshConversation(context.Context) (bool, error)
}

// Fresh configures how a Store discards inherited native conversation state.
type Fresh struct {
	Source FreshSource
	// MarkerDir holds the record of the Session already settled, normally
	// /data/.kagent. It is inside the restored data, so the record travels with
	// every snapshot of the Session.
	MarkerDir string
	// Clear lists the native transcript and history paths to remove. Anything
	// else in the provider's directories (credentials, configuration, caches)
	// and the workspace stay.
	Clear []string
}

const freshMarker = "conversation-settled"

// RequireFresh makes Settle discard inherited conversation state for a fresh
// fork. Call it once, before the Store serves turns.
func (s *Store) RequireFresh(fresh Fresh) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fresh = &fresh
}

// Settle runs before a turn reads the continuation. For a fresh fork's first
// turn it removes the continuation binding and the Clear paths, then records the
// Session as settled so later turns, and snapshots of this Session, keep the new
// conversation. Every other Session is recorded without a change. A failure
// leaves nothing recorded and must fail the turn: running on would continue the
// conversation the fork was asked to drop. The store lock is held only for the
// local deletes and the marker, never across the control-plane call.
func (s *Store) Settle(ctx context.Context) error {
	s.mu.RLock()
	f := s.fresh
	s.mu.RUnlock()
	if f == nil {
		return nil
	}
	id, err := f.Source.SessionID()
	if err != nil {
		return fmt.Errorf("identify Session: %w", err)
	}
	if settled, err := f.settled(id); err != nil || settled {
		return err
	}
	fresh, err := f.Source.FreshConversation(ctx)
	if err != nil {
		return fmt.Errorf("read fresh conversation flag: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// A concurrent Settle may have finished while the lock was free. Deleting
	// again would drop the binding its turn has since made.
	if settled, err := f.settled(id); err != nil || settled {
		return err
	}
	if fresh {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove continuation state: %w", err)
		}
		for _, path := range f.Clear {
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove native conversation state: %w", err)
			}
		}
	}
	if err := utils.EnsurePrivateDir(f.MarkerDir); err != nil {
		return fmt.Errorf("prepare conversation marker directory: %w", err)
	}
	if err := utils.ReplacePrivateFile(filepath.Join(f.MarkerDir, freshMarker), []byte(id+"\n")); err != nil {
		return fmt.Errorf("record conversation marker: %w", err)
	}
	return nil
}

// settled reports whether the Session has already been recorded.
func (f *Fresh) settled(id string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(f.MarkerDir, freshMarker))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read conversation marker: %w", err)
	}
	return strings.TrimSpace(string(b)) == id, nil
}
