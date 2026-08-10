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

type cliOptions struct {
	system              string
	allowAllTools       bool
	toolsDisabled       bool
	multiModal          bool
	contextWindowTokens int
	headless            bool
}

func buildFlagSet() (*flag.FlagSet, *cliOptions) {
	opts := &cliOptions{}
	fs := flag.NewFlagSet("o", flag.ExitOnError)
	fs.StringVar(&opts.system, "system", "", "override the model system prompt")
	fs.BoolVar(&opts.allowAllTools, "allow-all-tools", false, "run tools without approval prompts")
	fs.BoolVar(&opts.toolsDisabled, "no-tools", false, "disable tool use entirely")
	fs.BoolVar(&opts.multiModal, "multimodal", false, "enable multimodal input")
	fs.IntVar(&opts.contextWindowTokens, "context-window", 0, "context window tokens (0 = model default)")
	fs.BoolVar(&opts.headless, "headless", false, "print the response and exit (prompt from args or stdin)")
	return fs, opts
}

func main() {
	fs, opts := buildFlagSet()
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usageText(fs))
	}
	// explicit help goes to stdout; parse errors keep flag's stderr default
	for _, arg := range os.Args[1:] {
		if arg == "-h" || arg == "--help" || arg == "-help" {
			fmt.Print(usageText(fs))
			return
		}
	}
	_ = fs.Parse(os.Args[1:])

	model, prompt := "", ""
	if fs.NArg() > 0 {
		model = fs.Arg(0)
		prompt = strings.Join(fs.Args()[1:], " ")
	}
	// A positional prompt implies headless; explicit --headless with no
	// positional prompt reads the prompt from stdin.
	headless := opts.headless || prompt != ""

	if err := run(model, prompt, opts.system, opts.allowAllTools, opts.toolsDisabled, opts.multiModal, opts.contextWindowTokens, headless); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// usageText renders --help. The AGENTS section teaches non-interactive
// callers (coding agents, scripts) how to run the harness headlessly,
// including the approval requirement.
func usageText(fs *flag.FlagSet) string {
	var b strings.Builder
	b.WriteString(`o — Ollama agent harness

USAGE
  o [flags] [model]             interactive agent TUI (remembers your last model)
  o [flags] [model] "prompt"    headless: answer once and exit
  prompt | o --headless [model] headless with the prompt on stdin

FLAGS
`)
	var defaults strings.Builder
	out := fs.Output()
	fs.SetOutput(&defaults)
	fs.PrintDefaults()
	fs.SetOutput(out)
	b.WriteString(defaults.String())
	b.WriteString(`
HEADLESS OUTPUT CONTRACT
  stdout   final assistant answer only (safe to pipe)
  stderr   thinking, tool activity, warnings
  exit 0   the run finished
  exit 1   error, or a tool was denied (reason on stderr)

AGENTS
  ALWAYS pass --allow-all-tools when driving o headlessly. Most tools
  (bash, edit, web_fetch, ...) require approval; there is no human at the
  approval prompt headlessly, so without the flag the tool is denied, the
  run stops, and o exits 1.

  o --allow-all-tools glm-5.2:cloud "list the files in src/ and summarize them"

  Keep prompts self-contained: state the goal, the exact output format,
  and any constraints in one prompt. Use --no-tools for pure text work.
  Treat stderr as logs, stdout as the answer; check the exit code.
`)
	return b.String()
}

// run mirrors ollama's launchInteractiveModel flow from cmd/cmd.go, with a
// headless addition.
func run(model, prompt, system string, allowAllTools, toolsDisabled, multiModal bool, contextWindowTokens int, headless bool) error {
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
