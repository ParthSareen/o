package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/ParthSareen/o/api"
)

const (
	toolSkippedCanceled    = "Tool execution skipped because the run was canceled."
	toolSkippedBatchDenied = "Tool execution skipped because another tool call in this batch was denied."
)

// toolCallPlan is one tool call resolved against the registry.
type toolCallPlan struct {
	call     api.ToolCall
	tool     Tool // nil when the model named an unknown tool
	toolName string
	args     map[string]any
	// workingDir is where the call runs: the session directory when its
	// batch started.
	workingDir string
}

func (s *Session) planToolCall(call api.ToolCall, workingDir string) toolCallPlan {
	plan := toolCallPlan{
		call:       call,
		toolName:   call.Function.Name,
		args:       call.Function.Arguments.ToMap(),
		workingDir: workingDir,
	}
	if tool, ok := s.Tools.Get(plan.toolName); ok {
		plan.tool = tool
	}
	return plan
}

// toolSettlement is how one call ended: the journal status, the event
// status, and the content the model sees.
type toolSettlement struct {
	journal string
	status  ToolStatus
	// content is the model-facing result before it is fitted to the budget.
	content string
	// result is the journaled result; only successful calls record one.
	result string
	errMsg string
	images []api.ImageData
}

func toolDone(result ToolResult) toolSettlement {
	return toolSettlement{journal: ToolJournalDone, status: ToolStatusDone, content: result.Content, result: result.Content, images: result.Images}
}

func toolFailed(err error) toolSettlement {
	return toolSettlement{journal: ToolJournalFailed, status: ToolStatusFailed, content: fmt.Sprintf("Error: %v", err), errMsg: err.Error()}
}

func toolUnknown(name string) toolSettlement {
	return toolFailed(fmt.Errorf("unknown tool: %s", name))
}

func toolDenied(reason string) toolSettlement {
	return toolSettlement{journal: ToolJournalDenied, status: ToolStatusDenied, content: reason, errMsg: reason}
}

func toolSkipped(reason string) toolSettlement {
	return toolSettlement{journal: ToolJournalSkipped, status: ToolStatusSkipped, content: reason, errMsg: reason}
}

func toolDisabled() toolSettlement {
	return toolSettlement{journal: ToolJournalDisabled, status: ToolStatusDisabled, content: toolExecutionDisabledMessage, errMsg: toolExecutionDisabledMessage}
}

func toolBlocked(prior BlockedTool) toolSettlement {
	reason := blockedToolReason(prior)
	return toolSettlement{
		journal: ToolJournalBlocked,
		status:  ToolStatusSkipped,
		content: "Error: " + reason + ". Reconcile the interrupted run manually, or retry with a new request.",
		errMsg:  reason,
	}
}

// publishToolOutcome journals a settled call, then publishes tool_finished
// with the model-facing content. The event carries the session's current
// working directory, which UIs adopt as theirs.
func (s *Session) publishToolOutcome(ctx context.Context, meta eventMetadata, runID string, plan toolCallPlan, out toolSettlement, content string) error {
	if err := s.journalToolOutcome(runID, plan.call.ID, out.journal, out.result, out.errMsg); err != nil {
		return journalErr("tool outcome", err)
	}
	return s.emitIgnoringCanceled(ctx, newToolFinished(meta, out.status, plan.call.ID, plan.toolName, s.currentWorkingDir(), plan.args, content, out.errMsg))
}

// toolBatch settles one batch of tool calls into tool result messages,
// keeping a running token estimate so each result is fitted against the
// history plus the results already settled.
type toolBatch struct {
	session    *Session
	ctx        context.Context
	runID      string
	meta       eventMetadata
	budget     PromptBudget
	baseTokens int
	result     toolBatchResult
}

func (s *Session) newToolBatch(ctx context.Context, runID string, opts RunOptions, budget PromptBudget, messages []api.Message) *toolBatch {
	return &toolBatch{
		session:    s,
		ctx:        ctx,
		runID:      runID,
		meta:       newEventMetadata(runID, opts),
		budget:     budget,
		baseTokens: budget.Estimate(messages),
	}
}

