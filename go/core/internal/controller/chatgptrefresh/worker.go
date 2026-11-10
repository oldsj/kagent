package chatgptrefresh

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RunJob is a one-shot control-side worker. The controller supplies only Secret
// identity and a non-secret claim; credentials are fetched under resourceNames
// RBAC. The caller must supply a memory-backed private home. No OAuth endpoint
// or protocol is implemented here: only the official CLI spends the token.
func RunJob(ctx context.Context, kube client.Client, name types.NamespacedName, key, claim, home, executable string) error {
	var secret corev1.Secret
	if kube.Get(ctx, name, &secret) != nil {
		return errOperation
	}
	before := append([]byte(nil), secret.Data[AuthKey]...)
	if _, err := readAuth(before); err != nil {
		return err
	}
	if secret.Annotations[ClaimAnnotation] != claim || secret.Annotations[StateAnnotation] != statePending ||
		secret.Annotations[HashAnnotation] != authHash(before) || key == AuthKey || key == "" {
		return errOperation
	}
	if os.MkdirAll(home, 0700) != nil {
		return errOperation
	}
	if os.WriteFile(filepath.Join(home, "auth.json"), before, 0600) != nil {
		return errOperation
	}
	config := []byte("cli_auth_credentials_store = \"file\"\ncheck_for_update_on_startup = false\n[analytics]\nenabled = false\n[otel]\nexporter = \"none\"\n")
	if os.WriteFile(filepath.Join(home, "config.toml"), config, 0600) != nil {
		return errOperation
	}
	// Kubernetes may execute a Job more than once. Only one Pod can acquire
	// this resourceVersion-checked transition, even with overlapping Pods.
	secret.Annotations[StateAnnotation] = stateRunning
	if kube.Update(ctx, &secret) != nil {
		return errOperation
	}
	cliErr := refreshCLI(ctx, executable, home)
	after, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return errReauthentication
	}
	// account/read can succeed while refresh failed; trust only a validated
	// rotated file. Even if the RPC fails after rotation, persist valid tokens.
	auth, err := rotatedAuth(before, after, time.Now())
	if err != nil {
		_ = cliErr // Diagnostics are deliberately never surfaced.
		return errReauthentication
	}
	uid := secret.UID
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var current corev1.Secret
		if kube.Get(ctx, name, &current) == nil {
			if current.UID != uid {
				return errOperation
			}
			if bytes.Equal(current.Data[AuthKey], after) && bytes.Equal(current.Data[key], []byte(auth.Tokens.Access)) {
				return nil
			}
			if current.Annotations[ClaimAnnotation] != claim || current.Annotations[StateAnnotation] != stateRunning || !bytes.Equal(current.Data[AuthKey], before) {
				return errOperation // Owner re-seeded; don't overwrite the replacement.
			}
			current.Data[AuthKey] = after
			current.Data[key] = []byte(auth.Tokens.Access)
			for _, annotation := range []string{StateAnnotation, ClaimAnnotation, HashAnnotation, RetryAnnotation} {
				delete(current.Annotations, annotation)
			}
			if kube.Update(ctx, &current) == nil {
				return nil
			}
		}
		// Retry persistence only. Never run the CLI again with the original file.
		select {
		case <-ctx.Done():
			return errOperation
		case <-ticker.C:
		}
	}
}
