package main

// Pipe mode (`o --pipe`): a machine-readable NDJSON protocol over stdio for
// UI frontends (the macOS app, scripts, other agents). One process hosts one
// session; the frontend owns one process per window.
//
// Wire format — one JSON object per line.
//
// Commands (frontend -> o, stdin):
//
//	{"cmd":"prompt","text":"..."}   run one agent turn
//	{"cmd":"cancel"}                cancel the in-flight run
//
// Events (o -> frontend, stdout): the agent.Event stream (message_delta,
// thinking_delta, tool_call_detected/started/finished, compaction_*,
// run_finished, error), preceded by a single session_opened event that
// carries the session id, model, name, working directory, and — when
// resuming — the persisted message history so the UI can rebuild its view.
//
// stdout carries only NDJSON; diagnostics go to stderr.
//
// Pipe mode grants full tool access by default (no approval prompts); pass
// --allow-all-tools=false explicitly to require denial-free tools only.

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
	"github.com/ParthSareen/o/sessionstore"
)

// pipeCommand is one line of JSON on stdin from the frontend.
type pipeCommand struct {
	Cmd   string `json:"cmd"`
	Text  string `json:"text,omitempty"`
	Skill string `json:"skill,omitempty"` // activate a catalog skill for this turn ("/name" in the TUI)
}

// cmdMsg is a parsed command line or a read error.
type cmdMsg struct {
	cmd pipeCommand
	err error
}

// pipeEventSink serializes agent events as NDJSON on stdout. Deltas are
// buffered and flushed on a short interval; lifecycle events flush
// immediately so tool/approval state never lags behind the model stream.
type pipeEventSink struct {
	mu    sync.Mutex
	enc   *json.Encoder
	w     *bufio.Writer
	dirty bool
}

func newPipeEventSink(w io.Writer) *pipeEventSink {
	bw := bufio.NewWriterSize(w, 64*1024)
	return &pipeEventSink{enc: json.NewEncoder(bw), w: bw}
}

// startFlushLoop periodically flushes buffered delta events until ctx ends.
func (s *pipeEventSink) startFlushLoop(ctx context.Context) {
	go func() {
		t := time.NewTicker(15 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				s.flush()
				return
			case <-t.C:
				s.flush()
			}
		}
	}()
}

func (s *pipeEventSink) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirty {
		_ = s.w.Flush()
		s.dirty = false
	}
}

// FlushNow forces any buffered events out (used before exit).
func (s *pipeEventSink) FlushNow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.w.Flush()
	s.dirty = false
}

func (s *pipeEventSink) Emit(ev coreagent.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enc.Encode(ev); err != nil {
		return err
	}
	switch ev.Type {
	case coreagent.EventMessageDelta, coreagent.EventThinkingDelta, coreagent.EventCompactionProgress:
		s.dirty = true
	default:
		return s.w.Flush()
	}
	return nil
}

var _ coreagent.EventSink = (*pipeEventSink)(nil)

// pipeRunner hosts one agent session behind the NDJSON protocol.
type pipeRunner struct {
	opts         *agentTUIOptions
	store        *sessionstore.Store
	session      *coreagent.Session
	registry     *coreagent.Registry
	sink         *pipeEventSink
	stderr       io.Writer
	chatID       string
	systemPrompt string
	history      []api.Message
}

// runPipe is the entry for a fresh session (`o --pipe <model> [prompt]`).
// initialPrompt, when non-empty, runs as the first turn.
func runPipe(ctx context.Context, client *api.Client, opts *agentTUIOptions, store *sessionstore.Store, workingDir string, stdin io.Reader, stdout, stderr io.Writer, initialPrompt string) int {
	return runPipeSetup(ctx, client, opts, store, nil, workingDir, stdin, stdout, stderr, initialPrompt)
}

// runPipeResume is the entry for a resumed session (`o --pipe --resume-id <id>`).
func runPipeResume(ctx context.Context, client *api.Client, opts *agentTUIOptions, store *sessionstore.Store, sess *sessionstore.Session, workingDir string, stdin io.Reader, stdout, stderr io.Writer, initialPrompt string) int {
	return runPipeSetup(ctx, client, opts, store, sess, workingDir, stdin, stdout, stderr, initialPrompt)
}

