package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	coreagent "github.com/ParthSareen/o/agent"
)

// pipeApprovalReply is one parsed approval command from the frontend.
type pipeApprovalReply struct {
	ApprovalID string
	Allow      bool
	Reason     string
}

// approvalInactivityTimeout bounds one pending approval: nothing here can
// authorize a tool except an explicit frontend reply.
const approvalInactivityTimeout = 10 * time.Minute

// pipeApprovalPrompter asks the pipe frontend with an approval_requested
// event and waits for the correlated approval command reply, persisting the
// pending request and the settled decision (journal may be nil when the
// store is unavailable). Disconnect, ctx cancel, and timeout deny; they
// never imply permission.
type pipeApprovalPrompter struct {
	sink    *pipeEventSink
	journal coreagent.RunJournal
	// mu guards pending; replies carries queued frontend replies.
	mu      sync.Mutex
	pending map[string]string // approvalID -> runID
	replies chan pipeApprovalReply
}

func newPipeApprovalPrompter(sink *pipeEventSink, journal coreagent.RunJournal) *pipeApprovalPrompter {
	return &pipeApprovalPrompter{
		sink:    sink,
		journal: journal,
		pending: map[string]string{},
		replies: make(chan pipeApprovalReply, 16),
	}
}

// Deliver routes an approval command to a waiting prompter. It reports false
// for a stale reply (no matching pending approval) or a full queue; stale
// replies never authorize anything.
func (p *pipeApprovalPrompter) Deliver(reply pipeApprovalReply) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.pending[reply.ApprovalID]; !ok {
		return false
	}
	select {
	case p.replies <- reply:
		return true
	default:
		return false
	}
}

// PromptApproval implements coreagent.ApprovalPrompter.
func (p *pipeApprovalPrompter) PromptApproval(ctx context.Context, req coreagent.ApprovalRequest) (coreagent.Approval, error) {
	if len(req.Calls) == 0 {
		return coreagent.Approval{Allow: true}, nil
	}
	approvalID := newApprovalID()
	callsJSON, err := json.Marshal(req.Calls)
	if err != nil {
		callsJSON = []byte("[]")
	}
	if p.journal != nil {
		// The pending request commits before anything it covers can run.
		if err := p.journal.RecordApprovalRequested(coreagent.ApprovalRecord{
			ApprovalID: approvalID,
			RunID:      req.RunID,
			Calls:      string(callsJSON),
		}); err != nil {
			return coreagent.Approval{Allow: false, Reason: "could not persist the approval request; refusing to run without a durable record"}, nil
		}
	}
	p.mu.Lock()
	p.pending[approvalID] = req.RunID
	p.mu.Unlock()
	_ = p.sink.Emit(coreagent.Event{
		Type:          coreagent.EventApprovalRequested,
		RunID:         req.RunID,
		ApprovalID:    approvalID,
		ApprovalCalls: req.Calls,
	})

	timeout := time.NewTimer(approvalInactivityTimeout)
	defer timeout.Stop()

	for {
		select {
		case <-ctx.Done():
			// The frontend is gone or the run was canceled: settle expired.
			p.settle(approvalID, coreagent.ApprovalDecision{ApprovalID: approvalID, Expired: true, Reason: "run ended before a decision arrived"})
			return coreagent.Approval{Allow: false, Reason: "approval request expired without a reply; disconnect and timeout never imply permission"}, nil
		case <-timeout.C:
			p.settle(approvalID, coreagent.ApprovalDecision{ApprovalID: approvalID, Expired: true, Reason: "approval window elapsed"})
			return coreagent.Approval{Allow: false, Reason: "approval request expired without a reply; timeout never implies permission"}, nil
		case reply := <-p.replies:
			p.mu.Lock()
			_, isPending := p.pending[reply.ApprovalID]
			if !isPending || reply.ApprovalID != approvalID {
				p.mu.Unlock()
				_ = p.sink.Emit(coreagent.Event{Type: coreagent.EventError, ApprovalID: reply.ApprovalID, Error: fmt.Sprintf("stale approval reply %q: no matching pending approval; nothing was authorized", reply.ApprovalID)})
				continue
			}
			delete(p.pending, reply.ApprovalID)
			p.mu.Unlock()

			decision := coreagent.ApprovalDecision{ApprovalID: approvalID, Approved: reply.Allow, Reason: reply.Reason}
			if p.journal != nil {
				// The decision commits before execution continues.
				if err := p.journal.RecordApprovalDecision(decision); err != nil {
					return coreagent.Approval{Allow: false, Reason: fmt.Sprintf("could not persist the approval decision: %v", err)}, nil
				}
			}
			_ = p.sink.Emit(coreagent.Event{Type: coreagent.EventApprovalDecided, ApprovalID: approvalID, Content: approvalDecisionWireStatus(reply.Allow)})
			if reply.Allow {
				return coreagent.Approval{Allow: true}, nil
			}
			reason := reply.Reason
			if reason == "" {
				reason = "denied by the pipe frontend"
			}
			return coreagent.Approval{Allow: false, Reason: reason}, nil
		}
	}
}

// settle persists an expiry decision.
func (p *pipeApprovalPrompter) settle(approvalID string, decision coreagent.ApprovalDecision) {
	p.mu.Lock()
	_, wasPending := p.pending[approvalID]
	delete(p.pending, approvalID)
	p.mu.Unlock()
	if !wasPending {
		return
	}
	if p.journal != nil {
		_ = p.journal.RecordApprovalDecision(decision)
	}
	_ = p.sink.Emit(coreagent.Event{Type: coreagent.EventApprovalDecided, ApprovalID: approvalID, Content: "expired"})
}

func approvalDecisionWireStatus(approved bool) string {
	if approved {
		return "approved"
	}
	return "denied"
}

func newApprovalID() string {
	return uuid.NewString()
}
