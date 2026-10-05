package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/ParthSareen/o/api"
)

// RunJournal is the durable-execution boundary. Implementations persist run
// admission, per-tool intent and outcome, and the committed terminal state in
// separate transactions keyed by stable correlation IDs, so a recovered run's
// state never depends on a transaction staying open across a model request or
// an external tool invocation.
//
// A RunJournal is optional: Session with a nil Journal behaves exactly as it
// did before durable execution existed. The sessionstore.Store implementation
// is the production one; tests supply fakes.
type RunJournal interface {
	// AdmitRun durably registers a prompt before any model or tool work
	// starts, and returns either a fresh admission or, for a request ID
	// already committed for this session, the existing run's identity.
	// AdmitRun also classifies state left behind by an interrupted process
	// (runs without a terminal commit, tool intents without outcomes) and
	// reports which prior tool calls must not be blindly re-executed.
	AdmitRun(admission RunAdmission) (RunAdmissionResult, error)
	// RecordToolIntent persists the intent to invoke a tool. It must commit
	// before the tool runs.
	RecordToolIntent(intent ToolIntent) error
	// RecordToolOutcome persists a tool call's settled outcome.
	RecordToolOutcome(outcome ToolOutcome) error
	// RecordCompaction persists a compaction fact (trigger, summary) while
	// the run continues; raw history stays append-only around it.
	RecordCompaction(record CompactionRecord) error
	// RecordApprovalRequested persists a pending approval before any tool
	// covered by it can run.
	RecordApprovalRequested(record ApprovalRecord) error
	// RecordApprovalDecision persists the decision for a pending approval
	// before execution continues. Rejects a second decision for an already
	// settled approval.
	RecordApprovalDecision(decision ApprovalDecision) error
	// RecordRawSegment appends one span of the run's own messages to the
	// append-only record at flush time (run end, and around each
	// compaction), so compaction watermarks can reference what they cover.
	RecordRawSegment(runID string, segment []api.Message) error
	// CommitRun commits the terminal run state and the message projection
	// History in one transaction, and releases the session's run lock.
	// Terminal events (run_finished, then run_committed) must only be
	// published after CommitRun succeeds.
	CommitRun(terminal RunTerminal) (RunCommit, error)
}

// RunAdmission describes one admitted prompt. RequestID is empty for legacy
// callers, which get no retry-deduplication guarantee.
type RunAdmission struct {
	ChatID       string
	RequestID    string
	Model        string
	SystemPrompt string
	Skill        string
	Format       string
	Options      map[string]any
	Think        *api.ThinkValue
	// Prompt is the user-visible input (skill-only invocations use the
	// "/name" form).
	Prompt string
}

// BlockedTool is a prior attempt's tool call whose outcome is unknown after
// an interruption. Unless the tool is classified replay-safe, re-execution is
// refused for the matching (tool name, args) until reconciled.
type BlockedTool struct {
	// ArgsKey matches ToolArgsKey(name, args) for the prior call.
	ArgsKey     string
	Name        string
	Reason      string
	PriorRunID  string
	PriorCallID string
}

// RunAdmissionResult is the committed admission outcome.
type RunAdmissionResult struct {
	// RunID is the run ID every event of this run carries. On a replay it is
	// the original run's ID.
	RunID string
	// Attempt is 1 for a fresh admission, higher for a retry of a
	// failed/canceled/interrupted attempt under the same request ID.
	Attempt int
	// Replayed reports that the exact request was already committed; the
	// caller must not run the model or tools again.
	Replayed bool
	// Status is the replayed run's terminal status.
	Status string
	// Recovered carries human-readable lines about state this admission
	// classified from an earlier interrupted process.
	Recovered []string
	// BlockedTools lists prior tool calls whose re-execution is refused this
	// attempt.
	BlockedTools []BlockedTool
}

// ToolStatus values for journal rows and outcomes. They overlap with the
// ToolStatus event values where the meanings coincide.
const (
	ToolJournalPending  = "pending"
	ToolJournalDone     = "done"
	ToolJournalFailed   = "failed"
	ToolJournalDenied   = "denied"
	ToolJournalDisabled = "disabled"
	ToolJournalSkipped  = "skipped"
	ToolJournalBlocked  = "blocked"
	ToolJournalUnknown  = "unknown"
)

// Run status values for journal rows.
const (
	RunStatusAdmitted    = "admitted"
	RunStatusRunning     = "running"
	RunStatusCompleted   = "done"
	RunStatusFailed      = "failed"
	RunStatusInterrupted = "interrupted"
)

