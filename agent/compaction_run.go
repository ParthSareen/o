package agent

import (
	"context"
	"strings"

	"github.com/ParthSareen/o/api"
)

func (s *Session) maybeCompact(ctx context.Context, runID string, opts RunOptions, budget PromptBudget, messages []api.Message, latest api.ChatResponse, skipNotified bool) ([]api.Message, bool, bool, error) {
	if s.Compactor == nil {
		return messages, false, skipNotified, nil
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
		return messages, false, skipNotified, nil
	}
	if !result.Compacted {
		if result.Due && !skipNotified {
			if trigger == "" {
				trigger = CompactionTriggerDue
			}
			s.emitCompactionSkipped(runID, opts, trigger, result.Reason)
			skipNotified = true
		}
		return messages, false, skipNotified, nil
	}
	s.emitCompacted(runID, opts, result.Messages, trigger, result.Summary)
	if err := s.recordCompaction(runID, opts.ChatID, trigger, result.Summary); err != nil {
		return result.Messages, true, skipNotified, err
	}
	if err := s.checkPostCompactionPromptBudget(budget, opts, result.Messages); err != nil {
		return result.Messages, true, skipNotified, err
	}
	return result.Messages, true, skipNotified, nil
}

func (s *Session) compactForToolOutputOverflow(ctx context.Context, runID string, opts RunOptions, budget PromptBudget, messages []api.Message, latest api.ChatResponse, assistant api.Message, toolMessages []api.Message, overflows []toolOutputOverflow, skipNotified bool) ([]api.Message, bool, bool, error) {
	if s.Compactor == nil {
		return messages, false, skipNotified, nil
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
		return messages, false, skipNotified, nil
	}
	if !result.Compacted {
		if result.Due && !skipNotified {
			s.emitCompactionSkipped(runID, opts, CompactionTriggerToolOutput, result.Reason)
			skipNotified = true
		}
		return messages, false, skipNotified, nil
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
		refit.Images = msg.Images
		compacted = append(compacted, refit)
		batchTokens += estimateMessagesTokens([]api.Message{refit})
	}

	s.emitCompacted(runID, opts, compacted, CompactionTriggerToolOutput, result.Summary)
	if err := s.recordCompaction(runID, opts.ChatID, CompactionTriggerToolOutput, result.Summary); err != nil {
		return compacted, true, skipNotified, err
	}
	if err := s.checkPostCompactionPromptBudget(budget, opts, compacted); err != nil {
		return compacted, true, skipNotified, err
	}
	return compacted, true, skipNotified, nil
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

// recordCompaction journals one compaction fact; raw history stays append-
// only around it.
func (s *Session) recordCompaction(runID, chatID string, trigger CompactionTrigger, summary string) error {
	if s == nil || s.Journal == nil {
		return nil
	}
	return s.Journal.RecordCompaction(CompactionRecord{RunID: runID, SessionID: chatID, Trigger: string(trigger), Summary: summary})
}
