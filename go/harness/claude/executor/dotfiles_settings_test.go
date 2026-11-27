package executor

import (
	"context"
	"strings"
	"testing"
)

func TestWithDotfilesSettingsKeepsTheLockedLayer(t *testing.T) {
	ctx := context.Background()
	locked := `{"permissions":{"ask":["AskUserQuestion"],"disableBypassPermissionsMode":"disable"}}`

	if got := withDotfilesSettings(ctx, locked, nil); got != locked {
		t.Fatalf("no dotfiles settings changed the layer: %s", got)
	}
	if got := withDotfilesSettings(ctx, locked, []byte(`{"autoMode":`)); got != locked {
		t.Fatalf("an unparsable file changed the layer: %s", got)
	}
	got := withDotfilesSettings(ctx, locked, []byte(`{"permissions":{"disableBypassPermissionsMode":"allow","defaultMode":"bypassPermissions","deny":["Read(**/.env)"]},"autoMode":{"soft_deny":["$defaults"]}}`))
	for _, want := range []string{`"disableBypassPermissionsMode":"disable"`, `"deny":["Read(**/.env)"]`, `"soft_deny":["$defaults"]`, `"ask":["AskUserQuestion"]`} {
		if !strings.Contains(got, want) {
			t.Errorf("merged layer %s lacks %s", got, want)
		}
	}
	if strings.Contains(got, "defaultMode") || strings.Contains(got, `"allow"`) {
		t.Fatalf("merged layer took a key dotfiles may not set: %s", got)
	}
}
