package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	sessionsvc "github.com/kagent-dev/kagent/go/core/internal/service/session"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	"github.com/kagent-dev/kagent/go/core/internal/translator"
	"github.com/kagent-dev/kagent/go/harness/runtime/payload"
)

type compositionStore interface {
	GetLatestSuccessfulRevision(ctx context.Context, namespace, name string) (string, error)
	RecordRuntimeRevision(context.Context, database.RuntimeRevision, database.RecordRevisionOptions) (database.GoldenRetry, error)
}

type payloadKey struct {
	provider translator.HarnessType
	platform string
}

// CompositionPreparer prepares the composed variant of an Agent's latest
// revision for a Session-selected development environment. It runs outside
// store transactions: it creates the immutable ActorTemplate, records the
// variant, and waits for its golden snapshot. Session reservation then pins the
// variant only if its base is still the Agent's latest revision.
type CompositionPreparer struct {
	reconciliations interface {
		GetKey(string) *AgentReconciliation
	}
	templates actorTemplateClient
	store     compositionStore
	payloads  map[payloadKey]*configv1.RuntimePayload
	poll      time.Duration
}

// NewCompositionPreparer returns nil when the catalog is empty, which leaves
// composition disabled.
func NewCompositionPreparer(collections Collections, templates actorTemplateClient, store compositionStore, catalog *configv1.RuntimePayloadsConfig) (*CompositionPreparer, error) {
	payloads, err := runtimePayloadCatalog(catalog)
	if err != nil || len(payloads) == 0 {
		return nil, err
	}
	return &CompositionPreparer{
		reconciliations: collections.Reconciliations, templates: templates, store: store,
		payloads: payloads, poll: 2 * time.Second,
	}, nil
}

// ValidateRuntimePayloads rejects duplicate catalog entries.
func ValidateRuntimePayloads(catalog *configv1.RuntimePayloadsConfig) error {
	_, err := runtimePayloadCatalog(catalog)
	return err
}

func runtimePayloadCatalog(catalog *configv1.RuntimePayloadsConfig) (map[payloadKey]*configv1.RuntimePayload, error) {
	payloads := map[payloadKey]*configv1.RuntimePayload{}
	for _, entry := range catalog.GetPayloads() {
		key := payloadKey{provider: translator.HarnessType(entry.GetProvider()), platform: entry.GetPlatform()}
		if _, duplicate := payloads[key]; duplicate {
			return nil, fmt.Errorf("runtime payload catalog lists %s %s more than once", key.provider, key.platform)
		}
		payloads[key] = entry
	}
	return payloads, nil
}

// Prepare returns the variant's revision ID and its resolved composition once
// its golden snapshot is ready. The context bounds the wait.
func (p *CompositionPreparer) Prepare(ctx context.Context, agent *apiv1.ResourceReference, environment *apiv1.DevelopmentEnvironment) (string, *apiv1.RuntimeComposition, error) {
	state := p.reconciliations.GetKey(agent.GetNamespace() + "/" + agent.GetName())
	if state == nil || state.Target == nil {
		return "", nil, fmt.Errorf("%w: Agent %s/%s has no compiled revision", sessionsvc.ErrEnvironmentNotReady, agent.GetNamespace(), agent.GetName())
	}
	latest, err := p.store.GetLatestSuccessfulRevision(ctx, agent.GetNamespace(), agent.GetName())
	if errors.Is(err, database.ErrNotFound) {
		return "", nil, fmt.Errorf("%w: Agent has no prepared revision", sessionsvc.ErrEnvironmentNotReady)
	}
	if err != nil {
		return "", nil, err
	}
	base := state.Target
	if latest != base.RevisionID.String() {
		return "", nil, fmt.Errorf("%w: Agent revision %s is still being prepared", sessionsvc.ErrEnvironmentNotReady, base.RevisionID.Short())
	}
	entry := p.payloads[payloadKey{provider: base.Revision.Harness, platform: environment.GetPlatform()}]
	if entry == nil {
		return "", nil, fmt.Errorf("%w: no runtime payload for the %q harness on %s", sessionsvc.ErrEnvironmentUnsupported, base.Revision.Harness, environment.GetPlatform())
	}
	composed, err := translator.ComposeRevision(base.Revision, translator.Composition{
		DevelopmentImage: environment.GetImage(), PayloadImage: entry.GetImage(), CLIVersion: entry.GetCliVersion(),
		Platform: environment.GetPlatform(), PolicyIdentity: environment.GetPolicyIdentity(),
		Provider: base.Revision.Harness, Schema: payload.Schema,
	})
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", sessionsvc.ErrEnvironmentUnsupported, err)
	}
	composed.BaseRevision = base.RevisionID
	id, err := composed.Digest()
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", sessionsvc.ErrEnvironmentUnsupported, err)
	}
	bundle, err := composed.BootstrapBundle(id)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", sessionsvc.ErrEnvironmentUnsupported, err)
	}
	desired, err := substrate.ActorTemplateForRevision(&composed, id, bundle)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", sessionsvc.ErrEnvironmentUnsupported, err)
	}
	composition := &apiv1.RuntimeComposition{
		DevelopmentEnvironment: environment, PayloadImage: entry.GetImage(), CliVersion: entry.GetCliVersion(),
	}
	for {
		observed, err := p.ensureTemplate(ctx, desired)
		if err != nil {
			return "", nil, err
		}
		record, err := compositionRevision(&composed, id, base.RevisionID, composition, desired, observed)
		if err != nil {
			return "", nil, err
		}
		golden := observed.GetStatus().GetGoldenSnapshotStatus()
		switch {
		case golden.GetErrorMessage() != "":
			if _, err := p.store.RecordRuntimeRevision(ctx, record, database.RecordRevisionOptions{Bootstrap: bundle, GoldenFailedAt: time.Now()}); err != nil {
				return "", nil, fmt.Errorf("record failed composed revision: %w", err)
			}
			ref := observed.GetMetadata()
			if err := p.templates.DeleteActorTemplate(ctx, ref.GetAtespace(), ref.GetName(), ref.GetUid()); err != nil && status.Code(err) != codes.NotFound {
				return "", nil, fmt.Errorf("%w; remove failed ActorTemplate: %v", sessionsvc.ErrEnvironmentGoldenFailed, err)
			}
			return "", nil, fmt.Errorf("%w for revision %s", sessionsvc.ErrEnvironmentGoldenFailed, id.Short())
		case golden.GetGoldenTag() != nil:
			if _, err := p.store.RecordRuntimeRevision(ctx, record, database.RecordRevisionOptions{Bootstrap: bundle}); err != nil {
				return "", nil, fmt.Errorf("record composed revision: %w", err)
			}
			return id.String(), composition, nil
		}
		// Record before waiting so the variant and its bootstrap bundle exist
		// while the golden builds; its base keeps it from collection.
		if _, err := p.store.RecordRuntimeRevision(ctx, record, database.RecordRevisionOptions{Bootstrap: bundle}); err != nil {
			return "", nil, fmt.Errorf("record composed revision: %w", err)
		}
		select {
		case <-ctx.Done():
			return "", nil, fmt.Errorf("wait for composed golden snapshot: %w", ctx.Err())
		case <-time.After(p.poll):
		}
	}
}

