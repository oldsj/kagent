package chatgptrefresh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	kagentv1alpha3 "github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func jwt(expires time.Time) string {
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expires.Unix()))) + ".synthetic"
}

func credential(t *testing.T, expires time.Time, refresh string) []byte {
	t.Helper()
	var auth authFile
	auth.AuthMode = "chatgpt"
	auth.Tokens.Access, auth.Tokens.Refresh, auth.Tokens.AccountID = jwt(expires), refresh, "synthetic-account"
	data, err := json.Marshal(auth)
	require.NoError(t, err)
	return data
}

func kubeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme, rbacv1.AddToScheme, networkingv1.AddToScheme, kagentv1alpha3.AddToScheme} {
		require.NoError(t, add(scheme))
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func secretFixture(t *testing.T) *corev1.Secret {
	t.Helper()
	data := credential(t, time.Now().Add(time.Minute), "synthetic-old-refresh")
	auth, err := readAuth(data)
	require.NoError(t, err)
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: "team", UID: "secret-uid"}, Data: map[string][]byte{AuthKey: data, "access-token": []byte(auth.Tokens.Access), "unrelated": []byte("keep")}}
}

func TestExpiryAndMargin(t *testing.T) {
	now := time.Unix(1900000000, 0)
	for _, tc := range []struct {
		name    string
		token   string
		want    bool
		wantErr bool
	}{
		{"before margin", jwt(now.Add(safetyMargin + time.Second)), false, false},
		{"at margin", jwt(now.Add(safetyMargin)), true, false},
		{"expired", jwt(now.Add(-time.Second)), true, false},
		{"bad payload", "e30.SYNTHETIC_SECRET.e30", false, true},
		{"missing expiry", "e30.e30.e30", false, true},
		{"noninteger expiry", "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":"SYNTHETIC_SECRET"}`)) + ".e30", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := due([]byte(tc.token), now)
			require.Equal(t, tc.want, got)
			if tc.wantErr {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "SYNTHETIC_SECRET")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSchedulingDeduplicatesSecretAndRestrictsJob(t *testing.T) {
	secret := secretFixture(t)
	model := &kagentv1alpha3.ModelConfig{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "team"}, Spec: kagentv1alpha3.ModelConfigSpec{Provider: kagentv1alpha3.ModelProviderOpenAI, APIKeySecret: "auth", APIKeySecretKey: "access-token", OpenAI: &kagentv1alpha3.OpenAIConfig{AuthMethod: kagentv1alpha3.OpenAIAuthMethod_ChatGPT}}}
	second := model.DeepCopy()
	second.Name = "second"
	kube := kubeClient(t, secret, model, second)
	c := New(kube, []string{"team"}, "codex:pinned")
	require.True(t, c.NeedLeaderElection())
	require.NoError(t, c.sweep(t.Context()))
	require.NoError(t, c.sweep(t.Context()))
	var jobs batchv1.JobList
	require.NoError(t, kube.List(t.Context(), &jobs))
	require.Len(t, jobs.Items, 1)
	job := jobs.Items[0]
	require.EqualValues(t, 0, *job.Spec.BackoffLimit)
	require.Equal(t, corev1.RestartPolicyNever, job.Spec.Template.Spec.RestartPolicy)
	require.Equal(t, corev1.StorageMediumMemory, job.Spec.Template.Spec.Volumes[0].EmptyDir.Medium)
	var role rbacv1.Role
	require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(&job), &role))
	require.Equal(t, []string{"auth"}, role.Rules[0].ResourceNames)
	require.Equal(t, []string{"get", "update"}, role.Rules[0].Verbs)
	serialized, err := json.Marshal(resources(secret, "access-token", "codex:pinned", "claim"))
	require.NoError(t, err)
	require.NotContains(t, string(serialized), "synthetic-old-refresh")
	require.NotContains(t, string(serialized), string(secret.Data["access-token"]))
}

type failingUpdate struct {
	client.Client
	fail func(context.Context, client.Object) error
}

func (f *failingUpdate) Update(ctx context.Context, object client.Object, opts ...client.UpdateOption) error {
	if err := f.fail(ctx, object); err != nil {
		return err
	}
	return f.Client.Update(ctx, object, opts...)
}

func TestClaimConflictCreatesNoJob(t *testing.T) {
	secret := secretFixture(t)
	base := kubeClient(t, secret)
	kube := &failingUpdate{Client: base, fail: func(context.Context, client.Object) error {
		return apierrors.NewConflict(corev1.Resource("secrets"), "auth", errors.New("SYNTHETIC_SECRET"))
	}}
	c := New(kube, nil, "codex:pinned")
	err := c.reconcile(t.Context(), client.ObjectKeyFromObject(secret), "access-token", time.Now())
	require.Error(t, err)
	require.NotContains(t, err.Error(), "SYNTHETIC_SECRET")
	var jobs batchv1.JobList
	require.NoError(t, base.List(t.Context(), &jobs))
	require.Empty(t, jobs.Items)
}

func TestFailedJobTerminalVersusSafeRetry(t *testing.T) {
	for _, phase := range []string{statePending, stateRunning} {
		t.Run(phase, func(t *testing.T) {
			secret := secretFixture(t)
			secret.Annotations = map[string]string{StateAnnotation: phase, ClaimAnnotation: "claim", HashAnnotation: authHash(secret.Data[AuthKey])}
			objects := resources(secret, "access-token", "image", "claim")
			job := objects[len(objects)-1].(*batchv1.Job)
			job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
			kube := kubeClient(t, secret, job)
			c := New(kube, nil, "image")
			require.NoError(t, c.reconcile(t.Context(), client.ObjectKeyFromObject(secret), "access-token", time.Now()))
			require.NoError(t, kube.Get(t.Context(), client.ObjectKeyFromObject(secret), secret))
			if phase == stateRunning {
				require.True(t, RequiresReauthentication(secret))
				require.NoError(t, c.reconcile(t.Context(), client.ObjectKeyFromObject(secret), "access-token", time.Now()))
			} else {
				require.False(t, RequiresReauthentication(secret))
				retry, err := time.Parse(time.RFC3339Nano, secret.Annotations[RetryAnnotation])
				require.NoError(t, err)
				require.True(t, retry.After(time.Now()))
				require.NoError(t, c.reconcile(t.Context(), client.ObjectKeyFromObject(secret), "access-token", time.Now()))
				var jobs batchv1.JobList
				require.NoError(t, kube.List(t.Context(), &jobs))
				require.Empty(t, jobs.Items)
			}
		})
	}
}

// A synthetic CLI implements only version, initialize and account/read. It
// emits a secret-like stderr sentinel to prove diagnostics stay suppressed.
func fakeCLI(t *testing.T, home string, after []byte, fail bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	result, requests := filepath.Join(dir, "rotated.json"), filepath.Join(dir, "requests.jsonl")
	require.NoError(t, os.WriteFile(result, after, 0600))
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'codex-cli 0.148.0'; exit; fi\necho SYNTHETIC_SECRET >&2\nwhile IFS= read -r line; do\nprintf '%s\\n' \"$line\" >> '" + requests + "'\ncase \"$line\" in\n*initialize*) echo '{\"id\":1,\"result\":{}}';;\n*account/read*) "
	if fail {
		script += "echo '{\"id\":2,\"error\":{\"message\":\"SYNTHETIC_SECRET\"}}';;\n"
	} else {
		script += "cp '" + result + "' '" + filepath.Join(home, "auth.json") + "'; echo '{\"id\":2,\"result\":{}}';;\n"
	}
	script += "esac\ndone\n"
	executable := filepath.Join(dir, "codex")
	require.NoError(t, os.WriteFile(executable, []byte(script), 0700))
	return executable, requests
}

func TestWorkerRotationAndPersistenceConflict(t *testing.T) {
	secret := secretFixture(t)
	secret.Annotations = map[string]string{StateAnnotation: statePending, ClaimAnnotation: "claim", HashAnnotation: authHash(secret.Data[AuthKey])}
	base := kubeClient(t, secret)
	after := credential(t, time.Now().Add(time.Hour), "synthetic-new-refresh")
	home := filepath.Join(t.TempDir(), "codex-home")
	executable, requests := fakeCLI(t, home, after, false)
	conflicted := false
	kube := &failingUpdate{Client: base, fail: func(ctx context.Context, object client.Object) error {
		s := object.(*corev1.Secret)
		if !conflicted && string(s.Data[AuthKey]) == string(after) {
			conflicted = true
			var current corev1.Secret
			require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(s), &current))
			current.Labels = map[string]string{"concurrent": "keep"}
			require.NoError(t, base.Update(ctx, &current))
			return apierrors.NewConflict(corev1.Resource("secrets"), s.Name, errors.New("SYNTHETIC_SECRET"))
		}
		return nil
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, RunJob(ctx, kube, client.ObjectKeyFromObject(secret), "access-token", "claim", home, executable))
	require.True(t, conflicted)
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(secret), secret))
	require.Equal(t, after, secret.Data[AuthKey])
	auth, err := readAuth(after)
	require.NoError(t, err)
	require.Equal(t, auth.Tokens.Access, string(secret.Data["access-token"]))
	require.Equal(t, "keep", secret.Labels["concurrent"])
	require.Equal(t, []byte("keep"), secret.Data["unrelated"])
	require.Empty(t, secret.Annotations)
	data, err := os.ReadFile(requests)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(data), "account/read"))
	require.Contains(t, string(data), `"refreshToken":true`)
	require.NotContains(t, string(data), "thread/")
	require.NotContains(t, string(data), "turn/")
	// A duplicate Pod cannot run Codex or reuse the original token.
	require.Error(t, RunJob(ctx, kube, client.ObjectKeyFromObject(secret), "access-token", "claim", home, executable))
	newData, err := os.ReadFile(requests)
	require.NoError(t, err)
	require.Equal(t, data, newData)
}

func TestCLIFailureDoesNotExposeOrReplayCredential(t *testing.T) {
	secret := secretFixture(t)
	secret.Annotations = map[string]string{StateAnnotation: statePending, ClaimAnnotation: "claim", HashAnnotation: authHash(secret.Data[AuthKey])}
	kube := kubeClient(t, secret)
	home := filepath.Join(t.TempDir(), "home")
	executable, requests := fakeCLI(t, home, secret.Data[AuthKey], true)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	err := RunJob(ctx, kube, client.ObjectKeyFromObject(secret), "access-token", "claim", home, executable)
	require.ErrorIs(t, err, errReauthentication)
	require.NotContains(t, err.Error(), "SYNTHETIC_SECRET")
	require.NoError(t, kube.Get(ctx, client.ObjectKeyFromObject(secret), secret))
	require.Equal(t, stateRunning, secret.Annotations[StateAnnotation])
	require.Error(t, RunJob(ctx, kube, client.ObjectKeyFromObject(secret), "access-token", "claim", home, executable))
	data, err := os.ReadFile(requests)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(data), "account/read"))
}

func TestOverlappingPodsSpendCredentialOnce(t *testing.T) {
	secret := secretFixture(t)
	secret.Annotations = map[string]string{StateAnnotation: statePending, ClaimAnnotation: "claim", HashAnnotation: authHash(secret.Data[AuthKey])}
	kube := kubeClient(t, secret)
	after := credential(t, time.Now().Add(time.Hour), "synthetic-rotated-refresh")
	var homes, executables, paths [2]string
	for i := range homes {
		homes[i] = filepath.Join(t.TempDir(), "home")
		executables[i], paths[i] = fakeCLI(t, homes[i], after, false)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for i := range homes {
		workers.Go(func() {
			<-start
			results <- RunJob(ctx, kube, client.ObjectKeyFromObject(secret), "access-token", "claim", homes[i], executables[i])
		})
	}
	close(start)
	workers.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	require.Equal(t, 1, succeeded)
	calls := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if !os.IsNotExist(err) {
			require.NoError(t, err)
			calls += strings.Count(string(data), "account/read")
		}
	}
	require.Equal(t, 1, calls)
}