func (b *toolBatch) intent(plan toolCallPlan) error {
	if err := b.session.journalToolIntent(b.runID, plan.call.ID, plan.tool, plan.toolName, plan.args); err != nil {
		return journalErr("tool intent", err)
	}
	return nil
}

// settle records a call's outcome and appends its fitted result message.
func (b *toolBatch) settle(plan toolCallPlan, out toolSettlement) error {
	msg := b.budget.FitToolResult(plan.toolName, plan.call.ID, out.content, b.baseTokens, CeilingThreshold)
	msg.Images = out.images
	if err := b.session.publishToolOutcome(b.ctx, b.meta, b.runID, plan, out, msg.Content); err != nil {
		return err
	}
	b.result.messages = append(b.result.messages, msg)
	b.baseTokens += estimateMessagesTokens([]api.Message{msg})
	if toolOutputFullyOmitted(msg.Content) {
		b.result.overflows = append(b.result.overflows, toolOutputOverflow{toolName: plan.toolName, toolCallID: plan.call.ID, content: out.content})
	}
	return nil
}

// settleUnrun settles a call that never executes: its intent is journaled
// alongside the outcome as audit evidence.
func (b *toolBatch) settleUnrun(plan toolCallPlan, out toolSettlement) error {
	if err := b.intent(plan); err != nil {
		return err
	}
	return b.settle(plan, out)
}

func (b *toolBatch) cancel(plans []toolCallPlan) (toolBatchResult, error) {
	for _, plan := range plans {
		if err := b.settleUnrun(plan, toolSkipped(toolSkippedCanceled)); err != nil {
			return toolBatchResult{}, err
		}
	}
	b.result.stop = toolExecutionCanceled
	return b.result, nil
}

func (s *Session) executeToolCalls(ctx context.Context, runID string, opts RunOptions, budget PromptBudget, messages []api.Message, calls []api.ToolCall, blocked map[string]BlockedTool, denied map[string]string) (toolBatchResult, error) {
	b := s.newToolBatch(ctx, runID, opts, budget, messages)
	workingDir := s.currentWorkingDir()
	plans := make([]toolCallPlan, 0, len(calls))
	prompted := make([]bool, 0, len(calls))
	approvalReq := ApprovalRequest{RunID: runID, WorkingDir: workingDir, Task: latestUserMessageContent(messages)}
	for _, call := range calls {
		plan := s.planToolCall(call, workingDir)
		plans = append(plans, plan)
		_, wasDenied := denied[ToolArgsKey(plan.toolName, plan.args)]
		// Calls already refused this run get the refusal back below
		// without another prompt.
		ask := !wasDenied && plan.tool != nil && s.needsApproval(plan.tool, plan.toolName, plan.args, workingDir)
		prompted = append(prompted, ask)
		if ask {
			approvalReq.AddToolCall(call.ID, plan.toolName, toolApprovalScope(plan.tool, plan.toolName, plan.args, workingDir), plan.args)
		}
	}

	if len(approvalReq.Calls) > 0 {
		approval, err := s.authorizeToolCalls(ctx, approvalReq)
		if err != nil {
			if ctx.Err() != nil {
				return b.cancel(plans)
			}
			return toolBatchResult{}, err
		}
		if approval.Review != nil {
			if err := s.emit(newApprovalReviewed(b.meta, approval.Review)); err != nil {
				return toolBatchResult{}, err
			}
		}
		if !approval.Allow {
			return b.deny(plans, prompted, approval, denied)
		}
	}

	for i, plan := range plans {
		if ctx.Err() != nil {
			return b.cancel(plans[i:])
		}
		if err := b.execute(plan, blocked, denied); err != nil {
			return toolBatchResult{}, err
		}
	}
	if ctx.Err() != nil {
		b.result.stop = toolExecutionCanceled
	}
	return b.result, nil
}

