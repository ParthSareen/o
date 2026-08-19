package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ParthSareen/o/api"
	"github.com/ParthSareen/o/internal/modelref"
)

type ChatClient interface {
	Chat(context.Context, *api.ChatRequest, api.ChatResponseFunc) error
}

type Session struct {
	Client           ChatClient
	EventSinks       []EventSink
	Tools            *Registry
	Skills           *SkillCatalog
	DisableTools     bool
	ApprovalPrompter ApprovalPrompter
	ApprovalState    *ApprovalState
	WorkingDir       string
	Compactor        Compactor
	// Background, when set, is drained for finished background tasks at run
	// boundaries: completions are injected into the conversation so the
	// model sees (and can react to) them. Root sessions only — RLM child
	// sessions must leave this nil so only the root run consumes notices.
	Background BackgroundSource
}

type RunOptions struct {
	ChatID       string
	Model        string
	SystemPrompt string
	Messages     []api.Message
	NewMessages  []api.Message
	Format       string
	Options      map[string]any
	Think        *api.ThinkValue
	KeepAlive    *api.Duration
	// SkillName loads a catalog skill as an ordered synthetic tool call/result
	// before the first model request for this run.
	SkillName string
	// MaxToolRounds limits consecutive model/tool cycles. A positive value is
	// an explicit limit. Zero selects the model-specific default: local models
	// use the default guard and cloud models are unlimited. A negative value
	// disables the guard for tests or special callers.
	MaxToolRounds int
}

type RunResult struct {
	Messages   []api.Message
	Latest     api.ChatResponse
	WorkingDir string
}

const (
	defaultMaxToolRounds              = 300
	maxToolResultRunes                = 60000
	smallContextToolResultRunes       = 6000
	tinyContextToolResultRunes        = 3200
	smallContextToolResultTokenWindow = 8192
	tinyContextToolResultTokenWindow  = 4096
	toolTruncationMarkerReserveTokens = 64
	toolOutputFullOmissionPrefix      = "[tool output truncated: output omitted because the context is full]"
)

type toolOutputOverflow struct {
	toolName   string
	toolCallID string
	content    string
}

type toolBatchResult struct {
	messages  []api.Message
	stop      toolExecutionStop
	overflows []toolOutputOverflow
}

// toolExecutionStop is the batch-level outcome for a group of tool calls,
// distinct from per-call Event.Status values. The values overlap with
// runFinish.status ("denied", "canceled") because a denied or canceled
// batch also terminates the run with the matching status.
type toolExecutionStop string

const (
	toolExecutionDenied   toolExecutionStop = "denied"
	toolExecutionCanceled toolExecutionStop = "canceled"
)

// maxBackgroundContinuations caps how many times a single run can extend
// itself to report background task completions. Each completion is reported
// exactly once so continuations terminate on their own; the cap only guards
// against pathological crash-loops, and anything still buffered past the
// cap surfaces at the next run's start drain.
const maxBackgroundContinuations = 8

const toolExecutionDisabledMessage = "Tool execution disabled."

type runPhase int

const (
	runPhaseModel runPhase = iota
	runPhaseTools
	runPhaseCompact
	runPhaseDone
)

// run is the first-class per-run object that owns the model/tool/compact
// loop. It borrows a *Session for configuration and collaborators (client,
// tools, compactor, event sinks, approval) while holding all mutable per-run
// state. Keeping the loop on run (rather than on Session methods passed a
// state struct) clarifies ownership and makes "one Session, many runs"
// straightforward: each run carries its own phase, messages, and budget.
type run struct {
	session *Session

	runID string
	opts  RunOptions

	phase runPhase

	messages []api.Message
	latest   api.ChatResponse

	budget PromptBudget

	assistant        api.Message
	pendingToolCalls []api.ToolCall
	canceled         bool

	toolBatch *toolBatchResult

	consecutiveModelErrors  int
	toolRounds              int
	maxToolRounds           int
	backgroundContinuations int
	compactionSkipNotified  bool

	finish runFinish
}

type runFinish struct {
	status         RunStatus
	ignoreCanceled bool
	err            error
}

func (r *run) finishDone() {
	r.finish = runFinish{status: RunStatusDone}
	r.phase = runPhaseDone
}

func (r *run) finishDenied() {
	r.finish = runFinish{status: RunStatusDenied}
	r.phase = runPhaseDone
}

func (r *run) finishCanceled() {
	r.finish = runFinish{status: RunStatusCanceled, ignoreCanceled: true}
	r.phase = runPhaseDone
}

func (r *run) finishError(err error) {
	r.finish = runFinish{err: err}
	r.phase = runPhaseDone
}

