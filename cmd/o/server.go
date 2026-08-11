package main

// Debug server bootstrap: when `o` launches and no ollama server is
// reachable, start one in debug mode on port 11433 via watchy — but only if
// watchy is installed, only with the system ollama binary, and never by
// restarting or preempting a server that's already running.
//
// Decision ladder:
//  1. OLLAMA_HOST set        → respect it as-is (reachable or not; user's call)
//  2. default :11434 up      → do nothing (the overall ollama keeps ownership)
//  3. debug :11433 up        → reuse it (point this process at it, start nothing)
//  4. watchy installed       → watchy start a debug server on :11433, wait for
//                              readiness, point this process at it
//  5. otherwise              → do nothing; the api client will surface the
//                              usual connection error

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"
)

const (
	defaultOllamaHost = "http://127.0.0.1:11434"
	debugOllamaHost   = "http://127.0.0.1:11433"
	debugServerTask   = "o-ollama-debug-11433"
)

// serverBootstrap wires the policy to the environment; tests substitute fakes.
type serverBootstrap struct {
	probeServer  func(host string) bool // is an ollama server reachable at host?
	lookPath     func(name string) (string, error)
	getenv       func(key string) string
	setenv       func(key, value string) error
	startTask    func(name, command string) error
	readyTimeout time.Duration
	note         func(format string, args ...any)
}

func realServerBootstrap(note func(format string, args ...any)) serverBootstrap {
	client := &http.Client{Timeout: 2 * time.Second}
	return serverBootstrap{
		probeServer: func(host string) bool {
			resp, err := client.Get(host + "/api/version")
			if err != nil {
				return false
			}
			resp.Body.Close()
			return resp.StatusCode == http.StatusOK
		},
		lookPath:     exec.LookPath,
		getenv:       os.Getenv,
		setenv:       os.Setenv,
		startTask:    startWatchyTask,
		readyTimeout: 15 * time.Second,
		note:         note,
	}
}

func startWatchyTask(name, command string) error {
	watchy, err := exec.LookPath("watchy")
	if err != nil {
		return err
	}
	cmd := exec.Command(watchy, "start", command, "--name", name)
	cmd.Stdout, cmd.Stderr = nil, nil
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("watchy start: %w: %s", err, out)
	}
	return nil
}

// ensureDebugServer applies the decision ladder. It never blocks for long:
// probes are 2s max, readiness only after a spawn.
func ensureDebugServer(b serverBootstrap) {
	// 1. explicit OLLAMA_HOST: never override the user's choice
	if h := b.getenv("OLLAMA_HOST"); h != "" {
		if !b.probeServer(h) {
			b.note("warning: OLLAMA_HOST %s is set but no ollama server answers there", h)
		}
		return
	}
	// 2. the overall ollama owns the default port; never restart it
	if b.probeServer(defaultOllamaHost) {
		return
	}
	// 3. a debug server from an earlier launch? reuse, don't restart
	if b.probeServer(debugOllamaHost) {
		if err := b.setenv("OLLAMA_HOST", debugOllamaHost); err == nil {
			b.note("using existing ollama debug server on 127.0.0.1:11433")
		}
		return
	}
	// 4. nothing running: spin one up via watchy if we can
	if _, err := b.lookPath("watchy"); err != nil {
		return // no watchy: leave the default behavior untouched
	}
	ollamaBin, err := b.lookPath("ollama")
	if err != nil {
		b.note("warning: watchy is installed but the ollama binary was not found on PATH; no debug server started")
		return
	}
	command := fmt.Sprintf("env OLLAMA_HOST=127.0.0.1:11433 OLLAMA_DEBUG=1 %s serve", ollamaBin)
	if err := b.startTask(debugServerTask, command); err != nil {
		b.note("warning: could not start debug ollama server via watchy: %v", err)
		return
	}
	deadline := time.Now().Add(b.readyTimeout)
	for time.Now().Before(deadline) {
		if b.probeServer(debugOllamaHost) {
			if err := b.setenv("OLLAMA_HOST", debugOllamaHost); err == nil {
				b.note("no ollama server found; started debug server on 127.0.0.1:11433 via watchy (task %q; `watchy logs %s`)", debugServerTask, debugServerTask)
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	b.note("warning: debug ollama server did not come up within %s; check `watchy logs %s`", b.readyTimeout, debugServerTask)
}
