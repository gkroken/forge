// Package obs provides observability primitives: structured logging and
// Prometheus metrics. Both are initialised once at startup and then used
// throughout the codebase via the slog default logger and the Metrics struct.
package obs

import (
	"log/slog"
	"os"
)

// InitLog configures the global slog logger.
// format "text" → human-readable key=value (local dev).
// Any other value → newline-delimited JSON (production default).
func InitLog(format string) {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(os.Stderr, opts)
	} else {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}
