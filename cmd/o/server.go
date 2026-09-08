package main

// Dedicated server bootstrap: `o` always runs against its own ollama server
// on port 11433, started via watchy — even when a shared server is already
// listening on 11434. A dedicated server keeps o's model loads, VRAM use, and
// logs isolated from whatever else the user runs on the default port.
//
// Decision ladder:
//  1. OLLAMA_HOST set        → respect it as-is (reachable or not; user's call)
//  2. dedicated :11433 up    → reuse it (point this process at it, start nothing)
//  3. watchy + ollama found  → watchy start a dedicated server on :11433, wait
//                              for readiness, point this process at it
//  4. otherwise              → fall back to the shared server on :11434 if one
//                              answers; if not, the api client will surface the
//                              usual connection error

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"
)

const (
	defaultOllamaHost   = "http://127.0.0.1:11434" // shared fallback
	dedicatedOllamaHost = "http://127.0.0.1:11433" // o's own
	dedicatedServerTask = "o-ollama-11433"
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

// ensureDedicatedServer applies the decision ladder. It never blocks for long:
// probes are 2s max, readiness only after a spawn.
func ensureDedicatedServer(b serverBootstrap) {
	// 1. explicit OLLAMA_HOST: never override the user's choice
	if h := b.getenv("OLLAMA_HOST"); h != "" {
		if !b.probeServer(h) {
			b.note("warning: OLLAMA_HOST %s is set but no ollama server answers there", h)
		}
		return
	}
	// 2. a dedicated server from an earlier launch? reuse, don't restart
	if b.probeServer(dedicatedOllamaHost) {
		if err := b.setenv("OLLAMA_HOST", dedicatedOllamaHost); err == nil {
			b.note("using dedicated ollama server on 127.0.0.1:11433")
		}
		return
	}
	// 3. nothing running: spin up o's own server via watchy if we can
	if _, err := b.lookPath("watchy"); err != nil {
		fallBackToSharedServer(b, "watchy is not installed")
		return
	}
	ollamaBin, err := b.lookPath("ollama")
	if err != nil {
		fallBackToSharedServer(b, "the ollama binary was not found on PATH")
		return
	}
	command := fmt.Sprintf("env OLLAMA_HOST=127.0.0.1:11433 OLLAMA_DEBUG=1 %s serve", ollamaBin)
	if err := b.startTask(dedicatedServerTask, command); err != nil {
		fallBackToSharedServer(b, fmt.Sprintf("could not start the dedicated ollama server via watchy: %v", err))
		return
	}
	deadline := time.Now().Add(b.readyTimeout)
	for time.Now().Before(deadline) {
		if b.probeServer(dedicatedOllamaHost) {
			if err := b.setenv("OLLAMA_HOST", dedicatedOllamaHost); err == nil {
				b.note("started dedicated ollama server on 127.0.0.1:11433 via watchy (task %q; `watchy logs %s`)", dedicatedServerTask, dedicatedServerTask)
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	fallBackToSharedServer(b, fmt.Sprintf("the dedicated ollama server did not come up within %s (is something else listening on 127.0.0.1:11433? check `watchy logs %s`)", b.readyTimeout, dedicatedServerTask))
}

// fallBackToSharedServer is the last resort when no dedicated server could be
// started: use the system server on the default port if one answers, and let
// the api client surface the usual connection error otherwise.
func fallBackToSharedServer(b serverBootstrap, reason string) {
	if !b.probeServer(defaultOllamaHost) {
		b.note("warning: %s and nothing answers on 127.0.0.1:11434 either; no ollama server reachable", reason)
		return
	}
	if err := b.setenv("OLLAMA_HOST", defaultOllamaHost); err == nil {
		b.note("%s; using the shared ollama server on 127.0.0.1:11434", reason)
	}
}
