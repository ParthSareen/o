package main

import (
	"errors"
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
			f.notes = append(f.notes, format)
		},
	}
}

func TestServerExplicitHostUpIsRespected(t *testing.T) {
	f := newFakeBootstrap()
	f.env["OLLAMA_HOST"] = "http://example:1"
	f.up["http://example:1"] = true
	ensureDebugServer(f.asBootstrap())
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
	ensureDebugServer(f.asBootstrap())
	if len(f.started) != 0 {
		t.Fatalf("explicit host down must not spawn: %v", f.started)
	}
	if len(f.notes) != 1 {
		t.Fatalf("want one warning note: %v", f.notes)
	}
}

func TestServerDefaultRunningUntouched(t *testing.T) {
	f := newFakeBootstrap()
	f.up[defaultOllamaHost] = true
	ensureDebugServer(f.asBootstrap())
	if len(f.started) != 0 {
		t.Fatal("default server up: must not spawn or restart anything")
	}
	if _, ok := f.env["OLLAMA_HOST"]; ok {
		t.Fatal("env must stay unset when default server is up")
	}
	if len(f.notes) != 0 {
		t.Fatalf("quiet path expected: %v", f.notes)
	}
}

func TestServerReusesExistingDebugServer(t *testing.T) {
	f := newFakeBootstrap()
	f.up[debugOllamaHost] = true
	ensureDebugServer(f.asBootstrap())
	if len(f.started) != 0 {
		t.Fatal("existing debug server must not be restarted")
	}
	if f.env["OLLAMA_HOST"] != debugOllamaHost {
		t.Fatalf("env should point at debug server, got %q", f.env["OLLAMA_HOST"])
	}
}

func TestServerNoWatchyNoSpawn(t *testing.T) {
	f := newFakeBootstrap()
	f.lookPathErr["watchy"] = errors.New("not installed")
	ensureDebugServer(f.asBootstrap())
	if len(f.started) != 0 || len(f.notes) != 0 {
		t.Fatalf("no watchy → silent no-op: %v %v", f.started, f.notes)
	}
}

func TestServerNoOllamaBinaryWarns(t *testing.T) {
	f := newFakeBootstrap()
	delete(f.ollamaPath, "ollama")
	ensureDebugServer(f.asBootstrap())
	if len(f.started) != 0 {
		t.Fatal("no ollama binary: must not spawn")
	}
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "ollama binary") {
		t.Fatalf("want binary-missing warning: %v", f.notes)
	}
}

func TestServerSpawnsViaWatchyAndPointsEnv(t *testing.T) {
	f := newFakeBootstrap()
	// server becomes healthy after the spawn (watchy actually started it)
	f.readiness = func(host string) bool {
		return host == debugOllamaHost && len(f.started) > 0
	}
	ensureDebugServer(f.asBootstrap())
	if len(f.started) != 1 || f.started[0] != debugServerTask {
		t.Fatalf("want exactly one spawn of %q: %v", debugServerTask, f.started)
	}
	cmd := f.commands[0]
	for _, want := range []string{"OLLAMA_HOST=127.0.0.1:11433", "OLLAMA_DEBUG=1", "/usr/bin/ollama", "serve"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("spawn command %q missing %q", cmd, want)
		}
	}
	if f.env["OLLAMA_HOST"] != debugOllamaHost {
		t.Fatalf("env should point at debug server, got %q", f.env["OLLAMA_HOST"])
	}
	if len(f.notes) == 0 || !strings.Contains(f.notes[len(f.notes)-1], "watchy logs") {
		t.Fatalf("want note pointing at watchy logs: %v", f.notes)
	}
}

func TestServerSpawnNeverReadies(t *testing.T) {
	f := newFakeBootstrap()
	// probes stay down forever; short timeout keeps the test fast
	b := f.asBootstrap()
	b.readyTimeout = 10 * time.Millisecond
	ensureDebugServer(b)
	if _, ok := f.env["OLLAMA_HOST"]; ok {
		t.Fatal("do not point env at a server that never came up")
	}
	if len(f.notes) == 0 || !strings.Contains(f.notes[len(f.notes)-1], "did not come up") {
		t.Fatalf("want readiness-timeout warning: %v", f.notes)
	}
}

func TestServerSpawnFailureSurfaces(t *testing.T) {
	f := newFakeBootstrap()
	f.startTaskErr = errors.New("boom")
	ensureDebugServer(f.asBootstrap())
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "watchy") {
		t.Fatalf("want watchy-start failure warning: %v", f.notes)
	}
}
