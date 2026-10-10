package driver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/kagent-dev/kagent/go/harness/internal/utils"
)

type parser struct {
	health           *healthProducer
	emitted          map[string]string
	currentMessageID string
	activeBlock      *contentBlockRef
	tools            map[string]string
	emittedToolCalls map[string]struct{}
	emittedResults   map[string]struct{}
	terminal         bool
	// backgroundTasks holds the background tasks Claude has not yet reported
	// to the conversation, by ID.
	backgroundTasks map[string]taskPhase
}

// taskPhase is how far a background task is from being reported. A task is
// reported once a main-loop model request starts after it ends, which gives
// the model its outcome, and that request's iteration produces a result.
type taskPhase int

const (
	taskRunning taskPhase = iota
	taskEnded
	taskSeen
)

type contentBlockRef struct {
	messageID string
	index     int
}

// ParseJSONL parses a JSONL stream of Claude events and emits them to the
// provided event sink.
func ParseJSONL(r io.Reader, maxEventBytes int, emit func(Event) error) error {
	return parseJSONL(r, maxEventBytes, "", "unknown", emit)
}

func parseJSONL(r io.Reader, maxEventBytes int, workspace, version string, emit func(Event) error) error {
	if maxEventBytes <= 0 {
		return fmt.Errorf("max event bytes must be positive")
	}
	p := parser{
		health:  newHealthProducer(workspace, version),
		emitted: map[string]string{}, tools: map[string]string{},
		emittedToolCalls: map[string]struct{}{}, emittedResults: map[string]struct{}{},
		backgroundTasks: map[string]taskPhase{},
	}
	reader := bufio.NewReaderSize(r, min(maxEventBytes+1, 64*1024))
	for {
		line, err := readBoundedLine(reader, maxEventBytes)
		if len(bytes.TrimSpace(line)) > 0 {
			if parseErr := p.parseLine(line, emit); parseErr != nil {
				return parseErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read Claude event: %w", err)
		}
	}
	if !p.terminal {
		return fmt.Errorf("claude process exited without a terminal result event")
	}
	return p.health.finish(emit)
}

func readBoundedLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		fragment, err := r.ReadSlice('\n')
		if len(line)+len(fragment) > max {
			return nil, fmt.Errorf("claude event exceeds %d bytes", max)
		}
		line = append(line, fragment...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

func (p *parser) parseLine(line []byte, emit func(Event) error) error {
	var envelope struct {
		Type            string          `json:"type"`
		Subtype         string          `json:"subtype"`
		SessionID       string          `json:"session_id"`
		IsError         bool            `json:"is_error"`
		Result          string          `json:"result"`
		Event           json.RawMessage `json:"event"`
		Message         json.RawMessage `json:"message"`
		Usage           json.RawMessage `json:"usage"`
		UUID            string          `json:"uuid"`
		ParentToolUseID string          `json:"parent_tool_use_id"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return fmt.Errorf("decode Claude event: %w", err)
	}
	if err := p.health.start(emit); err != nil {
		return err
	}
	switch envelope.Type {
	case "system":
		if envelope.Subtype == "init" && envelope.SessionID != "" {
			return emit(Event{Kind: EventSessionStarted, SessionID: envelope.SessionID})
		}
		return p.parseBackgroundTask(envelope.Subtype, line, emit)
	case "stream_event":
		return p.parseStreamEvent(envelope.Event, emit)
	case "assistant":
		return p.parseAssistant(envelope.Message, emit)
	case "user":
		return p.parseUser(envelope.Message, emit)
	case "result":
		// A task-notification origin identifies a follow-up iteration of the
		// root conversation. Its result can supersede the earlier iteration.
		p.terminal = true
		if envelope.ParentToolUseID == "" {
			if err := p.health.usage(envelope.UUID, envelope.Usage, envelope.IsError || envelope.Subtype != "success", emit); err != nil {
				return err
			}
		}
		var err error
		if envelope.IsError || envelope.Subtype != "success" {
			message := utils.SafeDiagnostic(envelope.Result)
			if message == "" {
				message = "Claude execution failed"
			}
			err = emit(Event{Kind: EventFailed, Category: envelope.Subtype, SafeMessage: message})
		} else {
			err = emit(Event{Kind: EventCompleted, SessionID: envelope.SessionID, Result: envelope.Result})
		}
		if err != nil {
			return err
		}
		return p.updateBackgroundTasks(emit, func() {
			for id, phase := range p.backgroundTasks {
				if phase == taskSeen {
					delete(p.backgroundTasks, id)
				}
			}
		})
	}
	return nil
}

// inProcessTeammate tasks run inside the Claude process and do not hold the
// headless run open. Claude Code's own SDK task tracker excludes them too.
const inProcessTeammate = "in_process_teammate"

// parseBackgroundTask tracks Claude's background tasks from its task lifecycle
// events and emits the unreported count whenever it changes. A task runs from
// task_started until task_notification or a terminal task_updated;
// background_tasks_changed replaces the running set. How a task ended does not
// matter: completed, failed, killed, and stopped tasks all need the model to see
// the outcome before a result. A main-loop "requesting" status marks a new
// model request; background subagents' requests do not produce one. Tasks
// registered in the foreground block their tool call and are not tracked.
// Ambient monitors are excluded, as Claude does not wait for them either.
func (p *parser) parseBackgroundTask(subtype string, line []byte, emit func(Event) error) error {
	var task struct {
		TaskID          string `json:"task_id"`
		TaskType        string `json:"task_type"`
		Status          string `json:"status"`
		ParentToolUseID string `json:"parent_tool_use_id"`
		Ambient         bool   `json:"ambient"`
		IsBackgrounded  *bool  `json:"is_backgrounded"`
		Patch           struct {
			Status         string `json:"status"`
			IsBackgrounded *bool  `json:"is_backgrounded"`
		} `json:"patch"`
		Tasks []struct {
			TaskID   string `json:"task_id"`
			TaskType string `json:"task_type"`
			Ambient  bool   `json:"ambient"`
		} `json:"tasks"`
	}
	switch subtype {
	case "task_started", "task_updated", "task_notification", "background_tasks_changed":
	case "status":
		// Skip decoding on turns with no background tasks.
		if len(p.backgroundTasks) == 0 {
			return nil
		}
	default:
		return nil
	}
	if err := json.Unmarshal(line, &task); err != nil {
		return fmt.Errorf("decode Claude %s event: %w", subtype, err)
	}
	if subtype != "background_tasks_changed" && subtype != "status" && task.TaskID == "" {
		return fmt.Errorf("claude %s event requires a task_id", subtype)
	}
	return p.updateBackgroundTasks(emit, func() {
		switch subtype {
		case "status":
			if task.Status != "requesting" || task.ParentToolUseID != "" {
				return
			}
			for id, phase := range p.backgroundTasks {
				if phase == taskEnded {
					p.backgroundTasks[id] = taskSeen
				}
			}
		case "task_started":
			if task.TaskType != inProcessTeammate && !task.Ambient && (task.IsBackgrounded == nil || *task.IsBackgrounded) {
				p.startTask(task.TaskID)
			}
		case "task_updated":
			switch task.Patch.Status {
			case "completed", "failed", "killed":
				p.endTask(task.TaskID)
			default:
				if task.Patch.IsBackgrounded != nil && *task.Patch.IsBackgrounded {
					p.startTask(task.TaskID)
				}
			}
		case "task_notification":
			p.endTask(task.TaskID)
		case "background_tasks_changed":
			running, ambient := map[string]struct{}{}, map[string]struct{}{}
			for _, entry := range task.Tasks {
				switch {
				case entry.TaskID == "" || entry.TaskType == inProcessTeammate:
				case entry.Ambient:
					ambient[entry.TaskID] = struct{}{}
				default:
					running[entry.TaskID] = struct{}{}
				}
			}
			for id, phase := range p.backgroundTasks {
				if _, ok := running[id]; ok || phase != taskRunning {
					continue
				}
				if _, ok := ambient[id]; ok {
					delete(p.backgroundTasks, id)
					continue
				}
				// Claude sends this level signal just before a task's end events.
				p.endTask(id)
			}
			for id := range running {
				p.startTask(id)
			}
		}
	})
}

// startTask tracks a background task as running. A task already tracked keeps
// its phase, so repeated level signals cannot reopen an ended task.
func (p *parser) startTask(id string) {
	if _, ok := p.backgroundTasks[id]; !ok {
		p.backgroundTasks[id] = taskRunning
	}
}

// endTask marks a running background task as ended. Events for foreground or
// already reported tasks are ignored.
func (p *parser) endTask(id string) {
	if phase, ok := p.backgroundTasks[id]; ok && phase == taskRunning {
		p.backgroundTasks[id] = taskEnded
	}
}

// updateBackgroundTasks applies change and emits the unreported task count if
// it changed. Phase changes that keep the count do not reach the driver.
func (p *parser) updateBackgroundTasks(emit func(Event) error, change func()) error {
	before := len(p.backgroundTasks)
	change()
	if len(p.backgroundTasks) == before {
		return nil
	}
	return emit(Event{Kind: EventBackgroundTasks, BackgroundTasks: len(p.backgroundTasks)})
}

func (p *parser) parseStreamEvent(raw json.RawMessage, emit func(Event) error) error {
	var event struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
		ContentBlock struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content_block"`
	}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return fmt.Errorf("decode Claude stream event: %w", err)
	}
	switch event.Type {
	case "message_start":
		p.currentMessageID = event.Message.ID
		p.activeBlock = nil
	case "content_block_delta":
		if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
			key := p.blockKey(event.Index)
			p.emitted[key] += event.Delta.Text
			return emit(Event{Kind: EventTextDelta, Text: event.Delta.Text})
		}
	case "content_block_start":
		p.activeBlock = &contentBlockRef{messageID: p.currentMessageID, index: event.Index}
		if event.ContentBlock.Type == "tool_use" {
			if event.ContentBlock.ID == "" || event.ContentBlock.Name == "" {
				return fmt.Errorf("claude tool_use start requires an id and name")
			}
			if previous := p.tools[event.ContentBlock.ID]; previous != "" && previous != event.ContentBlock.Name {
				return fmt.Errorf("claude tool_use %q changed name from %q to %q", event.ContentBlock.ID, previous, event.ContentBlock.Name)
			}
			p.tools[event.ContentBlock.ID] = event.ContentBlock.Name
		}
	case "content_block_stop":
		if p.activeBlock != nil && p.activeBlock.messageID == p.currentMessageID && p.activeBlock.index == event.Index {
			p.activeBlock = nil
		}
	case "message_stop":
		p.activeBlock = nil
	}
	return nil
}

