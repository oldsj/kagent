package chatgptrefresh

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const SeedSecretAnnotation = "kagent.dev/chatgpt-auth-seed-secret"

type credentialDescriptor struct {
	key, account                 string
	keyConflict, accountConflict bool
}

// A populated destination belongs exclusively to rotation, even if its seed
// has changed or disappeared. Bootstrap failures never persist refresh state.
func (c *Controller) bootstrap(ctx context.Context, runtime *corev1.Secret, descriptor credentialDescriptor, now time.Time) (bool, error) {
	seedName, optedIn := runtime.Annotations[SeedSecretAnnotation]
	if !optedIn {
		return false, nil
	}
	for _, value := range runtime.Data {
		if len(value) != 0 {
			return false, nil
		}
	}
	for _, value := range runtime.StringData {
		if value != "" {
			return false, nil
		}
	}
	if runtime.Type != corev1.SecretTypeOpaque || runtime.Immutable != nil && *runtime.Immutable || runtime.DeletionTimestamp != nil {
		return true, errCredential
	}
	for _, annotation := range []string{StateAnnotation, ClaimAnnotation, HashAnnotation, RetryAnnotation} {
		if _, exists := runtime.Annotations[annotation]; exists {
			return true, errCredential
		}
	}
	if len(validation.IsDNS1123Subdomain(seedName)) != 0 || seedName == runtime.Name ||
		len(validation.IsDNS1123Subdomain(runtime.Name)) != 0 ||
		len(validation.IsConfigMapKey(descriptor.key)) != 0 || descriptor.key == AuthKey ||
		strings.TrimSpace(descriptor.account) == "" || descriptor.account != strings.TrimSpace(descriptor.account) ||
		descriptor.keyConflict || descriptor.accountConflict {
		return true, errCredential
	}
	// Inspect all Secret-UID-owned Jobs, including those outside the deterministic
	// refresh name. A terminal condition proves execution has settled.
	var jobs batchv1.JobList
	if c.client.List(ctx, &jobs, client.InNamespace(runtime.Namespace)) != nil {
		return true, errOperation
	}
	for _, job := range jobs.Items {
		for _, owner := range job.OwnerReferences {
			if owner.APIVersion == "v1" && owner.Kind == "Secret" && owner.UID == runtime.UID && !terminalJob(&job) {
				return true, nil
			}
		}
	}
	var seed corev1.Secret
	if err := c.client.Get(ctx, types.NamespacedName{Namespace: runtime.Namespace, Name: seedName}, &seed); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return true, errOperation
	}
	if validateSeed(&seed, descriptor, now) != nil {
		return true, errCredential
	}
	if runtime.Data == nil {
		runtime.Data = map[string][]byte{}
	}
	runtime.Data[AuthKey] = bytes.Clone(seed.Data[AuthKey])
	runtime.Data[descriptor.key] = bytes.Clone(seed.Data[descriptor.key])
	// Exactly one resourceVersion-fenced write. A conflict or ambiguous response
	// is re-read by the next ordinary sweep, never retried from this snapshot.
	if c.client.Update(ctx, runtime) != nil {
		return true, errOperation
	}
	return true, nil
}

func terminalJob(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) {
			return true
		}
	}
	return false
}

func validateSeed(seed *corev1.Secret, descriptor credentialDescriptor, now time.Time) error {
	auth, err := readAuth(seed.Data[AuthKey])
	if err != nil || auth.Tokens.AccountID != descriptor.account || !bytes.Equal(seed.Data[descriptor.key], []byte(auth.Tokens.Access)) {
		return errCredential
	}
	expires, err := expiry(seed.Data[descriptor.key])
	if err != nil || !expires.After(now.Add(safetyMargin)) {
		return errCredential
	}
	// Match the access token's ChatGPT account claim as well as the file account.
	// This decodes identity metadata; provider signature verification stays with
	// the existing credential consumer.
	parts := strings.Split(auth.Tokens.Access, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errCredential
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Auth.AccountID != descriptor.account {
		return errCredential
	}
	return nil
}