func runPipeSetup(ctx context.Context, client *api.Client, opts *agentTUIOptions, store *sessionstore.Store, sess *sessionstore.Session, workingDir string, stdin io.Reader, stdout, stderr io.Writer, initialPrompt string) int {
	catalog, err := coreagent.LoadDefaultSkills(workingDir)
	if err != nil {
		fmt.Fprintf(stderr, "error: load agent skills: %v\n", err)
		return 1
	}
	for _, d := range catalog.Diagnostics() {
		fmt.Fprintf(stderr, "warning: ignored invalid agent skill: %v\n", d)
	}

	registry := agentToolsRegistryWithRLM(ctx, client, opts.Model, catalog, opts.RLM, nil)

	systemPrompt := agentSystemPromptWithWorkingDir(
		opts.Model, opts.System,
		agentSkillSystemContext(catalog, registry, opts.ToolsDisabled),
		workingDir,
	)

	return runPipeSession(ctx, client, opts, store, catalog, registry, systemPrompt, sess, workingDir, stdin, stdout, stderr, initialPrompt)
}

// runPipeSession drives one pipe session against any ChatClient; split out
// so tests can run without a real server.
func runPipeSession(ctx context.Context, client coreagent.ChatClient, opts *agentTUIOptions, store *sessionstore.Store, catalog *coreagent.SkillCatalog, registry *coreagent.Registry, systemPrompt string, sess *sessionstore.Session, workingDir string, stdin io.Reader, stdout, stderr io.Writer, initialPrompt string) int {
	sink := newPipeEventSink(stdout)

	// Fresh sessions are lazy: no store row until the first prompt lands, so
	// opening a window and walking away doesn't litter the sidebar with
	// empty sessions.
	var chatID, name string
	var history []api.Message
	if sess != nil {
		chatID, name, history = sess.ID, sess.Name, sess.Messages
	}

	// Pipe mode grants full tool access by default; approval prompts have no
	// channel back to the frontend, so a required approval would stall the run.
	state := &coreagent.ApprovalState{}
	if opts.AllowAllTools {
		state.GrantAll()
	}

	session := &coreagent.Session{
		Client:           client,
		EventSinks:       []coreagent.EventSink{sink},
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

	r := &pipeRunner{
		opts:         opts,
		store:        store,
		session:      session,
		registry:     registry,
		sink:         sink,
		stderr:       stderr,
		chatID:       chatID,
		systemPrompt: systemPrompt,
		history:      history,
	}

	var skills []coreagent.SkillInfo
	if catalog != nil {
		for _, s := range catalog.List() {
			skills = append(skills, coreagent.SkillInfo{Name: s.Name, Description: s.Description})
		}
	}

	sink.FlushNow()
	if err := sink.Emit(coreagent.Event{
		Type:       coreagent.EventSessionOpened,
		ChatID:     chatID,
		Model:      opts.Model,
		Name:       name,
		WorkingDir: workingDir,
		Messages:   history,
		Skills:     skills,
	}); err != nil {
		fmt.Fprintf(stderr, "error: write session_opened: %v\n", err)
		return 1
	}
	sink.startFlushLoop(ctx)
	defer sink.FlushNow()

	return r.commandLoop(ctx, stdin, initialPrompt)
}

// commandLoop reads NDJSON commands from stdin until EOF or shutdown. A
// non-empty initialPrompt runs as the first turn before the loop.
func (r *pipeRunner) commandLoop(ctx context.Context, stdin io.Reader, initialPrompt string) int {
	cmds := make(chan cmdMsg)
	go scanPipeCommands(stdin, cmds)

	if text := strings.TrimSpace(initialPrompt); text != "" {
		if !r.runTurn(ctx, cmds, pipeCommand{Cmd: "prompt", Text: text}) {
			return 0
		}
	}

	for {
		select {
		case <-ctx.Done():
			return 1
		case m, ok := <-cmds:
			if !ok {
				return 0 // stdin closed: frontend is gone
			}
			if m.err != nil {
				r.emitError("invalid command: " + m.err.Error())
				continue
			}
			switch m.cmd.Cmd {
			case "prompt":
				if !r.runTurn(ctx, cmds, m.cmd) {
					return 0
				}
			case "cancel":
				// no run in flight; nothing to do
			case "inspect":
				r.emitInspect()
			default:
				r.emitError(fmt.Sprintf("unknown command %q (want prompt|cancel|inspect)", m.cmd.Cmd))
			}
		}
	}
}

// scanPipeCommands parses stdin line by line; closes cmds on EOF.
func scanPipeCommands(stdin io.Reader, cmds chan<- cmdMsg) {
	defer close(cmds)
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var c pipeCommand
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			cmds <- cmdMsg{err: err}
			continue
		}
		cmds <- cmdMsg{cmd: c}
	}
	if err := scanner.Err(); err != nil {
		cmds <- cmdMsg{err: err}
	}
}

