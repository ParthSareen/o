// Package launch: trimmed shim containing only the pieces the agent harness
// TUI needs. Extracted from github.com/ParthSareen/o/cmd/launch (launch.go,
// account.go, models.go, selector_hooks.go); the full launch package with its
// integration runners is intentionally not part of this repo.
package launch

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// ErrCancelled is returned when the user cancels a selection.
var ErrCancelled = errors.New("cancelled")

// DefaultUpgradeURL is the fixed destination for subscription upgrades.
const DefaultUpgradeURL = "https://ollama.com/upgrade"

// ErrPlanVerificationUnavailable indicates the plan could not be verified.
var ErrPlanVerificationUnavailable = errors.New("Could not verify Ollama plan. Try again in a moment or use a local model.")

type accountStateStatus int

const (
	accountStateUnknown accountStateStatus = iota
	accountStateSignedOut
	accountStateSignedIn
)

// AccountState is a minimal snapshot of the signed-in account.
type AccountState struct {
	Status accountStateStatus
	Plan   string
}

// PlanSatisfies reports whether currentPlan meets requiredPlan.
func PlanSatisfies(currentPlan, requiredPlan string) bool {
	required := normalizePlan(requiredPlan)
	if required == "" || required == "free" {
		return true
	}
	current := normalizePlan(currentPlan)
	return current != "" && current != "free"
}

func normalizePlan(plan string) string {
	return strings.ToLower(strings.TrimSpace(plan))
}

type ConfirmDefault int

const (
	ConfirmDefaultYes ConfirmDefault = iota
	ConfirmDefaultNo
)

// ConfirmOptions customizes labels for confirmation prompts.
type ConfirmOptions struct {
	YesLabel string
	NoLabel  string
	Default  ConfirmDefault
}

// LauncherState is the snapshot used to render the root launcher menu.
type LauncherState struct {
	LastSelection  string
	RunModel       string
	RunModelUsable bool
	Integrations   map[string]LauncherIntegrationState
	AccountState   *AccountState
}

// LauncherIntegrationState is the status for one launcher integration.
type LauncherIntegrationState struct {
	Name            string
	DisplayName     string
	Description     string
	Installed       bool
	AutoInstallable bool
	Selectable      bool
	Changeable      bool
	CurrentModel    string
	ModelUsable     bool
	InstallHint     string
	Editor          bool
}

// RunModelRequest controls how the root launcher resolves the chat model.
type RunModelRequest struct {
	ForcePicker bool
}

// IntegrationLaunchRequest controls the integration launcher flow.
type IntegrationLaunchRequest struct {
	Name           string
	ModelOverride  string
	ForceConfigure bool
	ConfigureOnly  bool
	Restore        bool
	ExtraArgs      []string
}

// SelectionItem represents a model row with selector-only UI state.
type SelectionItem struct {
	Name              string
	Description       string
	Recommended       bool
	AvailabilityBadge string
}

// OpenBrowser opens the URL in the user's browser.
func OpenBrowser(url string) {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("open", url).Start()
	case "linux":
		// Skip on headless systems where no display server is available
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		_ = exec.Command("xdg-open", url).Start()
	case "windows":
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
}
