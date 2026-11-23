package driver

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/harness/runtime"
)

type recordingSink struct {
	executions []runtime.ToolCall
	executed   []runtime.ToolResult
	text       strings.Builder
	sessions   []runtime.SessionStarted
	calls      []runtime.ToolCall
	results    []runtime.ToolResult
	models     []runtime.ModelCall
}

func (s *recordingSink) SessionStarted(event runtime.SessionStarted) error {
	s.sessions = append(s.sessions, event)
	return nil
}
func (s *recordingSink) TextDelta(event runtime.TextDelta) error {
	s.text.WriteString(event.Text)
	return nil
}
func (s *recordingSink) ToolCall(event runtime.ToolCall) error {
	s.calls = append(s.calls, event)
	return nil
}
func (s *recordingSink) ToolResult(event runtime.ToolResult) error {
	s.results = append(s.results, event)
	return nil
}
func (s *recordingSink) ModelCall(event runtime.ModelCall) {
	s.models = append(s.models, event)
}

func TestTranslatePinnedNotifications(t *testing.T) {
	sink := &recordingSink{}
	messages := []string{
		`{"jsonrpc":"2.0","method":"item/agentMessage/delta","params":{"threadId":"thread","turnId":"turn","itemId":"message","delta":"hello"}}`,
		`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"type":"commandExecution","id":"cmd","command":"pwd","commandActions":[],"cwd":"/data/workspace","status":"inProgress"}}}`,
		`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"commandExecution","id":"cmd","command":"pwd","commandActions":[],"cwd":"/data/workspace","aggregatedOutput":"/data/workspace","exitCode":0,"status":"completed"}}}`,
		`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"type":"mcpToolCall","id":"mcp","server":"tools","tool":"lookup","arguments":{"query":"safe"},"status":"inProgress"}}}`,
		`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"mcpToolCall","id":"mcp","server":"tools","tool":"lookup","arguments":{"query":"safe"},"result":{"content":"ok"},"status":"completed"}}}`,
		`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"completed"}}}`,
	}
	terminal := 0
	translator := newEventTranslator("thread", "turn")
	for _, raw := range messages {
		var message rpcMessage
		if err := json.Unmarshal([]byte(raw), &message); err != nil {
			t.Fatal(err)
		}
		_, done, err := translator.translate(message, sink)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			terminal++
		}
	}
	if sink.text.String() != "hello" || len(sink.calls) != 2 || len(sink.results) != 2 || terminal != 1 {
		t.Fatalf("sink = %#v, text %q, terminal %d", sink, sink.text.String(), terminal)
	}
	result, ok := sink.results[0].Result.(map[string]any)
	if !ok || result["exitCode"] != 0 {
		t.Fatalf("command result = %#v, want integer exitCode 0", sink.results[0].Result)
	}
	if call := sink.calls[1]; call.Name != "tools.lookup" || call.MCPServer != "tools" || call.MCPTool != "lookup" || sink.results[1].Name != "tools.lookup" {
		t.Fatalf("MCP events = %#v, %#v, want tools.lookup", sink.calls[1], sink.results[1])
	}
	if call := sink.calls[0]; call.MCPServer != "" || call.MCPTool != "" {
		t.Fatalf("non-MCP call = %#v, want no MCP server or tool", call)
	}
}

