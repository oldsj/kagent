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
	emitted          map[string]string
	currentMessageID string
	activeBlock      *contentBlockRef
	tools            map[string]string
	emittedToolCalls map[string]struct{}
	emittedResults   map[string]struct{}
	terminal         bool
	// liveTasks holds the IDs of background tasks Claude reports as running.
	liveTasks map[string]struct{}
	// endedTasks holds background tasks that ended after the latest result,
	// mapped to whether Claude killed or stopped them. The next result reports
	// their outcome to the model and clears the set.
	endedTasks map[string]bool
}

type contentBlockRef struct {
	messageID string
	index     int
}

// ParseJSONL parses a JSONL stream of Claude events and emits them to the
// provided event sink.
func ParseJSONL(r io.Reader, maxEventBytes int, emit func(Event) error) error {
	if maxEventBytes <= 0 {
		return fmt.Errorf("max event bytes must be positive")
	}
	p := parser{
		emitted: map[string]string{}, tools: map[string]string{},
		emittedToolCalls: map[string]struct{}{}, emittedResults: map[string]struct{}{},
		liveTasks: map[string]struct{}{}, endedTasks: map[string]bool{},
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
	return nil
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
		Type      string          `json:"type"`
		Subtype   string          `json:"subtype"`
		SessionID string          `json:"session_id"`
		IsError   bool            `json:"is_error"`
		Result    string          `json:"result"`
		Event     json.RawMessage `json:"event"`
		Message   json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return fmt.Errorf("decode Claude event: %w", err)
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
		// This iteration saw every task that ended before its result.
		return p.updateBackgroundTasks(emit, func() { clear(p.endedTasks) })
	}
	return nil
}

// inProcessTeammate tasks run inside the Claude process and do not hold the
// headless run open. Claude Code's own SDK task tracker excludes them too.
const inProcessTeammate = "in_process_teammate"

// parseBackgroundTask tracks Claude's background tasks from its task lifecycle
// events and emits their state whenever it changes. A task is live from
// task_started until task_notification or a terminal task_updated;
// background_tasks_changed replaces the live set. Tasks registered in the
// foreground block their tool call and end before that iteration's result.
// Ambient monitors are excluded, as Claude does not wait for them either.
func (p *parser) parseBackgroundTask(subtype string, line []byte, emit func(Event) error) error {
	var task struct {
		TaskID         string `json:"task_id"`
		TaskType       string `json:"task_type"`
		Status         string `json:"status"`
		Ambient        bool   `json:"ambient"`
		IsBackgrounded *bool  `json:"is_backgrounded"`
		Patch          struct {
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
	default:
		return nil
	}
	if err := json.Unmarshal(line, &task); err != nil {
		return fmt.Errorf("decode Claude %s event: %w", subtype, err)
	}
	if subtype != "background_tasks_changed" && task.TaskID == "" {
		return fmt.Errorf("claude %s event requires a task_id", subtype)
	}
	return p.updateBackgroundTasks(emit, func() {
		switch subtype {
		case "task_started":
			if task.TaskType != inProcessTeammate && !task.Ambient && (task.IsBackgrounded == nil || *task.IsBackgrounded) {
				p.liveTasks[task.TaskID] = struct{}{}
			}
		case "task_updated":
			switch task.Patch.Status {
			case "completed", "failed":
				p.endTask(task.TaskID, false)
			case "killed":
				p.endTask(task.TaskID, true)
			default:
				if task.Patch.IsBackgrounded != nil && *task.Patch.IsBackgrounded {
					p.liveTasks[task.TaskID] = struct{}{}
				}
			}
		case "task_notification":
			p.endTask(task.TaskID, task.Status == "stopped" || task.Status == "killed")
		case "background_tasks_changed":
			live, ambient := map[string]struct{}{}, map[string]struct{}{}
			for _, entry := range task.Tasks {
				switch {
				case entry.TaskID == "" || entry.TaskType == inProcessTeammate:
				case entry.Ambient:
					ambient[entry.TaskID] = struct{}{}
				default:
					live[entry.TaskID] = struct{}{}
				}
			}
			for id := range p.liveTasks {
				if _, ok := live[id]; ok {
					continue
				}
				if _, ok := ambient[id]; ok {
					delete(p.liveTasks, id)
					continue
				}
				// Claude sends this level signal before a task's end events,
				// which follow and say whether it was stopped.
				p.endTask(id, false)
			}
			for id := range live {
				p.liveTasks[id] = struct{}{}
				delete(p.endedTasks, id)
			}
		}
	})
}

// endTask moves a tracked background task to the ended set. Events for
// foreground or already reported tasks are ignored.
func (p *parser) endTask(id string, stopped bool) {
	_, live := p.liveTasks[id]
	wasStopped, ended := p.endedTasks[id]
	if !live && !ended {
		return
	}
	delete(p.liveTasks, id)
	p.endedTasks[id] = wasStopped || stopped
}

// updateBackgroundTasks applies change and emits the background task state if
// it changed.
func (p *parser) updateBackgroundTasks(emit func(Event) error, change func()) error {
	before := p.backgroundTasks()
	change()
	after := p.backgroundTasks()
	if after == before {
		return nil
	}
	return emit(Event{Kind: EventBackgroundTasks, BackgroundTasks: after})
}

func (p *parser) backgroundTasks() BackgroundTasks {
	state := BackgroundTasks{Live: len(p.liveTasks), Unreported: len(p.endedTasks)}
	for _, stopped := range p.endedTasks {
		if stopped {
			state.Stopped++
		}
	}
	return state
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
