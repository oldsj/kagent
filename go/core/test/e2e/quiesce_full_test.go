package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/kagent-dev/mockllm"
	ateapipb "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/metadata"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/internal/substrate"
)

// quiesceFullAgent is fixed so an installation with a session policy can list it.
const quiesceFullAgent = "quiesce-full-e2e"

// TestE2EClaudeFullQuiesceResumesFromMemory runs a Claude Harness with
// snapshotPolicy.onQuiesce Full. The idle Actor must suspend with a MEMORY
// snapshot, and the next turn must complete on the runtime resumed from it.
// Processes started by Claude's Bash tool are not expected to survive: the
// harness ends its native process tree at the end of every turn.
func TestE2EClaudeFullQuiesceResumesFromMemory(t *testing.T) {
	t.Parallel()
	target := interactionTarget(t)
	server := mockllm.NewServer(quiesceFullMock(t))
	baseURL, err := server.Start(t.Context())
	if err != nil {
		t.Fatalf("start mock LLM: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Stop(context.Background()); err != nil {
			t.Errorf("stop mock LLM: %v", err)
		}
	})
	proxy := mockLLMProxy(t, baseURL)
	recorder := startModelRecorder(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// mockllm matches only the last message; drop Claude's trailing
		// system-role context message as claude_turn_test does.
		if body, err := io.ReadAll(r.Body); err == nil {
			body = withoutTrailingSystemMessages(body)
			r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
			r.Header.Del("Content-Length")
		}
		proxy.ServeHTTP(w, r)
	}), nil)
	kube := interactionKubeClient(t)
	harness := createFullQuiesceHarness(t, kube)
	model := createClaudeMockModelNamed(t, kube, claudeTurnConfiguredModel, strings.TrimSuffix(reachableModelURL(t, recorder.URL), "/v1"))
	template := &v1alpha3.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: quiesceFullAgent, Namespace: "kagent", Labels: testHarness{name: harness.Name, runtimeLabel: "claude"}.labels()},
		Spec: v1alpha3.AgentTemplateSpec{
			ModelConfig:  &v1alpha3.ObjectReference{Name: model.Name},
			Description:  "Full quiesce E2E fixture",
			SystemPrompt: "Reply briefly.",
		},
	}
	createAndWaitInteractionTemplateForHarness(t, kube, template, harness.Name)
	fixture := newInteractionFixtureForHarnessTemplate(t, target, harness.Name, template.Name)

	for i, marker := range []string{"first", "second"} {
		task := sendClaudeTurn(t, fixture, "kagent-quiesce-proof "+marker, "", "")
		if task.Status.State != a2atype.TaskStateCompleted || !strings.Contains(taskText(task), "QUIESCE_DONE "+marker) {
			t.Fatalf("%s task state = %s, text = %q", marker, task.Status.State, taskText(task))
		}
		if i == 0 {
			waitActorSuspendedAt(t, fixture, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY)
		}
	}
}

// createFullQuiesceHarness clones the suite's Claude Harness with onQuiesce Full.
func createFullQuiesceHarness(t *testing.T, kube ctrlclient.Client) *v1alpha3.Harness {
	t.Helper()
	base := &v1alpha3.Harness{}
	if err := kube.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "kagent", Name: claudeE2EHarness}, base); err != nil {
		t.Fatalf("get Claude Harness: %v", err)
	}
	harness := &v1alpha3.Harness{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "quiesce-full-", Namespace: "kagent"},
		Spec:       *base.Spec.DeepCopy(),
	}
	harness.Spec.Substrate.SnapshotPolicy.OnQuiesce = v1alpha3.RuntimeSnapshotScopeFull
	if err := kube.Create(t.Context(), harness); err != nil {
		t.Fatalf("create Full quiesce Harness: %v", err)
	}
	t.Cleanup(func() {
		if err := kube.Delete(context.Background(), harness); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete Full quiesce Harness: %v", err)
		}
	})
	return harness
}

// waitActorSuspendedAt waits for the Session's Actor to suspend with a durable
// snapshot of the given fidelity.
func waitActorSuspendedAt(t *testing.T, fixture *interactionFixture, fidelity ateapipb.SnapshotFidelity) {
	t.Helper()
	actorID := substrate.ActorName(fixture.sessionID)
	ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(t.Context(), "x-user-id", "e2e"), time.Minute)
	defer cancel()
	var observed ateapipb.SnapshotFidelity
	err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		actor, err := findSubstrateActor(ctx, fixture.system, "", actorID)
		if err != nil {
			return false, err
		}
		if actor == nil || actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
			return false, nil
		}
		snapshot := substrate.LatestActorSnapshot(actor)
		observed = snapshot.GetFidelity()
		return substrate.SnapshotURI(snapshot) != "" && observed == fidelity, nil
	})
	if err != nil {
		t.Fatalf("Actor %s did not suspend with a durable %s snapshot (last %s): %v", actorID, fidelity, observed, err)
	}
}

func quiesceFullMock(t *testing.T) mockllm.Config {
	t.Helper()
	message := func(id, text string) map[string]any {
		return map[string]any{"id": id, "type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": text}},
			"model":   claudeTurnConfiguredModel, "stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 10, "output_tokens": 4}}
	}
	match := func(text string) map[string]any {
		return map[string]any{"match_type": "contains", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}}
	}
	mocks := []map[string]any{}
	for _, marker := range []string{"first", "second"} {
		mocks = append(mocks, map[string]any{"name": marker, "match": match("kagent-quiesce-proof " + marker), "response": message("msg_quiesce_"+marker, "QUIESCE_DONE "+marker)})
	}
	// Claude's auxiliary requests, such as title generation, match last.
	mocks = append(mocks, map[string]any{"name": "auxiliary", "match": match(""), "response": message("msg_auxiliary", "ok")})
	raw, err := json.Marshal(map[string]any{"anthropic": mocks})
	if err != nil {
		t.Fatal(err)
	}
	var cfg mockllm.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode Full quiesce mock: %v", err)
	}
	return cfg
}