func TestApprovalToolCorrelatesActiveCall(t *testing.T) {
	translator := newEventTranslator("thread", "turn")
	sink := &recordingSink{}
	for _, raw := range []string{
		`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"type":"mcpToolCall","id":"read","server":"protected","tool":"read","arguments":{"path":"source"},"status":"inProgress"}}}`,
		`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"type":"mcpToolCall","id":"delete","server":"protected","tool":"delete","arguments":{"path":"target"},"status":"inProgress"}}}`,
	} {
		var message rpcMessage
		if err := json.Unmarshal([]byte(raw), &message); err != nil {
			t.Fatal(err)
		}
		if _, _, err := translator.translate(message, sink); err != nil {
			t.Fatal(err)
		}
	}

	id, name, err := translator.approvalTool("", "protected", "", map[string]any{"path": "target"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "delete" || name != "protected.delete" {
		t.Fatalf("approval tool = %q/%q, want delete/protected.delete", id, name)
	}
	if _, _, err := translator.approvalTool("thread", "protected", "", map[string]any{"path": "target"}); err == nil {
		t.Fatal("approvalTool() accepted a duplicate approval")
	}
	translator.tools["copy"] = activeTool{name: "protected.copy", server: "protected", thread: "thread", arguments: map[string]any{"path": "source"}}
	if _, _, err := translator.approvalTool("thread", "protected", "", map[string]any{"path": "source"}); err == nil {
		t.Fatal("approvalTool() accepted ambiguous active calls")
	}
}

func TestApprovalToolMatchesTheRequestThread(t *testing.T) {
	translator := newEventTranslator("thread", "turn")
	sink := &recordingSink{}
	for _, raw := range []string{
		`{"method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"type":"collabAgentToolCall","id":"spawn-1","tool":"spawnAgent","receiverThreadIds":[],"status":"inProgress"}}}`,
		`{"method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"collabAgentToolCall","id":"spawn-1","tool":"spawnAgent","receiverThreadIds":["child"],"status":"completed"}}}`,
		`{"method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"type":"mcpToolCall","id":"main-write","server":"protected","tool":"write","arguments":{"value":7},"status":"inProgress"}}}`,
		`{"method":"item/started","params":{"threadId":"child","turnId":"child-turn","item":{"type":"mcpToolCall","id":"child-write","server":"protected","tool":"write","arguments":{"value":7},"status":"inProgress"}}}`,
	} {
		var message rpcMessage
		if err := json.Unmarshal([]byte(raw), &message); err != nil {
			t.Fatal(err)
		}
		if _, _, err := translator.translate(message, sink); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ thread, wantID string }{
		{thread: "child", wantID: "child-write"},
		{thread: "thread", wantID: "main-write"},
	} {
		id, _, err := translator.approvalTool(tc.thread, "protected", "", map[string]any{"value": float64(7)})
		if err != nil || id != tc.wantID {
			t.Fatalf("approval tool on %s = %q, %v, want %q", tc.thread, id, err, tc.wantID)
		}
	}
}

func TestTranslateChildNotifications(t *testing.T) {
	for _, method := range []string{"item/started", "item/completed", "item/agentMessage/delta", "turn/completed"} {
		t.Run(method, func(t *testing.T) {
			translator := newEventTranslator("parent", "turn")
			translator.tools["active"] = activeTool{name: "Agent"}
			sink := &recordingSink{}
			outcome, done, err := translator.translate(rpcMessage{
				Method: method,
				Params: json.RawMessage(`{"threadId":"child","turnId":"child-turn","itemId":"message","delta":"private child answer","item":{"type":"commandExecution","id":"active","command":"pwd","status":"completed"},"turn":{"id":"child-turn","status":"failed"}}`),
			}, sink)
			if err != nil || done || outcome.Failure != nil || outcome.Pending != nil {
				t.Fatalf("child notification: outcome=%#v done=%t error=%v", outcome, done, err)
			}
			if sink.text.Len() != 0 || len(sink.calls) != 0 || len(sink.results) != 0 || len(translator.tools) != 1 {
				t.Fatalf("child notification changed parent state: sink=%#v tools=%#v", sink, translator.tools)
			}
		})
	}
}

func TestTranslateRejectsInvalidParentIdentity(t *testing.T) {
	for _, method := range []string{"item/started", "item/completed", "item/agentMessage/delta", "turn/completed"} {
		for _, test := range []struct {
			name   string
			params string
		}{
			{name: "missing thread", params: `{"turnId":"turn","itemId":"message","turn":{"id":"turn","status":"completed"}}`},
			{name: "wrong turn", params: `{"threadId":"parent","turnId":"wrong","itemId":"message","turn":{"id":"wrong","status":"completed"}}`},
			{name: "missing turn", params: `{"threadId":"parent","itemId":"message","turn":{"status":"completed"}}`},
			{name: "malformed params", params: `{"threadId":123}`},
		} {
			t.Run(method+"/"+test.name, func(t *testing.T) {
				translator := newEventTranslator("parent", "turn")
				_, done, err := translator.translate(rpcMessage{Method: method, Params: json.RawMessage(test.params)}, &recordingSink{})
				if err == nil || done {
					t.Fatalf("invalid parent notification: done=%t error=%v", done, err)
				}
			})
		}
	}
	_, _, err := newEventTranslator("parent", "turn").translate(rpcMessage{
		Method: "item/agentMessage/delta", Params: json.RawMessage(`{"threadId":"parent","turnId":"turn","delta":"missing item ID"}`),
	}, &recordingSink{})
	if err == nil {
		t.Fatal("accepted parent text delta without an item ID")
	}
}

func TestRejectBufferedPostTerminalActivity(t *testing.T) {
	for _, test := range []struct {
		name    string
		method  string
		params  string
		wantErr string
	}{
		{name: "child item", method: "item/started", params: `{"threadId":"child"}`},
		{name: "child delta", method: "item/agentMessage/delta", params: `{"threadId":"child"}`},
		{name: "child turn", method: "turn/started", params: `{"threadId":"child"}`},
		{name: "child terminal", method: "turn/completed", params: `{"threadId":"child"}`},
		{name: "parent terminal", method: "turn/completed", params: `{"threadId":"parent"}`, wantErr: "duplicate terminal event"},
		{name: "parent activity", method: "item/started", params: `{"threadId":"parent"}`, wantErr: "activity after its terminal event"},
		{name: "missing thread", method: "item/started", params: `{}`, wantErr: "activity after its terminal event"},
		{name: "malformed identity", method: "item/started", params: `{"threadId":123}`, wantErr: "decode"},
		{name: "additive notification", method: "account/updated", params: `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			frames := make(chan rpcFrame, 2)
			// Ignoring a child's completion must not hide a later parent violation.
			frames <- rpcFrame{message: rpcMessage{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"child"}`)}}
			frames <- rpcFrame{message: rpcMessage{Method: test.method, Params: json.RawMessage(test.params)}}
			close(frames)
			err := newEventTranslator("parent", "turn").rejectBufferedPostTerminalActivity(frames)
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
		})
	}
	frames := make(chan rpcFrame, 1)
	wantErr := errors.New("broken protocol stream")
	frames <- rpcFrame{err: wantErr}
	if err := newEventTranslator("parent", "turn").rejectBufferedPostTerminalActivity(frames); !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
}

