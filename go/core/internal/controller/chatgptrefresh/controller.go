package chatgptrefresh

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	kagentv1alpha3 "github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/pkg/logging"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Controller schedules official CLI Jobs, never OAuth requests. Its uncached
// client is required for Secret resourceVersion checks. Each serial sweep
// deduplicates Secret references across ModelConfigs, and durable claims fence
// overlapping leaders, duplicate Job Pods, and restarts.
type Controller struct {
	client     client.Client
	namespaces []string
	image      string
}

var _ manager.LeaderElectionRunnable = (*Controller)(nil)

func New(kube client.Client, namespaces []string, image string) *Controller {
	return &Controller{client: kube, namespaces: namespaces, image: image}
}

func (c *Controller) NeedLeaderElection() bool { return true }

func (c *Controller) Start(ctx context.Context) error {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if c.sweep(ctx) != nil {
			// API/admission errors can echo credentials; never release their text.
			logging.FromContext(ctx).ErrorContext(ctx, "failed to reconcile chatgpt credential jobs")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
	return nil
}

func chatGPT(model *kagentv1alpha3.ModelConfig) bool {
	return model.Spec.Provider == kagentv1alpha3.ModelProviderOpenAI && model.Spec.OpenAI != nil && model.Spec.OpenAI.AuthMethod == kagentv1alpha3.OpenAIAuthMethod_ChatGPT
}

func (c *Controller) sweep(ctx context.Context) error {
	namespaces := c.namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}
	keys := make(map[types.NamespacedName]string)
	failed := false
	for _, namespace := range namespaces {
		var models kagentv1alpha3.ModelConfigList
		if c.client.List(ctx, &models, client.InNamespace(namespace)) != nil {
			failed = true
			continue
		}
		for _, model := range models.Items {
			if !chatGPT(&model) || model.Spec.APIKeySecret == "" || model.Spec.APIKeySecretKey == "" {
				continue
			}
			name := types.NamespacedName{Namespace: model.Namespace, Name: model.Spec.APIKeySecret}
			if previous, found := keys[name]; found && previous != model.Spec.APIKeySecretKey {
				keys[name] = ""
			} else if !found {
				keys[name] = model.Spec.APIKeySecretKey
			}
		}
	}
	for name, key := range keys {
		if key == "" {
			failed = true
			continue
		}
		if c.reconcile(ctx, name, key, time.Now()) != nil {
			failed = true
		}
	}
	if failed {
		return errOperation
	}
	return nil
}

func RequiresReauthentication(secret *corev1.Secret) bool {
	return secret.Annotations[StateAnnotation] == stateRejected && secret.Annotations[HashAnnotation] == authHash(secret.Data[AuthKey])
}

func (c *Controller) reconcile(ctx context.Context, name types.NamespacedName, key string, now time.Time) error {
	var secret corev1.Secret
	if err := c.client.Get(ctx, name, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errOperation
	}
	if len(secret.Data[AuthKey]) == 0 {
		return nil
	} // Existing access-only credentials remain manual.
	if RequiresReauthentication(&secret) {
		return nil
	}
	if secret.Immutable != nil && *secret.Immutable || key == AuthKey {
		return c.reject(ctx, &secret)
	}
	var job batchv1.Job
	jobKey := types.NamespacedName{Namespace: name.Namespace, Name: jobName(name)}
	err := c.client.Get(ctx, jobKey, &job)
	if err != nil && !apierrors.IsNotFound(err) {
		return errOperation
	}
	if err == nil {
		owner := metav1.GetControllerOf(&job)
		if owner == nil || owner.UID != secret.UID {
			return errOperation
		}
		// Terminal conditions, rather than failed Pod counts, prove that all
		// execution has settled. Never overlap replacement Jobs with old Pods.
		complete, failed := false, false
		for _, condition := range job.Status.Conditions {
			if condition.Status == corev1.ConditionTrue {
				complete = complete || condition.Type == batchv1.JobComplete
				failed = failed || condition.Type == batchv1.JobFailed
			}
		}
		if !complete && !failed {
			return nil
		}
		if job.Annotations[ClaimAnnotation] == secret.Annotations[ClaimAnnotation] && secret.Annotations[HashAnnotation] == authHash(secret.Data[AuthKey]) {
			if secret.Annotations[StateAnnotation] == stateRunning {
				return c.reject(ctx, &secret)
			}
			// Failure before acquiring the Pod's claim is safe to retry. Store a
			// jittered delay durably; deletion/restarts cannot cause a retry storm.
			if secret.Annotations[StateAnnotation] == statePending {
				secret.Annotations[RetryAnnotation] = now.Add(time.Minute + time.Duration(rand.Int64N(int64(time.Minute)))).Format(time.RFC3339Nano)
				delete(secret.Annotations, ClaimAnnotation)
				delete(secret.Annotations, StateAnnotation)
				if c.client.Update(ctx, &secret) != nil {
					return errOperation
				}
			}
		}
		if c.client.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationForeground)) != nil {
			return errOperation
		}
		return nil
	}
	if secret.Annotations[HashAnnotation] == authHash(secret.Data[AuthKey]) {
		if secret.Annotations[StateAnnotation] == stateRunning {
			return c.reject(ctx, &secret)
		} // Job deleted after CLI may have spent token.
		if retryAt, err := time.Parse(time.RFC3339Nano, secret.Annotations[RetryAnnotation]); err == nil && now.Before(retryAt) {
			return nil
		}
	}
	auth, err := readAuth(secret.Data[AuthKey])
	if err != nil {
		return c.reject(ctx, &secret)
	}
	if auth.Tokens.Access != string(secret.Data[key]) {
		return c.reject(ctx, &secret)
	}
	refresh, err := due(secret.Data[key], now)
	if err != nil {
		return c.reject(ctx, &secret)
	}
	if !refresh {
		return nil
	}
	if c.image == "" {
		return errOperation
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	if secret.Annotations[StateAnnotation] != statePending || secret.Annotations[HashAnnotation] != authHash(secret.Data[AuthKey]) {
		secret.Annotations[ClaimAnnotation] = uuid.NewString()
		secret.Annotations[StateAnnotation] = statePending
		secret.Annotations[HashAnnotation] = authHash(secret.Data[AuthKey])
		if c.client.Update(ctx, &secret) != nil {
			return errOperation
		}
	}
	return ensureResources(ctx, c.client, resources(&secret, key, c.image, secret.Annotations[ClaimAnnotation]))
}

func (c *Controller) reject(ctx context.Context, secret *corev1.Secret) error {
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[StateAnnotation] = stateRejected
	secret.Annotations[HashAnnotation] = authHash(secret.Data[AuthKey])
	if c.client.Update(ctx, secret) != nil {
		return errOperation
	}
	return nil
}
