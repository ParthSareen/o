// Package config persists the CLI's local model selection.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ParthSareen/o/cmd/internal/fileutil"
)

// integration is retained so existing config files keep their integration
// entries when the last model is saved.
type integration struct {
	Models    []string          `json:"models"`
	Aliases   map[string]string `json:"aliases,omitempty"`
	Onboarded bool              `json:"onboarded,omitempty"`
}

type config struct {
	Integrations  map[string]*integration `json:"integrations"`
	LastModel     string                  `json:"last_model,omitempty"`
	LastSelection string                  `json:"last_selection,omitempty"`
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ollama", "config.json"), nil
}

func legacyConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ollama", "config", "config.json"), nil
}

// migrateConfig moves the config from the legacy path to ~/.ollama/config.json
func migrateConfig() (bool, error) {
	oldPath, err := legacyConfigPath()
	if err != nil {
		return false, err
	}

	oldData, err := os.ReadFile(oldPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	// Ignore legacy files with invalid JSON and continue startup.
	if !json.Valid(oldData) {
		return false, nil
	}

	newPath, err := configPath()
	if err != nil {
		return false, err
	}

	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(newPath, oldData, 0o644); err != nil {
		return false, fmt.Errorf("write new config: %w", err)
	}

	_ = os.Remove(oldPath)
	_ = os.Remove(filepath.Dir(oldPath)) // clean up empty directory

	return true, nil
}

func load() (*config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil && os.IsNotExist(err) {
		if migrated, merr := migrateConfig(); merr == nil && migrated {
			data, err = os.ReadFile(path)
		}
	}
	if err != nil {
		if os.IsNotExist(err) {
			return &config{Integrations: make(map[string]*integration)}, nil
		}
		return nil, err
	}

	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w, at: %s", err, path)
	}
	if cfg.Integrations == nil {
		cfg.Integrations = make(map[string]*integration)
	}
	return &cfg, nil
}

func save(cfg *config) error {
	path, err := configPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	return fileutil.WriteWithBackup(path, data)
}

// LastModel returns the last model that was run, or empty string if none.
func LastModel() string {
	cfg, err := load()
	if err != nil {
		return ""
	}
	return cfg.LastModel
}

// SetLastModel saves the last model that was run.
func SetLastModel(model string) error {
	cfg, err := load()
	if err != nil {
		return err
	}
	cfg.LastModel = model
	return save(cfg)
}
