package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	a2apb "github.com/a2aproject/a2a-go/v2/a2apb/v1"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/proto"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	configv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/config/v1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	sessionsvc "github.com/kagent-dev/kagent/go/core/internal/service/session"
	"github.com/kagent-dev/kagent/go/core/internal/translator"
	claudeconfig "github.com/kagent-dev/kagent/go/harness/claude/config"
)

const compositionPayloadImage = "registry.example/runtime-claude@sha256:2222222222222222222222222222222222222222222222222222222222222222"

func TestCompositionPreparer(t *testing.T) {
	agent := &apiv1.ResourceReference{Namespace: "mainloop", Name: "coder"}
	selection := &apiv1.DevelopmentEnvironment{
		Image:    "registry.example/dev@sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Platform: "linux/arm64", PolicyIdentity: "project/v1",
	}

	t.Run("Given an Agent at its latest revision, when a development environment is prepared and its golden becomes ready, assert that the variant is recorded against its base", func(t *testing.T) {
		fixture := newCompositionFixture(t)
		fixture.templates.golden = &ateapipb.GoldenSnapshotStatus{GoldenTag: &ateapipb.ObjectRef{Atespace: "ate-golden", Name: "golden"}}

		revision, composition, err := fixture.preparer.Prepare(t.Context(), agent, selection)

		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		recorded := fixture.store.recorded
		if recorded.Revision != revision || recorded.BaseRevision != fixture.base.String() || revision == fixture.base.String() {
			t.Fatalf("recorded %s with base %s, returned %s; want a new variant of %s", recorded.Revision, recorded.BaseRevision, revision, fixture.base)
		}
		if !proto.Equal(recorded.Composition.Runtime, composition) || !proto.Equal(recorded.Composition.Environment, selection) ||
			composition.GetPayloadImage() != compositionPayloadImage || composition.GetProvider() != "claude" || composition.GetSchema() != 1 || composition.GetCliVersion() != "2.1.285" {
			t.Fatalf("composition = %v, recorded %v", composition, recorded.Composition)
		}
		if image := fixture.templates.template.GetContainers()[0].GetImage(); image != selection.GetImage() {
			t.Fatalf("ActorTemplate image = %q, want the development image", image)
		}
	})

	t.Run("Given a golden build that fails, when a development environment is prepared, assert that the failed template is removed for a later retry", func(t *testing.T) {
		fixture := newCompositionFixture(t)
		fixture.templates.golden = &ateapipb.GoldenSnapshotStatus{ErrorMessage: "pull failed"}

		_, _, err := fixture.preparer.Prepare(t.Context(), agent, selection)

		if !errors.Is(err, sessionsvc.ErrEnvironmentGoldenFailed) {
			t.Fatalf("Prepare() error = %v, want golden failure", err)
		}
		if len(fixture.templates.deletedUIDs) != 1 || fixture.templates.deletedUIDs[0] != "actor-uid" || fixture.store.options.GoldenFailedAt.IsZero() {
			t.Fatalf("deleted %v, failure recorded at %v; want the failed template removed and recorded", fixture.templates.deletedUIDs, fixture.store.options.GoldenFailedAt)
		}
	})

	t.Run("Given a golden that is still building, when the caller's deadline passes, assert that preparation stops with the variant recorded", func(t *testing.T) {
		fixture := newCompositionFixture(t)
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()

		_, _, err := fixture.preparer.Prepare(ctx, agent, selection)

		if !errors.Is(err, context.DeadlineExceeded) || fixture.store.recorded.BaseRevision != fixture.base.String() {
			t.Fatalf("Prepare() error = %v, recorded %+v; want the deadline after recording", err, fixture.store.recorded)
		}
	})

	refusals := []struct {
		name   string
		mutate func(*compositionFixture)
		want   error
	}{
		{"an Agent whose compiled target is not yet its latest revision", func(f *compositionFixture) { f.store.latest = "older" }, sessionsvc.ErrEnvironmentNotReady},
		{"an Agent with no prepared revision", func(f *compositionFixture) { f.store.latestErr = database.ErrNotFound }, sessionsvc.ErrEnvironmentNotReady},
		{"an unknown Agent", func(f *compositionFixture) { f.reconciliations.state = nil }, sessionsvc.ErrEnvironmentNotReady},
		{"a catalog without the selected platform", func(f *compositionFixture) { f.preparer.payloads = map[payloadKey]*configv1.RuntimePayload{} }, sessionsvc.ErrEnvironmentUnsupported},
		{"a WorkerPool pinned to another architecture", func(f *compositionFixture) {
			f.reconciliations.state.Target.Revision.WorkerPoolPlatform = "linux/amd64"
		}, sessionsvc.ErrEnvironmentUnsupported},
	}
	for _, refusal := range refusals {
		t.Run("Given "+refusal.name+", when a development environment is prepared, assert that no template is created", func(t *testing.T) {
			fixture := newCompositionFixture(t)
			refusal.mutate(fixture)

			_, _, err := fixture.preparer.Prepare(t.Context(), agent, selection)

			if !errors.Is(err, refusal.want) || fixture.templates.template != nil {
				t.Fatalf("Prepare() error = %v, template = %v; want %v and no template", err, fixture.templates.template, refusal.want)
			}
		})
	}
}

