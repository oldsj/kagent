package substrate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/egress"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NewRuntimeGeneration computes fresh private issuance intent. The caller must
// persist it before invoking any external effect. Token is never runtime input.
func NewRuntimeGeneration(sessionID, atespace, namespace string) (database.RuntimeGeneration, string, error) {
	id, err := uuid.Parse(sessionID)
	if err != nil {
		return database.RuntimeGeneration{}, "", err
	}
	entropy := make([]byte, 40)
	if _, err := rand.Read(entropy); err != nil {
		return database.RuntimeGeneration{}, "", err
	}
	generationID := uuid.New()
	token := hex.EncodeToString(entropy[:32])
	digest := sha256.Sum256([]byte(token))
	return database.RuntimeGeneration{ID: generationID, SessionID: id, Atespace: atespace, ActorName: "session-" + id.String() + "-" + hex.EncodeToString(entropy[32:]), CredentialURI: "ate-secret://k8s.io/default/" + namespace + "/" + egress.RuntimeSecretPrefix + generationID.String() + "/token", TokenDigest: digest[:], Phase: "allocated"}, token, nil
}

// RuntimeCredentialIssuer owns uncached private Secret I/O. It never returns
// token values and never updates a Secret to install another generation.
type RuntimeCredentialIssuer struct {
	kube      client.Client
	namespace string
}

func NewRuntimeCredentialIssuer(kube client.Client, namespace string) *RuntimeCredentialIssuer {
	return &RuntimeCredentialIssuer{kube: kube, namespace: namespace}
}
func (i *RuntimeCredentialIssuer) Namespace() string { return i.namespace }

func (i *RuntimeCredentialIssuer) key(g database.RuntimeGeneration) (client.ObjectKey, error) {
	key := client.ObjectKey{Namespace: i.namespace, Name: egress.RuntimeSecretPrefix + g.ID.String()}
	if g.CredentialURI != "ate-secret://k8s.io/default/"+key.Namespace+"/"+key.Name+"/token" {
		return key, fmt.Errorf("runtime credential reference changed")
	}
	return key, nil
}

// Ensure only creates when the original allocation's token remains known.
// Lost creation replies reconcile an exact immutable Secret; missing issuance
// material is a hold, never permission to rotate a digest or adopt a Secret.
func (i *RuntimeCredentialIssuer) Ensure(ctx context.Context, g database.RuntimeGeneration, originalToken string) error {
	key, err := i.key(g)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{}
	err = i.kube.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) && originalToken != "" {
		digest := sha256.Sum256([]byte(originalToken))
		if !equalDigest(digest[:], g.TokenDigest) {
			return fmt.Errorf("issuance token does not match durable intent")
		}
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: map[string]string{"kagent.dev/runtime-injection": "true", "kagent.dev/runtime-generation": g.ID.String()}}, Immutable: new(true), Data: map[string][]byte{"token": []byte(originalToken)}}
		if err := i.kube.Create(ctx, secret); err != nil {
			return fmt.Errorf("runtime credential issuance remains uncertain")
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("runtime credential issuance unavailable")
	}
	digest := sha256.Sum256(secret.Data["token"])
	if secret.Immutable == nil || !*secret.Immutable || secret.Labels["kagent.dev/runtime-generation"] != g.ID.String() || secret.Labels["kagent.dev/runtime-injection"] != "true" || !equalDigest(digest[:], g.TokenDigest) {
		return fmt.Errorf("runtime credential identity changed")
	}
	return nil
}
func equalDigest(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var difference byte
	for j := range a {
		difference |= a[j] ^ b[j]
	}
	return difference == 0
}

// Delete follows durable revocation; UID preconditions protect a foreign Secret.
func (i *RuntimeCredentialIssuer) Delete(ctx context.Context, g database.RuntimeGeneration) error {
	key, err := i.key(g)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{}
	if err := i.kube.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("runtime credential cleanup unavailable")
	}
	digest := sha256.Sum256(secret.Data["token"])
	if secret.Labels["kagent.dev/runtime-generation"] != g.ID.String() || secret.Labels["kagent.dev/runtime-injection"] != "true" || secret.Immutable == nil || !*secret.Immutable || !equalDigest(digest[:], g.TokenDigest) {
		return fmt.Errorf("refuse foreign runtime credential cleanup")
	}
	uid := types.UID(secret.UID)
	if err := i.kube.Delete(ctx, secret, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("runtime credential cleanup failed")
	}
	return nil
}

// RuntimeEgressPolicy confines name-bound credential effects to one exact
// callback host/scheme/port. User bindings cannot select this host or capability.
func RuntimeEgressPolicy(g database.RuntimeGeneration, callbackOrigin string, destinations []string, credentials []egress.Credential) (*ateapipb.EgressPolicy, error) {
	credentials, err := egress.CanonicalCredentials(credentials)
	if err != nil {
		return nil, err
	}
	callback, err := url.Parse(callbackOrigin)
	if err != nil || callback.Hostname() == "" || callback.Path != "" || callback.User != nil || callback.RawQuery != "" || callback.Fragment != "" {
		return nil, fmt.Errorf("invalid runtime callback origin")
	}
	callbackOrigin = egress.Origin(callback)
	for _, destination := range destinations {
		u, err := url.Parse(destination)
		if err != nil {
			return nil, fmt.Errorf("invalid runtime destination")
		}
		if strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), callback.Hostname()) && egress.Origin(u) != callbackOrigin {
			return nil, fmt.Errorf("runtime callback host has an alternate origin")
		}
	}
	for _, c := range credentials {
		if strings.EqualFold(c.Header, egress.RuntimeTokenHeader) || strings.EqualFold(c.Hostname, callback.Hostname()) {
			return nil, fmt.Errorf("reserved runtime credential destination")
		}
	}
	destinations = append(append([]string(nil), destinations...), callbackOrigin)
	credentials = append(append([]egress.Credential(nil), credentials...), egress.Credential{Hostname: callback.Hostname(), Header: egress.RuntimeTokenHeader, URI: g.CredentialURI})
	return ActorEgressPolicy(g.Atespace, destinations, credentials)
}
