package a2a

import (
	"testing"

	apia2a "github.com/kagent-dev/kagent/go/api/a2a"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

func TestToolResultPartCarriesAPermissionDenial(t *testing.T) {
	t.Run("Given a tool call the runtime's classifier refused, when its result is published, assert that the response names the check and its reason", func(t *testing.T) {
		part, err := toolResultPart(runtime.ToolResult{
			ID: "tool-1", Name: "Bash", Result: "Permission for this action was denied.", IsError: true, Status: runtime.ToolDeclined,
			Denial: &runtime.PermissionDenial{By: "classifier", Reason: "Downloads and runs a remote script."},
		})
		if err != nil {
			t.Fatal(err)
		}

		activity, ok := apia2a.ParseToolActivity(part)
		response, _ := activity.Response.(map[string]any)
		denial, _ := response["permissionDenied"].(map[string]any)
		if !ok || response["isError"] != true || denial["by"] != "classifier" || denial["reason"] != "Downloads and runs a remote script." {
			t.Fatalf("activity = %+v, want an error response with permissionDenied by the classifier", activity)
		}
	})

	t.Run("Given a tool call that ran and failed, when its result is published, assert that it carries no denial", func(t *testing.T) {
		part, err := toolResultPart(runtime.ToolResult{ID: "tool-2", Name: "Bash", Result: "exit 1", IsError: true})
		if err != nil {
			t.Fatal(err)
		}

		activity, _ := apia2a.ParseToolActivity(part)
		response, _ := activity.Response.(map[string]any)
		if _, denied := response["permissionDenied"]; denied {
			t.Fatalf("response = %v, want no permissionDenied", response)
		}
	})
}