func TestRuntimePayloadCatalogRejectsDuplicates(t *testing.T) {
	entry := &configv1.RuntimePayload{Provider: "claude", Platform: "linux/arm64", Image: compositionPayloadImage, CliVersion: "2.1.285"}

	err := ValidateRuntimePayloads(&configv1.RuntimePayloadsConfig{Payloads: []*configv1.RuntimePayload{entry, entry}})

	if err == nil {
		t.Fatal("ValidateRuntimePayloads() accepted a duplicate provider and platform")
	}
}

type compositionFixture struct {
	preparer        *CompositionPreparer
	templates       *goldenTemplates
	store           *compositionTestStore
	reconciliations *singleReconciliation
	base            translator.RevisionID
}

func newCompositionFixture(t *testing.T) *compositionFixture {
	t.Helper()
	config, err := json.Marshal(claudeconfig.Production("claude-sonnet-5-5", "build things"))
	if err != nil {
		t.Fatal(err)
	}
	revision := translator.Revision{
		ControlPlaneURL: "http://kagent-controller.kagent.svc:8083", CPU: "1", Memory: "1Gi",
		AgentName: "coder", AgentUID: "uid", Namespace: "mainloop", WorkerPoolName: "arm64", SnapshotLocation: "snapshots",
		Image:      "ghcr.example/kagent-claude@sha256:3333333333333333333333333333333333333333333333333333333333333333",
		ConfigJSON: config, Harness: translator.HarnessTypeClaude, WorkerPoolPlatform: "linux/arm64",
		AgentCard: &a2apb.AgentCard{Name: "coder", Version: "v1", Capabilities: &a2apb.AgentCapabilities{},
			SupportedInterfaces: []*a2apb.AgentInterface{{Url: "http://127.0.0.1:80", ProtocolBinding: "GRPC", ProtocolVersion: "1.0"}}, DefaultInputModes: []string{"text"}, DefaultOutputModes: []string{"text"}},
	}
	base, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	fixture := &compositionFixture{
		templates:       &goldenTemplates{fakeActorTemplates: &fakeActorTemplates{}},
		store:           &compositionTestStore{latest: base.String()},
		reconciliations: &singleReconciliation{state: &AgentReconciliation{Target: &compiledTarget{Revision: revision, RevisionID: base}}},
		base:            base,
	}
	fixture.preparer = &CompositionPreparer{
		reconciliations: fixture.reconciliations, templates: fixture.templates, store: fixture.store,
		payloads: map[payloadKey]*configv1.RuntimePayload{
			{provider: translator.HarnessTypeClaude, platform: "linux/arm64"}: {Provider: "claude", Platform: "linux/arm64", Image: compositionPayloadImage, CliVersion: "2.1.285"},
		},
		poll: time.Millisecond,
	}
	return fixture
}

type singleReconciliation struct{ state *AgentReconciliation }

func (s *singleReconciliation) GetKey(string) *AgentReconciliation { return s.state }

// goldenTemplates reports a fixed golden status on every existing template.
type goldenTemplates struct {
	*fakeActorTemplates
	golden *ateapipb.GoldenSnapshotStatus
}

func (g *goldenTemplates) GetActorTemplate(ctx context.Context, atespace, name string) (*ateapipb.ActorTemplate, error) {
	template, err := g.fakeActorTemplates.GetActorTemplate(ctx, atespace, name)
	if err != nil || g.golden == nil {
		return template, err
	}
	observed := proto.CloneOf(template)
	observed.Status = &ateapipb.ActorTemplateStatus{GoldenSnapshotStatus: g.golden}
	return observed, nil
}

type compositionTestStore struct {
	latest    string
	latestErr error
	recorded  database.RuntimeRevision
	options   database.RecordRevisionOptions
}

func (s *compositionTestStore) GetLatestSuccessfulRevision(context.Context, string, string) (string, error) {
	return s.latest, s.latestErr
}

func (s *compositionTestStore) RecordRuntimeRevision(_ context.Context, revision database.RuntimeRevision, options database.RecordRevisionOptions) (database.GoldenRetry, error) {
	s.recorded, s.options = revision, options
	return database.GoldenRetry{}, nil
}
