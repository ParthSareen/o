package agent

// Auto mode: a review model grades tool calls that would otherwise require a
// human approval prompt, using the same decision contract as the Codex
// Guardian setup in ollama's compat proxy (internal/proxy in the ollama
// repo): one submit_decision call carrying risk_level, user_authorization,
// outcome, and rationale. Anything malformed fails closed.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ParthSareen/o/api"
)

const (
	autoReviewDecisionToolName = "submit_decision"
	autoReviewTimeout          = 2 * time.Minute
	// autoReviewTaskLimit and autoReviewDetailLimit bound the review request
	// so grading stays cheap regardless of prompt or diff size.
	autoReviewTaskLimit   = 2000
	autoReviewDetailLimit = 4000
)

// autoReviewThinkLowRejected caches, per model, that the server rejects
// think="low". Grading only needs a submit_decision call, so the request
// asks for the lowest thinking level: GLM cloud models such as glm-5.3
// silently ignore think=false but honor "low". When a model rejects
// the level entirely (e.g. non-thinking models 400 on any truthy value),
// the discovery is cached in memory so only the first review per model
// pays for it.
var autoReviewThinkLowRejected sync.Map

// autoReviewThinkLow reports whether the review request should ask for the
// lowest thinking level.
func autoReviewThinkLow(model string) bool {
	_, rejected := autoReviewThinkLowRejected.Load(model)
	return !rejected
}

// ResolveAutoReviewModel maps the review-model config value to a concrete
// model. Empty or "selected" grades with the session model; any other value
// is taken as the model name.
func ResolveAutoReviewModel(configured, sessionModel string) string {
	switch strings.ToLower(strings.TrimSpace(configured)) {
	case "", "selected":
		return strings.TrimSpace(sessionModel)
	default:
		return strings.TrimSpace(configured)
	}
}

// AutoReviewer grades approval requests with a model. It is the model-side
// half of auto mode; AutoReviewPrompter wires it into the approval flow.
type AutoReviewer struct {
	Client ChatClient
	Model  string
}

type autoReviewDecision struct {
	RiskLevel         string `json:"risk_level"`
	UserAuthorization string `json:"user_authorization"`
	Outcome           string `json:"outcome"`
	Rationale         string `json:"rationale"`
}

// autoReviewDecisionTool mirrors the Guardian decision tool from the Codex
// compat proxy so both graders share one contract.
func autoReviewDecisionTool() api.Tool {
	props := api.NewToolPropertiesMap()
	props.Set("risk_level", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Risk of running the pending actions: low, medium, high, or critical.",
		Enum:        []any{"low", "medium", "high", "critical"},
	})
	props.Set("user_authorization", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "How clearly the user's request authorizes the pending actions.",
		Enum:        []any{"unknown", "low", "medium", "high"},
	})
	props.Set("outcome", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "Whether the agent may run the pending actions.",
		Enum:        []any{"allow", "deny"},
	})
	props.Set("rationale", api.ToolProperty{
		Type:        api.PropertyType{"string"},
		Description: "One or two sentences: the key risk and, on deny, what the agent should do instead.",
	})
	return api.Tool{
		Type: "function",
		Function: api.ToolFunction{
			Name:        autoReviewDecisionToolName,
			Description: "Submit the final approval decision after reviewing the pending actions.",
			Parameters: api.ToolFunctionParameters{
				Type:       "object",
				Properties: props,
				Required:   []string{"risk_level", "user_authorization", "outcome", "rationale"},
			},
		},
	}
}

