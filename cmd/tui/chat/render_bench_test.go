package chat

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

func benchTranscriptEntries(n int) []chatEntry {
	body := "Here is an explanation with `inline code` and a [link](https://example.com).\n\n" +
		"```go\n\tfmt.Println(\"hello world\")\n\tif err != nil { return err }\n```\n\n" +
		"- alpha item\n- beta item with more text\n"
	var entries []chatEntry
	for i := 0; i < n; i++ {
		entries = append(entries, newChatEntry(chatEntry{
			role:    "user",
			content: fmt.Sprintf("question %d about the repo layout and other things", i),
		}))
		entries = append(entries, newChatEntry(chatEntry{role: "assistant", content: body}))
	}
	return entries
}

func benchChatModel(entries int) chatModel {
	m := chatModel{entries: benchTranscriptEntries(entries / 2), memo: &transcriptMemo{}}
	m.width, m.height = 120, 40
	_ = m.View() // warm the per-entry and transcript caches
	return m
}

func BenchmarkViewWarm(b *testing.B) {
	for _, n := range []int{20, 100, 500, 2000} {
		b.Run(fmt.Sprintf("entries=%d", n), func(b *testing.B) {
			m := benchChatModel(n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = m.View()
			}
		})
	}
}

// BenchmarkViewStreaming measures the per-frame cost while the tail entry is
// rewritten, i.e. the streaming case where invalidation (not caching) is the
// dominant path.
func BenchmarkViewStreaming(b *testing.B) {
	m := benchChatModel(400)
	chunk := strings.Repeat("tok ", 16)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx := len(m.entries) - 1
		m.entries[idx].content += chunk
		m.markEntryDirty(idx)
		_ = m.View()
	}
}

// TestTranscriptMemoMatchesFreshRender drives a model with the memo enabled
// and one without through the same mutation script and requires identical
// transcripts at every step. It is the safety net for the memo's
// invalidation: any missed markEntryDirty that would freeze the view shows up
// here as a divergence as soon as the two models are compared.
func TestTranscriptMemoMatchesFreshRender(t *testing.T) {
	build := func(withMemo bool) chatModel {
		m := chatModel{entries: benchTranscriptEntries(2)}
		if withMemo {
			m.memo = &transcriptMemo{}
		}
		m.width, m.height = 100, 30
		return m
	}

	memoized := build(true)
	plain := build(false)

	apply := func(fn func(*chatModel)) {
		fn(&memoized)
		fn(&plain)
	}

	check := func(step string) {
		t.Helper()
		got := memoized.transcriptLines(memoized.viewWidth())
		want := plain.transcriptLines(memoized.viewWidth())
		if !slices.Equal(got, want) {
			t.Fatalf("step %q diverged: memoized %d lines, fresh %d lines", step, len(got), len(want))
		}
	}

	check("initial")

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		// Draw every random value once, outside the per-model closure, so both
		// models receive exactly the same mutation.
		op := rng.Intn(6)
		pick := rng.Intn(8)
		status := []string{"running", "done", "denied"}[rng.Intn(3)]
		width := 60 + rng.Intn(80)

		apply(func(m *chatModel) {
			switch op {
			case 0: // stream a delta into an entry
				idx := pick % len(m.entries)
				m.entries[idx].content += " streamed token"
				m.markEntryDirty(idx)
			case 1: // append an entry
				m.entries = append(m.entries, newChatEntry(chatEntry{role: "user", content: "appended prompt"}))
			case 2: // change a status (must be re-marked dirty)
				idx := pick % len(m.entries)
				m.entries[idx].status = status
				m.markEntryDirty(idx)
			case 3: // resize
				m.width = width
			case 4: // toggle expansion
				idx := pick % len(m.entries)
				m.entries[idx].expanded = !m.entries[idx].expanded
				m.markEntryDirty(idx)
			case 5: // drop the tail
				if len(m.entries) > 2 {
					m.entries = m.entries[:len(m.entries)-1]
				}
			}
		})
		check(fmt.Sprintf("op%d", i))
	}
}