func (s *Session) Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	if err := s.validateRun(opts); err != nil {
		return nil, err
	}
	if s.ApprovalState == nil {
		s.ApprovalState = &ApprovalState{}
	}
	runID := uuid.NewString()
	budget := s.runBudget(opts)
	messages, err := s.buildRunMessages(ctx, runID, opts, budget)
	if err != nil {
		return nil, err
	}
	activatedSkill, err := s.activateSkill(ctx, runID, opts)
	if err != nil {
		s.emit(newErrorEvent(newEventMetadata(runID, opts), err.Error()))
		return nil, err
	}
	if len(activatedSkill) > 0 {
		messages = append(messages, activatedSkill...)
		if err := s.checkPreflightPromptBudget(budget, opts, messages); err != nil {
			s.emit(newErrorEvent(newEventMetadata(runID, opts), err.Error()))
			return nil, err
		}
	}

	// Idle drain: report background tasks that finished while no run was
	// active, ahead of the new turn so the model sees them in context.
	if notice := s.backgroundNotice(); notice != "" {
		messages = append(messages, api.Message{Role: "user", Content: notice})
		// Best-effort: a dropped render must not fail the run.
		_ = s.emit(newBackgroundTasks(newEventMetadata(runID, opts), notice))
	}

	r := &run{
		session:       s,
		runID:         runID,
		opts:          opts,
		phase:         runPhaseModel,
		messages:      messages,
		budget:        budget,
		maxToolRounds: resolvedMaxToolRounds(opts.Model, opts.MaxToolRounds),
	}
	return r.run(ctx)
}

// run is the canonical loop driver: it dispatches the model -> tools ->
// compact phases until the run reaches a terminal state, then returns the
// accumulated result. The loop and phase orchestration live here rather than
// on Session so that per-run state is owned by the Run itself.
func (r *run) run(ctx context.Context) (*RunResult, error) {
	for {
		switch r.phase {
		case runPhaseModel:
			if err := r.runModelStep(ctx); err != nil {
				return nil, err
			}
		case runPhaseTools:
			if err := r.runToolStep(ctx); err != nil {
				return nil, err
			}
		case runPhaseCompact:
			if err := r.runCompactionStep(ctx); err != nil {
				return nil, err
			}
		case runPhaseDone:
			return r.finishRun(ctx)
		}
	}
}

// validateRun checks the preconditions for a run.
func (s *Session) validateRun(opts RunOptions) error {
	if s == nil {
		return errors.New("nil session")
	}
	if s.Client == nil {
		return errors.New("agent session requires a chat client")
	}
	if opts.Model == "" {
		return errors.New("agent session requires a model")
	}
	return nil
}

// buildRunMessages sanitizes the provided message history, runs the preflight
// prompt-budget check, and returns the initial message list for the run. It
// emits an EventError and returns it if the preflight check fails.
func (s *Session) buildRunMessages(ctx context.Context, runID string, opts RunOptions, budget PromptBudget) ([]api.Message, error) {
	messages := make([]api.Message, 0, len(opts.Messages)+len(opts.NewMessages))
	for _, msg := range opts.Messages {
		messages = append(messages, sanitizeMessageForRun(msg))
	}
	for _, msg := range opts.NewMessages {
		msg = sanitizeMessageForRun(msg)
		messages = append(messages, msg)
	}

	if err := s.checkPreflightPromptBudget(budget, opts, messages); err != nil {
		s.emit(newErrorEvent(newEventMetadata(runID, opts), err.Error()))
		return nil, err
	}
	return messages, nil
}

// runBudget builds the per-run PromptBudget from the compactor's configured
// capacity (resolved against runtime options) and binds the run's prompt
// shape (system prompt, tools, format) so estimation needs only messages. A
// nil compactor yields a budget with the prompt shape bound but a zero context
// window, which disables sizing, matching the previous behavior.
// ResolveCompactionThreshold is applied so a zero configured threshold falls
// back to the default fraction, preserving the former compactionThresholdTokens
// behavior relied on by compactor implementations that report 0.
func (s *Session) runBudget(opts RunOptions) PromptBudget {
	if s.Compactor == nil {
		return NewPromptBudget(0, 0, opts.SystemPrompt, s.availableTools(), opts.Format)
	}
	return NewPromptBudget(s.Compactor.ContextWindowTokens(opts.Options), ResolveCompactionThreshold(s.Compactor.Threshold()), opts.SystemPrompt, s.availableTools(), opts.Format)
}

