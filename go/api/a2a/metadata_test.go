package a2a

import (
	"testing"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
)

func TestSanitizeCallerRequest(t *testing.T) {
	part := NewToolResultPart("call-1", "lookup", map[string]any{"result": "ok"})
	part.SetMeta(outputSchemaSHA256MetadataKey, "caller-controlled")
	message := a2atype.NewMessage(a2atype.MessageRoleUser, part)
	message.Metadata = map[string]any{
		HITLExtensionURI:            map[string]any{"type": HITLTypeToolApprovalResponse},
		TimelinePositionMetadataKey: "caller-controlled",
		"application.example/value": "preserved",
	}
	req := &a2atype.SendMessageRequest{Message: message, Metadata: map[string]any{
		TaskCreatedAtMetadataKey: "caller-controlled",
		"application.example/id": "preserved",
	}}

	SanitizeCallerRequest(req)

	if _, ok := message.Metadata[HITLExtensionURI]; !ok {
		t.Fatal("HITL metadata was removed")
	}
	if _, ok := message.Metadata[TimelinePositionMetadataKey]; ok {
		t.Fatalf("gateway-owned metadata remains: %#v", message.Metadata)
	}
	if req.Metadata["application.example/id"] != "preserved" {
		t.Fatalf("application metadata was removed: %#v", req.Metadata)
	}
	if part.Metadata[PartTypeMetadataKey] != string(ToolResultKind) {
		t.Fatalf("public part type was removed: %#v", part.Metadata)
	}
	if _, ok := part.Metadata[outputSchemaSHA256MetadataKey]; !ok {
		t.Fatalf("non-gateway metadata was removed: %#v", part.Metadata)
	}
}

func TestSanitizeCallerRequestPreservesTurnOptions(t *testing.T) {
	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("hi"))
	message.Metadata = map[string]any{
		ModelMetadataKey:  "claude-opus-5",
		EffortMetadataKey: "high",
	}
	req := &a2atype.SendMessageRequest{Message: message}

	SanitizeCallerRequest(req)

	if model, ok := TurnModel(message); !ok || model != "claude-opus-5" {
		t.Fatalf("turn model override was stripped: %v, %v", model, ok)
	}
	if effort, ok := TurnEffort(message); !ok || effort != "high" {
		t.Fatalf("turn effort override was stripped: %v, %v", effort, ok)
	}
}

func TestTurnModelAndEffort(t *testing.T) {
	message := a2atype.NewMessage(a2atype.MessageRoleUser, a2atype.NewTextPart("hi"))
	if _, ok := TurnModel(message); ok {
		t.Fatal("expected no turn model override on a message without metadata")
	}
	if _, ok := TurnEffort(message); ok {
		t.Fatal("expected no turn effort override on a message without metadata")
	}

	message.Metadata = map[string]any{
		ModelMetadataKey:  "claude-opus-5",
		EffortMetadataKey: "high",
		// A non-string value must not be misread as a string override.
		"application.example/value": 1,
	}
	if model, ok := TurnModel(message); !ok || model != "claude-opus-5" {
		t.Fatalf("turn model = %v, %v", model, ok)
	}
	if effort, ok := TurnEffort(message); !ok || effort != "high" {
		t.Fatalf("turn effort = %v, %v", effort, ok)
	}
	if _, ok := TurnModel(nil); ok {
		t.Fatal("expected no turn model override for a nil message")
	}
}

func TestTimelineAndTaskCreatedAt(t *testing.T) {
	position := time.Date(2026, 9, 22, 12, 0, 0, 123, time.FixedZone("test", -4*60*60))
	message := a2atype.NewMessage(a2atype.MessageRoleAgent)
	SetTimelinePosition(message, position)
	gotPosition, ok := TimelinePosition(message)
	if !ok || !gotPosition.Equal(position) {
		t.Fatalf("timeline position = %v, %v", gotPosition, ok)
	}

	task := &a2atype.Task{}
	SetTaskCreatedAt(task, position)
	gotCreatedAt, ok := TaskCreatedAt(task)
	if !ok || !gotCreatedAt.Equal(position) {
		t.Fatalf("task creation time = %v, %v", gotCreatedAt, ok)
	}
}
