package chatgptrefresh

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	kagentv1alpha3 "github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/pkg/logging"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func bootstrapFixtures(t *testing.T) (*corev1.Secret, *corev1.Secret, *kagentv1alpha3.ModelConfig) {
	t.Helper()
	runtime := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "team", UID: "runtime-uid",
		Labels: map[string]string{"keep": "label"}, Annotations: map[string]string{SeedSecretAnnotation: "seed", "keep": "annotation"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "example.org/v1", Kind: "Credential", Name: "runtime-owner", UID: "owner-uid"}},
		Finalizers:      []string{"example.org/keep"}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"empty": {}}}
	token := seedJWT(time.Now().Add(time.Hour), "synthetic-account")
	// Extra fields and whitespace must survive rather than being re-marshaled.
	data := []byte(fmt.Sprintf(" {\n\"tokens\":{\"access_token\":%q,\"refresh_token\":\"SYNTHETIC_REFRESH\",\"account_id\":\"synthetic-account\"},\"codex_extra\":true}\n", token))
	seed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "team", UID: "seed-uid"}, Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{AuthKey: data, "access-token": []byte(token)}}
	model := &kagentv1alpha3.ModelConfig{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "team"}, Spec: kagentv1alpha3.ModelConfigSpec{
		Provider: kagentv1alpha3.ModelProviderOpenAI, APIKeySecret: "runtime", APIKeySecretKey: "access-token",
		OpenAI: &kagentv1alpha3.OpenAIConfig{AuthMethod: kagentv1alpha3.OpenAIAuthMethod_ChatGPT, AccountID: "synthetic-account"}}}
	return runtime, seed, model
}

