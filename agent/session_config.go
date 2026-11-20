package agent

// SessionConfig is what NewSession assembles a Session from. Zero values
// select the defaults documented per field.
type SessionConfig struct {
	Client           ChatClient
	EventSinks       []EventSink
	Tools            *Registry
	Skills           *SkillCatalog
	DisableTools     bool
	ApprovalPrompter ApprovalPrompter
	// ApprovalState defaults to an empty state (review mode).
	ApprovalState  *ApprovalState
	WorkingDir     string
	SupportsImages bool
	// Journal must be nil, not a typed nil pointer, when there is no store.
	Journal RunJournal
	// Compactor defaults to a SimpleCompactor over Client sized by
	// ContextWindowTokens (zero lets the compactor resolve the window).
	Compactor           Compactor
	ContextWindowTokens int
}

// NewSession builds a Session with the frontends' shared defaults: the
// background source comes from the tool registry, and compaction is on.
func NewSession(cfg SessionConfig) *Session {
	state := cfg.ApprovalState
	if state == nil {
		state = &ApprovalState{}
	}
	compactor := cfg.Compactor
	if compactor == nil {
		compactor = &SimpleCompactor{
			Client:  cfg.Client,
			Options: CompactionOptions{ContextWindowTokens: cfg.ContextWindowTokens},
		}
	}
	return &Session{
		Client:           cfg.Client,
		EventSinks:       cfg.EventSinks,
		Tools:            cfg.Tools,
		Skills:           cfg.Skills,
		DisableTools:     cfg.DisableTools,
		ApprovalPrompter: cfg.ApprovalPrompter,
		ApprovalState:    state,
		WorkingDir:       cfg.WorkingDir,
		SupportsImages:   cfg.SupportsImages,
		Journal:          cfg.Journal,
		Compactor:        compactor,
		Background:       cfg.Tools.BackgroundSource(),
	}
}
