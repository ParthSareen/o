// Command o runs the Ollama agent harness TUI against a local (or cloud) model.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ParthSareen/o/api"
	"github.com/ParthSareen/o/cmd/config"
	"github.com/spf13/cobra"
)

func main() {
	var (
		system              string
		allowAllTools       bool
		toolsDisabled       bool
		multiModal          bool
		contextWindowTokens int
		headless            bool
		mcpConfig           string
	)

	fs := flag.NewFlagSet("o", flag.ExitOnError)
	fs.StringVar(&system, "system", "", "override the model system prompt")
	fs.BoolVar(&allowAllTools, "allow-all-tools", false, "run tools without approval prompts")
	fs.BoolVar(&toolsDisabled, "no-tools", false, "disable tool use entirely")
	fs.BoolVar(&multiModal, "multimodal", false, "enable multimodal input")
	fs.IntVar(&contextWindowTokens, "context-window", 0, "context window tokens (0 = model default)")
	fs.BoolVar(&headless, "headless", false, "print the response and exit (prompt from args or stdin)")
	fs.StringVar(&mcpConfig, "mcp-config", "", "path to MCP config (default ~/.ollama/mcp.json)")
	_ = fs.Parse(os.Args[1:])

	model, prompt := "", ""
	if fs.NArg() > 0 {
		model = fs.Arg(0)
		prompt = strings.Join(fs.Args()[1:], " ")
	}
	// A positional prompt implies headless; explicit --headless with no
	// positional prompt reads the prompt from stdin.
	if prompt != "" {
		headless = true
	}

	if err := run(model, prompt, system, allowAllTools, toolsDisabled, multiModal, contextWindowTokens, headless, mcpConfig); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run mirrors ollama's launchInteractiveModel flow from cmd/cmd.go, with
// headless and MCP additions.
func run(model, prompt, system string, allowAllTools, toolsDisabled, multiModal bool, contextWindowTokens int, headless bool, mcpConfigPath string) error {
	if model == "" {
		model = config.LastModel()
	}
	if model == "" {
		return fmt.Errorf("model is required (run `o <model>` once; it is remembered after that)")
	}

	client, err := api.ClientFromEnvironment()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := setupMCP(ctx, mcpConfigPath, os.Stderr, !headless); err != nil {
		return err
	}
	defer closeMCP()

	cmd := &cobra.Command{}
	cmd.SetContext(ctx)

	opts := agentTUIOptions{
		Model:               model,
		System:              system,
		AllowAllTools:       allowAllTools,
		ToolsDisabled:       toolsDisabled,
		MultiModal:          multiModal,
		ContextWindowTokens: contextWindowTokens,
		Options:             map[string]any{},
	}

	info, err := prepareAgentModel(cmd, client, &opts, false)
	if err != nil {
		return err
	}
	opts.System = firstNonEmpty(system, info.System)

	if err := saveLastAgentModel(opts.Model); err != nil {
		return err
	}

	if headless {
		if prompt == "" {
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("read prompt from stdin: %w", err)
			}
			prompt = strings.TrimSpace(string(raw))
		}
		if prompt == "" {
			return fmt.Errorf("headless mode needs a prompt (positional args or stdin)")
		}
		if code := runHeadless(ctx, client, &opts, prompt, agentWorkingDir(), os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return nil
	}

	if err := GenerateAgentTUI(cmd, client, opts); err != nil {
		return fmt.Errorf("error running agent: %w", err)
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
