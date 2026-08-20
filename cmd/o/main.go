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
	"github.com/ParthSareen/o/sessionstore"
	"github.com/spf13/cobra"
)

type cliOptions struct {
	system              string
	allowAllTools       bool
	toolsDisabled       bool
	multiModal          bool
	contextWindowTokens int
	headless            bool
	pipe                bool
	resume              bool
	resumeID            string
	listSessions        bool
	name                string
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
	fs.BoolVar(&opts.pipe, "pipe", false, "machine-readable NDJSON session over stdio (for UI frontends); implies --allow-all-tools unless set explicitly")
	fs.BoolVar(&opts.resume, "resume", false, "resume the most recent session")
	fs.StringVar(&opts.resumeID, "resume-id", "", "resume a specific session by ID")
	fs.BoolVar(&opts.listSessions, "list", false, "list saved sessions and exit")
	fs.StringVar(&opts.name, "name", "", "set a human-readable name for a new session")
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
	if opts.pipe {
		applyPipeDefaults(fs, opts)
	}

	model, prompt := "", ""
	if fs.NArg() > 0 {
		model = fs.Arg(0)
		prompt = strings.Join(fs.Args()[1:], " ")
	}

	if opts.listSessions {
		if err := listAndPrintSessions(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	// A positional prompt implies headless; explicit --headless with no
	// positional prompt reads the prompt from stdin. In pipe mode a
	// positional prompt is the first turn of the NDJSON session instead.
	headless := opts.headless || (prompt != "" && !opts.pipe)

	if err := run(model, prompt, opts.system, opts.allowAllTools, opts.toolsDisabled, opts.multiModal, opts.contextWindowTokens, headless, opts.pipe, opts.resume, opts.resumeID, opts.name); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// listAndPrintSessions prints all saved sessions to stdout and exits.
func listAndPrintSessions() error {
	store, err := sessionstore.Open()
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	defer store.Close()

	sessions, err := store.ListSessions(50)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	if len(sessions) == 0 {
		fmt.Println("No saved sessions.")
		return nil
	}
	fmt.Printf("%-36s  %-20s  %-20s  %s\n", "ID", "Name", "Model", "Title")
	for _, s := range sessions {
		name := s.Name
		if strings.TrimSpace(name) == "" {
			name = "(unnamed)"
		}
		title := s.Title
		if strings.TrimSpace(title) == "" {
			title = "(untitled)"
		}
		fmt.Printf("%-36s  %-20s  %-20s  %s\n", s.ID, truncate(name, 20), truncate(s.Model, 20), title)
	}
	return nil
}

// truncate shortens s to max runes, appending "…" if it was longer.
func truncate(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
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
  o --resume                    resume the most recent session
  o --resume-id <id>           resume a specific session by ID
  o --list                      list saved sessions
  o --name <text> [model]       start a new session with a human-readable name

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
func run(model, prompt, system string, allowAllTools, toolsDisabled, multiModal bool, contextWindowTokens int, headless bool, pipe bool, resume bool, resumeID string, name string) error {
	// Open the session store for persistence (non-fatal if it fails).
	store, storeErr := sessionstore.Open()
	if storeErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not open session store: %v\n", storeErr)
	}

	// --resume / --resume-id: load a saved session and continue it.
	if resume || resumeID != "" {
		if store == nil {
			return fmt.Errorf("session store unavailable, cannot resume")
		}
		if resumeID == "" {
			// --resume without --resume-id: load the most recent session.
			meta, err := store.MostRecentSession()
			if err != nil {
				return fmt.Errorf("find latest session: %w", err)
			}
			if meta == nil {
				return fmt.Errorf("no saved sessions to resume")
			}
			resumeID = meta.ID
		}
		sess, err := store.LoadSession(resumeID)
		if err != nil {
			return fmt.Errorf("resume session: %w", err)
		}
		// Use the session's model unless overridden by a positional arg.
		if model == "" {
			model = sess.Model
		} else if model != sess.Model {
			// Persist an explicit override so the sidebar and future plain
			// resumes reflect the chosen model.
			if err := store.SetModel(sess.ID, model); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not save model choice: %v\n", err)
			}
		}
		if model == "" {
			model = config.LastModel()
		}
		if model == "" {
			return fmt.Errorf("model is required (run `o <model>` once; it is remembered after that)")
		}

		ensureDebugServer(realServerBootstrap(func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}))

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

		if pipe {
			if code := runPipeResume(ctx, client, &opts, store, sess, agentWorkingDir(), os.Stdin, os.Stdout, os.Stderr, prompt); code != 0 {
				os.Exit(code)
			}
			return nil
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
			if code := runHeadlessResume(ctx, client, &opts, store, sess, prompt, agentWorkingDir(), os.Stdout, os.Stderr); code != 0 {
				os.Exit(code)
			}
			return nil
		}

		// Interactive resume: pass the session's messages and ChatID to the TUI.
		opts.ChatID = sess.ID
		opts.Messages = sess.Messages
		if err := GenerateAgentTUI(cmd, client, opts, store); err != nil {
			return fmt.Errorf("error running agent: %w", err)
		}
		return nil
	}

	if model == "" {
		model = config.LastModel()
	}
	if model == "" {
		return fmt.Errorf("model is required (run `o <model>` once; it is remembered after that)")
	}

	// If no server answers, start a debug server on :11433 via watchy (never
	// restarts one that's already running).
	ensureDebugServer(realServerBootstrap(func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}))

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
		Name:                name,
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

	if pipe {
		if code := runPipe(ctx, client, &opts, store, agentWorkingDir(), os.Stdin, os.Stdout, os.Stderr, prompt); code != 0 {
			os.Exit(code)
		}
		return nil
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
		if code := runHeadless(ctx, client, &opts, store, prompt, agentWorkingDir(), os.Stdout, os.Stderr); code != 0 {
			os.Exit(code)
		}
		return nil
	}

	if err := GenerateAgentTUI(cmd, client, opts, store); err != nil {
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
