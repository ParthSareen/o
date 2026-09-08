package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeBootstrapState struct {
	up           map[string]bool
	ollamaPath   map[string]string
	lookPathErr  map[string]error
	env          map[string]string
	started      []string
	commands     []string
	notes        []string
	readiness    func(host string) bool // dynamic probe override
	startTaskErr error
	probes       int
}

func newFakeBootstrap() *fakeBootstrapState {
	return &fakeBootstrapState{
		up:          map[string]bool{},
		ollamaPath:  map[string]string{"watchy": "/usr/bin/watchy", "ollama": "/usr/bin/ollama"},
		lookPathErr: map[string]error{},
		env:         map[string]string{},
	}
}

func (f *fakeBootstrapState) asBootstrap() serverBootstrap {
	return serverBootstrap{
		probeServer: func(host string) bool {
			f.probes++
			if f.readiness != nil {
				return f.readiness(host)
			}
			return f.up[host]
		},
		lookPath: func(name string) (string, error) {
			if err := f.lookPathErr[name]; err != nil {
				return "", err
			}
			p, ok := f.ollamaPath[name]
			if !ok {
				return "", errors.New("not found: " + name)
			}
			return p, nil
		},
		getenv: func(k string) string { return f.env[k] },
		setenv: func(k, v string) error {
			f.env[k] = v
			return nil
		},
		startTask: func(name, command string) error {
			if f.startTaskErr != nil {
				return f.startTaskErr
			}
			f.started = append(f.started, name)
			f.commands = append(f.commands, command)
			return nil
		},
		readyTimeout: time.Second, // fake
		note: func(format string, args ...any) {
			f.notes = append(f.notes, fmt.Sprintf(format, args...))
		},
	}
}

func TestServerExplicitHostUpIsRespected(t *testing.T) {
	f := newFakeBootstrap()
	f.env["OLLAMA_HOST"] = "http://example:1"
	f.up["http://example:1"] = true
	ensureDedicatedServer(f.asBootstrap())
	if len(f.started) != 0 || f.env["OLLAMA_HOST"] != "http://example:1" {
		t.Fatalf("explicit host must be untouched: env=%v started=%v", f.env, f.started)
	}
	if len(f.notes) != 0 {
		t.Fatalf("no notes expected: %v", f.notes)
	}
}

func TestServerExplicitHostDownWarnsButDoesNotSpawn(t *testing.T) {
	f := newFakeBootstrap()
	f.env["OLLAMA_HOST"] = "http://example:1"
	ensureDedicatedServer(f.asBootstrap())
	if len(f.started) != 0 {
		t.Fatalf("explicit host down must not spawn: %v", f.started)
	}
	if len(f.notes) != 1 {
		t.Fatalf("want one warning note: %v", f.notes)
	}
}

// (The old "default server up → do nothing" case is now covered by
// TestServerSpawnsDedicatedEvenWhenSharedUp: o spawns its own server even
// when a shared one already listens on 11434.)

func TestServerReusesExistingDedicatedServer(t *testing.T) {
	f := newFakeBootstrap()
	f.up[dedicatedOllamaHost] = true
	ensureDedicatedServer(f.asBootstrap())
	if len(f.started) != 0 {
		t.Fatal("existing dedicated server must not be restarted")
	}
	if f.env["OLLAMA_HOST"] != dedicatedOllamaHost {
		t.Fatalf("env should point at dedicated server, got %q", f.env["OLLAMA_HOST"])
	}
}

func TestServerDedicatedPreferredOverShared(t *testing.T) {
	f := newFakeBootstrap()
	f.up[defaultOllamaHost] = true
	f.up[dedicatedOllamaHost] = true
	ensureDedicatedServer(f.asBootstrap())
	if len(f.started) != 0 {
		t.Fatal("must not spawn when a dedicated server already runs")
	}
	if f.env["OLLAMA_HOST"] != dedicatedOllamaHost {
		t.Fatalf("shared server must not win: env=%q", f.env["OLLAMA_HOST"])
	}
}

func TestServerSpawnsDedicatedEvenWhenSharedUp(t *testing.T) {
	f := newFakeBootstrap()
	f.up[defaultOllamaHost] = true
	// dedicated server becomes healthy after the spawn
	f.readiness = func(host string) bool {
		return host == dedicatedOllamaHost && len(f.started) > 0
	}
	ensureDedicatedServer(f.asBootstrap())
	if len(f.started) != 1 {
		t.Fatalf("a shared server on 11434 must not keep o from spawning its own: %v", f.started)
	}
	if f.env["OLLAMA_HOST"] != dedicatedOllamaHost {
		t.Fatalf("env should point at dedicated server, got %q", f.env["OLLAMA_HOST"])
	}
}

