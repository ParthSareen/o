package chat

import (
	"context"
	"strings"
	"testing"
)

func TestActiveSlashToken(t *testing.T) {
	for _, tc := range []struct {
		input     string
		cursor    int
		wantToken string
		wantStart int
		wantOK    bool
	}{
		{"please /rel", 11, "/rel", 7, true},
		{"/rel", 4, "/rel", 0, true},
		{"please x", 8, "", 0, false},
		{"please /rel now", 11, "/rel", 7, true}, // cursor inside the token
		{"a/b", 3, "", 0, false},                 // no leading slash token... actually starts with 'a'
		{"/", 1, "/", 0, true},
	} {
		t.Run(tc.input, func(t *testing.T) {
			runes := []rune(tc.input)
			start, token, ok := activeSlashToken(runes, tc.cursor)
			if ok != tc.wantOK || token != tc.wantToken || start != tc.wantStart {
				t.Fatalf("got (%d, %q, %v)", start, token, ok)
			}
		})
	}
}

func TestMidLineSlashCompletionShowsOnMatchOnly(t *testing.T) {
	catalog := writeTestSkillCatalog(t)

	m := chatModel{opts: Options{Skills: catalog}, input: []rune("draft notes with /rel")}
	lines := m.completionLines(80)
	if len(lines) == 0 {
		t.Fatal("matched mid-line /rel should show the completion dropdown")
	}
	if got := strings.Join(lines, " "); !strings.Contains(got, "/release-notes") {
		t.Fatalf("dropdown missing /release-notes: %q", got)
	}

	m.input = []rune("draft notes with /zzz")
	if lines := m.completionLines(80); len(lines) != 0 {
		t.Fatalf("unmatched mid-line token must not show a dropdown, got %v", lines)
	}
}

func TestMidLineTabCompletionReplacesTokenKeepingRest(t *testing.T) {
	catalog := writeTestSkillCatalog(t)
	m := chatModel{opts: Options{Skills: catalog}, input: []rune("please /rel today"), inputCursor: len("please /rel"), inputCursorSet: true}
	if !m.applySlashCompletion() {
		t.Fatal("Tab should complete the matched token")
	}
	if got := string(m.input); got != "please /release-notes today" {
		t.Fatalf("input = %q", got)
	}
	if m.normalizedInputCursor() != len("please /release-notes") {
		t.Fatalf("cursor = %d", m.normalizedInputCursor())
	}

	// no match: Tab must leave the line alone
	m.input = []rune("please /zzz today")
	m.inputCursor = len("please /zzz")
	if m.applySlashCompletion() {
		t.Fatal("unmatched token must not complete")
	}
	if got := string(m.input); got != "please /zzz today" {
		t.Fatalf("input changed: %q", got)
	}
}

func TestMidLineSkillInvocationRun(t *testing.T) {
	catalog := writeTestSkillCatalog(t)
	m := chatModel{ctx: context.Background(), opts: Options{Model: "test", Skills: catalog, Client: chatTestClient{}}, input: []rune("please /release-notes make it short")}
	updated, cmd := m.handleSubmit()
	if cmd == nil {
		t.Fatal("mid-line matched skill should start a run")
	}
	m = updated.(chatModel)
	for {
		msg, ok := <-m.events
		if !ok {
			t.Fatal("skill run closed before it finished")
		}
		updated, _ = m.Update(msg)
		m = updated.(chatModel)
		if _, done := msg.(chatRunDoneMsg); done {
			break
		}
	}
	if len(m.messages) < 1 || m.messages[0].Role != "user" || m.messages[0].Content != "please make it short" {
		t.Fatalf("user message = %#v, want line minus the /token", m.messages)
	}
	if len(m.messages) < 2 || len(m.messages[1].ToolCalls) != 1 || m.messages[1].ToolCalls[0].Function.Name != "skill" {
		t.Fatalf("synthetic skill call missing: %#v", m.messages)
	}
}

func TestUnknownSlashSubmitsAsPlainText(t *testing.T) {
	m := chatModel{ctx: context.Background(), opts: Options{Model: "test", Client: chatTestClient{}}, input: []rune("/nosuchthing please review")}
	updated, cmd := m.handleSubmit()
	m = updated.(chatModel)
	for _, e := range m.entries {
		if e.role == "error" {
			t.Fatalf("unknown slash must not produce an error entry: %#v", e)
		}
	}
	if cmd == nil {
		t.Fatal("unknown slash should still start a normal run")
	}
}
