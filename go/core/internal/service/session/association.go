package session

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
)

// generationNamespace scopes runtime generation IDs to this purpose.
var generationNamespace = uuid.MustParse("6f0e4b8a-3c1d-5e2f-9a7b-1c2d3e4f5a6b")

// GenerationID names the runtime generation of a Session: its Actor UID within
// the Session. A Session keeps one Actor for its whole life, so the ID changes
// only if that Actor is replaced.
func GenerationID(sessionID, actorUID string) string {
	return uuid.NewSHA1(generationNamespace, []byte(sessionID+"\x00"+actorUID)).String()
}

// runtimeAssociation describes the observed Actor when it is the Session's
// recorded runtime, or returns nil when this observation verifies nothing.
func runtimeAssociation(session *apiv1.Session, generation *database.SessionGeneration, actor *ateapipb.Actor) *apiv1.RuntimeAssociation {
	if session.GetState() != apiv1.RuntimeState_RUNTIME_STATE_READY || session.GetOperation() != apiv1.RuntimeOperation_RUNTIME_OPERATION_NONE ||
		generation == nil || generation.ActorUID == "" || actor == nil {
		return nil
	}
	binding := substrate.ActorBinding{Atespace: generation.TemplateAtespace, Name: substrate.ActorName(session.GetId()),
		TemplateAtespace: generation.TemplateAtespace, TemplateName: generation.TemplateName}
	if !binding.Matches(actor) || actor.GetMetadata().GetUid() != generation.ActorUID {
		return nil
	}
	return &apiv1.RuntimeAssociation{
		GenerationId: GenerationID(session.GetId(), generation.ActorUID),
		Atespace:     binding.Atespace, ActorName: binding.Name, ActorUid: generation.ActorUID,
		Phase:         "active",
		CurrentActive: actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_RUNNING,
	}
}

// RuntimeAssociation observes the Session's Actor. Failing to observe it is not
// an error: the association is then absent, which callers read as unknown.
func (w *ActorWorkflow) RuntimeAssociation(ctx context.Context, session *apiv1.Session) *apiv1.RuntimeAssociation {
	if session.GetState() != apiv1.RuntimeState_RUNTIME_STATE_READY || session.GetOperation() != apiv1.RuntimeOperation_RUNTIME_OPERATION_NONE {
		return nil
	}
	generation, err := w.store.GetSessionGeneration(ctx, session.GetId())
	if err != nil || generation.ActorUID == "" {
		return nil
	}
	actor, err := w.actors.GetActor(ctx, generation.TemplateAtespace, substrate.ActorName(session.GetId()))
	if err != nil {
		return nil
	}
	return runtimeAssociation(session, generation, actor)
}

type associationObserver interface {
	RuntimeAssociation(context.Context, *apiv1.Session) *apiv1.RuntimeAssociation
}

// WakeRuntime resumes the Session's recorded Actor when it is not running, as
// Substrate ingress would for an interaction. It keeps the Session READY and
// leaves lifecycle state untouched; a concurrent idle suspend simply requires
// another wake.
func (w *ActorWorkflow) WakeRuntime(ctx context.Context, session *apiv1.Session) error {
	generation, err := w.store.GetSessionGeneration(ctx, session.GetId())
	if err != nil {
		return err
	}
	name := substrate.ActorName(session.GetId())
	actor, err := w.actors.GetActor(ctx, generation.TemplateAtespace, name)
	if err != nil {
		return err
	}
	if runtimeAssociation(session, generation, actor) == nil {
		return fmt.Errorf("runtime Actor is not the Session's recorded runtime")
	}
	if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		return nil
	}
	if err := w.actors.EnsureActorEgressPolicy(ctx, generation.TemplateAtespace, name, session.GetPreparedRevision()); err != nil {
		return err
	}
	_, err = w.actors.ResumeActor(ctx, generation.TemplateAtespace, name)
	return err
}