func TestServerSpawnsViaWatchyAndPointsEnv(t *testing.T) {
	f := newFakeBootstrap()
	// server becomes healthy after the spawn (watchy actually started it)
	f.readiness = func(host string) bool {
		return host == dedicatedOllamaHost && len(f.started) > 0
	}
	ensureDedicatedServer(f.asBootstrap())
	if len(f.started) != 1 || f.started[0] != dedicatedServerTask {
		t.Fatalf("want exactly one spawn of %q: %v", dedicatedServerTask, f.started)
	}
	cmd := f.commands[0]
	for _, want := range []string{"OLLAMA_HOST=127.0.0.1:11433", "OLLAMA_DEBUG=1", "/usr/bin/ollama", "serve"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("spawn command %q missing %q", cmd, want)
		}
	}
	if f.env["OLLAMA_HOST"] != dedicatedOllamaHost {
		t.Fatalf("env should point at dedicated server, got %q", f.env["OLLAMA_HOST"])
	}
	if len(f.notes) == 0 || !strings.Contains(f.notes[len(f.notes)-1], "watchy logs") {
		t.Fatalf("want note pointing at watchy logs: %v", f.notes)
	}
}

func TestServerSpawnNeverReadiesFallsBackToShared(t *testing.T) {
	f := newFakeBootstrap()
	f.up[defaultOllamaHost] = true
	b := f.asBootstrap()
	b.readyTimeout = 10 * time.Millisecond // probes stay down forever
	ensureDedicatedServer(b)
	if f.env["OLLAMA_HOST"] != defaultOllamaHost {
		t.Fatalf("want fallback to shared server, env=%q", f.env["OLLAMA_HOST"])
	}
	if len(f.notes) == 0 || !strings.Contains(f.notes[len(f.notes)-1], "did not come up") {
		t.Fatalf("want readiness-timeout note: %v", f.notes)
	}
}

func TestServerSpawnNeverReadiesNothingUpWarns(t *testing.T) {
	f := newFakeBootstrap()
	b := f.asBootstrap()
	b.readyTimeout = 10 * time.Millisecond
	ensureDedicatedServer(b)
	if _, ok := f.env["OLLAMA_HOST"]; ok {
		t.Fatal("do not point env at a server that never came up")
	}
	if len(f.notes) == 0 || !strings.Contains(f.notes[len(f.notes)-1], "no ollama server reachable") {
		t.Fatalf("want no-server warning: %v", f.notes)
	}
}

func TestServerSpawnFailureFallsBackToShared(t *testing.T) {
	f := newFakeBootstrap()
	f.startTaskErr = errors.New("boom")
	f.up[defaultOllamaHost] = true
	ensureDedicatedServer(f.asBootstrap())
	if f.env["OLLAMA_HOST"] != defaultOllamaHost {
		t.Fatalf("want fallback to shared server, env=%q", f.env["OLLAMA_HOST"])
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "watchy") {
		t.Fatalf("want watchy-start failure note: %v", f.notes)
	}
}

func TestServerNoWatchyFallsBackToShared(t *testing.T) {
	f := newFakeBootstrap()
	f.lookPathErr["watchy"] = errors.New("not installed")
	f.up[defaultOllamaHost] = true
	ensureDedicatedServer(f.asBootstrap())
	if len(f.started) != 0 {
		t.Fatalf("no watchy: must not spawn: %v", f.started)
	}
	if f.env["OLLAMA_HOST"] != defaultOllamaHost {
		t.Fatalf("want fallback to shared server, env=%q", f.env["OLLAMA_HOST"])
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "watchy") {
		t.Fatalf("want fallback note mentioning watchy: %v", f.notes)
	}
}

func TestServerNoWatchyNothingUpWarns(t *testing.T) {
	f := newFakeBootstrap()
	f.lookPathErr["watchy"] = errors.New("not installed")
	ensureDedicatedServer(f.asBootstrap())
	if len(f.started) != 0 {
		t.Fatalf("no watchy: must not spawn: %v", f.started)
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "no ollama server reachable") {
		t.Fatalf("want no-server warning: %v", f.notes)
	}
}

func TestServerNoOllamaBinaryFallsBack(t *testing.T) {
	f := newFakeBootstrap()
	delete(f.ollamaPath, "ollama")
	f.up[defaultOllamaHost] = true
	ensureDedicatedServer(f.asBootstrap())
	if len(f.started) != 0 {
		t.Fatal("no ollama binary: must not spawn")
	}
	if f.env["OLLAMA_HOST"] != defaultOllamaHost {
		t.Fatalf("want fallback to shared server, env=%q", f.env["OLLAMA_HOST"])
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "ollama binary") {
		t.Fatalf("want binary-missing note: %v", f.notes)
	}
}
