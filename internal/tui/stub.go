//go:build !has_tui

// Package tui — stub for TUI-less builds.
//
// The real TUI (bubbletea + lipgloss + clipboard + charmbracelet glamour)
// adds ~500 KB to the binary and is only useful when the server is
// launched with a terminal attached. Most deployments run the service
// headlessly; this stub lets those builds drop the TUI dependencies
// entirely. Callers that hit the stub see a clear actionable error
// message rather than a silent no-op.
package tui

import (
	"errors"
	"io"

	log "github.com/sirupsen/logrus"
)

// errTUINotCompiled is the message surfaced to operators that built the
// server without the TUI and then asked for --tui mode. The rebuild hint
// keeps the remediation obvious.
var errTUINotCompiled = errors.New(
	"tui subcommand is not compiled in; rebuild with -tags=has_tui (or 'make slim') to enable it",
)

// Run reports the "not compiled in" error. main() checks the error and
// prints it before exiting with a non-zero code.
func Run(_ int, _ string, _ *LogHook, _ io.Writer) error {
	return errTUINotCompiled
}

// LogHook satisfies logrus.Hook so callers can still register it without
// checking build tags. Every call is a no-op.
type LogHook struct{}

// NewLogHook returns an empty hook. bufSize is accepted for signature
// compatibility with the real implementation.
func NewLogHook(_ int) *LogHook { return &LogHook{} }

// SetFormatter is a no-op in the stub; log records are never captured.
func (h *LogHook) SetFormatter(_ log.Formatter) {}

// Levels reports no levels, so logrus will skip calling Fire entirely.
// That's the cheapest possible disabled-hook implementation.
func (h *LogHook) Levels() []log.Level { return nil }

// Fire is never called in practice (Levels returns nil) but is required
// by the logrus.Hook interface.
func (h *LogHook) Fire(_ *log.Entry) error { return nil }

// Client is a placeholder so the --tui CLI command (if invoked) can still
// be parsed without build-tag juggling in the command layer.
type Client struct{}

// NewClient returns a stub. port and secretKey are accepted for signature
// compatibility.
func NewClient(_ int, _ string) *Client { return &Client{} }

// GetConfig is used by the bootstrap's TUI readiness probe; report the
// not-compiled-in error so the probe fails fast and the server prints a
// clear message instead of looping on a dead endpoint.
func (c *Client) GetConfig() (map[string]any, error) { return nil, errTUINotCompiled }

// SetSecretKey is a no-op.
func (c *Client) SetSecretKey(_ string) {}