// runTurn executes one agent turn, honoring cancel commands that arrive while
// the run is in flight. Returns false when stdin reached EOF (or the process
// is shutting down) — the caller should exit after the turn's events flush.
func (r *pipeRunner) runTurn(ctx context.Context, cmds chan cmdMsg, c pipeCommand) bool {
	text := strings.TrimSpace(c.Text)
	skill := strings.TrimSpace(c.Skill)
	if text == "" && skill == "" {
		r.emitError("empty prompt")
		return true
	}
	if text == "" {
		// Skill-only invocation: leave a visible user message, like the TUI.
		text = "/" + skill
	}

	// Lazily create the store row for fresh sessions on their first prompt.
	if r.chatID == "" && r.store != nil {
		created, err := r.store.CreateSession(r.opts.Model, r.session.WorkingDir, r.systemPrompt, r.opts.Name)
		if err != nil {
			fmt.Fprintf(r.stderr, "warning: could not create session: %v\n", err)
		} else {
			r.chatID = created.ID
			_ = r.sink.Emit(coreagent.Event{
				Type:   coreagent.EventSessionAssigned,
				ChatID: r.chatID,
				Name:   created.Name,
			})
		}
	}

	if r.store != nil && r.chatID != "" {
		if err := r.store.AddPrompt(r.chatID, text); err != nil {
			fmt.Fprintf(r.stderr, "warning: could not save prompt history: %v\n", err)
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type turnResult struct {
		res *coreagent.RunResult
		err error
	}
	done := make(chan turnResult, 1)
	go func() {
		res, err := r.session.Run(runCtx, coreagent.RunOptions{
			ChatID:       r.chatID,
			Model:        r.opts.Model,
			SystemPrompt: r.systemPrompt,
			Messages:     r.history,
			NewMessages:  []api.Message{{Role: "user", Content: text}},
			SkillName:    skill,
			Format:       r.opts.Format,
			Options:      r.opts.Options,
			Think:        r.opts.Think,
			KeepAlive:    r.opts.KeepAlive,
		})
		done <- turnResult{res, err}
	}()

	eof := false
	var result turnResult
loop:
	for {
		select {
		case result = <-done:
			break loop
		case m, ok := <-cmds:
			if !ok {
				// stdin closed (script finished / frontend gone): do NOT
				// cancel — let the run finish so its messages persist and
				// buffered events flush, then exit cleanly.
				eof = true
				cmds = nil // disable the (now always-ready) closed channel
				continue
			}
			if m.err != nil {
				r.emitError("invalid command: " + m.err.Error())
				continue
			}
			switch m.cmd.Cmd {
			case "cancel":
				cancel()
			case "inspect":
				r.emitInspect()
			case "prompt":
				r.emitError("a run is already in progress; wait for run_finished or send cancel")
			default:
				r.emitError(fmt.Sprintf("unknown command %q (want prompt|cancel|inspect)", m.cmd.Cmd))
			}
		case <-ctx.Done():
			cancel()
			cmds = nil
		}
	}

	if result.res != nil && len(result.res.Messages) > 0 {
		r.history = append(r.history, result.res.Messages...)
		if r.store != nil && r.chatID != "" {
			if err := r.store.AppendMessages(r.chatID, result.res.Messages); err != nil {
				fmt.Fprintf(r.stderr, "warning: could not save session: %v\n", err)
			}
		}
	}
	if result.err != nil && runCtx.Err() == nil {
		// The session already emitted an error event for the failure; log for
		// operators but keep the pipe clean and the process alive.
		fmt.Fprintf(r.stderr, "error: %v\n", result.err)
	}
	return !eof && ctx.Err() == nil
}

func (r *pipeRunner) emitError(msg string) {
	_ = r.sink.Emit(coreagent.Event{Type: coreagent.EventError, Error: msg})
}

// emitInspect reports the session's system prompt, registered tools, and
// current message history (what the TUI's /prompt command shows).
func (r *pipeRunner) emitInspect() {
	var tools []coreagent.ToolInfo
	for _, t := range r.registry.Tools() {
		tools = append(tools, coreagent.ToolInfo{Name: t.Function.Name, Description: t.Function.Description})
	}
	_ = r.sink.Emit(coreagent.Event{
		Type:       coreagent.EventInspect,
		ChatID:     r.chatID,
		Model:      r.opts.Model,
		WorkingDir: r.session.WorkingDir,
		System:     r.systemPrompt,
		Tools:      tools,
		Messages:   r.history,
	})
}

// applyPipeDefaults applies pipe-mode defaults: full tool access and RLM
// (subagents) are on unless the user passed those flags explicitly.
func applyPipeDefaults(fs *flag.FlagSet, opts *cliOptions) {
	seen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if !seen["allow-all-tools"] {
		opts.allowAllTools = true
	}
	if !seen["rlm"] {
		opts.rlm = true
	}
}