func (r *run) runModelStep(ctx context.Context) error {
	opts := r.opts
	meta := newEventMetadata(r.runID, opts)

	assistant, pendingToolCalls, canceled, err := r.session.chatRound(ctx, r.runID, opts, r.messages, &r.latest)
	if err != nil {
		var statusErr api.StatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode >= 500 && r.consecutiveModelErrors < 2 {
			r.consecutiveModelErrors++
			r.messages = append(r.messages, api.Message{
				Role:    "user",
				Content: fmt.Sprintf("Your previous response caused an error: %s\n\nPlease try again with a valid response.", statusErr.ErrorMessage),
			})
			return nil
		}
		r.session.emit(newErrorEvent(meta, err.Error()))
		return err
	}
	r.consecutiveModelErrors = 0
	r.assistant = assistant
	r.pendingToolCalls = pendingToolCalls
	r.canceled = canceled

	if !messageEmpty(assistant) {
		r.messages = append(r.messages, assistant)
	}

	if len(pendingToolCalls) == 0 {
		r.toolBatch = nil
		r.phase = runPhaseCompact
		return nil
	}

	if canceled {
		skipped, skipErr := r.session.skipToolCalls(ctx, r.runID, opts, pendingToolCalls, "Tool execution skipped because the run was canceled.")
		if skipErr != nil {
			r.session.emit(newErrorEvent(meta, skipErr.Error()))
			return skipErr
		}
		r.messages = append(r.messages, skipped...)
		r.finishCanceled()
		return nil
	}

	if r.session.DisableTools {
		batch, skipErr := r.session.disabledToolCalls(ctx, r.runID, opts, r.budget, r.messages, pendingToolCalls)
		if skipErr != nil {
			r.session.emit(newErrorEvent(meta, skipErr.Error()))
			return skipErr
		}
		r.messages = append(r.messages, batch.messages...)
		r.toolBatch = &batch
		r.phase = runPhaseCompact
		return nil
	}

	if r.session.Tools == nil {
		r.finishDone()
		return nil
	}

	if r.maxToolRounds >= 0 && r.toolRounds >= r.maxToolRounds {
		content := fmt.Sprintf("Tool execution skipped because the max tool-round limit of %d was reached. Send another message to continue.", r.maxToolRounds)
		toolMessages, skipErr := r.session.skipToolCalls(ctx, r.runID, opts, pendingToolCalls, content)
		if skipErr != nil {
			r.session.emit(newErrorEvent(meta, skipErr.Error()))
			return skipErr
		}
		r.messages = append(r.messages, toolMessages...)
		err := fmt.Errorf("tool round limit reached after %d rounds; send another message to continue", r.maxToolRounds)
		r.session.emit(newErrorEvent(meta, err.Error()))
		r.finishError(err)
		return nil
	}

	r.phase = runPhaseTools
	return nil
}

func (r *run) runToolStep(ctx context.Context) error {
	batch, err := r.session.executeToolCalls(ctx, r.runID, r.opts, r.budget, r.messages, r.pendingToolCalls)
	if err != nil {
		r.session.emit(newErrorEvent(newEventMetadata(r.runID, r.opts), err.Error()))
		return err
	}

	r.messages = append(r.messages, batch.messages...)
	r.toolBatch = &batch
	r.phase = runPhaseCompact
	return nil
}

func (r *run) runCompactionStep(ctx context.Context) error {
	opts := r.opts
	meta := newEventMetadata(r.runID, opts)
	var err error
	if r.toolBatch != nil && len(r.toolBatch.overflows) > 0 {
		r.messages, r.compactionSkipNotified, err = r.session.compactForToolOutputOverflow(ctx, r.runID, opts, r.budget, r.messages, r.latest, r.assistant, r.toolBatch.messages, r.toolBatch.overflows, r.compactionSkipNotified)
	} else {
		r.messages, r.compactionSkipNotified, err = r.session.maybeCompact(ctx, r.runID, opts, r.budget, r.messages, r.latest, r.compactionSkipNotified)
	}
	if err != nil {
		r.session.emit(newErrorEvent(meta, err.Error()))
		r.finishError(err)
		return nil
	}

	if r.toolBatch == nil {
		if r.canceled {
			r.finishCanceled()
			return nil
		}
		// End-of-run drain: report tasks that completed during the run and
		// take one more model step so the model can react, rather than
		// leaving the completion invisible until the next turn. The drain
		// reports each completion once and continuations are capped, so this
		// terminates; anything past the cap stays buffered for the next run's
		// start drain.
		if r.backgroundContinuations < maxBackgroundContinuations {
			if notice := r.session.backgroundNotice(); notice != "" {
				r.messages = append(r.messages, api.Message{Role: "user", Content: notice})
				_ = r.session.emit(newBackgroundTasks(newEventMetadata(r.runID, r.opts), notice))
				r.backgroundContinuations++
				r.assistant = api.Message{}
				r.phase = runPhaseModel
				return nil
			}
		}
		r.finishDone()
		return nil
	}

	switch r.toolBatch.stop {
	case toolExecutionDenied:
		r.finishDenied()
	case toolExecutionCanceled:
		r.finishCanceled()
	default:
		r.toolRounds++
		r.assistant = api.Message{}
		r.pendingToolCalls = nil
		r.toolBatch = nil
		r.phase = runPhaseModel
	}
	return nil
}

func (r *run) finishRun(ctx context.Context) (*RunResult, error) {
	if r.finish.status != "" {
		event := newRunFinished(newEventMetadata(r.runID, r.opts), r.finish.status)
		var err error
		if r.finish.ignoreCanceled {
			err = r.session.emitIgnoringCanceled(ctx, event)
		} else {
			err = r.session.emit(event)
		}
		if err != nil {
			return nil, err
		}
	}
	return &RunResult{Messages: r.messages, Latest: r.latest, WorkingDir: r.session.WorkingDir}, r.finish.err
}

