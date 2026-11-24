package agent

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/ollama/ollama/api"
)

// modelRetryDelays is the backoff between resends of a model request that
// failed transiently; its length caps the retries per model step.
var modelRetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// isToolCallParseError reports a server-side failure to parse the model's
// tool-call output. Resending the same request would likely fail the same
// way, so the model is asked to correct itself instead.
func isToolCallParseError(err api.StatusError) bool {
	if err.StatusCode < 500 {
		return false
	}
	msg := strings.ToLower(err.ErrorMessage)
	return strings.Contains(msg, "tool call") && (strings.Contains(msg, "pars") || strings.Contains(msg, "invalid"))
}

// isTransientModelError reports failures worth resending unchanged: server
// errors, rate limits, timeouts, and dropped connections.
func isTransientModelError(err error) bool {
	var statusErr api.StatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusRequestTimeout, http.StatusTooManyRequests:
			return true
		}
		return statusErr.StatusCode >= 500
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
