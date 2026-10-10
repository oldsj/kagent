package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/kagent-dev/kagent/go/harness/runtime"
)

type healthProducer struct {
	epoch, workspace, version string
	sequence, revision        int64
	started, failed           bool
	tools                     map[string]runtime.HealthTool
	results                   map[string]struct{}
}

func newHealthProducer(workspace, version string) *healthProducer {
	if version == "" {
		version = "unknown"
	}
	return &healthProducer{epoch: uuid.NewString(), workspace: workspace, version: version, tools: map[string]runtime.HealthTool{}, results: map[string]struct{}{}}
}

func (p *healthProducer) emit(event runtime.HealthEvent, emit func(Event) error) error {
	p.sequence++
	event.SchemaVersion, event.ProducerEpoch, event.TurnKey = runtime.HealthSchema, p.epoch, p.epoch
	event.Sequence, event.EventID = p.sequence, fmt.Sprintf("%s:%d", p.epoch, p.sequence)
	event.Provider, event.RuntimeVersion, event.Source = "claude", p.version, "claude_result_stream"
	event.OccurredAt, event.TimeSource, event.TimePrecision = time.Now().UTC(), "adapter_observed", "nanosecond"
	return emit(Event{Kind: EventHealth, Health: event})
}

func (p *healthProducer) start(emit func(Event) error) error {
	if p.started {
		return nil
	}
	p.started = true
	return p.emit(runtime.HealthEvent{Kind: "turn", Turn: &runtime.HealthTurn{State: "started", Coverage: "partial"}}, emit)
}

func (p *healthProducer) finish(emit func(Event) error) error {
	state := "finished"
	if p.failed {
		state = "failed"
	}
	return p.emit(runtime.HealthEvent{Kind: "turn", Turn: &runtime.HealthTurn{State: state, Coverage: "complete"}}, emit)
}

func opaqueKey(epoch, value string) string {
	digest := sha256.Sum256([]byte(epoch + ":" + value))
	return hex.EncodeToString(digest[:])
}

func (p *healthProducer) tool(id, name string, input map[string]any, completed, failed bool, emit func(Event) error) error {
	tool := p.tools[id]
	if !completed {
		tool = runtime.HealthTool{CallKey: opaqueKey(p.epoch, id), ToolClass: "other", OperationKey: opaqueKey(p.epoch, name), Phase: "started", Action: "other", Outcome: "unknown", Category: "none"}
		switch name {
		case "Read":
			tool.ToolClass, tool.Action = "builtin", "read"
		case "Edit", "Write":
			tool.ToolClass, tool.Action = "builtin", "edit"
		case "Grep", "Glob":
			tool.ToolClass, tool.Action = "builtin", "search"
		case "Bash":
			tool.ToolClass = "shell"
		default:
			if strings.HasPrefix(name, "mcp__") {
				tool.ToolClass = "mcp"
			}
		}
		if tool.Action == "read" || tool.Action == "edit" {
			path, _ := input["file_path"].(string)
			tool.Path = safeHealthPath(p.workspace, path)
		}
		p.tools[id] = tool
	} else {
		tool.Phase, tool.Outcome = "finished", "success"
		if failed {
			tool.Outcome, tool.Category = "error", "tool_error"
			if tool.ToolClass == "mcp" {
				tool.Category = "mcp_error"
			}
		}
	}
	return p.emit(runtime.HealthEvent{Kind: "tool", Tool: &tool}, emit)
}

// Lexical only: never read files, resolve symlinks, inspect commands or retain
// arguments. Sensitive paths are omitted until project-scoped masking exists.
func safeHealthPath(workspace, path string) *string {
	if workspace == "" || path == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	relative, err := filepath.Rel(workspace, filepath.Clean(path))
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil
	}
	for part := range strings.SplitSeq(strings.ToLower(relative), string(filepath.Separator)) {
		if strings.HasPrefix(part, ".env") || strings.Contains(part, "credential") || strings.Contains(part, "secret") || part == ".ssh" || part == ".aws" || part == ".kube" || strings.HasSuffix(part, ".pem") || strings.HasSuffix(part, ".key") {
			return nil
		}
	}
	if len(relative) > 256 || strings.ContainsAny(relative, "\n\r\x00?@") {
		return nil
	}
	relative = filepath.ToSlash(relative)
	return &relative
}

func (p *healthProducer) usage(id string, raw json.RawMessage, failed bool, emit func(Event) error) error {
	if id != "" {
		if _, seen := p.results[id]; seen {
			return nil
		}
		p.results[id] = struct{}{}
	}
	p.revision++
	p.failed = failed
	var counts struct {
		Input  *int64 `json:"input_tokens"`
		Output *int64 `json:"output_tokens"`
	}
	if err := json.Unmarshal(raw, &counts); err != nil {
		// Missing/malformed usage is an observation gap, not an execution failure.
		counts.Input, counts.Output = nil, nil
	}
	if counts.Input != nil && *counts.Input < 0 {
		counts.Input = nil
	}
	if counts.Output != nil && *counts.Output < 0 {
		counts.Output = nil
	}
	if p.revision > 1 {
		// Result iterations are not yet qualified as additive or cumulative.
		// Withhold the turn total instead of assigning an ambiguous counter.
		counts.Input, counts.Output = nil, nil
	}
	coverage := "complete"
	if counts.Input == nil || counts.Output == nil || p.revision > 1 {
		coverage = "partial"
	}
	return p.emit(runtime.HealthEvent{Kind: "usage", Usage: &runtime.HealthUsage{Scope: "turn", Basis: "turn_total", Inclusion: "tree_inclusive", SampleKey: p.epoch, Revision: p.revision, InputTokens: counts.Input, OutputTokens: counts.Output, Coverage: coverage}}, emit)
}
