package chatgptrefresh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const jobLabel = "kagent.dev/chatgpt-refresh-job"

func jobName(name types.NamespacedName) string {
	hash := sha256.Sum256([]byte(name.String()))
	return "chatgpt-refresh-" + hex.EncodeToString(hash[:12])
}

func resources(secret *corev1.Secret, key, image, claim string) []client.Object {
	name := jobName(client.ObjectKeyFromObject(secret))
	metadata := metav1.ObjectMeta{Name: name, Namespace: secret.Namespace,
		Labels:          map[string]string{jobLabel: name},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Secret", Name: secret.Name, UID: secret.UID, Controller: new(true)}}}
	sa := &corev1.ServiceAccount{ObjectMeta: *metadata.DeepCopy()}
	role := &rbacv1.Role{ObjectMeta: *metadata.DeepCopy(), Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{secret.Name}, Verbs: []string{"get", "update"}}}}
	binding := &rbacv1.RoleBinding{ObjectMeta: *metadata.DeepCopy(), RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: secret.Namespace}}}
	// Standard NetworkPolicy has no DNS-name match. Permit DNS and HTTPS for
	// the official login authority and Kubernetes API; no Actor policy changes.
	// Installations may further constrain hosts using their CNI's FQDN policy.
	policy := &networkingv1.NetworkPolicy{ObjectMeta: *metadata.DeepCopy(), Spec: networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{jobLabel: name}},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		Egress: []networkingv1.NetworkPolicyEgressRule{{Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: new(corev1.ProtocolTCP), Port: new(intstr.FromInt32(443))},
			{Protocol: new(corev1.ProtocolTCP), Port: new(intstr.FromInt32(6443))}, // API server after service DNAT.
			{Protocol: new(corev1.ProtocolTCP), Port: new(intstr.FromInt32(53))},
			{Protocol: new(corev1.ProtocolUDP), Port: new(intstr.FromInt32(53))},
		}}},
	}}
	job := &batchv1.Job{ObjectMeta: *metadata.DeepCopy(), Spec: batchv1.JobSpec{
		Parallelism: new(int32(1)), Completions: new(int32(1)), BackoffLimit: new(int32(0)), ActiveDeadlineSeconds: new(int64(180)),
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{jobLabel: name}}, Spec: corev1.PodSpec{
			ServiceAccountName: name, RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: new(true), RunAsUser: new(int64(65532)), RunAsGroup: new(int64(65532)), FSGroup: new(int64(65532))},
			Containers: []corev1.Container{{Name: "refresh", Image: image, Command: []string{"/usr/local/bin/kagent-credential-refresh"},
				Args:            []string{"--namespace", secret.Namespace, "--secret", secret.Name, "--access-key", key, "--claim", claim},
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: new(false), ReadOnlyRootFilesystem: new(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				VolumeMounts:    []corev1.VolumeMount{{Name: "home", MountPath: "/work"}, {Name: "tmp", MountPath: "/tmp"}},
				Resources:       corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}},
			}},
			Volumes: []corev1.Volume{{Name: "home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: new(resource.MustParse("64Mi"))}}}, {Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: new(resource.MustParse("16Mi"))}}}},
		}},
	}}
	job.Annotations = map[string]string{ClaimAnnotation: claim}
	return []client.Object{sa, role, binding, policy, job}
}

// Create prerequisites before the Job. Never adopt or overwrite a foreign
// resource at the deterministic name; every resource is tied to Secret UID.
func ensureResources(ctx context.Context, kube client.Client, desired []client.Object) error {
	for _, object := range desired {
		if err := kube.Create(ctx, object); err == nil {
			continue
		} else if !apierrors.IsAlreadyExists(err) {
			return errOperation
		}
		current := object.DeepCopyObject().(client.Object)
		if kube.Get(ctx, client.ObjectKeyFromObject(object), current) != nil {
			return errOperation
		}
		owner := metav1.GetControllerOf(current)
		if owner == nil || owner.UID != object.GetOwnerReferences()[0].UID {
			return errOperation
		}
		switch actual := current.(type) {
		case *rbacv1.Role:
			if !apiequality.Semantic.DeepEqual(actual.Rules, object.(*rbacv1.Role).Rules) {
				return errOperation
			}
		case *rbacv1.RoleBinding:
			desired := object.(*rbacv1.RoleBinding)
			if !apiequality.Semantic.DeepEqual(actual.RoleRef, desired.RoleRef) || !apiequality.Semantic.DeepEqual(actual.Subjects, desired.Subjects) {
				return errOperation
			}
		}
		if _, isJob := object.(*batchv1.Job); isJob && current.GetAnnotations()[ClaimAnnotation] != object.GetAnnotations()[ClaimAnnotation] {
			return errOperation
		}
	}
	return nil
}
