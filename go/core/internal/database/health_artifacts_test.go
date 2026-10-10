package database

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aevent"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The same sanitized v1 fixture is consumed by Mainloop's PostgreSQL/restart
// acceptance. This proves gateway storage and GetTask recovery retain its data.
func TestHealthArtifactsRoundTrip(t *testing.T) {
	client, ctx := NewClient(setupTestDB(t)), t.Context()
	sessionFixture(t, client, ctx, "team-a", "revision", "assistant", "kagent")
	session, _, err := client.CreateSession(ctx, newSessionRequest(uuid.NewString(), "assistant", "kagent", "Health"), uuid.NewString())
	require.NoError(t, err)
	_, err = markSessionReady(ctx, client, session.Id, "fixture.example")
	require.NoError(t, err)
	data, err := os.ReadFile("testdata/health-tasks.json")
	require.NoError(t, err)
	var fixtures []struct {
		ID        string `json:"id"`
		Artifacts []struct {
			ID    string `json:"artifactId"`
			Name  string `json:"name"`
			Parts []struct {
				Data map[string]any `json:"data"`
			} `json:"parts"`
		} `json:"artifacts"`
	}
	require.NoError(t, json.Unmarshal(data, &fixtures))
	for _, fixture := range fixtures {
		task := newSessionTask(fixture.ID, "message-"+fixture.ID)
		task.ContextID = session.ContextId
		_, err = client.CreateRuntimeTask(ctx, session.Id, taskMutationHash("hash-"+fixture.ID), task, "")
		require.NoError(t, err)
		for _, artifact := range fixture.Artifacts {
			artifact.Parts[0].Data["runtime_session_id"] = session.ContextId
			event := &a2a.TaskArtifactUpdateEvent{TaskID: task.ID, ContextID: task.ContextID, LastChunk: true, Artifact: &a2a.Artifact{ID: a2a.ArtifactID(artifact.ID), Name: artifact.Name, Parts: a2a.ContentParts{a2a.NewDataPart(artifact.Parts[0].Data)}}}
			task, err = a2aevent.ApplyUpdate(task, event)
			require.NoError(t, err)
			require.NoError(t, saveRuntimeTask(t, client, session.Id, task, event, nil))
		}
		// Replaying the artifact replaces the same stable ID, including in GetTask.
		replay := &a2a.TaskArtifactUpdateEvent{TaskID: task.ID, ContextID: task.ContextID, Artifact: task.Artifacts[0]}
		task, err = a2aevent.ApplyUpdate(task, replay)
		require.NoError(t, err)
		require.NoError(t, saveRuntimeTask(t, client, session.Id, task, replay, nil))
		terminal := &a2a.TaskStatusUpdateEvent{TaskID: task.ID, ContextID: task.ContextID, Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}
		task, err = a2aevent.ApplyUpdate(task, terminal)
		require.NoError(t, err)
		require.NoError(t, saveRuntimeTask(t, client, session.Id, task, terminal, &SessionTaskSnapshot{Atespace: "team-a", URI: "fixture-health-" + fixture.ID, ContentScope: "DATA"}))
		recovered, err := client.GetSessionTask(ctx, session.Id, fixture.ID, nil)
		require.NoError(t, err)
		require.Len(t, recovered.Artifacts, len(fixture.Artifacts))
		for index, artifact := range recovered.Artifacts {
			encoded, err := json.Marshal(artifact.Parts[0].Data())
			require.NoError(t, err)
			want, err := json.Marshal(fixture.Artifacts[index].Parts[0].Data)
			require.NoError(t, err)
			require.JSONEq(t, string(want), string(encoded))
			require.Equal(t, fixture.Artifacts[index].ID, string(artifact.ID))
		}
	}
}