// deny settles a batch whose approval was refused. The calls the human saw
// are denied; the rest of the batch is skipped, since running part of a
// batch the model planned as a unit is not what anyone approved.
func (b *toolBatch) deny(plans []toolCallPlan, prompted []bool, approval Approval, denied map[string]string) (toolBatchResult, error) {
	content := deniedToolContent(approval.Reason, approval.Continue)
	if approval.Continue && denied != nil {
		// A continuing deny refuses the calls the human saw for the rest of
		// the run, so a model that retries them gets the refusal back
		// instead of re-prompting.
		for i, plan := range plans {
			if prompted[i] {
				denied[ToolArgsKey(plan.toolName, plan.args)] = content
			}
		}
	}
	for i, plan := range plans {
		out := toolSkipped(toolSkippedBatchDenied)
		if prompted[i] {
			out = toolDenied(content)
		} else if reason, ok := denied[ToolArgsKey(plan.toolName, plan.args)]; ok {
			out = toolDenied(reason)
		}
		if err := b.settleUnrun(plan, out); err != nil {
			return toolBatchResult{}, err
		}
	}
	if !approval.Continue {
		b.result.stop = toolExecutionDenied
	}
	return b.result, nil
}

// execute runs one approved call, or hands back a refusal for calls denied
// earlier in the run or left with an unknown outcome by an interrupted run.
func (b *toolBatch) execute(plan toolCallPlan, blocked map[string]BlockedTool, denied map[string]string) error {
	s := b.session
	if plan.tool == nil {
		return b.settleUnrun(plan, toolUnknown(plan.toolName))
	}
	if err := b.intent(plan); err != nil {
		return err
	}
	key := ToolArgsKey(plan.toolName, plan.args)
	if reason, ok := denied[key]; ok {
		return b.settle(plan, toolDenied(reason))
	}
	if prior, ok := blocked[key]; ok {
		return b.settle(plan, toolBlocked(prior))
	}
	if err := s.emit(newToolStarted(b.meta, plan.call.ID, plan.toolName, plan.workingDir, plan.args)); err != nil {
		return err
	}
	result, err := s.Tools.Execute(b.ctx, ToolContext{WorkingDir: plan.workingDir, SupportsImages: s.SupportsImages, ToolCallID: plan.call.ID}, plan.call)
	if err != nil {
		return b.settle(plan, toolFailed(err))
	}
	s.applyToolWorkingDir(result.WorkingDir)
	return b.settle(plan, toolDone(result))
}

func (s *Session) disabledToolCalls(ctx context.Context, runID string, opts RunOptions, budget PromptBudget, messages []api.Message, calls []api.ToolCall) (toolBatchResult, error) {
	b := s.newToolBatch(ctx, runID, opts, budget, messages)
	workingDir := s.currentWorkingDir()
	for _, call := range calls {
		if err := b.settleUnrun(s.planToolCall(call, workingDir), toolDisabled()); err != nil {
			return toolBatchResult{}, err
		}
	}
	return b.result, nil
}

func (s *Session) skipToolCalls(ctx context.Context, runID string, opts RunOptions, budget PromptBudget, messages []api.Message, calls []api.ToolCall, reason string) ([]api.Message, error) {
	b := s.newToolBatch(ctx, runID, opts, budget, messages)
	workingDir := s.currentWorkingDir()
	for _, call := range calls {
		if err := b.settleUnrun(s.planToolCall(call, workingDir), toolSkipped(reason)); err != nil {
			return nil, err
		}
	}
	return b.result.messages, nil
}

// journalToolIntent records the intent to invoke one call. A nil tool (or nil
// journal) records with the conservative non-replay-safe classification.
func (s *Session) journalToolIntent(runID, callID string, tool Tool, toolName string, args map[string]any) error {
	return s.journalNestedToolIntent(runID, callID, "", tool, toolName, args)
}

