package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	apiworkspace "github.com/kagent-dev/kagent/go/api/workspace"
	"github.com/kagent-dev/kagent/go/harness/internal/utils"
)

const (
	issuedFile = "native-preparation.issued"
	resultFile = "native-preparation.result"
	ackFile    = "native-preparation.ack"
	setupFile  = "native-setup.json"
	// maxResultBytes bounds the durable result this runtime rereads.
	maxResultBytes = 16 << 10
	// pollInterval is how often an unprepared runtime asks for its assignment.
	pollInterval = time.Second
)

// Preparation is an immutable, controller-assigned action. Observation is a
// distinct read-only challenge against its original effect, never another setup.
type Preparation struct {
	SessionID        string  `json:"session_id"`
	ContextID        string  `json:"context_id"`
	CreateRequestID  string  `json:"create_request_id"`
	ActionID         string  `json:"action_id"`
	RequestDigest    string  `json:"request_digest"`
	ExecutionID      string  `json:"execution_id"`
	ChallengeID      string  `json:"challenge_id"`
	Sequence         uint64  `json:"sequence"`
	GenerationID     string  `json:"generation_id"`
	Atespace         string  `json:"atespace"`
	ActorName        string  `json:"actor_name"`
	ActorUID         string  `json:"actor_uid"`
	PreparedRevision string  `json:"prepared_revision"`
	Workspace        Request `json:"workspace"`
	DevelopmentImage string  `json:"development_image"`
	Platform         string  `json:"platform"`
	PolicyIdentity   string  `json:"policy_identity"`
	PayloadImage     string  `json:"payload_image"`
	Provider         string  `json:"provider"`
	Schema           uint32  `json:"schema"`
	CLIVersion       string  `json:"cli_version"`
	Profile          string  `json:"profile"`
	SetupDigest      string  `json:"setup_digest"`
	ConfigDigest     string  `json:"config_digest"`
	MCPDigest        string  `json:"mcp_digest"`
}

// PreparationResult is the bounded set of facts a runtime reports for an
// assignment. It never carries command output.
type PreparationResult struct {
	Assignment      Preparation `json:"assignment"`
	HEAD            string      `json:"head"`
	Branch          string      `json:"branch"`
	TransportDigest string      `json:"transport_digest"`
	Hook            string      `json:"hook"`
	ObservedAt      time.Time   `json:"observed_at"`
	Confirmed       bool        `json:"confirmed"`
}

// PreparationState is the control plane's view of this Session's preparation.
// Assignment is set only while an action awaits this runtime.
type PreparationState struct {
	Required   bool
	Ready      bool
	Assignment *Preparation
}

// PreparationSource uses the same authenticated workspace connection. Loading
// can issue a durable assignment; repeating that load cannot grant a second effect.
type PreparationSource interface {
	Preparation(context.Context) (PreparationState, error)
	CompletePreparation(context.Context, PreparationResult) error
}

// SetupInstaller installs fixed role setup once, or, with observe set, checks
// the installed hook without changing it. It returns the runner that replaces
// the current one (nil keeps it) and the native hook it used. It must never
// start a native process or bind history.
type SetupInstaller interface {
	Setup(ctx context.Context, input Preparation, observe bool) (Runner, string, error)
}

// Prepare separates bootstrap from Run and shares its serialization. A durable
// issued file precedes all effects. An interrupted issued action is observed or
// held forever; neither a new process nor the bootstrap started marker resets it.
func (b *Bootstrapper) Prepare(ctx context.Context, input Preparation, installer SetupInstaller) (PreparationResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.prepare(ctx, input, installer)
}

