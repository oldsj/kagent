package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

// preparedConfig is a compiled config_json for the default Mainloop instance,
// in the shape ConfigDigests accepts.
const preparedConfig = `{"mcp_servers":{"mainloop":{"url":"http://mainloop-mcp.mainloop.svc.cluster.local/mcp"}},"git":{"origins":["github.com"],"readProxyOrigin":"http://mainloop-git-read.mainloop.svc.cluster.local","pushProxyOrigin":"http://mainloop-git-push.mainloop.svc.cluster.local"}}`

func TestRestoreSetupProfilesAndFencing(t *testing.T) {
	for _, profile := range []string{"supervisor", "child", "agent"} {
		t.Run(profile, func(t *testing.T) {
			raw := []byte(preparedConfig)
			stateDir := t.TempDir()
			config, mcp, err := apiworkspace.ConfigDigests(raw)
			require.NoError(t, err)
			setup, err := apiworkspace.SetupDigest(profile)
			require.NoError(t, err)
			input := Preparation{Profile: profile, SetupDigest: setup, ConfigDigest: config, MCPDigest: mcp}
			require.NoError(t, CheckSetup(raw, stateDir, input, false))
			restored, err := RestoreSetup(raw, stateDir)
			require.NoError(t, err)
			require.Equal(t, input, *restored)
			path := filepath.Join(stateDir, "native-setup.json")
			original, err := os.ReadFile(path)
			require.NoError(t, err)
			for _, change := range []string{"profile", "setup_digest", "config_digest", "mcp_digest"} {
				t.Run(change, func(t *testing.T) {
					previous := setup
					switch change {
					case "profile":
						previous = profile
					case "config_digest":
						previous = config
					case "mcp_digest":
						previous = mcp
					}
					tampered := []byte(strings.Replace(string(original), previous, "unknown", 1))
					require.NoError(t, os.WriteFile(path, tampered, 0600))
					_, err := RestoreSetup(raw, stateDir)
					require.Error(t, err)
					require.Error(t, CheckSetup(raw, stateDir, input, true))
					actual, err := os.ReadFile(path)
					require.NoError(t, err)
					require.Equal(t, tampered, actual)
					require.NoError(t, os.WriteFile(path, original, 0600))
				})
			}
		})
	}
}

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

func preparationFixture(t *testing.T, profile string) (*gitProxyFixture, *Bootstrapper, *fakeRunner, *fixtureInstaller, Preparation) {
	t.Helper()
	f := newGitProxyFixture(t)
	f.available.Store(true)
	request := &Request{Repo: "https://github.com/owner/repo.git", Ref: f.sha, Branch: "feature-prepared", Depth: 1}
	b, _, next := proxyBootstrapper(t, f, true, request)
	installer := &fixtureInstaller{dir: b.stateDir, config: []byte(preparedConfig)}
	config, mcp, err := apiworkspace.ConfigDigests(installer.config)
	require.NoError(t, err)
	digest, err := apiworkspace.SetupDigest(profile)
	require.NoError(t, err)
	input := Preparation{SessionID: uuid.NewString(), ContextID: uuid.NewString(), CreateRequestID: "create", ActionID: "create:prepare", RequestDigest: apiworkspace.Digest([]byte("request")), ExecutionID: uuid.NewString(), ChallengeID: uuid.NewString(), GenerationID: uuid.NewString(), ActorUID: "owned-uid", PreparedRevision: "original", Workspace: *request, Provider: "codex", Profile: profile, SetupDigest: digest, ConfigDigest: config, MCPDigest: mcp}
	return f, b, next, installer, input
}

func TestPreparationActualCheckoutAndReadOnlyChallenge(t *testing.T) {
	for _, profile := range []string{"supervisor", "child", "agent"} {
		t.Run(profile, func(t *testing.T) {
			f, b, next, installer, input := preparationFixture(t, profile)
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
		})
	}
}

