package chat

import (
	"strings"
	"testing"

	coreagent "github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
)

func cachedTokens(v int) *int { return &v }

func TestTurnMetricsStatus(t *testing.T) {
	cases := []struct {
		name    string
		metrics *api.Metrics
		want    string
	}{
		{
			name: "in out and cache hit",
			metrics: &api.Metrics{
				PromptEvalCount:       68,
				EvalCount:             11,
				PromptEvalCachedCount: cachedTokens(40),
			},
			want: "in 68  out 11  ch 59%",
		},
		{
			name: "no cache hit yet",
			metrics: &api.Metrics{
				PromptEvalCount:       68,
				EvalCount:             11,
				PromptEvalCachedCount: cachedTokens(0),
			},
			want: "in 68  out 11  ch 0%",
		},
		{
			name: "cache hit not reported",
			metrics: &api.Metrics{
				PromptEvalCount: 68,
				EvalCount:       11,
			},
			want: "in 68  out 11",
		},
		{
			name:    "no usage reported",
			metrics: &api.Metrics{TotalDuration: 10_000_000},
			want:    "",
		},
		{
			name:    "no metrics at all",
			metrics: nil,
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := chatModel{lastTurnMetrics: tc.metrics}
			if got := m.turnMetricsStatus(); got != tc.want {
				t.Fatalf("turnMetricsStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTurnMetricsStatusClampsAndCompacts(t *testing.T) {
	m := chatModel{lastTurnMetrics: &api.Metrics{
		PromptEvalCount:       20480,
		EvalCount:             1,
		PromptEvalCachedCount: cachedTokens(20480),
	}}
	if got := m.turnMetricsStatus(); got != "in 20k  out 1  ch 100%" {
		t.Fatalf("turnMetricsStatus = %q", got)
	}

	m.lastTurnMetrics.PromptEvalCount = 100
	*m.lastTurnMetrics.PromptEvalCachedCount = 120
	if got := m.turnMetricsStatus(); got != "in 100  out 1  ch 100%" {
		t.Fatalf("over-reporting cache clamp = %q, want clamped 100%%", got)
	}
}

func TestApplyAgentEventMetricsFeedsStatusLine(t *testing.T) {
	m := chatModel{running: true}
	m.applyAgentEvent(coreagent.Event{
		Type: coreagent.EventMetrics,
		Metrics: &api.Metrics{
			PromptEvalCount:       68,
			EvalCount:             11,
			PromptEvalCachedCount: cachedTokens(40),
		},
	})
	if m.lastTurnMetrics == nil || m.lastTurnMetrics.PromptEvalCount != 68 {
		t.Fatalf("lastTurnMetrics = %#v", m.lastTurnMetrics)
	}

	line := strings.TrimSpace(stripANSI(strings.Join(m.renderModelStatusLines(120), "\n")))
	if line != "in 68  out 11  ch 59%" {
		t.Fatalf("status line without model = %q, want metrics segment", line)
	}

	// A nil-metrics event must not clobber the last known usage.
	m.applyAgentEvent(coreagent.Event{Type: coreagent.EventMetrics})
	if m.lastTurnMetrics == nil {
		t.Fatal("nil metrics event clobbered last known usage")
	}
}

func TestApplyAgentEventMetricsClearsOnNewChat(t *testing.T) {
	m := chatModel{running: true}
	m.applyAgentEvent(coreagent.Event{
		Type: coreagent.EventMetrics,
		Metrics: &api.Metrics{
			PromptEvalCount: 68,
			EvalCount:       11,
		},
	})

	_, _ = m.resetChat("ready")
	if m.lastTurnMetrics != nil {
		t.Fatalf("metrics survived /new: %#v", m.lastTurnMetrics)
	}
	if line := stripANSI(strings.Join(m.renderModelStatusLines(120), "\n")); strings.Contains(line, "in ") {
		t.Fatalf("status line still shows metrics after /new: %q", line)
	}
}