// Review grades the request and returns the resulting approval. Calls that
// are statically safe run without a model round-trip. The grading request
// asks for the lowest thinking level (the rationale in submit_decision
// already covers the reasoning; GLM cloud models ignore think=false but
// honor "low"); if the server rejects the level, the model is cached in
// memory and the request is retried with the default think behavior. A nil
// error means the decision was valid (allow or deny); an error means grading
// failed and the caller must fall back to prompting or denying.
func (r *AutoReviewer) Review(ctx context.Context, req ApprovalRequest) (Approval, error) {
	if r == nil || r.Client == nil || strings.TrimSpace(r.Model) == "" {
		return Approval{}, errors.New("auto review is not configured")
	}
	if len(req.Calls) == 0 {
		return Approval{Allow: true}, nil
	}
	if autoReviewStaticallySafe(req) {
		return Approval{Allow: true}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, autoReviewTimeout)
	defer cancel()
	started := time.Now()

	stream := false
	messages := []api.Message{
		{Role: "system", Content: autoReviewSystemPrompt(req)},
		{Role: "user", Content: autoReviewUserPrompt(req)},
	}
	tools := api.Tools{autoReviewDecisionTool()}
	grade := func(thinkLow bool) (*autoReviewDecision, error) {
		chatReq := &api.ChatRequest{
			Model:    r.Model,
			Messages: messages,
			Tools:    tools,
			Stream:   &stream,
		}
		if thinkLow {
			chatReq.Think = &api.ThinkValue{Value: "low"}
		}
		var decision *autoReviewDecision
		err := r.Client.Chat(ctx, chatReq, func(resp api.ChatResponse) error {
			if !resp.Done {
				return nil
			}
			d, err := autoReviewDecisionFromMessage(resp.Message)
			if err != nil {
				return err
			}
			decision = d
			return nil
		})
		return decision, err
	}

	wantThinkLow := autoReviewThinkLow(r.Model)
	decision, err := grade(wantThinkLow)
	if err != nil && wantThinkLow && isUnsupportedThinkError(err) {
		autoReviewThinkLowRejected.Store(r.Model, struct{}{})
		decision, err = grade(false)
	}
	if err != nil {
		return Approval{}, err
	}
	if decision == nil {
		return Approval{}, errors.New("review model returned no decision")
	}
	approval := decision.approval()
	info := ReviewDecision{
		Model:     r.Model,
		Outcome:   decision.Outcome,
		Risk:      decision.RiskLevel,
		Rationale: decision.Rationale,
		Duration:  time.Since(started),
		Calls:     len(req.Calls),
	}
	approval.Review = &info
	return approval, nil
}

// autoReviewDecisionFromMessage extracts the decision from a tool call, with
// a strict text-JSON fallback for models that ignore tools.
func autoReviewDecisionFromMessage(msg api.Message) (*autoReviewDecision, error) {
	var found *autoReviewDecision
	for _, call := range msg.ToolCalls {
		if call.Function.Name != autoReviewDecisionToolName {
			continue
		}
		if found != nil {
			return nil, errors.New("review model called submit_decision more than once")
		}
		d, err := validateAutoReviewDecision([]byte(call.Function.Arguments.String()))
		if err != nil {
			return nil, err
		}
		found = &d
	}
	if found != nil {
		return found, nil
	}

	text := strings.TrimSpace(msg.Content)
	if text == "" {
		return nil, errors.New("review model returned no decision")
	}
	// Fallback: the full decision JSON as assistant text.
	if d, err := validateAutoReviewDecision([]byte(text)); err == nil {
		return &d, nil
	}
	// Compact form: a bare allow, as in the Codex contract. Denies must come
	// through the full form so the rationale is always present.
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	var compact struct {
		Outcome string `json:"outcome"`
	}
	if err := decoder.Decode(&compact); err != nil {
		return nil, errors.New("review model did not call submit_decision")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("review model did not call submit_decision")
	}
	if compact.Outcome != "allow" {
		return nil, errors.New("review model did not call submit_decision")
	}
	return &autoReviewDecision{Outcome: "allow", Rationale: "compact allow"}, nil
}

// validateAutoReviewDecision strictly decodes and checks a decision, the
// same way the Codex compat proxy validates Guardian output.
func validateAutoReviewDecision(data []byte) (autoReviewDecision, error) {
	var decision autoReviewDecision
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decision); err != nil {
		return decision, fmt.Errorf("decode review decision: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return decision, errors.New("review decision contains trailing data")
	}
	if !oneOfAutoReview(decision.RiskLevel, "low", "medium", "high", "critical") {
		return decision, fmt.Errorf("invalid review risk_level %q", decision.RiskLevel)
	}
	if !oneOfAutoReview(decision.UserAuthorization, "unknown", "low", "medium", "high") {
		return decision, fmt.Errorf("invalid review user_authorization %q", decision.UserAuthorization)
	}
	if !oneOfAutoReview(decision.Outcome, "allow", "deny") {
		return decision, fmt.Errorf("invalid review outcome %q", decision.Outcome)
	}
	if strings.TrimSpace(decision.Rationale) == "" {
		return decision, errors.New("review rationale is empty")
	}
	return decision, nil
}

