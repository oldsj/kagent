package session

import (
	"context"
	"testing"

	"github.com/google/uuid"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"

	apiv1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
)

func TestRuntimeAssociation(t *testing.T) {
	const sessionID = "0193c6a4-7a8e-7d4f-9b2c-1e5f6a7b8c9d"
	readySession := func() *apiv1.Session {
		return &apiv1.Session{Id: sessionID, State: apiv1.RuntimeState_RUNTIME_STATE_READY, Operation: apiv1.RuntimeOperation_RUNTIME_OPERATION_NONE}
	}
	generation := func() *database.SessionGeneration {
		return &database.SessionGeneration{SessionID: sessionID, ActorUID: "actor-uid", TemplateAtespace: "kagent", TemplateName: "revision-template"}
	}
	actor := func(state ateapipb.ActorState) *ateapipb.Actor {
		return &ateapipb.Actor{
			Metadata:      &ateapipb.ResourceMetadata{Atespace: "kagent", Name: substrate.ActorName(sessionID), Uid: "actor-uid"},
			ActorTemplate: &ateapipb.ObjectRef{Atespace: "kagent", Name: "revision-template"},
			Status:        &ateapipb.ActorStatus{State: state},
		}
	}

	t.Run("Given a READY Session whose recorded Actor is running, when it is observed, assert that the association is active and current", func(t *testing.T) {
		association := runtimeAssociation(readySession(), generation(), actor(ateapipb.ActorState_ACTOR_STATE_RUNNING))

		if association == nil || !association.GetCurrentActive() || association.GetPhase() != "active" ||
			association.GetActorUid() != "actor-uid" || association.GetAtespace() != "kagent" || association.GetActorName() != substrate.ActorName(sessionID) {
			t.Fatalf("association = %v, want an active running association", association)
		}
		if _, err := uuid.Parse(association.GetGenerationId()); err != nil || association.GetGenerationId() != GenerationID(sessionID, "actor-uid") {
			t.Fatalf("generation ID = %q, want the stable UUID for the Session's Actor", association.GetGenerationId())
		}
	})

	t.Run("Given a READY Session whose recorded Actor is suspended, when it is observed, assert that the association reports it asleep", func(t *testing.T) {
		association := runtimeAssociation(readySession(), generation(), actor(ateapipb.ActorState_ACTOR_STATE_SUSPENDED))

		if association == nil || association.GetCurrentActive() || association.GetPhase() != "active" {
			t.Fatalf("association = %v, want present with current_active=false", association)
		}
	})

	t.Run("Given two Actor UIDs for one Session, when their generation IDs are derived, assert that they differ and are stable", func(t *testing.T) {
		first, again, replaced := GenerationID(sessionID, "actor-uid"), GenerationID(sessionID, "actor-uid"), GenerationID(sessionID, "other-uid")

		if first != again || first == replaced {
			t.Fatalf("generation IDs = %s, %s, %s; want stable per Actor and distinct across Actors", first, again, replaced)
		}
	})

	unverified := []struct {
		name       string
		session    func() *apiv1.Session
		generation func() *database.SessionGeneration
		actor      func() *ateapipb.Actor
	}{
		{"a Session in a lifecycle operation", func() *apiv1.Session {
			s := readySession()
			s.Operation = apiv1.RuntimeOperation_RUNTIME_OPERATION_REVERT
			return s
		}, generation, func() *ateapipb.Actor { return actor(ateapipb.ActorState_ACTOR_STATE_RUNNING) }},
		{"a Session that is not READY", func() *apiv1.Session {
			s := readySession()
			s.State = apiv1.RuntimeState_RUNTIME_STATE_CREATING
			return s
		}, generation, func() *ateapipb.Actor { return actor(ateapipb.ActorState_ACTOR_STATE_RUNNING) }},
		{"no recorded Actor", readySession, func() *database.SessionGeneration {
			g := generation()
			g.ActorUID = ""
			return g
		}, func() *ateapipb.Actor { return actor(ateapipb.ActorState_ACTOR_STATE_RUNNING) }},
		{"a replaced Actor", readySession, generation, func() *ateapipb.Actor {
			a := actor(ateapipb.ActorState_ACTOR_STATE_RUNNING)
			a.Metadata.Uid = "other-uid"
			return a
		}},
		{"an Actor from another template", readySession, generation, func() *ateapipb.Actor {
			a := actor(ateapipb.ActorState_ACTOR_STATE_RUNNING)
			a.ActorTemplate.Name = "other-template"
			return a
		}},
		{"no observed Actor", readySession, generation, func() *ateapipb.Actor { return nil }},
	}
	for _, scenario := range unverified {
		t.Run("Given "+scenario.name+", when it is observed, assert that no association is reported", func(t *testing.T) {
			association := runtimeAssociation(scenario.session(), scenario.generation(), scenario.actor())

			if association != nil {
				t.Fatalf("association = %v, want none", association)
			}
		})
	}
}

type observingWorkflow struct {
	serviceTestWorkflow
	association *apiv1.RuntimeAssociation
}

func (w observingWorkflow) RuntimeAssociation(context.Context, *apiv1.Session) *apiv1.RuntimeAssociation {
	return w.association
}

func TestServiceGetReportsTheRuntimeAssociation(t *testing.T) {
	t.Run("Given a workflow that observes the Session's Actor, when the Session is read, assert that the association is reported", func(t *testing.T) {
		observed := &apiv1.RuntimeAssociation{GenerationId: "generation", ActorUid: "actor-uid", Phase: "active", CurrentActive: true}
		store := &serviceTestStore{getResult: &apiv1.Session{Id: "0193c6a4-7a8e-7d4f-9b2c-1e5f6a7b8c9d", Creator: "alice", State: apiv1.RuntimeState_RUNTIME_STATE_READY}}
		service := NewService(store, serviceTestAuthorizer{}, observingWorkflow{association: observed})

		session, err := service.Get(serviceTestContext("alice"), "0193c6a4-7a8e-7d4f-9b2c-1e5f6a7b8c9d")

		if err != nil || session.GetRuntimeAssociation() != observed {
			t.Fatalf("Get() = %v, %v; want the observed association", session.GetRuntimeAssociation(), err)
		}
	})
}