func (s *Session) journalNestedToolIntent(runID, callID, parentCallID string, tool Tool, toolName string, args map[string]any) error {
	if s == nil || s.Journal == nil {
		return nil
	}
	return s.Journal.RecordToolIntent(ToolIntent{
		RunID:            runID,
		ToolCallID:       callID,
		ParentToolCallID: parentCallID,
		Name:             toolName,
		Args:             args,
		ReplaySafe:       toolReplaySafe(tool),
	})
}

// journalToolOutcome records a settled tool-call outcome.
func (s *Session) journalToolOutcome(runID, callID, status, result, errMsg string) error {
	if s == nil || s.Journal == nil {
		return nil
	}
	return s.Journal.RecordToolOutcome(ToolOutcome{RunID: runID, ToolCallID: callID, Status: status, Result: result, Err: errMsg})
}

// journalErr wraps a journal write failure: un-journaled state must not be
// treated as execution state.
func journalErr(what string, err error) error {
	return fmt.Errorf("journal %s: %w", what, err)
}

// toolReplaySafe reports whether a tool explicitly declares that re-running
// it with an unknown earlier outcome is safe. Everything unclassified is
// treated as unsafe.
func toolReplaySafe(tool Tool) bool {
	if safe, ok := tool.(ReplaySafeTool); ok {
		return safe.ReplaySafe()
	}
	return false
}

// blockedToolReason explains why a prior attempt's call is refused.
func blockedToolReason(blocked BlockedTool) string {
	return fmt.Sprintf("tool call has an unknown outcome from interrupted run %s (prior call %s); refusing to re-execute automatically", blocked.PriorRunID, blocked.PriorCallID)
}

// deniedToolContent renders the model-facing result of a refused call. When
// the run continues past the denial, the text says the tool did not run and
// that retrying is pointless, because the same call is refused for the rest
// of the run without another prompt.
func deniedToolContent(reason string, continuing bool) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "Tool execution denied."
	}
	if !continuing {
		return reason
	}
	return reason + " The tool did not run. Do not retry this call; choose another approach or ask the user how to proceed."
}

// nestedRunIDs carries what a composing tool's nested calls need to route
// through the run's normal execution path.
type nestedRunIDs struct {
	runID   string
	meta    eventMetadata
	blocked map[string]BlockedTool
	denied  map[string]string
}

// installNestedExecutor wires composing tools (codemode) to this run's
// nested-call path. Run uninstalls it on return, so an executor cannot
// outlive the run it belongs to.
func (s *Session) installNestedExecutor(config nestedRunIDs) {
	if s == nil || s.Tools == nil {
		return
	}
	exec := runNestedExecutor{session: s, config: config}
	s.Tools.VisitTools(func(tool Tool) {
		if setter, ok := tool.(NestedExecutorSetter); ok {
			setter.SetNestedExecutor(exec)
		}
	})
}

func (s *Session) uninstallNestedExecutor() {
	if s == nil || s.Tools == nil {
		return
	}
	s.Tools.VisitTools(func(tool Tool) {
		if setter, ok := tool.(NestedExecutorSetter); ok {
			setter.SetNestedExecutor(nil)
		}
	})
}

// runNestedExecutor points composing tools at the session's nested-call path.
type runNestedExecutor struct {
	session *Session
	config  nestedRunIDs
}

func (e runNestedExecutor) ExecuteNested(ctx context.Context, parentToolCallID, toolName string, args map[string]any) (ToolResult, error) {
	return e.session.executeNestedToolCall(ctx, e.config, parentToolCallID, toolName, args)
}

