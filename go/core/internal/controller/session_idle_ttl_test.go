package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kagent-dev/kagent/go/core/internal/database"
)

func TestAgentReconciliationSessionIdleTTL(t *testing.T) {
	scenarios := []struct {
		name     string
		override *metav1.Duration
		want     *int64
	}{
		{name: "Given a Harness without an override, when the Agent reconciles, assert that the controller default applies"},
		{name: "Given a zero override, when the Agent reconciles, assert that expiration is disabled",
			override: &metav1.Duration{}, want: new(int64(0))},
		{name: "Given a fractional override, when the Agent reconciles, assert that it rounds up to whole seconds",
			override: &metav1.Duration{Duration: 1500 * time.Millisecond}, want: new(int64(2))},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			collections, harnesses := newPreparationTestCollections(t, "gvisor")
			before := collections.Reconciliations.List()[0].Target.RevisionID

			harness := harnesses.List()[0].DeepCopy()
			harness.Spec.SessionIdleTTL = scenario.override
			harnesses.UpdateObject(harness)

			want := &database.SessionIdleTTLPolicy{Seconds: scenario.want}
			waitFor(t, func() bool {
				states := collections.Reconciliations.List()
				return len(states) == 1 && states[0].Target != nil && equalIdleTTL(states[0].SessionIdleTTL, want)
			})
			require.Equal(t, before, collections.Reconciliations.List()[0].Target.RevisionID,
				"idle TTL is mutable lifecycle policy, not revision identity")
		})
	}

	t.Run("Given a resolved override, when the Harness is deleted, assert that the policy becomes unresolved", func(t *testing.T) {
		collections, harnesses := newPreparationTestCollections(t, "gvisor")
		harness := harnesses.List()[0].DeepCopy()
		harness.Spec.SessionIdleTTL = &metav1.Duration{}
		harnesses.UpdateObject(harness)
		waitFor(t, func() bool { return collections.Reconciliations.List()[0].SessionIdleTTL.Seconds != nil })

		harnesses.DeleteObject(harness.Namespace + "/" + harness.Name)

		waitFor(t, func() bool {
			states := collections.Reconciliations.List()
			return len(states) == 1 && states[0].SessionIdleTTL == nil
		})
	})
}

func equalIdleTTL(got, want *database.SessionIdleTTLPolicy) bool {
	if got == nil || want == nil {
		return got == want
	}
	if got.Seconds == nil || want.Seconds == nil {
		return got.Seconds == want.Seconds
	}
	return *got.Seconds == *want.Seconds
}
