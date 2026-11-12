package sdkrunner_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/sdkrunner/sdkrunnertest"
)

const wait = 5 * time.Second

type fixture struct {
	session      *sdkrunner.Session
	launcher     *sdkrunnertest.Launcher
	continuation *sdkrunnertest.Continuation
	activity     *sdkrunnertest.Activity
	dir          string
	profiles     []sdkrunner.Profile
	prepared     int
}

func newFixture(t *testing.T, handle func(*sdkrunnertest.Bridge, sdkrunnertest.Op)) *fixture {
	t.Helper()
	f := &fixture{
		launcher: sdkrunnertest.NewLauncher(handle), continuation: &sdkrunnertest.Continuation{},
		activity: &sdkrunnertest.Activity{}, dir: t.TempDir(),
	}
	session, err := sdkrunner.New(sdkrunner.Config{
		Launcher: f.launcher,
		Spec: func(p sdkrunner.Profile) (sdkrunner.Spec, error) {
			f.profiles = append(f.profiles, p)
			return sdkrunner.Spec{Cwd: "/data/workspace", Executable: "/usr/local/bin/claude", Model: "opus"}, nil
		},
		Prepare:      func(context.Context) error { f.prepared++; return nil },
		Continuation: f.continuation, Activity: f.activity, Dir: f.dir, StopGrace: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	f.session = session
	return f
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func next(t *testing.T, sub *sdkrunner.Subscription) sdkrunner.Item {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	item, err := sub.Next(ctx)
	if err != nil {
		t.Fatalf("next item: %v", err)
	}
	return item
}

func TestSessionStartsLazilyAndReportsActivityOnlyOnChange(t *testing.T) {
	f := newFixture(t, sdkrunnertest.Echo("11111111-1111-1111-1111-111111111111"))
	// Construction (boot, golden snapshot) launches nothing and reports nothing.
	if f.launcher.Launches() != 0 || len(f.activity.Got()) != 0 {
		t.Fatalf("launches=%d reports=%v before demand", f.launcher.Launches(), f.activity.Got())
	}
	sub, err := f.session.BeginTurn()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.session.Submit(context.Background(), "hi", "u1", sdkrunner.OriginA2A); err != nil {
		t.Fatal(err)
	}
	var sawResult bool
	for !sawResult {
		item := next(t, sub)
		if isResult, uuids := sdkrunner.ResultUUIDs(item.Raw); isResult {
			sawResult = len(uuids) == 1 && uuids[0] == "u1"
		}
	}
	f.session.EndTurn(sub)
	eventually(t, "idle report", func() bool { got := f.activity.Got(); return len(got) == 2 && !got[1] })
	if got := f.activity.Got(); !got[0] {
		t.Fatalf("reports = %v, want [true false]", got)
	}
	if f.continuation.ID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("continuation = %q, want the SDK session bound", f.continuation.ID)
	}
	if f.launcher.Launches() != 1 || f.prepared != 1 || f.profiles[0] != sdkrunner.ProfileManaged {
		t.Fatalf("launches=%d prepared=%d profiles=%v", f.launcher.Launches(), f.prepared, f.profiles)
	}
}

func TestSessionRestartsAfterBridgeExitAndResumesBoundConversation(t *testing.T) {
	f := newFixture(t, sdkrunnertest.Echo("22222222-2222-2222-2222-222222222222"))
	sub, _ := f.session.BeginTurn()
	if err := f.session.Submit(context.Background(), "one", "u1", sdkrunner.OriginA2A); err != nil {
		t.Fatal(err)
	}
	first := f.launcher.Started(wait)
	if first == nil || first.Spec.Resume != "" {
		t.Fatalf("first start = %+v, want a fresh conversation", first)
	}
	for item := next(t, sub); item.Kind != sdkrunner.ItemSDK || !strings.Contains(string(item.Raw), `"result"`); item = next(t, sub) {
	}
	// A restored bridge that segfaults: the turn learns of it, the next
	// demand starts a new bridge resuming the same conversation.
	first.Exit()
	for item := next(t, sub); item.Kind != sdkrunner.ItemExited; item = next(t, sub) {
	}
	f.session.EndTurn(sub)
	if err := f.session.Submit(context.Background(), "two", "u2", sdkrunner.OriginClient); err != nil {
		t.Fatal(err)
	}
	second := f.launcher.Started(wait)
	if second == nil || second.Spec.Resume != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("second start = %+v, want resume of the bound conversation", second)
	}
}

func TestPermissionFirstAnswerWins(t *testing.T) {
	f := newFixture(t, func(b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
		if op.Op == "user" {
			b.Emit(map[string]any{"k": "perm", "id": "p1", "toolName": "Bash", "input": map[string]any{"command": "touch x"}, "toolUseID": "t1"})
		}
	})
	sub, _ := f.session.BeginTurn()
	if err := f.session.Submit(context.Background(), "do it", "u1", sdkrunner.OriginA2A); err != nil {
		t.Fatal(err)
	}
	item := next(t, sub)
	if item.Kind != sdkrunner.ItemPerm || item.Perm.ToolName != "Bash" {
		t.Fatalf("item = %+v, want the permission request", item)
	}
	if ok, err := f.session.AnswerPerm("p1", "allow", false, "", sdkrunner.OriginClient); !ok || err != nil {
		t.Fatalf("client answer = %v %v", ok, err)
	}
	if ok, _ := f.session.AnswerPerm("p1", "deny", false, "", sdkrunner.OriginA2A); ok {
		t.Fatal("a second answer must not resolve the request again")
	}
	if item := next(t, sub); item.Kind != sdkrunner.ItemPermDone || item.PermID != "p1" {
		t.Fatalf("item = %+v, want the client's resolution", item)
	}
	eventually(t, "perm op", func() bool {
		for _, op := range f.launcher.Ops() {
			if op.Op == "perm" && op.ID == "p1" && op.Behavior == "allow" {
				return true
			}
		}
		return false
	})
}

func TestReplayResumesTheConversationOnAFreshBridge(t *testing.T) {
	t.Run("Given a running bridge, when a replay begins, assert that the turn is registered on a new bridge that resumes the conversation and sees none of the old one's events", func(t *testing.T) {
		f := newFixture(t, sdkrunnertest.Echo("22222222-2222-2222-2222-222222222222"))
		if err := f.session.Submit(context.Background(), "one", "u1", sdkrunner.OriginClient); err != nil {
			t.Fatal(err)
		}
		first := f.launcher.Started(wait)
		eventually(t, "the conversation to be bound", func() bool {
			_, bound, _ := f.continuation.Load()
			return bound
		})

		sub, err := f.session.BeginReplay(context.Background())

		if err != nil {
			t.Fatalf("BeginReplay() error = %v", err)
		}
		second := f.launcher.Started(wait)
		if second == nil || second == first || second.Spec.Resume != "22222222-2222-2222-2222-222222222222" {
			t.Fatalf("replay bridge = %+v, want a new bridge resuming the bound conversation", second)
		}
		if f.launcher.Launches() != 2 || !f.session.TurnActive() {
			t.Fatalf("launches = %d, turn active = %v; want 2 and an active turn", f.launcher.Launches(), f.session.TurnActive())
		}
		for _, op := range f.launcher.Ops() {
			if op.Op == "user" && op.UUID != "u1" {
				t.Fatalf("a replay sent user message %q", op.UUID)
			}
		}
		// The new bridge's events reach the turn; the old bridge's exit did not.
		second.SDK(map[string]any{"type": "result", "subtype": "success", "result": "replayed"})
		if item := next(t, sub); item.Kind != sdkrunner.ItemSDK || !strings.Contains(string(item.Raw), "replayed") {
			t.Fatalf("first item = %+v, want the replay's own result", item)
		}
		if _, err := f.session.BeginReplay(context.Background()); err == nil {
			t.Fatal("a second turn must not replace the active one")
		}
	})
}

func TestClientMessagesWaitForAPendingAnswer(t *testing.T) {
	refused := errors.New("the conversation is waiting for an answer")
	for _, scenario := range []struct {
		name    string
		gate    error
		wantErr error
		sent    int
	}{
		{name: "Given a deferred call waiting for its answer, when a client sends a message, assert that it is refused and never reaches Claude", gate: refused, wantErr: refused},
		{name: "Given nothing waiting, when a client sends a message, assert that it reaches Claude", sent: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			launcher := sdkrunnertest.NewLauncher(sdkrunnertest.Echo("22222222-2222-2222-2222-222222222222"))
			session, err := sdkrunner.New(sdkrunner.Config{
				Launcher: launcher,
				Spec: func(sdkrunner.Profile) (sdkrunner.Spec, error) {
					return sdkrunner.Spec{Cwd: "/data/workspace", Executable: "/usr/local/bin/claude"}, nil
				},
				Continuation: &sdkrunnertest.Continuation{}, Dir: t.TempDir(), StopGrace: time.Second,
				ClientGate: func() error { return scenario.gate },
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })

			err = session.SubmitFromClient(context.Background(), "hello", "u1")

			if !errors.Is(err, scenario.wantErr) {
				t.Fatalf("SubmitFromClient() error = %v, want %v", err, scenario.wantErr)
			}
			if scenario.sent == 1 {
				eventually(t, "the message to reach the bridge", func() bool { return len(launcher.Ops()) > 1 })
			}
			users := 0
			for _, op := range launcher.Ops() {
				if op.Op == "user" {
					users++
				}
			}
			if users != scenario.sent {
				t.Fatalf("user messages sent to Claude = %d, want %d", users, scenario.sent)
			}
		})
	}
}

