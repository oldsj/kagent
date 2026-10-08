package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The fixture binds setup to the actual bootstrap state directory.
type fixtureInstaller struct {
	dir                    string
	installs, observations int
	config                 []byte
}

func (s *fixtureInstaller) Setup(_ context.Context, input Preparation, observe bool) (Runner, string, error) {
	if observe {
		s.observations++
	} else {
		s.installs++
	}
	return nil, "developer_instruction", CheckSetup(s.config, s.dir, input, observe)
}

func preparationFixture(t *testing.T) (*gitProxyFixture, *Bootstrapper, *fakeRunner, *fixtureInstaller, Preparation) {
	t.Helper()
	f := newGitProxyFixture(t)
	f.available.Store(true)
	request := &Request{Repo: "https://github.com/owner/repo.git", Ref: f.sha, Branch: "feature-prepared", Depth: 1}
	b, _, next := proxyBootstrapper(t, f, true, request)
	installer := &fixtureInstaller{dir: b.stateDir, config: []byte(`{"mcp_servers":{"mainloop":{"url":"http://mainloop-mcp.mainloop.svc.cluster.local/mcp"}}}`)}
	config, mcp, err := ConfigDigests(installer.config)
	require.NoError(t, err)
	digest, err := SetupDigest("child")
	require.NoError(t, err)
	input := Preparation{SessionID: uuid.NewString(), ContextID: uuid.NewString(), CreateRequestID: "create", ActionID: "create:prepare", RequestDigest: Digest([]byte("request")), ExecutionID: uuid.NewString(), ChallengeID: uuid.NewString(), GenerationID: uuid.NewString(), ActorUID: "owned-uid", PreparedRevision: "original", Workspace: *request, Provider: "codex", Profile: "child", SetupDigest: digest, ConfigDigest: config, MCPDigest: mcp}
	return f, b, next, installer, input
}

func TestPreparationActualCheckoutAndReadOnlyChallenge(t *testing.T) {
	f, b, next, installer, input := preparationFixture(t)
	result, err := b.Prepare(t.Context(), input, installer)
	require.NoError(t, err)
	require.True(t, result.Confirmed)
	require.Equal(t, f.sha, result.HEAD)
	require.Equal(t, input.Workspace.Branch, result.Branch)
	require.Equal(t, 1, installer.installs)
	require.Zero(t, next.calls)
	requests, receives := f.snapshot()
	require.NotEmpty(t, requests)
	require.Zero(t, receives)
	input.Sequence, input.ChallengeID = 1, uuid.NewString()
	observed, err := b.Prepare(t.Context(), input, installer)
	require.NoError(t, err)
	require.True(t, observed.ObservedAt.After(result.ObservedAt))
	require.Equal(t, 1, installer.installs)
	require.Equal(t, 1, installer.observations)
	after, _ := f.snapshot()
	require.Len(t, after, len(requests), "observation cannot fetch or rerun checkout")
	restarted, err := New(next, Config{Dir: b.dir, StateDir: b.stateDir, Policy: b.policy, Source: b.source, Environment: f.environment()})
	require.NoError(t, err)
	_, err = restarted.Prepare(t.Context(), input, installer)
	require.NoError(t, err)
	require.Equal(t, 1, installer.installs)
	require.Zero(t, next.calls)
}

func TestPreparationRejectsChangedFilesystemAndIdentity(t *testing.T) {
	for _, name := range []string{"HEAD despite done", "branch", "read remote", "push remote", "redirect", "duplicate remote", "command override", "setup", "D", "R", "UID", "config digest", "foreign Git", "incomplete checkout"} {
		t.Run(name, func(t *testing.T) {
			f, b, next, installer, input := preparationFixture(t)
			switch name {
			case "foreign Git":
				require.NoError(t, os.MkdirAll(b.dir, 0755))
				run(t, b.dir, "init")
			case "incomplete checkout":
				f.available.Store(false)
				_, err := b.Prepare(t.Context(), input, installer)
				require.Error(t, err)
				f.available.Store(true)
			default:
				_, err := b.Prepare(t.Context(), input, installer)
				require.NoError(t, err)
				input.Sequence, input.ChallengeID = 1, uuid.NewString()
				switch name {
				case "HEAD despite done":
					agentCommit(t, b.dir)
				case "branch":
					run(t, b.dir, "branch", "-m", "wrong")
				case "read remote":
					run(t, b.dir, "config", "remote.origin.url", "https://github.com/owner/repo.git")
				case "push remote":
					run(t, b.dir, "config", "remote.origin.pushurl", "https://github.com/owner/repo.git")
				case "redirect":
					run(t, b.dir, "config", "http.followRedirects", "true")
				case "duplicate remote":
					run(t, b.dir, "config", "--add", "remote.origin.url", "http://other.invalid/repo")
				case "command override":
					run(t, b.dir, "config", "remote.origin.uploadpack", "unapproved")
				case "setup":
					require.NoError(t, os.WriteFile(filepath.Join(b.stateDir, "native-setup.json"), []byte(`{}`), 0600))
				case "D":
					input.DevelopmentImage = "changed"
				case "R":
					input.PayloadImage = "changed"
				case "UID":
					input.ActorUID = "changed"
				case "config digest":
					input.ConfigDigest = Digest([]byte("changed"))
				}
			}
			before, _ := os.ReadFile(filepath.Join(b.dir, ".git", "HEAD"))
			_, err := b.Prepare(t.Context(), input, installer)
			require.Error(t, err)
			after, _ := os.ReadFile(filepath.Join(b.dir, ".git", "HEAD"))
			require.Equal(t, before, after)
			require.Zero(t, next.calls)
			require.LessOrEqual(t, installer.installs, 1)
		})
	}
}

type lostCallbackSource struct {
	*fakeSource
	assigned *Preparation
	ready    bool
	replies  []PreparationResult
	fail     bool
}

func (s *lostCallbackSource) Preparation(context.Context) (PreparationState, error) {
	a := s.assigned
	s.assigned = nil
	return PreparationState{Required: true, Ready: s.ready, Assignment: a}, nil
}
func (s *lostCallbackSource) CompletePreparation(_ context.Context, r PreparationResult) error {
	s.replies = append(s.replies, r)
	if s.fail {
		s.fail = false
		return context.DeadlineExceeded
	}
	s.ready = true
	return nil
}

func TestPreparationLostCallbackRestartRetainsOriginalResult(t *testing.T) {
	f, b, next, installer, input := preparationFixture(t)
	source := &lostCallbackSource{fakeSource: &fakeSource{request: &input.Workspace}, assigned: &input, fail: true}
	b.source = source
	require.ErrorIs(t, b.PollPreparation(t.Context(), installer), context.DeadlineExceeded)
	require.Len(t, source.replies, 1)
	require.True(t, source.replies[0].Confirmed)
	requests, _ := f.snapshot()
	f.available.Store(false)
	restarted, err := New(next, Config{Dir: b.dir, StateDir: b.stateDir, Policy: b.policy, Source: source, Environment: f.environment()})
	require.NoError(t, err)
	require.NoError(t, restarted.PollPreparation(t.Context(), installer))
	require.Len(t, source.replies, 2)
	require.Equal(t, source.replies[0], source.replies[1])
	after, _ := f.snapshot()
	require.Len(t, after, len(requests))
	require.Equal(t, 1, installer.installs)
	require.Zero(t, next.calls)
	require.NoError(t, restarted.PollPreparation(t.Context(), installer))
	require.Len(t, source.replies, 2)
}