func (b *Bootstrapper) prepare(ctx context.Context, input Preparation, installer SetupInstaller) (PreparationResult, error) {
	result := PreparationResult{Assignment: input}
	if installer == nil || input.ActionID == "" || input.ExecutionID == "" || input.ChallengeID == "" || input.RequestDigest == "" || !fullSHA.MatchString(input.Workspace.Ref) || input.Workspace.Branch == "" {
		return result, errors.New("invalid assigned preparation")
	}
	digest, err := apiworkspace.SetupDigest(input.Profile)
	if err != nil || digest != input.SetupDigest {
		return result, errors.New("assigned setup digest differs")
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	if err := utils.EnsurePrivateDir(b.stateDir); err != nil {
		return result, err
	}
	// The issued record names the effect, not the challenge, so every later
	// challenge of the same action observes it.
	effect := input
	effect.Sequence, effect.ChallengeID = 0, ""
	raw, err := json.Marshal(effect)
	if err != nil {
		return result, err
	}
	issued := filepath.Join(b.stateDir, issuedFile)
	observe := input.Sequence != 0
	previous, err := os.ReadFile(issued)
	switch {
	case err == nil:
		if string(previous) != string(raw) {
			return result, errors.New("preparation identity changed")
		}
		observe = true
	case !os.IsNotExist(err):
		return result, err
	case observe:
		return result, errors.New("original preparation issue record is missing")
	default:
		if err := createDurable(issued, raw); err != nil {
			return result, err
		}
		// Preparation never adopts or resets an interrupted or foreign checkout.
		if exists(filepath.Join(b.dir, ".git")) || (markers{b.stateDir}).started() || bootstrapped(b.stateDir) {
			return result, errors.New("preparation workspace was already used")
		}
		if failure := b.bootstrap(ctx, input.Workspace); failure != "" {
			return result, errors.New("assigned checkout failed")
		}
	}
	if !bootstrapped(b.stateDir) {
		return result, errors.New("original checkout is incomplete")
	}
	head, branch, transport, err := b.observe(ctx, input.Workspace)
	if err != nil {
		return result, err
	}
	next, hook, err := installer.Setup(ctx, input, observe)
	if err != nil {
		return result, err
	}
	if next != nil {
		b.next = next
	}
	result.HEAD, result.Branch, result.TransportDigest, result.Hook = head, branch, transport, hook
	result.ObservedAt, result.Confirmed = time.Now().UTC(), true
	return result, nil
}

// createDurable creates path with data, failing if it exists, and syncs both
// the file and its directory before returning.
func createDurable(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// PollPreparation services only the assigned workspace action. It shares the
// bootstrap lock with turns and reports bounded facts, never command output.
//
// The result is persisted before it is reported and acknowledged after, so a
// lost reply or a restarted process resends the original bytes and time
// rather than preparing again.
func (b *Bootstrapper) PollPreparation(ctx context.Context, installer SetupInstaller) error {
	source, ok := b.source.(PreparationSource)
	if !ok {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.noPreparation || b.settled {
		return nil
	}
	state, err := source.Preparation(ctx)
	if err != nil {
		return err
	}
	if !state.Required {
		b.noPreparation = true
		return nil
	}
	// The control plane issues one preparation per Session and never changes
	// it once settled, so a ready Session has nothing left to poll for.
	if state.Ready && state.Assignment == nil {
		b.settled = true
		return nil
	}
	resultPath := filepath.Join(b.stateDir, resultFile)
	ackPath := filepath.Join(b.stateDir, ackFile)
	var result PreparationResult
	var data []byte
	if state.Assignment != nil {
		result, err = b.prepare(ctx, *state.Assignment, installer)
		if err != nil {
			result = PreparationResult{Assignment: *state.Assignment, ObservedAt: time.Now().UTC()}
		}
		if data, err = json.Marshal(result); err != nil {
			return err
		}
		if err := utils.ReplacePrivateFile(resultPath, data); err != nil {
			return err
		}
	} else {
		data, err = readResult(resultPath)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		// An acknowledged result settled the preparation, confirmed or not;
		// after a definite failure the control plane keeps turns refused.
		if ack, _ := os.ReadFile(ackPath); string(ack) == apiworkspace.Digest(data) {
			b.settled = true
			return nil
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return err
		}
	}
	if err := source.CompletePreparation(ctx, result); err != nil {
		return err
	}
	return utils.ReplacePrivateFile(ackPath, []byte(apiworkspace.Digest(data)))
}

func readResult(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > maxResultBytes {
		return nil, errors.New("invalid durable preparation result")
	}
	return data, nil
}

// ServePreparations reads assignments on the runtime's existing channel until
// the control plane reports that this Session needs none or its preparation
// has settled. The runtime context
// owns this loop; it owns no lifecycle retries or timeouts.
func (b *Bootstrapper) ServePreparations(ctx context.Context, installer SetupInstaller) {
	b.mu.Lock()
	b.setupInstaller = installer
	b.mu.Unlock()
	if _, ok := b.source.(PreparationSource); !ok {
		return
	}
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			_ = b.PollPreparation(ctx, installer)
			b.mu.Lock()
			done := b.noPreparation || b.settled
			b.mu.Unlock()
			if done {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// preparationAdmission holds a turn until a required preparation is ready,
// then re-observes the installed hook before the turn runs.
func (b *Bootstrapper) preparationAdmission(ctx context.Context) error {
	source, ok := b.source.(PreparationSource)
	if !ok {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.noPreparation {
		return nil
	}
	state, err := source.Preparation(ctx)
	if err != nil {
		return err
	}
	if !state.Required {
		b.noPreparation = true
		return nil
	}
	if !state.Ready {
		return errors.New("workspace preparation is incomplete")
	}
	data, err := readResult(filepath.Join(b.stateDir, resultFile))
	if err != nil || b.setupInstaller == nil {
		return errors.New("installed setup is unavailable")
	}
	var result PreparationResult
	if json.Unmarshal(data, &result) != nil || !result.Confirmed {
		return errors.New("installed setup is incomplete")
	}
	_, _, err = b.setupInstaller.Setup(ctx, result.Assignment, true)
	return err
}

type installedSetup struct {
	Profile      string `json:"profile"`
	SetupDigest  string `json:"setup_digest"`
	ConfigDigest string `json:"config_digest"`
	MCPDigest    string `json:"mcp_digest"`
}

// RestoreSetup reads the setup a previous process installed, or nil when none
// was. It reads only the fixed overlay and cannot change compiler inputs: the
// record must match configJSON's digests.
func RestoreSetup(configJSON []byte, stateDir string) (*Preparation, error) {
	data, err := os.ReadFile(filepath.Join(stateDir, setupFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var saved installedSetup
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, err
	}
	config, mcp, err := apiworkspace.ConfigDigests(configJSON)
	setup, setupErr := apiworkspace.SetupDigest(saved.Profile)
	if err != nil || setupErr != nil || saved.ConfigDigest != config || saved.MCPDigest != mcp || saved.SetupDigest != setup {
		return nil, errors.New("installed native setup differs")
	}
	return &Preparation{Profile: saved.Profile, SetupDigest: setup, ConfigDigest: config, MCPDigest: mcp}, nil
}

// CheckSetup validates the original configuration against input and either
// records the installed fixed hook or, with observe set, requires its exact
// previous record. The record never contains prompt text.
func CheckSetup(configJSON []byte, stateDir string, input Preparation, observe bool) error {
	config, mcp, err := apiworkspace.ConfigDigests(configJSON)
	setup, setupErr := apiworkspace.SetupDigest(input.Profile)
	if err != nil || setupErr != nil || config != input.ConfigDigest || mcp != input.MCPDigest || setup != input.SetupDigest {
		return errors.New("preparation configuration differs")
	}
	data, err := json.Marshal(installedSetup{input.Profile, setup, config, mcp})
	if err != nil {
		return err
	}
	path := filepath.Join(stateDir, setupFile)
	if observe {
		previous, err := os.ReadFile(path)
		if err != nil || string(previous) != string(data) {
			return errors.New("installed setup differs")
		}
		return nil
	}
	return utils.ReplacePrivateFile(path, data)
}
