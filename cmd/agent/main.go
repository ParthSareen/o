// Command o runs the Ollama agent harness TUI against a local (or cloud) model.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ParthSareen/o/api"
	"github.com/spf13/cobra"
)

func main() {
	var (
		system              string
		allowAllTools       bool
		toolsDisabled       bool
		multiModal          bool
		contextWindowTokens int
	)

	fs := flag.NewFlagSet("o", flag.ExitOnError)
	fs.StringVar(&system, "system", "", "override the model system prompt")
	fs.BoolVar(&allowAllTools, "allow-all-tools", false, "run tools without approval prompts")
	fs.BoolVar(&toolsDisabled, "no-tools", false, "disable tool use entirely")
	fs.BoolVar(&multiModal, "multimodal", false, "enable multimodal input")
	fs.IntVar(&contextWindowTokens, "context-window", 0, "context window tokens (0 = model default)")
	_ = fs.Parse(os.Args[1:])

	model := ""
	if fs.NArg() > 0 {
		model = fs.Arg(0)
	}

	if err := run(model, system, allowAllTools, toolsDisabled, multiModal, contextWindowTokens); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run mirrors ollama's launchInteractiveModel flow from cmd/cmd.go.
func run(model, system string, allowAllTools, toolsDisabled, multiModal bool, contextWindowTokens int) error {
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return err
	}

	cmd := &cobra.Command{}

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