// Preparation is only assigned to sessions that RequiresWorkspacePreparation,
// which always pairs a read proxy with a push proxy. observe must keep
// rejecting a read-only policy even though the checkout itself succeeds,
// rather than silently confirming a session that can never push.
func TestPreparationRejectsReadOnlyPolicy(t *testing.T) {
	f := newGitProxyFixture(t)
	f.available.Store(true)
	request := &Request{Repo: "https://github.com/owner/repo.git", Ref: f.sha, Branch: "feature-prepared", Depth: 1}
	b, _, next := proxyBootstrapper(t, f, false, request)
	installer := &fixtureInstaller{dir: b.stateDir, config: []byte(preparedConfig)}
	config, mcp, err := apiworkspace.ConfigDigests(installer.config)
	require.NoError(t, err)
	digest, err := apiworkspace.SetupDigest("supervisor")
	require.NoError(t, err)
	input := Preparation{SessionID: uuid.NewString(), ContextID: uuid.NewString(), CreateRequestID: "create", ActionID: "create:prepare", RequestDigest: apiworkspace.Digest([]byte("request")), ExecutionID: uuid.NewString(), ChallengeID: uuid.NewString(), GenerationID: uuid.NewString(), ActorUID: "owned-uid", PreparedRevision: "original", Workspace: *request, Provider: "codex", Profile: "supervisor", SetupDigest: digest, ConfigDigest: config, MCPDigest: mcp}
	_, err = b.Prepare(t.Context(), input, installer)
	require.EqualError(t, err, "preparation requires fixed proxy transport")
	require.Zero(t, installer.installs)
	require.Zero(t, next.calls)
	require.True(t, bootstrapped(b.stateDir), "the checkout itself must still succeed in read-only mode")
}