func seedJWT(expires time.Time, account string) string {
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"https://api.openai.com/auth":{"chatgpt_account_id":%q}}`, expires.Unix(), account))) + ".synthetic"
}

func replaceSeedToken(t *testing.T, seed *corev1.Secret, token string) {
	t.Helper()
	auth, err := readAuth(seed.Data[AuthKey])
	require.NoError(t, err)
	auth.Tokens.Access = token
	seed.Data[AuthKey], err = json.Marshal(auth)
	require.NoError(t, err)
	seed.Data["access-token"] = []byte(token)
}

func TestBootstrapLateSeedAndAutomaticHandoff(t *testing.T) {
	runtime, seed, model := bootstrapFixtures(t)
	kube := kubeClient(t, runtime, model)
	c := New(kube, []string{"team"}, "image")
	var before corev1.Secret
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(runtime), &before))
	require.NoError(t, c.sweep(t.Context()))
	var waiting corev1.Secret
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(runtime), &waiting))
	require.Equal(t, before, waiting)
	require.NoError(t, kube.Create(t.Context(), seed))
	require.NoError(t, c.sweep(t.Context()))
	var after corev1.Secret
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(runtime), &after))
	require.Equal(t, seed.Data[AuthKey], after.Data[AuthKey])
	require.Equal(t, seed.Data["access-token"], after.Data["access-token"])
	require.Equal(t, before.UID, after.UID)
	require.Equal(t, before.Labels, after.Labels)
	require.Equal(t, before.Annotations, after.Annotations)
	require.Equal(t, before.OwnerReferences, after.OwnerReferences)
	require.Equal(t, before.Finalizers, after.Finalizers)
	require.Equal(t, before.Type, after.Type)
	require.Contains(t, after.Data, "empty")
	require.Empty(t, after.Data["empty"])
	require.NotEqual(t, before.ResourceVersion, after.ResourceVersion)
	var jobs batchv1.JobList
	require.NoError(t, kube.List(t.Context(), &jobs))
	require.Empty(t, jobs.Items)
	// A new seed and a fresh controller must not write the populated runtime.
	replaceSeedToken(t, seed, seedJWT(time.Now().Add(2*time.Hour), "synthetic-account"))
	require.NoError(t, kube.Update(t.Context(), seed))
	require.NoError(t, New(kube, []string{"team"}, "image").sweep(t.Context()))
	var restarted corev1.Secret
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(runtime), &restarted))
	require.Equal(t, after, restarted)
	// Once due, rotation claims this same destination through the existing path.
	replaceSeedToken(t, &after, seedJWT(time.Now().Add(time.Minute), "synthetic-account"))
	require.NoError(t, kube.Update(t.Context(), &after))
	require.NoError(t, c.sweep(t.Context()))
	require.NoError(t, kube.List(t.Context(), &jobs))
	require.Len(t, jobs.Items, 1)
	require.Equal(t, runtime.UID, metav1.GetControllerOf(&jobs.Items[0]).UID)
	serialized, err := json.Marshal(jobs)
	require.NoError(t, err)
	require.NotContains(t, string(serialized), "SYNTHETIC_REFRESH")
	require.NotContains(t, string(serialized), string(seed.Data["access-token"]))
}

func TestBootstrapInvalidInputsDoNotPersistFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*corev1.Secret, *corev1.Secret, *kagentv1alpha3.ModelConfig)
	}{
		{"cross namespace", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) {
			r.Annotations[SeedSecretAnnotation] = "other/seed"
		}},
		{"noncanonical name", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) {
			r.Annotations[SeedSecretAnnotation] = " seed "
		}},
		{"empty reference", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { r.Annotations[SeedSecretAnnotation] = "" }},
		{"self reference", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { r.Annotations[SeedSecretAnnotation] = r.Name }},
		{"malformed auth", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) {
			s.Data[AuthKey] = []byte("SYNTHETIC_REFRESH")
		}},
		{"missing auth", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { delete(s.Data, AuthKey) }},
		{"empty refresh token", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) {
			s.Data[AuthKey] = bytes.ReplaceAll(s.Data[AuthKey], []byte("SYNTHETIC_REFRESH"), nil)
		}},
		{"missing pair", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { delete(s.Data, "access-token") }},
		{"mismatched pair", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) {
			s.Data["access-token"] = []byte("SYNTHETIC_ACCESS")
		}},
		{"expired", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) {
			replaceSeedToken(t, s, seedJWT(time.Now().Add(-time.Minute), "synthetic-account"))
		}},
		{"inside margin", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) {
			replaceSeedToken(t, s, seedJWT(time.Now().Add(safetyMargin), "synthetic-account"))
		}},
		{"file account", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { m.Spec.OpenAI.AccountID = "different" }},
		{"claims account", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) {
			replaceSeedToken(t, s, seedJWT(time.Now().Add(time.Hour), "different"))
		}},
		{"missing claims account", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) {
			replaceSeedToken(t, s, jwt(time.Now().Add(time.Hour)))
		}},
		{"empty account", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { m.Spec.OpenAI.AccountID = "" }},
		{"empty key", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { m.Spec.APIKeySecretKey = "" }},
		{"auth key", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { m.Spec.APIKeySecretKey = AuthKey }},
		{"invalid key", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { m.Spec.APIKeySecretKey = "invalid/key" }},
		{"immutable", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { r.Immutable = new(true) }},
		{"wrong type", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { r.Type = corev1.SecretTypeTLS }},
		{"claim", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { r.Annotations[ClaimAnnotation] = "claim" }},
		{"empty claim", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { r.Annotations[ClaimAnnotation] = "" }},
		{"state", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) {
			r.Annotations[StateAnnotation] = stateRejected
		}},
		{"hash", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { r.Annotations[HashAnnotation] = "hash" }},
		{"retry", func(r, s *corev1.Secret, m *kagentv1alpha3.ModelConfig) { r.Annotations[RetryAnnotation] = "retry" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime, seed, model := bootstrapFixtures(t)
			tc.change(runtime, seed, model)
			kube := kubeClient(t, runtime, seed, model)
			var before, after corev1.Secret
			require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(runtime), &before))
			err := New(kube, []string{"team"}, "image").sweep(t.Context())
			require.Error(t, err)
			require.NotContains(t, err.Error(), "SYNTHETIC")
			require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(runtime), &after))
			require.Equal(t, before, after)
			var jobs batchv1.JobList
			require.NoError(t, kube.List(t.Context(), &jobs))
			require.Empty(t, jobs.Items)
		})
	}
}

func TestBootstrapModelAgreement(t *testing.T) {
	for _, conflict := range []string{"none", "key", "account", "empty key", "empty account"} {
		t.Run(conflict, func(t *testing.T) {
			runtime, seed, model := bootstrapFixtures(t)
			second := model.DeepCopy()
			second.Name = "second"
			switch conflict {
			case "key":
				second.Spec.APIKeySecretKey = "other-key"
			case "account":
				second.Spec.OpenAI.AccountID = "other-account"
			case "empty key":
				second.Spec.APIKeySecretKey = ""
			case "empty account":
				second.Spec.OpenAI.AccountID = ""
			}
			third := model.DeepCopy()
			third.Name = "third" // A later agreeing model cannot erase conflict.
			kube := kubeClient(t, runtime, seed, model, second, third)
			err := New(kube, nil, "image").sweep(t.Context())
			require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(runtime), runtime))
			if conflict == "none" {
				require.NoError(t, err)
				require.Equal(t, seed.Data[AuthKey], runtime.Data[AuthKey])
			} else {
				require.Error(t, err)
				require.Empty(t, runtime.Data[AuthKey])
				require.Equal(t, map[string]string{SeedSecretAnnotation: "seed", "keep": "annotation"}, runtime.Annotations)
			}
		})
	}
}

func TestBootstrapOwnedJobs(t *testing.T) {
	for _, phase := range []string{"active", "failed pod only", "complete", "failed", "foreign UID"} {
		t.Run(phase, func(t *testing.T) {
			runtime, seed, model := bootstrapFixtures(t)
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "any-owned-job", Namespace: runtime.Namespace,
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Secret", Name: runtime.Name, UID: runtime.UID}}}}
			switch phase {
			case "failed pod only":
				job.Status.Failed = 1
			case "complete":
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			case "failed":
				job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
			case "foreign UID":
				job.OwnerReferences[0].UID = "old-secret-uid"
			}
			kube := kubeClient(t, runtime, seed, model, job)
			require.NoError(t, New(kube, nil, "image").sweep(t.Context()))
			require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(runtime), runtime))
			if phase == "active" || phase == "failed pod only" {
				require.Empty(t, runtime.Data[AuthKey])
			} else {
				require.Equal(t, seed.Data[AuthKey], runtime.Data[AuthKey])
			}
			var jobs batchv1.JobList
			require.NoError(t, kube.List(t.Context(), &jobs))
			require.Len(t, jobs.Items, 1) // Bootstrap neither deletes nor launches Jobs.
		})
	}
}

func TestBootstrapSingleFencedWrite(t *testing.T) {
	for _, response := range []string{"conflict", "concurrent credential writer", "ambiguous success"} {
		t.Run(response, func(t *testing.T) {
			runtime, seed, model := bootstrapFixtures(t)
			base := kubeClient(t, runtime, seed, model)
			writes := 0
			kube := &failingUpdate{Client: base, fail: func(ctx context.Context, object client.Object) error {
				writes++
				var current corev1.Secret
				require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(runtime), &current))
				require.Equal(t, current.ResourceVersion, object.GetResourceVersion())
				if response == "ambiguous success" {
					require.NoError(t, base.Update(ctx, object))
				} else {
					current.Labels["concurrent"] = "preserved"
					if response == "concurrent credential writer" {
						current.Data["access-token"] = []byte("CONCURRENT_ACCESS")
					}
					require.NoError(t, base.Update(ctx, &current))
				}
				return apierrors.NewConflict(corev1.Resource("secrets"), runtime.Name, errors.New("SYNTHETIC_REFRESH"))
			}}
			err := New(kube, nil, "image").sweep(t.Context())
			require.ErrorIs(t, err, errOperation)
			require.NotContains(t, err.Error(), "SYNTHETIC")
			require.Equal(t, 1, writes)
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(runtime), runtime))
			if response != "ambiguous success" {
				require.Empty(t, runtime.Data[AuthKey])
			}
			require.NoError(t, New(base, nil, "image").sweep(t.Context()))
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(runtime), runtime))
			if response == "concurrent credential writer" {
				require.Empty(t, runtime.Data[AuthKey])
				require.Equal(t, []byte("CONCURRENT_ACCESS"), runtime.Data["access-token"])
			} else {
				require.Equal(t, seed.Data[AuthKey], runtime.Data[AuthKey])
			}
			if response != "ambiguous success" {
				require.Equal(t, "preserved", runtime.Labels["concurrent"])
			}
		})
	}
}

func TestBootstrapPopulatedAndManualCompatibility(t *testing.T) {
	for _, mode := range []string{"no annotation", "manual access only", "access only", "unrelated populated", "rejected", "expired"} {
		t.Run(mode, func(t *testing.T) {
			runtime, seed, model := bootstrapFixtures(t)
			switch mode {
			case "no annotation":
				delete(runtime.Annotations, SeedSecretAnnotation)
			case "manual access only":
				delete(runtime.Annotations, SeedSecretAnnotation)
				runtime.Data["access-token"] = []byte("MANUAL_ACCESS")
			case "access only":
				runtime.Data["access-token"] = []byte("MANUAL_ACCESS")
			case "unrelated populated":
				runtime.Data["unrelated"] = []byte("keep")
			case "rejected", "expired":
				runtime.Data[AuthKey] = credential(t, time.Now().Add(-time.Hour), "OLD_REFRESH")
				auth, err := readAuth(runtime.Data[AuthKey])
				require.NoError(t, err)
				runtime.Data["access-token"] = []byte(auth.Tokens.Access)
				if mode == "rejected" {
					runtime.Annotations[StateAnnotation] = stateRejected
					runtime.Annotations[HashAnnotation] = authHash(runtime.Data[AuthKey])
				}
			}
			kube := kubeClient(t, runtime, seed, model)
			var before, after corev1.Secret
			require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(runtime), &before))
			require.NoError(t, New(kube, nil, "image").sweep(t.Context()))
			require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(runtime), &after))
			require.Equal(t, before.Data, after.Data)
			if mode != "expired" {
				require.Equal(t, before, after)
			} else {
				require.Equal(t, statePending, after.Annotations[StateAnnotation])
			}
		})
	}
}

func TestBootstrapSourceNamespaceAndCorrection(t *testing.T) {
	runtime, seed, model := bootstrapFixtures(t)
	foreign := seed.DeepCopy()
	foreign.Namespace = "other"
	kube := kubeClient(t, runtime, foreign, model)
	c := New(kube, nil, "image")
	require.NoError(t, c.sweep(t.Context()))
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(runtime), runtime))
	require.Empty(t, runtime.Data[AuthKey])
	valid := seed.DeepCopy()
	seed.Data["access-token"] = []byte("INCORRECT_ACCESS")
	require.NoError(t, kube.Create(t.Context(), seed))
	require.Error(t, c.sweep(t.Context()))
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(runtime), runtime))
	require.Empty(t, runtime.Data[AuthKey])
	require.NotContains(t, runtime.Annotations, StateAnnotation)
	seed.Data = valid.Data
	require.NoError(t, kube.Update(t.Context(), seed))
	require.NoError(t, c.sweep(t.Context()))
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(runtime), runtime))
	require.Equal(t, valid.Data[AuthKey], runtime.Data[AuthKey])
}

func TestSeedExpiryStrictSafetyMargin(t *testing.T) {
	now := time.Unix(1900000000, 0)
	for _, offset := range []time.Duration{-time.Second, 0, time.Second} {
		t.Run(offset.String(), func(t *testing.T) {
			_, seed, _ := bootstrapFixtures(t)
			replaceSeedToken(t, seed, seedJWT(now.Add(safetyMargin+offset), "synthetic-account"))
			err := validateSeed(seed, credentialDescriptor{key: "access-token", account: "synthetic-account"}, now)
			if offset > 0 {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errCredential)
			}
		})
	}
}

func TestBootstrapDiagnosticsSuppressAdmissionCredentials(t *testing.T) {
	runtime, seed, model := bootstrapFixtures(t)
	base := kubeClient(t, runtime, seed, model)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	kube := &failingUpdate{Client: base, fail: func(context.Context, client.Object) error {
		cancel() // Stop after this sweep's normal sanitized diagnostic.
		return errors.New("SYNTHETIC_REFRESH " + string(seed.Data[AuthKey]))
	}}
	var output bytes.Buffer
	logger, err := logging.New(&output, "debug")
	require.NoError(t, err)
	require.NoError(t, New(kube, nil, "image").Start(logging.IntoContext(ctx, logger)))
	require.Contains(t, output.String(), "failed to reconcile chatgpt credential jobs")
	require.NotContains(t, output.String(), "SYNTHETIC_REFRESH")
	require.NotContains(t, output.String(), string(seed.Data[AuthKey]))
	require.NotContains(t, output.String(), string(seed.Data["access-token"]))
}

func TestBootstrapEmptyDestinationShapes(t *testing.T) {
	for _, shape := range []string{"nil data", "empty map", "empty credential pair"} {
		t.Run(shape, func(t *testing.T) {
			runtime, seed, model := bootstrapFixtures(t)
			runtime.Immutable = new(false)
			switch shape {
			case "nil data":
				runtime.Data = nil
			case "empty map":
				runtime.Data = map[string][]byte{}
			case "empty credential pair":
				runtime.Data[AuthKey] = []byte{}
				runtime.Data["access-token"] = []byte{}
			}
			kube := kubeClient(t, runtime, seed, model)
			require.NoError(t, New(kube, nil, "image").sweep(t.Context()))
			require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(runtime), runtime))
			require.Equal(t, seed.Data[AuthKey], runtime.Data[AuthKey])
			require.Equal(t, seed.Data["access-token"], runtime.Data["access-token"])
		})
	}
}
