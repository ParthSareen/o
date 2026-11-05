// Package launch: trimmed shim containing only the pieces the agent harness
// TUI needs. Extracted from github.com/ParthSareen/o/cmd/launch (launch.go,
// account.go, models.go, selector_hooks.go); the full launch package with its
// integration runners is intentionally not part of this repo.
package launch

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// DefaultUpgradeURL is the fixed destination for subscription upgrades.
const DefaultUpgradeURL = "https://ollama.com/upgrade"

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
