package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"strings"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
	"github.com/kagent-dev/kagent/go/core/internal/translator"
	"github.com/kagent-dev/kagent/go/harness/runtime/payload"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
)

// RuntimePayloadCatalog is operator configuration, keyed by provider/platform.
// No payload image is accepted from a Session caller.
// PayloadRelease binds the selected image to its CLI version. Catalog updates
// can promote a new R without rebuilding the controller or the development D.
type PayloadRelease struct {
	Image      string `json:"image"`
	CLIVersion string `json:"cliVersion"`
}

type RuntimePayloadCatalog map[string]PayloadRelease

func ParseRuntimePayloadCatalog(raw string) (RuntimePayloadCatalog, error) {
	catalog := RuntimePayloadCatalog{}
	if strings.TrimSpace(raw) == "" {
		return catalog, nil
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode runtime payload catalog: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("runtime payload catalog must contain one JSON object")
	}
	for key, release := range catalog {
		provider, platform, _ := strings.Cut(key, "/")
		selection := translator.Composition{DevelopmentImage: release.Image, PayloadImage: release.Image, CLIVersion: release.CLIVersion, Provider: translator.HarnessType(provider), Platform: platform, PolicyIdentity: "catalog", Schema: payload.Schema}
		if err := selection.Validate(); err != nil {
			return nil, fmt.Errorf("invalid runtime catalog entry %q: %w", key, err)
		}
	}
	return catalog, nil
}

// EnvironmentPreparer owns per-composition Substrate preparation. It reads the
// compiled Agent target without changing shared definition/observation pointers.
type EnvironmentPreparer struct {
	collections Collections
	store       runtimeRevisionStore
	templates   actorTemplateClient
	catalog     RuntimePayloadCatalog
}

func NewEnvironmentPreparer(collections Collections, store runtimeRevisionStore, templates actorTemplateClient, catalog RuntimePayloadCatalog) *EnvironmentPreparer {
	copy := RuntimePayloadCatalog{}
	maps.Copy(copy, catalog)
	return &EnvironmentPreparer{collections: collections, store: store, templates: templates, catalog: copy}
}

// Prepare resolves the operator payload, prepares its immutable golden, and
// records a ready revision before reservation. Pending/failed goldens return
// errors: a retry observes the same template, never replaces a Full snapshot.
func (p *EnvironmentPreparer) Prepare(ctx context.Context, agent *apiv1alpha1.ResourceReference, environment *apiv1alpha1.DevelopmentEnvironment) (string, *apiv1alpha1.RuntimeComposition, error) {
	state := p.collections.Reconciliations.GetKey(agent.GetNamespace() + "/" + agent.GetName())
	if state == nil || state.Target == nil {
		return "", nil, fmt.Errorf("agent has no compiled runtime target")
	}
	base := state.Target.Revision
	if p.collections.WorkerPools == nil {
		return "", nil, fmt.Errorf("worker pool architecture is unavailable")
	}
	pool := p.collections.WorkerPools.GetKey(base.Namespace + "/" + base.WorkerPoolName)
	if pool == nil {
		return "", nil, fmt.Errorf("composed worker pool %s/%s is unavailable", base.Namespace, base.WorkerPoolName)
	}
	if err := validateWorkerPoolPlatform(*pool, environment.GetPlatform()); err != nil {
		return "", nil, err
	}
	release := p.catalog[string(base.NativeProvider)+"/"+environment.GetPlatform()]
	image := release.Image
	if image == "" {
		return "", nil, fmt.Errorf("no accepted payload for provider %q platform %q", base.NativeProvider, environment.GetPlatform())
	}
	selection := translator.Composition{DevelopmentImage: environment.GetImage(), PayloadImage: image, CLIVersion: release.CLIVersion, Platform: environment.GetPlatform(), PolicyIdentity: environment.GetPolicyIdentity(), Provider: base.NativeProvider, Schema: payload.Schema}
	revision, err := translator.ComposeRevision(base, selection)
	if err != nil {
		return "", nil, err
	}
	id, err := revision.Digest()
	if err != nil {
		return "", nil, err
	}
	template, err := substrate.ActorTemplateForRevision(&revision, id)
	if err != nil {
		return "", nil, err
	}
	observed, err := p.templates.GetActorTemplate(ctx, revision.Namespace, template.GetMetadata().GetName())
	if status.Code(err) == codes.NotFound {
		if err = p.templates.EnsureAtespace(ctx, revision.Namespace); err != nil {
			return "", nil, err
		}
		observed, err = p.templates.CreateActorTemplate(ctx, template)
		if status.Code(err) == codes.AlreadyExists {
			observed, err = p.templates.GetActorTemplate(ctx, revision.Namespace, template.GetMetadata().GetName())
		}
	}
	if err != nil {
		return "", nil, fmt.Errorf("prepare composed ActorTemplate: %w", err)
	}
	if !substrate.ActorTemplateSpecEqual(observed, template) {
		return "", nil, fmt.Errorf("composed ActorTemplate differs from immutable revision")
	}
	composition := &apiv1alpha1.RuntimeComposition{PayloadImage: image, Provider: string(base.NativeProvider), Schema: payload.Schema, CliVersion: release.CLIVersion}
	snapshot, err := json.Marshal(database.EnvironmentRevisionSnapshot{BaseRevision: state.Target.RevisionID.String(), Environment: environment, Composition: composition, Provenance: base.Provenance})
	if err != nil {
		return "", nil, fmt.Errorf("encode composed revision snapshot: %w", err)
	}
	err = p.store.RecordRuntimeRevision(ctx, database.RuntimeRevision{Revision: id.String(), Namespace: base.Namespace, AgentName: base.AgentName, AgentUID: base.AgentUID, SourceSnapshot: snapshot, AgentCard: base.AgentCard, Credentials: base.Credentials, EgressDestinations: base.EgressDestinations, GitOrigins: base.GitOrigins, ActorTemplateAtespace: observed.GetMetadata().GetAtespace(), ActorTemplateName: observed.GetMetadata().GetName(), ActorTemplateUID: observed.GetMetadata().GetUid()}, observed.GetStatus().GetGoldenSnapshotStatus().GetGoldenTag() != nil)
	if err != nil {
		return "", nil, fmt.Errorf("persist composed revision: %w", err)
	}
	golden := observed.GetStatus().GetGoldenSnapshotStatus()
	if golden.GetErrorMessage() != "" {
		return "", nil, fmt.Errorf("%w: revision %s requires corrected inputs", substrate.ErrGoldenSnapshotFailed, id.String())
	}
	if golden.GetGoldenTag() == nil {
		return "", nil, fmt.Errorf("composed revision %s golden preparation pending", id.String())
	}
	return id.String(), composition, nil
}

// A pool without an explicit architecture constraint may resolve multi-arch
// image indexes to a different child than the selected Session platform.
func validateWorkerPoolPlatform(pool *atev1alpha1.WorkerPool, platform string) error {
	if pool == nil || pool.Spec.Template == nil {
		return fmt.Errorf("composed worker pool requires an explicit architecture node selector")
	}
	os, arch, _ := strings.Cut(platform, "/")
	selector := pool.Spec.Template.NodeSelector
	if os != "linux" || (arch != "amd64" && arch != "arm64") || selector[corev1.LabelArchStable] != arch || (selector[corev1.LabelOSStable] != "" && selector[corev1.LabelOSStable] != os) {
		return fmt.Errorf("selected runtime platform %q does not match worker pool %s/%s node selectors", platform, pool.Namespace, pool.Name)
	}
	return nil
}
