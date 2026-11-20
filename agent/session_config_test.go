package agent

import "testing"

type stubBackground struct{ BackgroundSource }

func TestNewSessionDefaults(t *testing.T) {
	background := stubBackground{}
	registry := &Registry{Background: background}
	session := NewSession(SessionConfig{Client: &fakeClient{}, Tools: registry, ContextWindowTokens: 4096})

	compactor, ok := session.Compactor.(*SimpleCompactor)
	if !ok || compactor.Options.ContextWindowTokens != 4096 || compactor.Client != session.Client {
		t.Fatalf("compactor = %#v, want a SimpleCompactor over the client sized to the window", session.Compactor)
	}
	if session.Background != BackgroundSource(background) {
		t.Fatal("background source should come from the registry")
	}
	if session.ApprovalState == nil || session.Journal != nil {
		t.Fatalf("want an approval state and no journal, got %+v / %v", session.ApprovalState, session.Journal)
	}
	if NewSession(SessionConfig{}).Background != nil {
		t.Fatal("a nil registry should leave no background source")
	}
}
