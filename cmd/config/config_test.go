package config

import (
	"os"
	"path/filepath"
	"testing"
)

// setTestHome sets both HOME (Unix) and USERPROFILE (Windows) for cross-platform tests
func setTestHome(t *testing.T, dir string) {
	t.Setenv("HOME", dir)
	t.Setenv("TMPDIR", dir)
	t.Setenv("USERPROFILE", dir)
}

func TestConfigPath(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	path, err := configPath()
	if err != nil {
		t.Fatal(err)
	}

	expected := filepath.Join(tmpDir, ".ollama", "config.json")
	if path != expected {
		t.Errorf("expected %s, got %s", expected, path)
	}
}

func TestLoad(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	t.Run("returns empty config when file does not exist", func(t *testing.T) {
		cfg, err := load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg == nil {
			t.Fatal("expected non-nil config")
		}
		if cfg.Integrations == nil {
			t.Error("expected non-nil Integrations map")
		}
		if len(cfg.Integrations) != 0 {
			t.Errorf("expected empty Integrations, got %d", len(cfg.Integrations))
		}
	})

	t.Run("loads existing config", func(t *testing.T) {
		path, _ := configPath()
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(`{"integrations":{"test":{"models":["model-a"]}}}`), 0o644)

		cfg, err := load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Integrations["test"] == nil {
			t.Fatal("expected test integration")
		}
		if len(cfg.Integrations["test"].Models) != 1 {
			t.Errorf("expected 1 model, got %d", len(cfg.Integrations["test"].Models))
		}
	})

	t.Run("returns error for corrupted JSON", func(t *testing.T) {
		path, _ := configPath()
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(`{corrupted`), 0o644)

		_, err := load()
		if err == nil {
			t.Error("expected error for corrupted JSON")
		}
	})
}