// ToolIntent records the intent to invoke a tool.
type ToolIntent struct {
	RunID string
	// ToolCallID is the call's ID as issued by the model, or a synthetic
	// nested-call ID.
	ToolCallID string
	// ParentToolCallID correlates nested calls (e.g. codemode) to the
	// composing call.
	ParentToolCallID string
	Name             string
	Args             map[string]any
	// ReplaySafe must be true only for tools that explicitly declare their
	// invocation safe to re-run with an unknown earlier outcome.
	ReplaySafe bool
}

// ToolOutcome is a settled tool-call outcome.
type ToolOutcome struct {
	RunID      string
	ToolCallID string
	Status     string
	Result     string
	Err        string
}

// CompactionRecord is one compaction fact. Compaction rewrites the model
// context projection, never the append-only record. SessionID resolves the
// owning session when the compaction runs outside any run (manual); RunID
// correlates it to a run when set.
type CompactionRecord struct {
	RunID     string
	SessionID string
	Trigger   string
	Summary   string
}

// ApprovalRecord is a pending approval request persisted before any tool it
// covers can run.
type ApprovalRecord struct {
	ApprovalID string
	RunID      string
	// Calls is the JSON-encoded set of correlated tool calls.
	Calls string
}

// ApprovalDecision settles a pending approval. Decision persistence must
// happen before the tool executes.
type ApprovalDecision struct {
	ApprovalID string
	Approved   bool
	Expired    bool
	Reason     string
}

// RunTerminal is the terminal commit for a run. History is the full message
// projection (prior + new); raw segments have already been journaled at
// flush time via RecordRawSegment.
type RunTerminal struct {
	RunID  string
	ChatID string
	// Status is one of the committed terminal statuses: done, denied,
	// canceled, or failed.
	Status string
	Err    string
	// History, when non-empty, is synced into the stored message projection
	// (append or replace on drift, matching SyncMessages semantics).
	History []api.Message
}

// RunCommit is the committed terminal outcome.
type RunCommit struct {
	// Seq is the committed session_event sequence for run_committed.
	Seq int64
}

// RunConflictError reports a request ID reused with different canonical
// input. The existing run is left untouched.
type RunConflictError struct {
	RequestID     string
	ExistingRunID string
	Detail        string
}

func (e *RunConflictError) Error() string {
	return fmt.Sprintf("request conflict: request id %q already used for run %s (%s)", e.RequestID, e.ExistingRunID, e.Detail)
}

// RunBusyError reports that a run on this session is already in flight in
// another process that is still alive. Sessions have one execution owner;
// a second writer is rejected rather than interleaved.
type RunBusyError struct {
	SessionID string
	Owner     string
}

func (e *RunBusyError) Error() string {
	return fmt.Sprintf("session %s has a run in progress in another process (owner %s)", e.SessionID, e.Owner)
}

// ErrApprovalSettled is returned when a decision arrives for an approval
// that already has one.
var ErrApprovalSettled = fmt.Errorf("approval already settled")

// ReplaySafeTool is implemented by tools whose invocation is safe to re-run
// even when an earlier invocation's outcome is unknown (read-only queries
// where the call itself cannot change the world). Every tool that does not
// implement this is treated as unsafe.
type ReplaySafeTool interface {
	ReplaySafe() bool
}

// NestedExecutor executes one tool call nested inside a composing call (e.g.
// codemode). Nested calls go through the session's normal authorization and
// journaling path with parent correlation, so a script gets no bypass.
type NestedExecutor interface {
	ExecuteNested(ctx context.Context, parentToolCallID, toolName string, args map[string]any) (ToolResult, error)
}

// NestedExecutorSetter is implemented by composing tools that receive their
// executor for the current run.
type NestedExecutorSetter interface {
	SetNestedExecutor(executor NestedExecutor)
}

// ToolArgsKey derives the stable (tool name, canonical args) key used to
// correlate a re-admitted attempt's calls with a prior attempt's unresolved
// intents. Encoding/json marshals maps with sorted keys, so two calls with
// equal arguments produce the same key.
func ToolArgsKey(name string, args map[string]any) string {
	blob, err := json.Marshal(map[string]any{"name": name, "args": args})
	if err != nil {
		// Non-marshalable args can never match a stored row; the name alone
		// still gives a deterministic key.
		blob = []byte("name:" + name)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

func journalRunStatus(status RunStatus) string {
	if status == "" {
		return RunStatusFailed
	}
	return string(status)
}