func TestPermissionAnswerCanReplaceTheToolInput(t *testing.T) {
	f := newFixture(t, func(b *sdkrunnertest.Bridge, op sdkrunnertest.Op) {
		if op.Op == "user" {
			b.Emit(map[string]any{"k": "perm", "id": "p1", "toolName": "AskUserQuestion", "input": map[string]any{"questions": []any{}}, "toolUseID": "t1"})
		}
	})
	sub, _ := f.session.BeginTurn()
	if err := f.session.Submit(context.Background(), "ask", "u1", sdkrunner.OriginA2A); err != nil {
		t.Fatal(err)
	}
	if item := next(t, sub); item.Kind != sdkrunner.ItemPerm {
		t.Fatalf("item = %+v, want the permission request", item)
	}

	ok, err := f.session.AllowPermWith("p1", map[string]any{"answers": map[string]any{"Which?": "A"}}, sdkrunner.OriginA2A)

	if !ok || err != nil {
		t.Fatalf("AllowPermWith() = %v, %v", ok, err)
	}
	eventually(t, "the answer to reach the bridge", func() bool {
		for _, op := range f.launcher.Ops() {
			if op.Op == "perm" && op.ID == "p1" {
				answers, _ := op.UpdatedInput["answers"].(map[string]any)
				return op.Behavior == "allow" && answers["Which?"] == "A"
			}
		}
		return false
	})
}