func TestDeclinedCommandNeverRan(t *testing.T) {
	sink := &recordingSink{}
	translator := newEventTranslator("thread", "turn")
	for _, raw := range []string{
		`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"type":"commandExecution","id":"c1","command":"rm -rf x","cwd":"/w","status":"inProgress"}}}`,
		`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"commandExecution","id":"c1","command":"rm -rf x","cwd":"/w","status":"declined"}}}`,
	} {
		var message rpcMessage
		if err := json.Unmarshal([]byte(raw), &message); err != nil {
			t.Fatal(err)
		}
		if _, _, err := translator.translate(message, sink); err != nil {
			t.Fatal(err)
		}
	}
	if len(sink.results) != 1 || sink.results[0].Status != runtime.ToolDeclined || sink.results[0].IsError {
		t.Fatalf("results = %#v, want one declined result", sink.results)
	}
}

func TestTerminalClosesOpenToolsAsUnfinished(t *testing.T) {
	sink := &recordingSink{}
	translator := newEventTranslator("thread", "turn")
	for _, raw := range []string{
		`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"type":"commandExecution","id":"c1","command":"sleep 9","cwd":"/w","status":"inProgress"}}}`,
		`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"interrupted"}}}`,
	} {
		var message rpcMessage
		if err := json.Unmarshal([]byte(raw), &message); err != nil {
			t.Fatal(err)
		}
		if _, _, err := translator.translate(message, sink); err != nil {
			t.Fatal(err)
		}
	}
	if len(sink.results) != 1 || sink.results[0].Status != runtime.ToolUnfinished || !sink.results[0].IsError {
		t.Fatalf("results = %#v, want one unfinished error result", sink.results)
	}
}

