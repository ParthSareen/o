package main

import (
	"flag"
	"strings"
	"testing"
)

func TestUsageDocumentsEveryFlag(t *testing.T) {
	fs, _ := buildFlagSet()
	help := usageText(fs)
	missing := []string{}
	fs.VisitAll(func(f *flag.Flag) {
		if !strings.Contains(help, "-"+f.Name) {
			missing = append(missing, f.Name)
		}
	})
	if len(missing) > 0 {
		t.Fatalf("help omits flags: %v\n%s", missing, help)
	}
}

func TestUsageAgentGuidance(t *testing.T) {
	fs, _ := buildFlagSet()
	help := usageText(fs)
	for _, want := range []string{
		"AGENTS",
		"--allow-all-tools", // approvals must be on for headless use
		"approval prompt",
		"exit 1", // denial contract
		"stdout", // answer channel
		"stderr", // log channel
		"--no-tools",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("help missing %q:\n%s", want, help)
		}
	}
}