func oneOfAutoReview(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// approval applies the review policy to a validated decision: allows run,
// denies block with the rationale for the agent, and critical-risk actions
// are denied even when the reviewer said allow.
func (d autoReviewDecision) approval() Approval {
	if d.Outcome == "allow" && d.RiskLevel != "critical" {
		return Approval{Allow: true}
	}
	if d.Outcome == "deny" || d.RiskLevel == "critical" {
		return Approval{
			Allow:  false,
			Reason: fmt.Sprintf("Auto review denied (%s risk): %s", d.RiskLevel, d.Rationale),
		}
	}
	return Approval{Allow: true}
}

// AutoReviewPrompter implements ApprovalPrompter for auto mode. In auto mode
// the reviewer grades each request; statically safe requests skip grading.
// Review mode delegates to Next unchanged, and grading failures fall back to
// Next when it is set (the human prompt) or deny when DenyOnFailure is set
// (headless and pipe runs have no human to fall back on).
// With EscalateDeny,
// non-critical reviewer denies also fall back to Next so the user gets the
// final word; critical-risk denies stay final.
type AutoReviewPrompter struct {
	Reviewer      *AutoReviewer
	State         *ApprovalState
	Next          ApprovalPrompter
	DenyOnFailure bool
	EscalateDeny  bool
}

func (p AutoReviewPrompter) PromptApproval(ctx context.Context, req ApprovalRequest) (Approval, error) {
	if p.State != nil && p.State.Mode() != ApprovalModeAuto {
		return p.next(ctx, req)
	}
	approval, err := p.Reviewer.Review(ctx, req)
	if err != nil {
		if p.DenyOnFailure {
			return Approval{
				Allow:  false,
				Reason: "Auto review failed, denying: " + err.Error(),
			}, nil
		}
		return p.next(ctx, req)
	}
	if approval.Allow || !p.escalates(approval) {
		return approval, nil
	}
	// The reviewer denied; give the human the final word with the verdict
	// attached so the prompt can explain why it appeared.
	escalated := req
	escalated.DeniedReview = approval.Review
	human, herr := p.next(ctx, escalated)
	if herr != nil {
		return approval, nil
	}
	human.Review = approval.Review
	return human, nil
}

// escalates reports whether a reviewer deny should be handed to Next for a
// human decision: escalation must be enabled with a Next to ask (headless
// runs have neither), and critical-risk denies stay final, failing closed.
func (p AutoReviewPrompter) escalates(approval Approval) bool {
	if !p.EscalateDeny || p.Next == nil {
		return false
	}
	return approval.Review == nil || approval.Review.Risk != "critical"
}

func (p AutoReviewPrompter) next(ctx context.Context, req ApprovalRequest) (Approval, error) {
	if p.Next == nil {
		return Approval{
			Reason: "Tool execution requires approval, but no approval prompter is available.",
		}, nil
	}
	return p.Next.PromptApproval(ctx, req)
}

const autoReviewSystemPromptTemplate = `You are the permission reviewer for an AI coding agent running in a user's terminal. Decide whether the agent may run the pending actions automatically, without asking the user.

Working directory: %s

Allow actions that are ordinary, reversible development work in the working directory and that serve the user's request: reading and editing source files, building, testing, searching, and fetching public documentation.

Deny actions that are dangerous, out of scope, or that a careful engineer would not run unattended:
- sending secrets, credentials, tokens, or private data to any external destination, including through web requests or git pushes
- deleting or destroying data irreversibly, or wiping files the agent did not create (rm -rf, git reset --hard, git push --force, dropping databases)
- installing or executing untrusted code (curl | bash, scripts from unknown URLs), or altering the system (sudo, service changes, installs outside the project)
- accessing credentials or secrets (~/.ssh, .env, cloud keys) or anything outside the working directory
- changing git remotes, force pushes, or rewriting shared history

Judge each action against the user's request. When the user explicitly asked for the action, allow it. When in doubt, deny and say what the agent should do instead.`

func autoReviewSystemPrompt(req ApprovalRequest) string {
	return fmt.Sprintf(autoReviewSystemPromptTemplate, req.WorkingDir)
}

func autoReviewUserPrompt(req ApprovalRequest) string {
	var b strings.Builder
	if task := strings.TrimSpace(req.Task); task != "" {
		fmt.Fprintf(&b, "The user's request:\n%s\n\n", truncateRunesAutoReview(task, autoReviewTaskLimit))
	}
	b.WriteString("Pending actions:\n\n")
	for i, call := range req.Calls {
		argsJSON, err := json.Marshal(call.Args)
		detail := string(argsJSON)
		if err != nil || detail == "" {
			detail = "{}"
		}
		fmt.Fprintf(&b, "%d. %s %s\n", i+1, strings.TrimSpace(call.ToolName), truncateRunesAutoReview(detail, autoReviewDetailLimit))
	}
	b.WriteString("\nReview the pending actions and call submit_decision exactly once with your final decision.")
	return b.String()
}

func truncateRunesAutoReview(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// autoReviewStaticallySafe reports whether every call in the request is
// known-safe read-only work that needs no grading. Anything uncertain goes
// to the reviewer instead.
func autoReviewStaticallySafe(req ApprovalRequest) bool {
	for _, call := range req.Calls {
		switch strings.TrimSpace(call.ToolName) {
		case "read":
			// The read tool confines paths to the working directory and
			// rejects symlinks pointing outside it.
		case "bash":
			command, _ := call.Args["command"].(string)
			if !bashCommandReadOnly(command) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// readOnlyBashBinaries are commands with no write or execute side effects.
var readOnlyBashBinaries = map[string]bool{
	"cat": true, "cd": true, "diff": true, "du": true, "echo": true,
	"file": true, "find": true, "grep": true, "head": true, "ls": true,
	"pwd": true, "rg": true, "stat": true, "tail": true, "wc": true,
	"which": true,
}

// readOnlyGitSubcommands are git invocations that only read.
var readOnlyGitSubcommands = map[string]bool{
	"diff": true, "log": true, "show": true, "status": true,
}

// findWriteFlags make find mutate or execute, disqualifying it from the
// static tier.
var findWriteFlags = map[string]bool{
	"-delete": true, "-exec": true, "-execdir": true, "-fprint": true,
	"-fprintf": true, "-ok": true, "-okdir": true,
}

// bashCommandReadOnly reports whether the command is provably read-only:
// only allowlisted read-only binaries over relative paths, with no
// redirections, substitutions, backgrounding, or absolute paths. Commands
// that fail any check are simply graded by the reviewer instead.
func bashCommandReadOnly(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	if strings.ContainsAny(command, "`><") || strings.Contains(command, "$(") {
		return false
	}
	for _, part := range strings.Split(command, "&&") {
		for _, alt := range strings.Split(part, "||") {
			if strings.Contains(alt, "&") {
				return false
			}
			for _, statement := range splitBashStatements(alt) {
				for _, segment := range strings.Split(statement, "|") {
					if strings.TrimSpace(segment) == "" {
						continue
					}
					if !bashSegmentReadOnly(segment) {
						return false
					}
				}
			}
		}
	}
	return true
}

func splitBashStatements(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '\n' })
}

// bashSegmentReadOnly checks one pipe-free command segment. Env assignments
// may prefix the binary; every argument must avoid variable expansion, home
// shortcuts, and absolute paths.
func bashSegmentReadOnly(segment string) bool {
	fields := strings.Fields(segment)
	i := 0
	for i < len(fields) && isEnvAssignment(fields[i]) {
		i++
	}
	if i >= len(fields) {
		return false
	}
	rest := fields[i:]
	binary := rest[0]
	if strings.ContainsAny(segment, "$~") {
		return false
	}
	for _, field := range rest {
		if strings.HasPrefix(field, "/") {
			return false
		}
	}
	switch binary {
	case "git":
		if len(rest) < 2 {
			return false
		}
		return readOnlyGitSubcommands[rest[1]]
	case "find":
		for _, field := range rest[1:] {
			if findWriteFlags[field] {
				return false
			}
		}
		return true
	default:
		return readOnlyBashBinaries[binary]
	}
}

func isEnvAssignment(field string) bool {
	name, _, ok := strings.Cut(field, "=")
	return ok && name != "" && isEnvName(name)
}

func isEnvName(s string) bool {
	for i, r := range s {
		if i == 0 {
			if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
				return false
			}
			continue
		}
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