func TestPreparationRejectsChangedFilesystemAndIdentity(t *testing.T) {
	for _, profile := range []string{"supervisor", "child", "agent"} {
		t.Run(profile, func(t *testing.T) {
			for _, name := range []string{"HEAD despite done", "branch", "read remote", "push remote", "redirect", "duplicate remote", "command override", "setup", "D", "R", "UID", "session", "context", "generation", "profile", "digest", "config digest", "foreign Git", "incomplete checkout"} {
				t.Run(name, func(t *testing.T) {
					f, b, next, installer, input := preparationFixture(t, profile)
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
						case "session":
							input.SessionID = uuid.NewString()
						case "context":
							input.ContextID = uuid.NewString()
						case "generation":
							input.GenerationID = uuid.NewString()
						case "profile":
							input.Profile = "supervisor"
							if profile == input.Profile {
								input.Profile = "agent"
							}
							var err error
							input.SetupDigest, err = apiworkspace.SetupDigest(input.Profile)
							require.NoError(t, err)
						case "digest":
							input.SetupDigest = apiworkspace.Digest([]byte("changed"))
						case "config digest":
							input.ConfigDigest = apiworkspace.Digest([]byte("changed"))
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
		})
	}
}

type lostCallbackSource struct {
	*fakeSource
	assigned *Preparation
	ready    bool
	replies  []PreparationResult
	fail     bool
	reads    int
}

func (s *lostCallbackSource) Preparation(context.Context) (PreparationState, error) {
	s.reads++
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
	for _, profile := range []string{"supervisor", "child", "agent"} {
		t.Run(profile, func(t *testing.T) {
			f, b, next, installer, input := preparationFixture(t, profile)
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
		})
	}
}

// A turn of a Session that requires preparation is held until the control
// plane reports it ready, and then re-observes the installed setup first.
func TestPreparationAdmissionHoldsTurnsUntilReady(t *testing.T) {
	_, b, next, installer, input := preparationFixture(t, "agent")
	source := &lostCallbackSource{fakeSource: &fakeSource{request: &input.Workspace}}
	b.source = source
	b.ServePreparations(t.Context(), nil)
	b.setupInstaller = installer

	outcome, err := b.Run(t.Context(), runtime.Turn{}, nil)
	require.NoError(t, err)
	require.NotNil(t, outcome.Failure)
	require.Equal(t, "Workspace preparation is incomplete.", outcome.Failure.Message)
	require.Zero(t, next.calls)

	source.assigned = &input
	require.NoError(t, b.PollPreparation(t.Context(), installer))
	require.True(t, source.ready)
	require.Equal(t, 1, installer.installs)

	outcome, err = b.Run(t.Context(), runtime.Turn{}, nil)
	require.NoError(t, err)
	require.Nil(t, outcome.Failure)
	require.Equal(t, 1, next.calls)
	require.Equal(t, 1, installer.observations)
	require.Empty(t, b.Ready(t.Context()))
	require.Equal(t, 2, installer.observations)

	// Once confirmed, polling stops: one more read reports ready, and later
	// polls make no call. Turns still check admission.
	require.NoError(t, b.PollPreparation(t.Context(), installer))
	reads := source.reads
	require.NoError(t, b.PollPreparation(t.Context(), installer))
	require.Equal(t, reads, source.reads)
	require.Equal(t, 1, installer.installs)
	outcome, err = b.Run(t.Context(), runtime.Turn{}, nil)
	require.NoError(t, err)
	require.Nil(t, outcome.Failure)
	require.Greater(t, source.reads, reads)
}

type unrequiredSource struct {
	*fakeSource
	reads int
}

func (s *unrequiredSource) Preparation(context.Context) (PreparationState, error) {
	s.reads++
	return PreparationState{}, nil
}
func (s *unrequiredSource) CompletePreparation(context.Context, PreparationResult) error {
	return context.Canceled
}

// A Session that requires no preparation is asked once per process and its
// turns run without any setup check.
func TestPreparationNotRequiredIsCached(t *testing.T) {
	f := newGitProxyFixture(t)
	f.available.Store(true)
	request := &Request{Repo: "https://github.com/owner/repo.git", Ref: f.sha, Branch: "feature", Depth: 1}
	b, _, next := proxyBootstrapper(t, f, true, request)
	source := &unrequiredSource{fakeSource: &fakeSource{request: request}}
	b.source = source
	for range 2 {
		outcome, err := b.Run(t.Context(), runtime.Turn{}, nil)
		require.NoError(t, err)
		require.Nil(t, outcome.Failure)
	}
	require.Equal(t, 2, next.calls)
	require.Equal(t, 1, source.reads)
	require.NoError(t, b.PollPreparation(t.Context(), &fixtureInstaller{}))
	require.Equal(t, 1, source.reads)
}

func TestPreparationWireRoundTrip(t *testing.T) {
	want := Preparation{
		SessionID: "s", ContextID: "c", CreateRequestID: "cr", ActionID: "a", RequestDigest: "rd", ExecutionID: "e",
		ChallengeID: "ch", Sequence: 7, GenerationID: "g", Atespace: "at", ActorName: "an", ActorUID: "au",
		PreparedRevision: "pr", Workspace: Request{Repo: "https://github.com/o/r", Ref: strings.Repeat("a", 40), Branch: "b", Depth: 3},
		DevelopmentImage: "d", Platform: "p", PolicyIdentity: "pi", PayloadImage: "pay", Provider: "claude", Schema: 2,
		CLIVersion: "1.2.3", Profile: "child", SetupDigest: "sd", ConfigDigest: "cd", MCPDigest: "md",
	}
	require.Equal(t, want, PreparationFromWire(PreparationToWire(want)))
}

// failingSource never reports the Session ready, as the control plane does
// after a definite failure.
type failingSource struct {
	*fakeSource
	assigned *Preparation
	reads    int
	replies  []PreparationResult
}

func (s *failingSource) Preparation(context.Context) (PreparationState, error) {
	s.reads++
	a := s.assigned
	s.assigned = nil
	return PreparationState{Required: true, Assignment: a}, nil
}
func (s *failingSource) CompletePreparation(_ context.Context, r PreparationResult) error {
	s.replies = append(s.replies, r)
	return nil
}

// Once a definite failure is acknowledged, polling stops and turns stay held.
func TestPreparationStopsPollingAfterAcknowledgedFailure(t *testing.T) {
	_, b, next, installer, input := preparationFixture(t, "agent")
	input.ActionID = ""
	source := &failingSource{fakeSource: &fakeSource{request: &input.Workspace}, assigned: &input}
	b.source = source
	require.NoError(t, b.PollPreparation(t.Context(), installer))
	require.Len(t, source.replies, 1)
	require.False(t, source.replies[0].Confirmed)
	require.NoError(t, b.PollPreparation(t.Context(), installer))
	reads := source.reads
	require.NoError(t, b.PollPreparation(t.Context(), installer))
	require.Equal(t, reads, source.reads)
	require.Len(t, source.replies, 1)
	require.Zero(t, installer.installs)

	b.setupInstaller = installer
	outcome, err := b.Run(t.Context(), runtime.Turn{}, nil)
	require.NoError(t, err)
	require.NotNil(t, outcome.Failure)
	require.Zero(t, next.calls)
}