// ensureTemplate creates the immutable ActorTemplate or returns the matching
// existing one. A differing template with the same name is a conflict.
func (p *CompositionPreparer) ensureTemplate(ctx context.Context, desired *ateapipb.ActorTemplate) (*ateapipb.ActorTemplate, error) {
	ref := desired.GetMetadata()
	observed, err := p.templates.GetActorTemplate(ctx, ref.GetAtespace(), ref.GetName())
	if status.Code(err) == codes.NotFound {
		if err := p.templates.EnsureAtespace(ctx, ref.GetAtespace()); err != nil {
			return nil, fmt.Errorf("ensure Atespace %s: %w", ref.GetAtespace(), err)
		}
		observed, err = p.templates.CreateActorTemplate(ctx, desired)
		if status.Code(err) == codes.InvalidArgument {
			return nil, fmt.Errorf("%w: Substrate rejected the composed ActorTemplate", sessionsvc.ErrEnvironmentUnsupported)
		}
		if status.Code(err) == codes.AlreadyExists {
			observed, err = p.templates.GetActorTemplate(ctx, ref.GetAtespace(), ref.GetName())
		}
	}
	if err != nil {
		return nil, fmt.Errorf("reconcile composed ActorTemplate %s/%s: %w", ref.GetAtespace(), ref.GetName(), err)
	}
	if !substrate.ActorTemplateSpecEqual(observed, desired) {
		return nil, fmt.Errorf("existing immutable ActorTemplate %s/%s differs from the composed revision", ref.GetAtespace(), ref.GetName())
	}
	return observed, nil
}

func compositionRevision(composed *translator.Revision, id, base translator.RevisionID, composition *apiv1.RuntimeComposition, desired, observed *ateapipb.ActorTemplate) (database.RuntimeRevision, error) {
	digest, err := substrate.ActorTemplateSpecDigest(desired)
	if err != nil {
		return database.RuntimeRevision{}, err
	}
	ref := desired.GetMetadata()
	return database.RuntimeRevision{
		Revision: id.String(), Namespace: composed.Namespace,
		AgentName: composed.AgentName, AgentUID: composed.AgentUID,
		SourceSnapshot: composed.Provenance, AgentCard: composed.AgentCard,
		EgressDestinations:      composed.EgressDestinations,
		Credentials:             composed.Credentials,
		TokenPassthroughTargets: composed.TokenPassthroughTargets,
		GitOrigins:              composed.GitOrigins,
		GitEgressRestricted:     composed.GitEgressRestricted,
		ActorTemplateAtespace:   ref.GetAtespace(), ActorTemplateName: ref.GetName(),
		ActorTemplateSpecDigest: digest, ActorTemplateUID: observed.GetMetadata().GetUid(),
		Composition: composition, BaseRevision: base.String(),
	}, nil
}
