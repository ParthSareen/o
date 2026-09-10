package agent

import (
	"context"
	"strings"
	"sync"

	"github.com/ParthSareen/o/api"
)

type ApprovalRequest struct {
	WorkingDir string
	// Task is the most recent user message, giving auto review the context
	// of what the agent was asked to do.
	Task  string
	Calls []ApprovalToolCall
	// DeniedReview carries the review model's deny verdict when auto mode
	// escalated the request to a human after denial, so the prompt can show
	// why it appeared.
	DeniedReview *ReviewDecision
}

func (r *ApprovalRequest) AddToolCall(id, name, scope string, args map[string]any) {
	r.Calls = append(r.Calls, ApprovalToolCall{
		ToolCallID:    id,
		ToolName:      name,
		Args:          args,
		ApprovalScope: scope,
	})
}

type ApprovalToolCall struct {
	ToolCallID    string
	ToolName      string
	Args          map[string]any
	ApprovalScope string
}

type Approval struct {
	Allow       bool
	AllowAll    bool
	AllowScopes []string
	Reason      string
	// Review carries the auto-review verdict when the decision came from the
	// review model, so the session can surface it to UIs.
	Review *ReviewDecision
}

type ApprovalPrompter interface {
	PromptApproval(context.Context, ApprovalRequest) (Approval, error)
}

// ApprovalMode is the session's permission mode: review prompts for every
// tool call, auto grades calls that would prompt with a review model, and
// full runs everything without approval.
type ApprovalMode int

const (
	ApprovalModeReview ApprovalMode = iota
	ApprovalModeAuto
	ApprovalModeFull
)

type ApprovalState struct {
	mu       sync.RWMutex
	allowAll bool
	auto     bool
	scopes   map[string]bool
}

func (s *ApprovalState) Set(allowAll bool, scopes map[string]bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowAll = allowAll
	s.auto = false
	s.scopes = cloneApprovalScopes(scopes)
}

// SetMode switches the permission mode.
func (s *ApprovalState) SetMode(mode ApprovalMode) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch mode {
	case ApprovalModeFull:
		s.allowAll = true
		s.auto = false
	case ApprovalModeAuto:
		s.allowAll = false
		s.auto = true
	default:
		s.allowAll = false
		s.auto = false
	}
}

// Mode reports the current permission mode. A state with allow-all granted
// is full even if auto was also set.
func (s *ApprovalState) Mode() ApprovalMode {
	if s == nil {
		return ApprovalModeReview
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch {
	case s.allowAll:
		return ApprovalModeFull
	case s.auto:
		return ApprovalModeAuto
	default:
		return ApprovalModeReview
	}
}

// GrantAll grants blanket approval for all future tool calls.
func (s *ApprovalState) GrantAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowAll = true
}

// AllGranted reports whether blanket approval has been granted.
func (s *ApprovalState) AllGranted() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.allowAll
}

func (s *ApprovalState) Allows(scope string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.allowAll || s.scopes[scope]
}

// Apply merges an approval's scopes and allow-all flag into the state. It
// returns true if the approval grants permission (allow-all or at least one
// scope). It does not mutate the approval; the caller sets Allow based on the
// returned value.
func (s *ApprovalState) Apply(result *Approval) bool {
	if s == nil || result == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	granted := false
	if result.AllowAll {
		s.allowAll = true
		granted = true
	}
	if len(result.AllowScopes) > 0 {
		granted = true
		s.grantScopesLocked(result.AllowScopes)
	}
	return granted
}

// GrantScopes merges the given scopes into the state.
func (s *ApprovalState) GrantScopes(scopes []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grantScopesLocked(scopes)
}

// grantScopesLocked adds trimmed, non-empty scopes to the state. Caller must
// hold s.mu.
func (s *ApprovalState) grantScopesLocked(scopes []string) {
	if s.scopes == nil {
		s.scopes = make(map[string]bool, len(scopes))
	}
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope != "" {
			s.scopes[scope] = true
		}
	}
}

func cloneApprovalScopes(src map[string]bool) map[string]bool {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]bool, len(src))
	for scope, allowed := range src {
		if allowed {
			dst[scope] = true
		}
	}
	return dst
}

// latestUserMessageContent returns the content of the most recent user
// message, giving approval reviewers the context of the current task.
func latestUserMessageContent(messages []api.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

func (s *Session) needsApproval(tool Tool, name string, args map[string]any) bool {
	return ToolRequiresApproval(tool, args) && !s.allows(toolApprovalScope(tool, name, args))
}

// allows reports whether scope is permitted by the session's accumulated approval state.
func (s *Session) allows(scope string) bool {
	if s == nil || s.ApprovalState == nil {
		return false
	}
	return s.ApprovalState.Allows(scope)
}

// applyApproval merges an approval result into the session's state and marks
// the result as allowed when scopes or allow-all were granted.
func (s *Session) applyApproval(result *Approval) {
	if s == nil || result == nil {
		return
	}
	if s.ApprovalState == nil {
		s.ApprovalState = &ApprovalState{}
	}
	if s.ApprovalState.Apply(result) {
		result.Allow = true
	}
}

func (s *Session) authorizeToolCalls(ctx context.Context, req ApprovalRequest) (Approval, error) {
	if s == nil || len(req.Calls) == 0 || (s.ApprovalState != nil && s.ApprovalState.AllGranted()) {
		return Approval{Allow: true}, nil
	}
	if s.ApprovalPrompter == nil {
		return Approval{
			Reason: "Tool execution requires approval, but no approval prompter is available.",
		}, nil
	}

	result, err := s.ApprovalPrompter.PromptApproval(ctx, req)
	if err != nil {
		return Approval{}, err
	}
	s.applyApproval(&result)
	return result, nil
}

// toolApprovalScope returns the approval scope key for a tool invocation.
// If the tool implements ScopedTool, its ApprovalScope method determines the
// scope (e.g. shell tools scope to "<tool>\x00<command>"). Otherwise the scope
// is the trimmed tool name.
func toolApprovalScope(tool Tool, toolName string, args map[string]any) string {
	if scoped, ok := tool.(ScopedTool); ok {
		return scoped.ApprovalScope(args)
	}
	return strings.TrimSpace(toolName)
}