func TestMigrateConfig(t *testing.T) {
	t.Run("migrates legacy file to new location", func(t *testing.T) {
		tmpDir := t.TempDir()
		setTestHome(t, tmpDir)

		legacyDir := filepath.Join(tmpDir, ".ollama", "config")
		os.MkdirAll(legacyDir, 0o755)
		data := []byte(`{"integrations":{"claude":{"models":["llama3.2"]}}}`)
		os.WriteFile(filepath.Join(legacyDir, "config.json"), data, 0o644)

		migrated, err := migrateConfig()
		if err != nil {
			t.Fatal(err)
		}
		if !migrated {
			t.Fatal("expected migration to occur")
		}

		newPath, _ := configPath()
		got, err := os.ReadFile(newPath)
		if err != nil {
			t.Fatalf("new config not found: %v", err)
		}
		if string(got) != string(data) {
			t.Errorf("content mismatch: got %s", got)
		}

		if _, err := os.Stat(filepath.Join(legacyDir, "config.json")); !os.IsNotExist(err) {
			t.Error("legacy file should have been removed")
		}

		if _, err := os.Stat(legacyDir); !os.IsNotExist(err) {
			t.Error("legacy directory should have been removed")
		}
	})

	t.Run("no-op when no legacy file exists", func(t *testing.T) {
		tmpDir := t.TempDir()
		setTestHome(t, tmpDir)

		migrated, err := migrateConfig()
		if err != nil {
			t.Fatal(err)
		}
		if migrated {
			t.Error("expected no migration")
		}
	})

	t.Run("skips corrupt legacy file", func(t *testing.T) {
		tmpDir := t.TempDir()
		setTestHome(t, tmpDir)

		legacyDir := filepath.Join(tmpDir, ".ollama", "config")
		os.MkdirAll(legacyDir, 0o755)
		os.WriteFile(filepath.Join(legacyDir, "config.json"), []byte(`{corrupt`), 0o644)

		migrated, err := migrateConfig()
		if err != nil {
			t.Fatal(err)
		}
		if migrated {
			t.Error("should not migrate corrupt file")
		}

		if _, err := os.Stat(filepath.Join(legacyDir, "config.json")); os.IsNotExist(err) {
			t.Error("corrupt legacy file should not have been deleted")
		}
	})

	t.Run("new path takes precedence over legacy", func(t *testing.T) {
		tmpDir := t.TempDir()
		setTestHome(t, tmpDir)

		legacyDir := filepath.Join(tmpDir, ".ollama", "config")
		os.MkdirAll(legacyDir, 0o755)
		os.WriteFile(filepath.Join(legacyDir, "config.json"), []byte(`{"integrations":{"old":{"models":["old-model"]}}}`), 0o644)

		newDir := filepath.Join(tmpDir, ".ollama")
		os.WriteFile(filepath.Join(newDir, "config.json"), []byte(`{"integrations":{"new":{"models":["new-model"]}}}`), 0o644)

		cfg, err := load()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := cfg.Integrations["new"]; !ok {
			t.Error("expected new-path integration to be loaded")
		}
		if _, ok := cfg.Integrations["old"]; ok {
			t.Error("legacy integration should not have been loaded")
		}
	})

	t.Run("idempotent when called twice", func(t *testing.T) {
		tmpDir := t.TempDir()
		setTestHome(t, tmpDir)

		legacyDir := filepath.Join(tmpDir, ".ollama", "config")
		os.MkdirAll(legacyDir, 0o755)
		os.WriteFile(filepath.Join(legacyDir, "config.json"), []byte(`{"integrations":{}}`), 0o644)

		if _, err := migrateConfig(); err != nil {
			t.Fatal(err)
		}

		migrated, err := migrateConfig()
		if err != nil {
			t.Fatal(err)
		}
		if migrated {
			t.Error("second migration should be a no-op")
		}
	})

	t.Run("legacy directory preserved if not empty", func(t *testing.T) {
		tmpDir := t.TempDir()
		setTestHome(t, tmpDir)

		legacyDir := filepath.Join(tmpDir, ".ollama", "config")
		os.MkdirAll(legacyDir, 0o755)
		os.WriteFile(filepath.Join(legacyDir, "config.json"), []byte(`{"integrations":{}}`), 0o644)
		os.WriteFile(filepath.Join(legacyDir, "other-file.txt"), []byte("keep me"), 0o644)

		if _, err := migrateConfig(); err != nil {
			t.Fatal(err)
		}

		if _, err := os.Stat(legacyDir); os.IsNotExist(err) {
			t.Error("directory with other files should not have been removed")
		}
		if _, err := os.Stat(filepath.Join(legacyDir, "other-file.txt")); os.IsNotExist(err) {
			t.Error("other files in legacy directory should be untouched")
		}
	})

	t.Run("save writes to new path after migration", func(t *testing.T) {
		tmpDir := t.TempDir()
		setTestHome(t, tmpDir)

		legacyDir := filepath.Join(tmpDir, ".ollama", "config")
		os.MkdirAll(legacyDir, 0o755)
		os.WriteFile(filepath.Join(legacyDir, "config.json"), []byte(`{"integrations":{"claude":{"models":["llama3.2"]}}}`), 0o644)

		// load triggers migration, then save should write to new path
		if err := SetLastModel("qwen2.5"); err != nil {
			t.Fatal(err)
		}

		newPath := filepath.Join(tmpDir, ".ollama", "config.json")
		if _, err := os.Stat(newPath); os.IsNotExist(err) {
			t.Error("save should write to new path")
		}

		// old path should not be recreated
		if _, err := os.Stat(filepath.Join(legacyDir, "config.json")); !os.IsNotExist(err) {
			t.Error("save should not recreate legacy path")
		}
	})

	t.Run("load triggers migration transparently", func(t *testing.T) {
		tmpDir := t.TempDir()
		setTestHome(t, tmpDir)

		legacyDir := filepath.Join(tmpDir, ".ollama", "config")
		os.MkdirAll(legacyDir, 0o755)
		os.WriteFile(filepath.Join(legacyDir, "config.json"), []byte(`{"integrations":{"claude":{"models":["llama3.2"]}}}`), 0o644)

		cfg, err := load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Integrations["claude"] == nil || cfg.Integrations["claude"].Models[0] != "llama3.2" {
			t.Error("migration via load() did not preserve data")
		}
	})
}

func TestSave(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)

	t.Run("creates config file", func(t *testing.T) {
		cfg := &config{
			Integrations: map[string]*integration{
				"test": {Models: []string{"model-a", "model-b"}},
			},
		}

		if err := save(cfg); err != nil {
			t.Fatal(err)
		}

		path, _ := configPath()
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Error("config file was not created")
		}
	})

	t.Run("round-trip preserves data", func(t *testing.T) {
		cfg := &config{
			Integrations: map[string]*integration{
				"claude": {Models: []string{"llama3.2", "mistral"}},
				"codex":  {Models: []string{"qwen2.5"}},
			},
		}

		if err := save(cfg); err != nil {
			t.Fatal(err)
		}

		loaded, err := load()
		if err != nil {
			t.Fatal(err)
		}

		if len(loaded.Integrations) != 2 {
			t.Errorf("expected 2 integrations, got %d", len(loaded.Integrations))
		}
		if loaded.Integrations["claude"] == nil {
			t.Error("missing claude integration")
		}
		if len(loaded.Integrations["claude"].Models) != 2 {
			t.Errorf("expected 2 models for claude, got %d", len(loaded.Integrations["claude"].Models))
		}
	})
}