// backgroundNotice drains pending background task completions and renders
// them as one synthetic user message. Empty when no source is set or nothing
// finished. Completions are removed from the source by the drain, so each is
// reported exactly once.
func (s *Session) backgroundNotice() string {
	if s == nil || s.Background == nil {
		return ""
	}
	completions := s.Background.DrainCompletions()
	if len(completions) == 0 {
		return ""
	}
	return formatBackgroundNotice(completions)
}

// formatBackgroundNotice renders completion entries plus bounded failure
// tails as a system-attributed notice.
func formatBackgroundNotice(completions []BackgroundCompletion) string {
	var sb strings.Builder
	sb.WriteString("[background task update — system notice, not a user message]\n")
	for _, c := range completions {
		var status string
		switch {
		case c.Killed:
			status = "killed"
		case c.Failure != "":
			status = "failed to run: " + c.Failure
		case c.ExitCode == 0:
			status = "finished: exit 0"
		default:
			status = fmt.Sprintf("failed: exit %d", c.ExitCode)
		}
		fmt.Fprintf(&sb, "\n%s: %s after %s — %q\n  log: %s\n",
			c.ID, status, formatBackgroundTaskDuration(c.Duration), backgroundCommandSummary(c.Command), c.LogPath)
		if tail := strings.TrimRight(c.Tail, "\n"); tail != "" {
			sb.WriteString("  log tail:\n")
			for _, line := range strings.Split(tail, "\n") {
				sb.WriteString("    " + line + "\n")
			}
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func backgroundCommandSummary(command string) string {
	summary := strings.Join(strings.Fields(command), " ")
	runes := []rune(summary)
	if len(runes) > 60 {
		summary = string(runes[:60]) + "…"
	}
	return summary
}

func formatBackgroundTaskDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(10 * time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

func (s *Session) chatRound(ctx context.Context, runID string, opts RunOptions, messages []api.Message, latest *api.ChatResponse) (api.Message, []api.ToolCall, bool, error) {
	meta := newEventMetadata(runID, opts)
	var tools api.Tools
	if !s.DisableTools {
		tools = s.availableTools()
	}
	req := buildChatRequest(opts, messages, tools)

	assistant := api.Message{Role: "assistant"}
	var pendingToolCalls []api.ToolCall

	err := s.Client.Chat(ctx, &req, func(response api.ChatResponse) error {
		if response.Message.Role != "" {
			assistant.Role = response.Message.Role
		}

		if messageEmpty(response.Message) {
			*latest = response
			return nil
		}

		if response.Message.Thinking != "" {
			assistant.Thinking += response.Message.Thinking
			if err := s.emit(newThinkingDelta(meta, response.Message.Thinking)); err != nil {
				return err
			}
		}

		if response.Message.Content != "" {
			assistant.Content += response.Message.Content
			if err := s.emit(newMessageDelta(meta, response.Message.Content)); err != nil {
				return err
			}
		}

		if len(response.Message.ToolCalls) > 0 {
			assistant.ToolCalls = append(assistant.ToolCalls, response.Message.ToolCalls...)
			pendingToolCalls = append(pendingToolCalls, response.Message.ToolCalls...)
			if err := s.emit(newToolCallDetected(meta, response.Message.ToolCalls)); err != nil {
				return err
			}
		}

		*latest = response
		return nil
	})
	if err != nil {
		if isContextCanceledError(ctx, err) {
			return assistant, pendingToolCalls, true, nil
		}
		return assistant, pendingToolCalls, false, err
	}

	return assistant, pendingToolCalls, false, nil
}

func buildChatRequest(opts RunOptions, messages []api.Message, tools api.Tools) api.ChatRequest {
	requestMessages := sanitizeMessagesForRequest(messages)
	if strings.TrimSpace(opts.SystemPrompt) != "" {
		withSystem := make([]api.Message, 0, len(requestMessages)+1)
		withSystem = append(withSystem, api.Message{Role: "system", Content: opts.SystemPrompt})
		requestMessages = append(withSystem, requestMessages...)
	}

	format := opts.Format
	if format == "json" {
		format = `"` + format + `"`
	}

	req := api.ChatRequest{
		Model:    opts.Model,
		Messages: requestMessages,
		Format:   json.RawMessage(format),
		Options:  opts.Options,
		Think:    opts.Think,
	}
	if opts.KeepAlive != nil {
		req.KeepAlive = opts.KeepAlive
	}
	if len(tools) > 0 {
		req.Tools = tools
	}
	return req
}

func (s *Session) executeToolCalls(ctx context.Context, runID string, opts RunOptions, budget PromptBudget, messages []api.Message, calls []api.ToolCall) (toolBatchResult, error) {
	meta := newEventMetadata(runID, opts)
	batch := toolBatchResult{
		messages: make([]api.Message, 0, len(calls)),
	}
	// Pre-compute the full-history token estimate once per batch instead of
	// re-marshaling the entire history for each tool call. Per-call deltas
	// (tool messages already appended this batch) are tracked in batchTokens
	// and added to historyTokens for a lightweight running total.
	historyTokens := budget.Estimate(messages)
	batchTokens := 0

	type plannedToolCall struct {
		call       api.ToolCall
		tool       Tool
		toolName   string
		args       map[string]any
		workingDir string
	}
	plans := make([]plannedToolCall, 0, len(calls))
	batchWorkingDir := s.currentWorkingDir()
	approvalReq := ApprovalRequest{WorkingDir: batchWorkingDir}
	for _, call := range calls {
		toolName := call.Function.Name
		args := call.Function.Arguments.ToMap()
		tool, ok := s.Tools.Get(toolName)
		plans = append(plans, plannedToolCall{
			call:       call,
			tool:       tool,
			toolName:   toolName,
			args:       args,
			workingDir: batchWorkingDir,
		})
		if ok && s.needsApproval(tool, toolName, args) {
			approvalReq.AddToolCall(call.ID, toolName, toolApprovalScope(tool, toolName, args), args)
		}
	}

	if len(approvalReq.Calls) > 0 {
		approvalResult, err := s.authorizeToolCalls(ctx, approvalReq)
		if err != nil {
			if ctx.Err() != nil {
				skipped, skipErr := s.skipToolCalls(ctx, runID, opts, calls, "Tool execution skipped because the run was canceled.")
				if skipErr != nil {
					return toolBatchResult{}, skipErr
				}
				batch.messages = append(batch.messages, skipped...)
				batch.stop = toolExecutionCanceled
				return batch, nil
			}
			return toolBatchResult{}, err
		}
		if !approvalResult.Allow {
			content := approvalResult.Reason
			if content == "" {
				content = "Tool execution denied."
			}
			for _, plan := range plans {
				msg := budget.FitToolResult(plan.toolName, plan.call.ID, content, historyTokens+batchTokens, CeilingThreshold)
				batch.messages = append(batch.messages, msg)
				batchTokens += estimateMessagesTokens([]api.Message{msg})
				deniedContent := msg.Content
				if emitErr := s.emit(newToolFinished(meta, "denied", plan.call.ID, plan.toolName, "", plan.args, deniedContent, deniedContent)); emitErr != nil {
					return toolBatchResult{}, emitErr
				}
			}
			batch.stop = toolExecutionDenied
			return batch, nil
		}
	}

	for i, plan := range plans {
		call := plan.call
		toolName := plan.toolName
		args := plan.args
		if ctx.Err() != nil {
			skipped, skipErr := s.skipToolCalls(ctx, runID, opts, calls[i:], "Tool execution skipped because the run was canceled.")
			if skipErr != nil {
				return toolBatchResult{}, skipErr
			}
			batch.messages = append(batch.messages, skipped...)
			batch.stop = toolExecutionCanceled
			return batch, nil
		}
		if plan.tool == nil {
			content := fmt.Sprintf("Error: unknown tool: %s", toolName)
			msg := budget.FitToolResult(toolName, call.ID, content, historyTokens+batchTokens, CeilingThreshold)
			batch.messages = append(batch.messages, msg)
			batchTokens += estimateMessagesTokens([]api.Message{msg})
			content = msg.Content
			if toolOutputFullyOmitted(content) {
				batch.overflows = append(batch.overflows, toolOutputOverflow{toolName: toolName, toolCallID: call.ID, content: fmt.Sprintf("Error: unknown tool: %s", toolName)})
			}
			if emitErr := s.emit(newToolFinished(meta, "failed", call.ID, toolName, "", args, content, fmt.Sprintf("unknown tool: %s", toolName))); emitErr != nil {
				return toolBatchResult{}, emitErr
			}
			continue
		}

		if err := s.emit(newToolStarted(meta, call.ID, toolName, plan.workingDir, args)); err != nil {
			return toolBatchResult{}, err
		}

		result, err := s.Tools.Execute(ctx, ToolContext{WorkingDir: plan.workingDir, EventSinks: s.EventSinks, ToolCallID: call.ID}, call)
		if err != nil {
			rawContent := fmt.Sprintf("Error: %v", err)
			msg := budget.FitToolResult(toolName, call.ID, rawContent, historyTokens+batchTokens, CeilingThreshold)
			batch.messages = append(batch.messages, msg)
			batchTokens += estimateMessagesTokens([]api.Message{msg})
			content := msg.Content
			if toolOutputFullyOmitted(content) {
				batch.overflows = append(batch.overflows, toolOutputOverflow{toolName: toolName, toolCallID: call.ID, content: rawContent})
			}
			if emitErr := s.emitIgnoringCanceled(ctx, newToolFinished(meta, "failed", call.ID, toolName, "", args, content, err.Error())); emitErr != nil {
				return toolBatchResult{}, emitErr
			}
			if ctx.Err() != nil {
				skipped, skipErr := s.skipToolCalls(ctx, runID, opts, calls[i+1:], "Tool execution skipped because the run was canceled.")
				if skipErr != nil {
					return toolBatchResult{}, skipErr
				}
				batch.messages = append(batch.messages, skipped...)
				batch.stop = toolExecutionCanceled
				return batch, nil
			}
			continue
		}

		eventWorkingDir := plan.workingDir
		if s.applyToolWorkingDir(result.WorkingDir) {
			eventWorkingDir = s.WorkingDir
		}
		rawContent := result.Content

		msg := budget.FitToolResult(toolName, call.ID, rawContent, historyTokens+batchTokens, CeilingThreshold)
		batch.messages = append(batch.messages, msg)
		batchTokens += estimateMessagesTokens([]api.Message{msg})
		content := msg.Content

		if toolOutputFullyOmitted(content) {
			batch.overflows = append(batch.overflows, toolOutputOverflow{toolName: toolName, toolCallID: call.ID, content: rawContent})
		}
		if err := s.emitIgnoringCanceled(ctx, newToolFinished(meta, "done", call.ID, toolName, eventWorkingDir, args, content, "")); err != nil {
			return toolBatchResult{}, err
		}
		if ctx.Err() != nil {
			skipped, skipErr := s.skipToolCalls(ctx, runID, opts, calls[i+1:], "Tool execution skipped because the run was canceled.")
			if skipErr != nil {
				return toolBatchResult{}, skipErr
			}
			batch.messages = append(batch.messages, skipped...)
			batch.stop = toolExecutionCanceled
			return batch, nil
		}
	}
	return batch, nil
}

func (s *Session) disabledToolCalls(ctx context.Context, runID string, opts RunOptions, budget PromptBudget, messages []api.Message, calls []api.ToolCall) (toolBatchResult, error) {
	meta := newEventMetadata(runID, opts)
	batch := toolBatchResult{
		messages: make([]api.Message, 0, len(calls)),
	}
	historyTokens := budget.Estimate(messages)
	batchTokens := 0
	for _, call := range calls {
		toolName := call.Function.Name
		args := call.Function.Arguments.ToMap()
		msg := budget.FitToolResult(toolName, call.ID, toolExecutionDisabledMessage, historyTokens+batchTokens, CeilingThreshold)
		batch.messages = append(batch.messages, msg)
		batchTokens += estimateMessagesTokens([]api.Message{msg})
		if emitErr := s.emitIgnoringCanceled(ctx, newToolFinished(meta, "disabled", call.ID, toolName, "", args, msg.Content, msg.Content)); emitErr != nil {
			return toolBatchResult{}, emitErr
		}
	}
	return batch, nil
}

func (s *Session) skipToolCalls(ctx context.Context, runID string, opts RunOptions, calls []api.ToolCall, content string) ([]api.Message, error) {
	meta := newEventMetadata(runID, opts)
	toolMessages := make([]api.Message, 0, len(calls))
	for _, call := range calls {
		toolName := call.Function.Name
		args := call.Function.Arguments.ToMap()
		msg := toolMessage(toolName, call.ID, content)
		toolMessages = append(toolMessages, msg)
		if emitErr := s.emitIgnoringCanceled(ctx, newToolFinished(meta, "skipped", call.ID, toolName, "", args, msg.Content, msg.Content)); emitErr != nil {
			return nil, emitErr
		}
	}
	return toolMessages, nil
}

func (s *Session) currentWorkingDir() string {
	if s.WorkingDir != "" {
		return s.WorkingDir
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	s.WorkingDir = wd
	return s.WorkingDir
}

func (s *Session) applyToolWorkingDir(next string) bool {
	next = strings.TrimSpace(next)
	if next == "" {
		return false
	}
	current := s.currentWorkingDir()
	nextAbs, err := canonicalSessionPath(next)
	if err != nil {
		return false
	}
	if current == nextAbs {
		return false
	}
	s.WorkingDir = nextAbs
	return true
}

func canonicalSessionPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	return abs, nil
}

func isContextCanceledError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	return ctx != nil && errors.Is(ctx.Err(), context.Canceled) && strings.Contains(err.Error(), "context canceled")
}

func (s *Session) maybeCompact(ctx context.Context, runID string, opts RunOptions, budget PromptBudget, messages []api.Message, latest api.ChatResponse, skipNotified bool) ([]api.Message, bool, error) {
	if s.Compactor == nil {
		return messages, skipNotified, nil
	}
	req := s.compactionRequest(runID, opts, budget, messages, latest)
	trigger := s.autoCompactionTrigger(req)
	if trigger != "" {
		s.emitCompactionStarted(runID, opts, trigger)
	}
	result, err := s.Compactor.MaybeCompact(ctx, req)
	if err != nil {
		if result.Due && !skipNotified {
			if trigger == "" {
				trigger = CompactionTriggerError
			}
			s.emitCompactionSkipped(runID, opts, trigger, result.Reason)
			skipNotified = true
		}
		return messages, skipNotified, nil
	}
	if !result.Compacted {
		if result.Due && !skipNotified {
			if trigger == "" {
				trigger = CompactionTriggerDue
			}
			s.emitCompactionSkipped(runID, opts, trigger, result.Reason)
			skipNotified = true
		}
		return messages, skipNotified, nil
	}
	s.emitCompacted(runID, opts, result.Messages, trigger, result.Summary)
	if err := s.checkPostCompactionPromptBudget(budget, opts, result.Messages); err != nil {
		return result.Messages, skipNotified, err
	}
	return result.Messages, skipNotified, nil
}

func (s *Session) compactForToolOutputOverflow(ctx context.Context, runID string, opts RunOptions, budget PromptBudget, messages []api.Message, latest api.ChatResponse, assistant api.Message, toolMessages []api.Message, overflows []toolOutputOverflow, skipNotified bool) ([]api.Message, bool, error) {
	if s.Compactor == nil {
		return messages, skipNotified, nil
	}

	keepUserTurns := 0
	req := s.compactionRequest(runID, opts, budget, messages, latest)
	req.Force = true
	req.KeepUserTurns = &keepUserTurns
	s.emitCompactionStarted(runID, opts, CompactionTriggerToolOutput)

	result, err := s.Compactor.MaybeCompact(ctx, req)
	if err != nil {
		if result.Due && !skipNotified {
			s.emitCompactionSkipped(runID, opts, CompactionTriggerToolOutput, result.Reason)
			skipNotified = true
		}
		return messages, skipNotified, nil
	}
	if !result.Compacted {
		if result.Due && !skipNotified {
			s.emitCompactionSkipped(runID, opts, CompactionTriggerToolOutput, result.Reason)
			skipNotified = true
		}
		return messages, skipNotified, nil
	}

	overflowByID := make(map[string]toolOutputOverflow, len(overflows))
	for _, overflow := range overflows {
		overflowByID[overflow.toolCallID] = overflow
	}

	compacted := append([]api.Message(nil), result.Messages...)
	if !messageEmpty(assistant) {
		compacted = append(compacted, assistant)
	}

	historyTokens := budget.Estimate(compacted)
	batchTokens := 0
	for _, msg := range toolMessages {
		content := msg.Content
		toolName := msg.ToolName
		if overflow, ok := overflowByID[msg.ToolCallID]; ok {
			content = overflow.content
			if overflow.toolName != "" {
				toolName = overflow.toolName
			}
		}
		refit := budget.FitToolResult(toolName, msg.ToolCallID, content, historyTokens+batchTokens, CeilingContextWindow)
		compacted = append(compacted, refit)
		batchTokens += estimateMessagesTokens([]api.Message{refit})
	}

	s.emitCompacted(runID, opts, compacted, CompactionTriggerToolOutput, result.Summary)
	if err := s.checkPostCompactionPromptBudget(budget, opts, compacted); err != nil {
		return compacted, skipNotified, err
	}
	return compacted, skipNotified, nil
}

func (s *Session) compactionRequest(runID string, opts RunOptions, budget PromptBudget, messages []api.Message, latest api.ChatResponse) CompactionRequest {
	meta := newEventMetadata(runID, opts)
	return CompactionRequest{
		ChatID:       opts.ChatID,
		Model:        opts.Model,
		Messages:     messages,
		Latest:       latest,
		Options:      opts.Options,
		KeepAlive:    opts.KeepAlive,
		Think:        opts.Think,
		ContinueTask: true,
		Budget:       budget,
		Progress: func(progress CompactionProgress) {
			_ = s.emit(newCompactionProgress(meta, progress.Tokens))
		},
	}
}

func (s *Session) emitCompactionStarted(runID string, opts RunOptions, trigger CompactionTrigger) {
	_ = s.emit(newCompactionStarted(newEventMetadata(runID, opts), trigger))
}

func (s *Session) emitCompactionSkipped(runID string, opts RunOptions, trigger CompactionTrigger, reason string) {
	_ = s.emit(newCompactionSkipped(newEventMetadata(runID, opts), trigger, CompactionSkippedMessage(reason)))
}

func (s *Session) emitCompacted(runID string, opts RunOptions, messages []api.Message, trigger CompactionTrigger, summary string) {
	_ = s.emit(newCompacted(newEventMetadata(runID, opts), messages, trigger, summary))
}

func (s *Session) autoCompactionTrigger(req CompactionRequest) CompactionTrigger {
	if s.Compactor == nil {
		return ""
	}
	trigger, should := s.Compactor.ShouldCompact(req)
	if should {
		return trigger
	}
	return ""
}

func CompactionSkippedMessage(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "compaction could not run"
	}
	return reason
}

func resolvedMaxToolRounds(model string, value int) int {
	if value != 0 {
		return value
	}
	if modelref.HasExplicitCloudSource(model) {
		return -1
	}
	return defaultMaxToolRounds
}

func toolMessageWithLimit(toolName, toolCallID, content string, maxRunes int) api.Message {
	return api.Message{
		Role:       "tool",
		Content:    truncateToolResultContentTo(content, maxRunes),
		ToolName:   toolName,
		ToolCallID: toolCallID,
	}
}

func smallContextToolResultLimitRunes(contextWindow int) int {
	switch {
	case contextWindow > 0 && contextWindow <= tinyContextToolResultTokenWindow:
		return tinyContextToolResultRunes
	case contextWindow > 0 && contextWindow <= smallContextToolResultTokenWindow:
		return smallContextToolResultRunes
	default:
		return 0
	}
}

func (s *Session) availableTools() api.Tools {
	if s == nil || s.Tools == nil {
		return nil
	}
	return s.Tools.Tools()
}

func toolMessage(toolName, toolCallID, content string) api.Message {
	return toolMessageWithLimit(toolName, toolCallID, content, maxToolResultRunes)
}

func sanitizeMessageForRun(msg api.Message) api.Message {
	if msg.Role == "tool" {
		msg.Content = truncateToolResultContent(msg.Content)
	}
	return msg
}

func sanitizeMessagesForRequest(messages []api.Message) []api.Message {
	if len(messages) == 0 {
		return nil
	}
	sanitized := make([]api.Message, len(messages))
	for i, msg := range messages {
		sanitized[i] = sanitizeMessageForRun(msg)
	}
	return sanitized
}

func truncateToolResultContent(content string) string {
	return truncateToolResultContentTo(content, maxToolResultRunes)
}

func truncateToolResultContentTo(content string, maxRunes int) string {
	return Truncate(content, TruncateConfig{
		MaxRunes:           maxRunes,
		HeadTail:           true,
		HeadPct:            75,
		Label:              "tool output",
		Hint:               "Use a narrower command, line range, or search query if more detail is needed.",
		FullOmissionPrefix: toolOutputFullOmissionPrefix,
	})
}

// TruncateConfig configures content truncation via Truncate.
type TruncateConfig struct {
	MaxRunes           int    // rune limit; <= 0 means full omission
	HeadTail           bool   // true = head + tail split; false = head only
	HeadPct            int    // percentage of MaxRunes for head (e.g. 75); tail gets the rest
	Label              string // e.g. "tool output", "summary", "stdout"
	Hint               string // guidance text appended to marker (optional)
	FullOmissionPrefix string // marker prefix when MaxRunes <= 0
}

// Truncate truncates content to at most cfg.MaxRunes runes. When HeadTail is
// true, it preserves the first HeadPct% and last (100-HeadPct)% of the budget
// with a marker between; otherwise it keeps only the head. MaxRunes <= 0
// triggers full omission using FullOmissionPrefix. All token counts in
// markers use ApproximateTokens.
func Truncate(content string, cfg TruncateConfig) string {
	runes := []rune(content)
	total := len(runes)

	if cfg.MaxRunes <= 0 {
		return fmt.Sprintf("%s omitted ~%d tokens.%s]", cfg.FullOmissionPrefix, ApproximateTokens(total), truncHint(cfg.Hint))
	}
	if total <= cfg.MaxRunes {
		return content
	}

	if !cfg.HeadTail {
		head := cfg.MaxRunes
		omitted := total - head
		return string(runes[:head]) + TruncMarker(cfg.Label, head, 0, omitted, false, cfg.Hint)
	}

	head := cfg.MaxRunes * cfg.HeadPct / 100
	tail := cfg.MaxRunes - head
	omitted := total - head - tail
	return string(runes[:head]) + TruncMarker(cfg.Label, head, tail, omitted, true, cfg.Hint) + string(runes[len(runes)-tail:])
}

func truncHint(hint string) string {
	hint = strings.TrimSpace(hint)
	if hint == "" {
		return ""
	}
	if !strings.HasSuffix(hint, ".") {
		hint += "."
	}
	return " " + hint
}

// TruncMarker formats a truncation marker with consistent wording. head and
// tail are rune counts; omitted is the count of runes removed. headTail
// selects the head+tail vs head-only format. hint is optional guidance text.
func TruncMarker(label string, head, tail, omitted int, headTail bool, hint string) string {
	var b strings.Builder
	b.WriteString("\n\n[")
	b.WriteString(label)
	b.WriteString(" truncated: ")
	if headTail {
		fmt.Fprintf(&b, "showing first ~%d tokens and last ~%d tokens; ", ApproximateTokens(head), ApproximateTokens(tail))
	} else {
		fmt.Fprintf(&b, "showing first ~%d tokens; ", ApproximateTokens(head))
	}
	fmt.Fprintf(&b, "omitted ~%d tokens.%s]", ApproximateTokens(omitted), truncHint(hint))
	if headTail {
		b.WriteString("\n\n")
	}
	return b.String()
}

func toolOutputFullyOmitted(content string) bool {
	return strings.HasPrefix(content, toolOutputFullOmissionPrefix)
}

// ApproximateTokens estimates token count from a character/byte count using
// the standard ~4 chars-per-token heuristic. It is intentionally rough; all
// callers use it only for sizing/truncation decisions, not billing.
func ApproximateTokens(n int) int {
	if n <= 0 {
		return 0
	}
	return max(1, (n+3)/4)
}

func messageEmpty(msg api.Message) bool {
	return msg.Content == "" && msg.Thinking == "" && len(msg.ToolCalls) == 0
}
