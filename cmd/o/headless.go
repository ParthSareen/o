package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/sessionstore"
	"github.com/ParthSareen/o/api"
)

// headlessPrompter answers approval prompts without a TTY. With allowAll it
// approves everything; otherwise it denies with a reason the model can read
// and relay.
type headlessPrompter struct{ allowAll bool }

func (p headlessPrompter) PromptApproval(context.Context, coreagent.ApprovalRequest) (coreagent.Approval, error) {
	if p.allowAll {
		return coreagent.Approval{Allow: true, AllowAll: true}, nil
	}
	return coreagent.Approval{
		Allow:  false,
		Reason: "Tool execution denied: running headless without --allow-all-tools.",
	}, nil
}

// headlessRenderer consumes agent events: assistant content goes to stdout
// (pipe-friendly), thinking/tool activity/compaction to stderr.
type headlessRenderer struct {
	stdout io.Writer
	stderr io.Writer
	// thinking mirrors deltas to stderr
	thinking bool
	// inThinking tracks whether a stderr thinking line is open
	inThinking bool
	sawError   bool
	runStatus  coreagent.RunStatus
}

func newHeadlessRenderer(stdout, stderr io.Writer, showThinking bool) *headlessRenderer {
	return &headlessRenderer{stdout: stdout, stderr: stderr, thinking: showThinking}
}

func (r *headlessRenderer) Emit(ev coreagent.Event) error {
	switch ev.Type {
	case coreagent.EventMessageDelta:
		r.closeThinking()
		fmt.Fprint(r.stdout, ev.Content)
	case coreagent.EventThinkingDelta:
		if r.thinking {
			fmt.Fprint(r.stderr, ev.Thinking)
			r.inThinking = true
		}
	case coreagent.EventToolStarted:
		r.closeThinking()
		fmt.Fprintf(r.stderr, "→ %s %s\n", ev.ToolName, shortArgs(ev.Args))
	case coreagent.EventToolFinished:
		r.closeThinking()
		switch ev.ToolStatus {
		case coreagent.ToolStatusFailed:
			fmt.Fprintf(r.stderr, "✗ %s failed: %s\n", ev.ToolName, oneLine(ev.Error))
		case coreagent.ToolStatusDenied:
			fmt.Fprintf(r.stderr, "✗ %s denied: %s\n", ev.ToolName, oneLine(ev.Error))
		default:
			fmt.Fprintf(r.stderr, "✓ %s done\n", ev.ToolName)
		}
	case coreagent.EventCompactionStarted:
		fmt.Fprintf(r.stderr, "… compacting context (%s)\n", ev.CompactionTrigger)
	case coreagent.EventBackgroundTasks:
		r.closeThinking()
		// Content is pre-formatted (one line per task plus failure tails);
		// print as-is like other lifecycle notes.
		fmt.Fprintf(r.stderr, "%s\n", ev.Content)
	case coreagent.EventError:
		r.closeThinking()
		r.sawError = true
		fmt.Fprintf(r.stderr, "error: %s\n", ev.Error)
	case coreagent.EventRunFinished:
		r.runStatus = ev.Status
	}
	return nil
}

func (r *headlessRenderer) closeThinking() {
	if r.inThinking {
		fmt.Fprintln(r.stderr)
		r.inThinking = false
	}
}

func shortArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	s := string(raw)
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:197] + "..."
	}
	return s
}

// runHeadless runs one agent turn non-interactively and returns a process
// exit code: 0 on a finished run, 1 on error/denial/cancel.
func runHeadless(ctx context.Context, client *api.Client, opts *agentTUIOptions, store *sessionstore.Store, prompt, workingDir string, stdout, stderr io.Writer) int {
	catalog, err := coreagent.LoadDefaultSkills(workingDir)
	if err != nil {
		fmt.Fprintf(stderr, "error: load agent skills: %v\n", err)
		return 1
	}
	for _, d := range catalog.Diagnostics() {
		fmt.Fprintf(stderr, "warning: ignored invalid agent skill: %v\n", d)
	}

	registry := agentToolsRegistryWithRLM(ctx, client, opts.Model, catalog, opts.RLM, nil)
	if len(registry.Names()) > 0 {
		fmt.Fprintf(stderr, "tools: %s\n", strings.Join(registry.Names(), ", "))
	}

	systemPrompt := agentSystemPromptWithWorkingDir(
		opts.Model, opts.System,
		agentSkillSystemContext(catalog, registry, opts.ToolsDisabled),
		workingDir,
	)

	return runHeadlessSession(ctx, client, opts, store, catalog, registry, systemPrompt, prompt, workingDir, stdout, stderr)
}

