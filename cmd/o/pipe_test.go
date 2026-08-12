package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
	"github.com/ParthSareen/o/sessionstore"
)

// pipeHarness runs a pipe session against a fake client, with stdin under
// test control and stdout collected for event assertions.
type pipeHarness struct {
	t       *testing.T
	stdin   *io.PipeWriter
	outBuf  *lockedBuffer
	errBuf  *lockedBuffer
	codeCh  chan int
	writeMu sync.Mutex
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) events(t *testing.T) []coreagent.Event {
	t.Helper()
	var evs []coreagent.Event
	for _, line := range strings.Split(strings.TrimSpace(b.String()), "\n") {
		if line == "" {
			continue
		}
		var ev coreagent.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad event line %q: %v", line, err)
		}
		evs = append(evs, ev)
	}
	return evs
}

// waitFor polls stdout until pred matches an event or the deadline passes.
func (h *pipeHarness) waitFor(pred func(coreagent.Event) bool) coreagent.Event {
	return h.waitForAfter(0, pred)
}

// waitForAfter waits for a matching event at or after the given index in the
// accumulated buffer — so a run_finished from an earlier turn can't satisfy
// a wait begun in a later turn.
func (h *pipeHarness) waitForAfter(offset int, pred func(coreagent.Event) bool) coreagent.Event {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		evs := h.outBuf.events(h.t)
		if offset < len(evs) {
			for _, ev := range evs[offset:] {
				if pred(ev) {
					return ev
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for event; got %s", h.outBuf.String())
	return coreagent.Event{}
}

func (h *pipeHarness) send(t *testing.T, line string) {
	t.Helper()
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if _, err := h.stdin.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
}

func startPipe(t *testing.T, fc coreagent.ChatClient, registry *coreagent.Registry) *pipeHarness {
	t.Helper()
	inR, inW := io.Pipe()
	h := &pipeHarness{
		t:      t,
		stdin:  inW,
		outBuf: &lockedBuffer{},
		errBuf: &lockedBuffer{},
		codeCh: make(chan int, 1),
	}
	opts := &agentTUIOptions{Model: "test-model", AllowAllTools: true, Options: map[string]any{}}
	go func() {
		h.codeCh <- runPipeSession(context.Background(), fc, opts, nil, nil, registry, "system prompt", nil, t.TempDir(), inR, h.outBuf, h.errBuf, "")
	}()
	h.waitFor(func(ev coreagent.Event) bool { return ev.Type == coreagent.EventSessionOpened })
	return h
}

func (h *pipeHarness) promptAndWait(t *testing.T, text string) []coreagent.Event {
	t.Helper()
	before := len(h.outBuf.events(t))
	h.send(t, `{"cmd":"prompt","text":`+jsonString(text)+`}`)
	h.waitForAfter(before, func(ev coreagent.Event) bool { return ev.Type == coreagent.EventRunFinished })
	return h.outBuf.events(t)[before:]
}

func (h *pipeHarness) close(t *testing.T) int {
	t.Helper()
	if err := h.stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	select {
	case code := <-h.codeCh:
		return code
	case <-time.After(5 * time.Second):
		t.Fatal("pipe session did not exit after stdin EOF")
		return -1
	}
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func resumedSession() *sessionstore.Session {
	return &sessionstore.Session{
		ID:    "sess-1",
		Model: "test-model",
		Name:  "named",
		Messages: []api.Message{
			{Role: "user", Content: "before"},
			{Role: "assistant", Content: "earlier answer"},
		},
	}
}

func TestPipeSessionOpenedFirst(t *testing.T) {
	h := startPipe(t, &fakeClient{}, &coreagent.Registry{})
	opened := h.outBuf.events(t)[0]
	if opened.Type != coreagent.EventSessionOpened {
		t.Fatalf("first event = %s", opened.Type)
	}
	if opened.Model != "test-model" {
		t.Fatalf("model = %q", opened.Model)
	}
	if opened.WorkingDir == "" {
		t.Fatal("session_opened must carry workingDir")
	}
	if code := h.close(t); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
}

func TestPipePromptStreamsAndFinishes(t *testing.T) {
	fc := &fakeClient{responses: [][]api.ChatResponse{textChunks("hello world")}}
	h := startPipe(t, fc, &coreagent.Registry{})
	evs := h.promptAndWait(t, "say hi")

	var opened coreagent.Event
	var deltas, finished string
	for _, ev := range evs {
		switch ev.Type {
		case coreagent.EventSessionOpened:
			opened = ev
		case coreagent.EventMessageDelta:
			deltas += ev.Content
		case coreagent.EventRunFinished:
			finished = string(ev.Status)
		}
	}
	if opened.Type == coreagent.EventSessionOpened {
		t.Fatal("session_opened must be emitted exactly once, before the loop")
	}
	if deltas != "hello world" {
		t.Fatalf("deltas = %q", deltas)
	}
	if finished != string(coreagent.RunStatusDone) {
		t.Fatalf("run status = %q", finished)
	}
	if code := h.close(t); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
}

func TestPipeToolRoundEmitsLifecycle(t *testing.T) {
	tool := &upperTool{}
	registry := &coreagent.Registry{}
	registry.Register(tool)
	fc := &fakeClient{responses: [][]api.ChatResponse{
		toolCallChunks("upper", map[string]any{"text": "hi"}),
		textChunks("done"),
	}}
	h := startPipe(t, fc, registry)
	evs := h.promptAndWait(t, "run the tool")

	var sawStarted, sawFinished bool
	for _, ev := range evs {
		if ev.Type == coreagent.EventToolStarted && ev.ToolName == "upper" {
			sawStarted = true
			if ev.ToolStatus != coreagent.ToolStatusRunning {
				t.Fatalf("tool_started status = %q", ev.ToolStatus)
			}
		}
		if ev.Type == coreagent.EventToolFinished && ev.ToolName == "upper" {
			sawFinished = true
			if ev.ToolStatus != coreagent.ToolStatusDone || ev.Content != "HI" {
				t.Fatalf("tool_finished = %+v", ev)
			}
		}
	}
	if !sawStarted || !sawFinished {
		t.Fatalf("missing tool lifecycle events: %v", evs)
	}
	if tool.called != 1 {
		t.Fatalf("tool called %d times", tool.called)
	}
	h.close(t)
}

func TestPipeHistoryGrowsAcrossTurns(t *testing.T) {
	fc := &fakeClient{responses: [][]api.ChatResponse{
		textChunks("first answer"),
		textChunks("second answer"),
	}}
	h := startPipe(t, fc, &coreagent.Registry{})
	h.promptAndWait(t, "one")
	h.promptAndWait(t, "two")

	if len(fc.requests) != 2 {
		t.Fatalf("requests = %d", len(fc.requests))
	}
	if got := len(fc.requests[1].Messages); got < 3 {
		t.Fatalf("second request must carry prior history; got %d messages", got)
	}
	last := fc.requests[1].Messages[len(fc.requests[1].Messages)-1]
	if last.Role != "user" || last.Content != "two" {
		t.Fatalf("last message = %+v", last)
	}
	h.close(t)
}

func TestPipeUnknownAndBusyCommands(t *testing.T) {
	fc := &fakeClient{responses: [][]api.ChatResponse{textChunks("ok")}}
	h := startPipe(t, fc, &coreagent.Registry{})
	h.send(t, `{"cmd":"bogus"}`)
	h.waitFor(func(ev coreagent.Event) bool {
		return ev.Type == coreagent.EventError && strings.Contains(ev.Error, "unknown command")
	})
	// process stays alive and still serves prompts
	evs := h.promptAndWait(t, "still there")
	var deltas string
	for _, ev := range evs {
		if ev.Type == coreagent.EventMessageDelta {
			deltas += ev.Content
		}
	}
	if deltas != "ok" {
		t.Fatalf("deltas = %q", deltas)
	}
	h.close(t)
}

func TestPipeEOFDuringRunStillCompletes(t *testing.T) {
	fc := &fakeClient{responses: [][]api.ChatResponse{textChunks("done before exit")}}
	h := startPipe(t, fc, &coreagent.Registry{})
	h.send(t, `{"cmd":"prompt","text":"go"}`)
	// Close stdin immediately: the run must finish and flush, then exit 0.
	h.stdin.Close()
	select {
	case code := <-h.codeCh:
		if code != 0 {
			t.Fatalf("exit code = %d, stderr = %s", code, h.errBuf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not exit after EOF")
	}
	var deltas string
	var finished bool
	for _, ev := range h.outBuf.events(t) {
		if ev.Type == coreagent.EventMessageDelta {
			deltas += ev.Content
		}
		if ev.Type == coreagent.EventRunFinished && ev.Status == coreagent.RunStatusDone {
			finished = true
		}
	}
	if deltas != "done before exit" || !finished {
		t.Fatalf("events truncated on EOF: %s", h.outBuf.String())
	}
}

func TestPipeResumeEmitsHistory(t *testing.T) {
	// runPipeSession takes a *sessionstore.Session for resume; nil store is
	// fine, but we need a session object with messages.
	fc := &fakeClient{}
	inR, inW := io.Pipe()
	out := &lockedBuffer{}
	codeCh := make(chan int, 1)
	opts := &agentTUIOptions{Model: "test-model", AllowAllTools: true, Options: map[string]any{}}
	sess := resumedSession()
	go func() {
		codeCh <- runPipeSession(context.Background(), fc, opts, nil, nil, &coreagent.Registry{}, "system prompt", sess, t.TempDir(), inR, out, &lockedBuffer{}, "")
	}()
	inW.Close()
	<-codeCh
	evs := out.events(t)
	if len(evs) == 0 || evs[0].Type != coreagent.EventSessionOpened {
		t.Fatalf("events = %v", evs)
	}
	if len(evs[0].Messages) != 2 || evs[0].ChatID != "sess-1" || evs[0].Name != "named" {
		t.Fatalf("opened = %+v", evs[0])
	}
}

func TestPipeSessionOpenedListsSkills(t *testing.T) {
	project := t.TempDir()
	skillDir := filepath.Join(project, ".agents", "skills", "greet")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: greet\ndescription: Greet warmly.\n---\nAlways greet the user warmly.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, err := coreagent.LoadDefaultSkills(project)
	if err != nil {
		t.Fatal(err)
	}

	fc := &fakeClient{responses: [][]api.ChatResponse{textChunks("hi there")}}
	inR, inW := io.Pipe()
	out := &lockedBuffer{}
	codeCh := make(chan int, 1)
	opts := &agentTUIOptions{Model: "test-model", AllowAllTools: true, Options: map[string]any{}}
	go func() {
		codeCh <- runPipeSession(context.Background(), fc, opts, nil, catalog, &coreagent.Registry{}, "system prompt", nil, project, inR, out, &lockedBuffer{}, "")
	}()
	inW.Close()
	<-codeCh

	opened := out.events(t)[0]
	var names []string
	for _, s := range opened.Skills {
		names = append(names, s.Name)
		if s.Name == "greet" && s.Description != "Greet warmly." {
			t.Fatalf("greet skill missing description: %+v", s)
		}
	}
	if !slices.Contains(names, "greet") {
		t.Fatalf("skills = %v", names)
	}
}

func TestPipeSkillActivationReachedModelRequest(t *testing.T) {
	project := t.TempDir()
	skillDir := filepath.Join(project, ".agents", "skills", "greet")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: greet\ndescription: Greet warmly.\n---\nAlways greet the user warmly.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, err := coreagent.LoadDefaultSkills(project)
	if err != nil {
		t.Fatal(err)
	}

	fc := &fakeClient{responses: [][]api.ChatResponse{textChunks("warm hello")}}
	inR, inW := io.Pipe()
	out := &lockedBuffer{}
	codeCh := make(chan int, 1)
	opts := &agentTUIOptions{Model: "test-model", AllowAllTools: true, Options: map[string]any{}}
	go func() {
		codeCh <- runPipeSession(context.Background(), fc, opts, nil, catalog, &coreagent.Registry{}, "system prompt", nil, project, inR, out, &lockedBuffer{}, "")
	}()
	// skill-only prompt (no text): must be accepted and activate the skill
	if _, err := inW.Write([]byte("{\"cmd\":\"prompt\",\"skill\":\"greet\"}\n")); err != nil {
		t.Fatal(err)
	}
	inW.Close()
	if code := <-codeCh; code != 0 {
		t.Fatalf("exit code = %d, out = %s", code, out.String())
	}

	if len(fc.requests) == 0 {
		t.Fatalf("no model requests; events: %s", out.String())
	}
	var sawSkill bool
	for _, m := range fc.requests[0].Messages {
		if strings.Contains(m.Content, `<skill name="greet"`) {
			sawSkill = true
		}
	}
	if !sawSkill {
		t.Fatalf("skill content not injected into request: %+v", fc.requests[0].Messages)
	}
}

func TestPipeEmptyPromptRejected(t *testing.T) {
	h := startPipe(t, &fakeClient{}, &coreagent.Registry{})
	h.send(t, `{"cmd":"prompt","text":"  "}`)
	h.send(t, `{"cmd":"prompt"}`)
	count := 0
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && count < 2 {
		for _, ev := range h.outBuf.events(t) {
			if ev.Type == coreagent.EventError && strings.Contains(ev.Error, "empty prompt") {
				count++
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if count < 2 {
		t.Fatalf("expected 2 empty-prompt errors, got %d", count)
	}
	h.close(t)
}

func TestPipeInspectReportsPromptToolsMessages(t *testing.T) {
	registry := &coreagent.Registry{}
	registry.Register(&upperTool{})
	fc := &fakeClient{responses: [][]api.ChatResponse{textChunks("answer one"), textChunks("answer two")}}
	h := startPipe(t, fc, registry)

	// while running: snapshot should contain the system prompt + tools
	h.send(t, `{"cmd":"prompt","text":"first"}`)
	h.waitForAfter(0, func(ev coreagent.Event) bool { return ev.Type == coreagent.EventRunFinished })
	h.send(t, `{"cmd":"inspect"}`)
	openCount := len(h.outBuf.events(t))
	ev := h.waitFor(func(e coreagent.Event) bool { return e.Type == coreagent.EventInspect })
	_ = openCount
	if ev.System != "system prompt" {
		t.Fatalf("system = %q", ev.System)
	}
	if len(ev.Tools) != 1 || ev.Tools[0].Name != "upper" {
		t.Fatalf("tools = %+v", ev.Tools)
	}
	// history so far: user + assistant from turn 1
	if len(ev.Messages) != 2 || ev.Messages[0].Role != "user" || ev.Messages[1].Content != "answer one" {
		t.Fatalf("messages = %+v", ev.Messages)
	}
	h.close(t)
}
