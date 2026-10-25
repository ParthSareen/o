package chat

import (
	"context"
	"time"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
	"github.com/ParthSareen/o/sessionstore"
)

// LLM session naming. Sessions start with a cheap derived title (first user
// message, truncated) so the /sessions and /resume lists always have
// something readable. When a session accumulates enough user turns, one
// small LLM call rewrites the title into a short name grounded in the first
// five user messages.
const (
	// sessionTitleMinUserMessages: the autonamer fires exactly once per
	// session, when the history first holds this many user messages. Sessions
	// that end sooner keep the derived title.
	sessionTitleMinUserMessages = coreagent.SessionTitleMaxUserMessages

	// sessionTitleTimeout bounds the naming call so a hung model cannot park
	// the goroutine forever.
	sessionTitleTimeout = 45 * time.Second
)

// maybeNameSession fires the naming call for a just-completed run.
//
// It runs once per title: the call goes out when the session first holds
// sessionTitleMinUserMessages user messages. Once a title is set — by this
// call or by an earlier process — the flags or the stored title itself keep
// it from running again, so later runs and compaction rewrites never re-name
// a titled session. The goroutine only touches the thread-safe store;
// chatModel state is not written from it.
func (m *chatModel) maybeNameSession(messages []api.Message) {
	if m.store == nil || m.chatID == "" || m.opts.Client == nil || m.titleNamed {
		return
	}
	first, total := coreagent.SessionTitleUserMessages(messages, coreagent.SessionTitleMaxUserMessages)
	if len(first) == 0 || total < sessionTitleMinUserMessages {
		return
	}
	// Skip sessions that already carry a set (non-derived) title, named by an
	// earlier process or through the store. A titled session never re-names,
	// so compaction history rewrites are irrelevant too.
	title, err := m.store.Title(m.chatID)
	if err == nil && title != "" && title != sessionstore.DerivedTitle(first[0]) {
		m.titleNamed = true
		return
	}
	m.titleNamed = true

	client, model, chatID := m.opts.Client, m.opts.Model, m.chatID
	go func() {
		ctx, cancel := context.WithTimeout(m.ctx, sessionTitleTimeout)
		defer cancel()
		name, err := coreagent.GenerateSessionTitle(ctx, client, model, first)
		if err != nil || name == "" {
			// Best-effort: the derived title stands; a later process run
			// treats the session as still unnamed.
			return
		}
		_ = m.store.SetTitle(chatID, name)
	}()
}
