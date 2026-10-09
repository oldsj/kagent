package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestStandingSetupDigests(t *testing.T) {
	for _, test := range []struct {
		profile, digest string
	}{
		{"supervisor", "c9b2c5bf815f6b0dd45ee11f02e343ab6a25d4ce39c93663e6feb25281cb87f6"},
		{"child", "7e8a64abb17aaa575ab317f07ad673defc495d97adb641283e5d1cc636b30258"},
		{"agent", "a5fb1bb1e406ff7937b9d9e2e862dff43925d4df54d4bdd9fb010d7e2825ccb3"},
	} {
		t.Run(test.profile, func(t *testing.T) {
			digest, err := SetupDigest(test.profile)
			require.NoError(t, err)
			require.Equal(t, test.digest, digest)
		})
	}
	for _, profile := range []string{"", "owner", "Agent", "arbitrary"} {
		_, err := Standing(profile)
		require.Error(t, err)
		_, err = SetupDigest(profile)
		require.Error(t, err)
	}
}

func TestRestoreSetupProfilesAndFencing(t *testing.T) {
	for _, profile := range []string{"supervisor", "child", "agent"} {
		t.Run(profile, func(t *testing.T) {
			raw := []byte(`{"mcp_servers":{"mainloop":{"url":"http://mainloop-mcp.mainloop.svc.cluster.local/mcp"}}}`)
			stateDir := t.TempDir()
			config, mcp, err := ConfigDigests(raw)
			require.NoError(t, err)
			setup, err := SetupDigest(profile)
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
	installer := &fixtureInstaller{dir: b.stateDir, config: []byte(`{"mcp_servers":{"mainloop":{"url":"http://mainloop-mcp.mainloop.svc.cluster.local/mcp"}}}`)}
	config, mcp, err := ConfigDigests(installer.config)
	require.NoError(t, err)
	digest, err := SetupDigest(profile)
	require.NoError(t, err)
	input := Preparation{SessionID: uuid.NewString(), ContextID: uuid.NewString(), CreateRequestID: "create", ActionID: "create:prepare", RequestDigest: Digest([]byte("request")), ExecutionID: uuid.NewString(), ChallengeID: uuid.NewString(), GenerationID: uuid.NewString(), ActorUID: "owned-uid", PreparedRevision: "original", Workspace: *request, Provider: "codex", Profile: profile, SetupDigest: digest, ConfigDigest: config, MCPDigest: mcp}
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
							input.SetupDigest, err = SetupDigest(input.Profile)
							require.NoError(t, err)
						case "digest":
							input.SetupDigest = Digest([]byte("changed"))
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