// runHeadlessSession drives one agent session against any ChatClient; split
// out so tests can run without a real server.
func runHeadlessSession(ctx context.Context, client coreagent.ChatClient, opts *agentTUIOptions, store *sessionstore.Store, catalog *coreagent.SkillCatalog, registry *coreagent.Registry, systemPrompt, prompt, workingDir string, stdout, stderr io.Writer) int {
	state := &coreagent.ApprovalState{}
	if opts.AllowAllTools {
		state.GrantAll()
	}

	renderer := newHeadlessRenderer(stdout, stderr, true)
	session := &coreagent.Session{
		Client:           client,
		EventSinks:       []coreagent.EventSink{coreagent.EventSinkFunc(renderer.Emit)},
		Tools:            registry,
		Skills:           catalog,
		DisableTools:     opts.ToolsDisabled,
		ApprovalPrompter: headlessPrompter{allowAll: opts.AllowAllTools},
		ApprovalState:    state,
		WorkingDir:       workingDir,
		Compactor: &coreagent.SimpleCompactor{
			Client:  client,
			Options: coreagent.CompactionOptions{ContextWindowTokens: opts.ContextWindowTokens},
		},
		Background: registry.BackgroundSource(),
	}

	// Create a session in the store for headless mode so the conversation
	// is persisted and can be resumed later.
	var chatID string
	if store != nil {
		sess, err := store.CreateSession(opts.Model, workingDir, systemPrompt, opts.Name)
		if err != nil {
			fmt.Fprintf(stderr, "warning: could not create session: %v\n", err)
		} else {
			chatID = sess.ID
			if err := store.AddPrompt(chatID, prompt); err != nil {
				fmt.Fprintf(stderr, "warning: could not save prompt history: %v\n", err)
			}
		}
	}

	result, err := session.Run(ctx, coreagent.RunOptions{
		ChatID:       chatID,
		Model:        opts.Model,
		SystemPrompt: systemPrompt,
		NewMessages:  []api.Message{{Role: "user", Content: prompt}},
		Format:       opts.Format,
		Options:      opts.Options,
		Think:        opts.Think,
		KeepAlive:    opts.KeepAlive,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprintln(stdout)

	// Persist the conversation to the session store.
	if store != nil && chatID != "" && result != nil {
		if err := store.AppendMessages(chatID, result.Messages); err != nil {
			fmt.Fprintf(stderr, "warning: could not save session: %v\n", err)
		}
	}

	if renderer.sawError || renderer.runStatus == coreagent.RunStatusDenied || renderer.runStatus == coreagent.RunStatusCanceled {
		return 1
	}
	return 0
}

// runHeadlessResume runs one headless turn against a previously saved session,
// loading its message history and appending the new turn's result.
func runHeadlessResume(ctx context.Context, client *api.Client, opts *agentTUIOptions, store *sessionstore.Store, sess *sessionstore.Session, prompt, workingDir string, stdout, stderr io.Writer) int {
	catalog, err := coreagent.LoadDefaultSkills(workingDir)
	if err != nil {
		fmt.Fprintf(stderr, "error: load agent skills: %v\n", err)
		return 1
	}
	for _, d := range catalog.Diagnostics() {
		fmt.Fprintf(stderr, "warning: ignored invalid agent skill: %v\n", d)
	}

	registry := agentToolsRegistryWithRLM(ctx, client, opts.Model, catalog, opts.RLM, nil)
	if len(registry.Names()) > 0 {
		fmt.Fprintf(stderr, "tools: %s\n", strings.Join(registry.Names(), ", "))
	}

	systemPrompt := agentSystemPromptWithWorkingDir(
		opts.Model, opts.System,
		agentSkillSystemContext(catalog, registry, opts.ToolsDisabled),
		workingDir,
	)

	state := &coreagent.ApprovalState{}
	if opts.AllowAllTools {
		state.GrantAll()
	}

	renderer := newHeadlessRenderer(stdout, stderr, true)
	session := &coreagent.Session{
		Client:           client,
		EventSinks:       []coreagent.EventSink{coreagent.EventSinkFunc(renderer.Emit)},
		Tools:            registry,
		Skills:           catalog,
		DisableTools:     opts.ToolsDisabled,
		ApprovalPrompter: headlessPrompter{allowAll: opts.AllowAllTools},
		ApprovalState:    state,
		WorkingDir:       workingDir,
		Compactor: &coreagent.SimpleCompactor{
			Client:  client,
			Options: coreagent.CompactionOptions{ContextWindowTokens: opts.ContextWindowTokens},
		},
		Background: registry.BackgroundSource(),
	}

	if store != nil {
		if err := store.AddPrompt(sess.ID, prompt); err != nil {
			fmt.Fprintf(stderr, "warning: could not save prompt history: %v\n", err)
		}
	}

	result, err := session.Run(ctx, coreagent.RunOptions{
		ChatID:       sess.ID,
		Model:        opts.Model,
		SystemPrompt: systemPrompt,
		Messages:     sess.Messages,
		NewMessages:  []api.Message{{Role: "user", Content: prompt}},
		Format:       opts.Format,
		Options:      opts.Options,
		Think:        opts.Think,
		KeepAlive:    opts.KeepAlive,
	})
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	fmt.Fprintln(stdout)

	if store != nil && result != nil {
		if err := store.AppendMessages(sess.ID, result.Messages[len(sess.Messages):]); err != nil {
			fmt.Fprintf(stderr, "warning: could not save session: %v\n", err)
		}
	}

	if renderer.sawError || renderer.runStatus == coreagent.RunStatusDenied || renderer.runStatus == coreagent.RunStatusCanceled {
		return 1
	}
	return 0
}