// The two items are the shapes Codex 0.161.0 sends for a hosted search: the start has no query
// yet, and the completion carries it with the action.
const (
	webSearchStarted   = `{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"type":"webSearch","id":"ws_1","query":"","action":{"type":"other"},"results":null}}}`
	webSearchCompleted = `{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"webSearch","id":"ws_1","query":"latest PostgreSQL 17 minor release","action":{"type":"search","query":"latest PostgreSQL 17 minor release","queries":null},"results":null}}}`
)

func translateAll(t *testing.T, translator *eventTranslator, sink *recordingSink, messages ...string) {
	t.Helper()
	for _, raw := range messages {
		var message rpcMessage
		if err := json.Unmarshal([]byte(raw), &message); err != nil {
			t.Fatal(err)
		}
		if _, _, err := translator.translate(message, sink); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWebSearchIsOneCallNamingTheQuery(t *testing.T) {
	sink := &recordingSink{}
	translator := newEventTranslator("thread", "turn")
	translator.modelCalls["thread"] = callTime{start: translator.now()}

	// The start publishes nothing: Codex has not said what it is searching for yet.
	translateAll(t, translator, sink, webSearchStarted)
	if len(sink.calls) != 0 || len(sink.results) != 0 {
		t.Fatalf("after the start: calls %#v, results %#v, want none", sink.calls, sink.results)
	}
	// The search runs inside the model's response, so the model call stays open.
	if call := translator.modelCalls["thread"]; !call.end.IsZero() {
		t.Fatalf("model call ended at %v, want it open", call.end)
	}

	translateAll(t, translator, sink, webSearchCompleted)
	if len(sink.calls) != 1 || len(sink.results) != 1 {
		t.Fatalf("calls %#v, results %#v, want one of each", sink.calls, sink.results)
	}
	call, result := sink.calls[0], sink.results[0]
	if call.ID != "ws_1" || call.Name != "WebSearch" || call.Arguments["query"] != "latest PostgreSQL 17 minor release" {
		t.Fatalf("call = %#v, want WebSearch ws_1 with the query", call)
	}
	action, ok := call.Arguments["action"].(map[string]any)
	if !ok || action["type"] != "search" {
		t.Fatalf("action = %#v, want the search action", call.Arguments["action"])
	}
	if call.MCPServer != "" || call.MCPTool != "" {
		t.Fatalf("call = %#v, want no MCP server or tool", call)
	}
	if result.ID != "ws_1" || result.Name != "WebSearch" || result.IsError || result.Status != runtime.ToolRan {
		t.Fatalf("result = %#v, want WebSearch ws_1 ran", result)
	}
	if len(translator.tools) != 0 {
		t.Fatalf("open tools = %#v, want none", translator.tools)
	}
}

func TestWebSearchKeepsItsResults(t *testing.T) {
	sink := &recordingSink{}
	translator := newEventTranslator("thread", "turn")
	translateAll(t, translator, sink, webSearchStarted,
		`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"webSearch","id":"ws_1","query":"q","action":{"type":"search","query":"q","queries":null},"results":[{"type":"text_result","url":"https://example.com/a","title":"A"}]}}}`)
	result, ok := sink.results[0].Result.(map[string]any)
	if !ok {
		t.Fatalf("result = %#v, want a map", sink.results[0].Result)
	}
	results, ok := result["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("results = %#v, want the one result Codex sent", result["results"])
	}
}

func TestWebSearchWithoutAStartIsStillPublished(t *testing.T) {
	sink := &recordingSink{}
	translateAll(t, newEventTranslator("thread", "turn"), sink, webSearchCompleted)
	if len(sink.calls) != 1 || len(sink.results) != 1 || sink.calls[0].Name != "WebSearch" {
		t.Fatalf("calls %#v, results %#v, want one WebSearch call and result", sink.calls, sink.results)
	}
}

func TestUnfinishedWebSearchIsPublishedWhenTheTurnEnds(t *testing.T) {
	sink := &recordingSink{}
	translateAll(t, newEventTranslator("thread", "turn"), sink, webSearchStarted,
		`{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"thread","turn":{"id":"turn","status":"interrupted"}}}`)
	if len(sink.calls) != 1 || sink.calls[0].ID != "ws_1" || sink.calls[0].Name != "WebSearch" {
		t.Fatalf("calls = %#v, want the held WebSearch call", sink.calls)
	}
	if len(sink.results) != 1 || sink.results[0].ID != "ws_1" || sink.results[0].Status != runtime.ToolUnfinished || !sink.results[0].IsError {
		t.Fatalf("results = %#v, want one unfinished error result", sink.results)
	}
}

func TestSubagentWebSearchNamesTheAgentCallThatStartedIt(t *testing.T) {
	sink := &recordingSink{}
	translator := newEventTranslator("parent", "turn")
	translator.spawnedBy["child"] = "agent-1"
	translateAll(t, translator, sink,
		`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"child","turnId":"child-turn","item":{"type":"webSearch","id":"ws_2","query":"","action":null}}}`,
		`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"child","turnId":"child-turn","item":{"type":"webSearch","id":"ws_2","query":"https://example.com/a","action":{"type":"openPage","url":"https://example.com/a"}}}}`)
	if len(sink.calls) != 1 || len(sink.results) != 1 {
		t.Fatalf("calls %#v, results %#v, want one of each", sink.calls, sink.results)
	}
	if call := sink.calls[0]; call.Name != "WebSearch" || call.ParentToolID != "agent-1" || call.Arguments["query"] != "https://example.com/a" {
		t.Fatalf("call = %#v, want WebSearch under agent-1 naming the page", call)
	}
}

func TestAToolReusingAHeldWebSearchIDReplacesIt(t *testing.T) {
	sink := &recordingSink{}
	translateAll(t, newEventTranslator("thread", "turn"), sink, webSearchStarted,
		`{"jsonrpc":"2.0","method":"item/started","params":{"threadId":"thread","turnId":"turn","item":{"type":"commandExecution","id":"ws_1","command":"pwd","cwd":"/w","status":"inProgress"}}}`,
		webSearchCompleted,
		`{"jsonrpc":"2.0","method":"item/completed","params":{"threadId":"thread","turnId":"turn","item":{"type":"commandExecution","id":"ws_1","command":"pwd","cwd":"/w","exitCode":0,"status":"completed"}}}`)
	if len(sink.calls) != 1 || sink.calls[0].Name != "command_execution" || len(sink.results) != 1 || sink.results[0].Name != "command_execution" {
		t.Fatalf("calls %#v, results %#v, want only the command", sink.calls, sink.results)
	}
}

func TestUsageReadsTheTokenBreakdown(t *testing.T) {
	sink := &recordingSink{}
	translator := newEventTranslator("thread", "turn")
	raw := `{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"thread","turnId":"turn","tokenUsage":{"total":{"inputTokens":10,"cachedInputTokens":4,"cacheWriteInputTokens":3,"outputTokens":2,"reasoningOutputTokens":1},"last":{"inputTokens":10,"cachedInputTokens":4,"cacheWriteInputTokens":3,"outputTokens":2,"reasoningOutputTokens":1}}}}`
	var message rpcMessage
	if err := json.Unmarshal([]byte(raw), &message); err != nil {
		t.Fatal(err)
	}
	if _, _, err := translator.translate(message, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.models) != 1 || sink.models[0].CacheWriteTokens != 3 || sink.models[0].CacheReadTokens != 4 || sink.models[0].ReasoningOutputTokens != 1 {
		t.Fatalf("model calls = %+v, want cache write 3, cache read 4 and reasoning 1", sink.models)
	}
}

func (s *recordingSink) ToolExecutionStarted(call runtime.ToolCall) {
	s.executions = append(s.executions, call)
}
func (s *recordingSink) ToolExecutionFinished(result runtime.ToolResult) {
	s.executed = append(s.executed, result)
}