func (p *parser) parseAssistant(raw json.RawMessage, emit func(Event) error) error {
	var message struct {
		ID      string `json:"id"`
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return fmt.Errorf("decode Claude assistant message: %w", err)
	}
	if message.ID != "" {
		p.currentMessageID = message.ID
	}
	for i, content := range message.Content {
		key := p.blockKey(i)
		// Claude Code emits an assistant envelope for the currently open content
		// block. See https://code.claude.com/docs/en/agent-sdk/streaming-output
		if len(message.Content) == 1 && p.activeBlock != nil && p.activeBlock.messageID == p.currentMessageID {
			key = p.blockKey(p.activeBlock.index)
		}
		switch content.Type {
		case "text":
			previous := p.emitted[key]
			if previous == "" {
				p.emitted[key] = content.Text
				if content.Text != "" {
					if err := emit(Event{Kind: EventTextDelta, Text: content.Text}); err != nil {
						return err
					}
				}
			} else if len(content.Text) > len(previous) && content.Text[:len(previous)] == previous {
				suffix := content.Text[len(previous):]
				p.emitted[key] = content.Text
				if suffix != "" {
					if err := emit(Event{Kind: EventTextDelta, Text: suffix}); err != nil {
						return err
					}
				}
			}
		case "tool_use":
			if content.ID == "" || content.Name == "" {
				return fmt.Errorf("claude assistant tool_use requires an id and name")
			}
			if previous := p.tools[content.ID]; previous != "" && previous != content.Name {
				return fmt.Errorf("claude tool_use %q changed name from %q to %q", content.ID, previous, content.Name)
			}
			p.tools[content.ID] = content.Name
			if _, emitted := p.emittedToolCalls[content.ID]; emitted {
				continue
			}
			p.emittedToolCalls[content.ID] = struct{}{}
			if err := p.health.tool(content.ID, content.Name, content.Input, false, false, emit); err != nil {
				return err
			}
			if err := emit(Event{Kind: EventToolActivity, ToolID: content.ID, ToolName: content.Name, ToolPhase: "started", Metadata: content.Input}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *parser) parseUser(raw json.RawMessage, emit func(Event) error) error {
	var message struct {
		Content []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		return fmt.Errorf("decode Claude user message: %w", err)
	}
	for _, content := range message.Content {
		if content.Type != "tool_result" {
			continue
		}
		name := p.tools[content.ToolUseID]
		if content.ToolUseID == "" || name == "" {
			return fmt.Errorf("claude tool_result references unknown tool_use id %q", content.ToolUseID)
		}
		if _, emitted := p.emittedResults[content.ToolUseID]; emitted {
			return fmt.Errorf("claude tool_result for %q was emitted more than once", content.ToolUseID)
		}
		var result any
		if len(content.Content) != 0 && string(content.Content) != "null" {
			if err := json.Unmarshal(content.Content, &result); err != nil {
				return fmt.Errorf("decode Claude tool_result %q content: %w", content.ToolUseID, err)
			}
		}
		p.emittedResults[content.ToolUseID] = struct{}{}
		if err := p.health.tool(content.ToolUseID, name, nil, true, content.IsError, emit); err != nil {
			return err
		}
		if err := emit(Event{
			Kind: EventToolActivity, ToolID: content.ToolUseID, ToolName: name,
			ToolPhase: "completed", ToolResult: result, ToolError: content.IsError,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (p *parser) blockKey(index int) string {
	return p.currentMessageID + ":" + strconv.Itoa(index)
}