func bundle(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestServerBootstrapIsTrustOnFirstUse(t *testing.T) {
	f := newFixture(t, nil)
	srv := httptest.NewServer(sdkrunner.NewServer(context.Background(), f.session))
	defer srv.Close()
	put := func(token string, body []byte) int {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/bootstrap", bytes.NewReader(body))
		req.Header.Set("x-runner-token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := put("short", nil); code != http.StatusConflict {
		t.Fatalf("short token = %d, want 409", code)
	}
	if code := put(strings.Repeat("a", 24), bundle(t, map[string]string{"../escape": "x"})); code != http.StatusBadRequest {
		t.Fatalf("escaping bundle = %d, want 400", code)
	}
	token := strings.Repeat("b", 32)
	if code := put(token, bundle(t, map[string]string{".claude/settings.json": "{}"})); code != http.StatusOK {
		t.Fatalf("bootstrap = %d, want 200", code)
	}
	if code := put(strings.Repeat("c", 32), nil); code != http.StatusConflict {
		t.Fatalf("second bootstrap = %d, want 409", code)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "home", ".claude", "settings.json")); err != nil {
		t.Fatalf("home not installed: %v", err)
	}
	if info, err := os.Stat(filepath.Join(f.dir, "token")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file = %v %v, want 0600", info, err)
	}
	// A restarted harness keeps the token.
	again := httptest.NewServer(sdkrunner.NewServer(context.Background(), f.session))
	defer again.Close()
	resp, err := http.Get(again.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	if body.String() != "ready\n" {
		t.Fatalf("healthz = %q, want ready", body.String())
	}
}

func TestServerWebSocketHelloCommandsAndAuth(t *testing.T) {
	f := newFixture(t, sdkrunnertest.Echo("33333333-3333-3333-3333-333333333333"))
	server := sdkrunner.NewServer(context.Background(), f.session)
	srv := httptest.NewServer(server)
	defer srv.Close()
	token := strings.Repeat("t", 32)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/bootstrap", nil)
	req.Header.Set("x-runner-token", token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("bootstrap: %v %v", resp, err)
	}
	ws := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if _, resp, err := websocket.Dial(ctx, ws+"?t=wrong", nil); err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong token: %v", err)
	}
	conn, _, err := websocket.Dial(ctx, ws+"?t="+token, &websocket.DialOptions{Subprotocols: []string{sdkrunner.Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	if conn.Subprotocol() != sdkrunner.Subprotocol {
		t.Fatalf("subprotocol = %q", conn.Subprotocol())
	}
	read := func() map[string]any {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var event map[string]any
		_ = json.Unmarshal(data, &event)
		return event
	}
	if hello := read(); hello["k"] != "hello" || hello["v"] != float64(1) {
		t.Fatalf("first frame = %v", hello)
	}
	// Attaching is demand: the bridge starts and activity goes busy.
	if f.launcher.Started(wait) == nil {
		t.Fatal("attach did not start the bridge")
	}
	eventually(t, "busy", func() bool { got := f.activity.Got(); return len(got) == 1 && got[0] })
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"k":"user","text":"hello","uuid":"44444444-4444-4444-4444-444444444444"}`))
	var kinds []string
	var lastSeq float64
	for {
		event := read()
		if seq, _ := event["seq"].(float64); seq <= lastSeq {
			t.Fatalf("seq %v after %v", seq, lastSeq)
		} else {
			lastSeq = seq
		}
		kinds = append(kinds, event["k"].(string))
		if event["k"] == "user" && event["origin"] != "client" {
			t.Fatalf("user event = %v", event)
		}
		if event["k"] == "turn" {
			if event["state"] != "idle" {
				t.Fatalf("turn = %v", event)
			}
			break
		}
	}
	for _, k := range []string{"user", "sdk", "meta", "turn"} {
		if !strings.Contains(","+strings.Join(kinds, ",")+",", ","+k+",") {
			t.Fatalf("events %v missing %s", kinds, k)
		}
	}
	// Reattaching with since replays nothing older.
	conn.Close(websocket.StatusNormalClosure, "")
	eventually(t, "idle", func() bool { got := f.activity.Got(); return len(got) == 2 && !got[1] })
	again, _, err := websocket.Dial(ctx, ws+"?t="+token+"&since="+jsonNumber(lastSeq), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer again.CloseNow()
	_, data, _ := again.Read(ctx)
	var hello struct {
		Backlog []json.RawMessage `json:"backlog"`
	}
	_ = json.Unmarshal(data, &hello)
	if len(hello.Backlog) != 0 {
		t.Fatalf("backlog after since = %d entries, want 0", len(hello.Backlog))
	}
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func TestClientsSeeTheA2AMessageOfEachUserMessage(t *testing.T) {
	t.Run("Given an A2A message in the conversation, when a client attaches after its result, assert that its echo and turn name the message", func(t *testing.T) {
		f := newFixture(t, sdkrunnertest.Echo("55555555-5555-5555-5555-555555555555"))
		sub, err := f.session.BeginTurn()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.session.SubmitMessage(context.Background(), "steer", "u1", "m-1", sdkrunner.OriginA2A); err != nil {
			t.Fatal(err)
		}
		for item := next(t, sub); ; item = next(t, sub) {
			if isResult, _ := sdkrunner.ResultUUIDs(item.Raw); isResult {
				break
			}
		}
		f.session.EndTurn(sub)

		backlog := attachBacklog(t, f.session)

		var user, turn map[string]any
		for _, event := range backlog {
			switch event["k"] {
			case "user":
				user = event
			case "turn":
				turn = event
			}
		}
		if user == nil || user["uuid"] != "u1" || user["mid"] != "m-1" {
			t.Fatalf("user echo = %v, want uuid u1 with mid m-1", user)
		}
		if mids, _ := turn["mids"].([]any); turn == nil || len(mids) != 1 || mids[0] != "m-1" {
			t.Fatalf("turn = %v, want mids [m-1]", turn)
		}
	})
}

// attachBacklog attaches a client and returns the backlog its hello replays.
func attachBacklog(t *testing.T, session *sdkrunner.Session) []map[string]any {
	t.Helper()
	srv := httptest.NewServer(sdkrunner.NewServer(context.Background(), session))
	t.Cleanup(srv.Close)
	token := strings.Repeat("t", 32)
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/bootstrap", nil)
	req.Header.Set("x-runner-token", token)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("bootstrap: %v %v", resp, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws?t="+token, &websocket.DialOptions{Subprotocols: []string{sdkrunner.Subprotocol}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hello struct {
		Backlog []map[string]any `json:"backlog"`
	}
	if err := json.Unmarshal(data, &hello); err != nil {
		t.Fatal(err)
	}
	return hello.Backlog
}