// executeNestedToolCall runs one tool call nested inside a composing call
// (e.g. codemode) through the same authorization, journaling, blocking, and
// cancellation rules as top-level calls, with the parent correlation for the
// journal.
func (s *Session) executeNestedToolCall(ctx context.Context, config nestedRunIDs, parentToolCallID, toolName string, args map[string]any) (ToolResult, error) {
	tool, ok := s.Tools.Get(toolName)
	if !ok {
		return ToolResult{}, fmt.Errorf("unknown tool: %s", toolName)
	}
	if s.DisableTools {
		return ToolResult{}, errors.New(toolExecutionDisabledMessage)
	}
	workingDir := s.currentWorkingDir()
	callID := "nested-" + uuid.NewString()
	plan := toolCallPlan{
		call: api.ToolCall{ID: callID, Function: api.ToolCallFunction{
			Name:      toolName,
			Arguments: argsToToolCallArguments(args),
		}},
		tool:       tool,
		toolName:   toolName,
		args:       args,
		workingDir: workingDir,
	}
	// refuse settles a call that does not run and returns its refusal.
	refuse := func(out toolSettlement) (ToolResult, error) {
		if err := s.publishToolOutcome(ctx, config.meta, config.runID, plan, out, out.content); err != nil {
			return ToolResult{}, err
		}
		return ToolResult{}, errors.New(out.errMsg)
	}

	if err := s.journalNestedToolIntent(config.runID, callID, parentToolCallID, tool, toolName, args); err != nil {
		return ToolResult{}, journalErr("nested tool intent", err)
	}
	key := ToolArgsKey(toolName, args)
	if prior, ok := config.blocked[key]; ok {
		return refuse(toolBlocked(prior))
	}
	if reason, ok := config.denied[key]; ok {
		return refuse(toolDenied(reason))
	}
	if s.needsApproval(tool, toolName, args, workingDir) {
		req := ApprovalRequest{RunID: config.runID, WorkingDir: workingDir}
		req.AddToolCall(callID, toolName, toolApprovalScope(tool, toolName, args, workingDir), args)
		approval, err := s.authorizeToolCalls(ctx, req)
		if err != nil {
			out := toolFailed(err)
			return ToolResult{}, errors.Join(err, s.publishToolOutcome(ctx, config.meta, config.runID, plan, out, out.content))
		}
		if approval.Review != nil {
			_ = s.emit(newApprovalReviewed(config.meta, approval.Review))
		}
		if !approval.Allow {
			reason := deniedToolContent(approval.Reason, true)
			if config.denied != nil {
				config.denied[key] = reason
			}
			return refuse(toolDenied(reason))
		}
	}

	if err := s.emit(newToolStarted(config.meta, callID, toolName, workingDir, args)); err != nil {
		return ToolResult{}, err
	}
	result, err := s.Tools.Execute(ctx, ToolContext{WorkingDir: workingDir, SupportsImages: s.SupportsImages, ToolCallID: callID}, plan.call)
	if err != nil {
		out := toolFailed(err)
		if perr := s.publishToolOutcome(ctx, config.meta, config.runID, plan, out, out.content); perr != nil {
			return ToolResult{}, perr
		}
		return ToolResult{}, err
	}
	s.applyToolWorkingDir(result.WorkingDir)
	result.Content = capNestedResult(result.Content)
	out := toolDone(result)
	if err := s.publishToolOutcome(ctx, config.meta, config.runID, plan, out, out.content); err != nil {
		return ToolResult{}, err
	}
	return result, nil
}

// nestedResultMaxRunes bounds a nested call's result payload, both in the
// journal and toward the calling script.
const nestedResultMaxRunes = 16384

// capNestedResult truncates a nested result to a bounded head+tail form.
func capNestedResult(content string) string {
	return Truncate(content, TruncateConfig{
		MaxRunes: nestedResultMaxRunes,
		HeadTail: true,
		HeadPct:  75,
		Label:    "tool output",
	})
}

// argsToToolCallArguments converts a plain args map into the ordered
// arguments struct tools receive, iterating keys sorted for determinism.
func argsToToolCallArguments(args map[string]any) api.ToolCallFunctionArguments {
	tc := api.NewToolCallFunctionArguments()
	for _, key := range slices.Sorted(maps.Keys(args)) {
		tc.Set(key, args[key])
	}
	return tc
}
