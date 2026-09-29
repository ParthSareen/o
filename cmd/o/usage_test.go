package main

import (
	"flag"
	"strings"
	"testing"
	"time"
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

func TestSystemPromptTeachesHeadlessDelegation(t *testing.T) {
	prompt := agentDefaultSystemPromptWithWorkingDir(time.Now(), "test-model", "/tmp")
	for _, want := range []string{"o --headless", "--allow-all-tools"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("system prompt missing %q", want)
		}
	}
}

func TestAutoIsDefault(t *testing.T) {
	_, opts := buildFlagSet()
	if !opts.autoReview {
		t.Fatal("auto review should default to on")
	}
	if opts.allowAllTools {
		t.Fatal("--allow-all-tools must stay opt-in")
	}
}

func TestApplyPipeDefaultsKeepAutoExplicit(t *testing.T) {
	t.Run("no flags grants full access and no grading", func(t *testing.T) {
		fs, opts := buildFlagSet()
		_ = fs.Parse([]string{"--pipe", "m"})
		applyPipeDefaults(fs, opts)
		if !opts.allowAllTools || opts.autoReview {
			t.Fatalf("pipe defaults: allow-all %v, auto %v", opts.allowAllTools, opts.autoReview)
		}
	})
	t.Run("explicit --auto survives", func(t *testing.T) {
		fs, opts := buildFlagSet()
		_ = fs.Parse([]string{"--pipe", "--auto", "m"})
		applyPipeDefaults(fs, opts)
		if !opts.allowAllTools || !opts.autoReview {
			t.Fatalf("explicit --auto lost: allow-all %v, auto %v", opts.allowAllTools, opts.autoReview)
		}
	})
	t.Run("explicit --allow-all-tools=false without --auto denies", func(t *testing.T) {
		fs, opts := buildFlagSet()
		_ = fs.Parse([]string{"--pipe", "--allow-all-tools=false", "m"})
		applyPipeDefaults(fs, opts)
		if opts.allowAllTools || opts.autoReview {
			t.Fatalf("denial-free launch became auto-graded: allow-all %v, auto %v", opts.allowAllTools, opts.autoReview)
		}
	})
}
