// Package protocol defines everything that travels between the wudo client and
// the wudo service: the HTTP endpoints, the JSON control messages and the
// one-byte channel prefix used on binary WebSocket frames.
package protocol

import (
	"os"
	"strings"
	"time"
)

// Wire protocol version. The client and the service must agree on it.
const SchemaVersion = 1

// HTTP paths served by the wudo service.
const (
	// PathStatus answers GET requests with a Status document.
	PathStatus = "/v1/status"
	// PathRun upgrades to a WebSocket and runs one command.
	PathRun = "/v1/run"
)

// Channels carried by binary WebSocket frames. The first byte of every binary
// frame is one of these; the rest is payload.
const (
	// ChanStdout carries the command's standard output (service -> client).
	ChanStdout byte = 0
	// ChanStderr carries the command's standard error (service -> client).
	// In TTY mode the child has a single stream, so output arrives as stdout.
	ChanStderr byte = 1
	// ChanStdin carries the client's standard input (client -> service).
	ChanStdin byte = 2
	// ChanStderrAlias is accepted on input as an alias for ChanStdin so that
	// older/alternative clients keep working.
	ChanStderrAlias byte = 3
)

// Message type discriminators used in JSON control frames.
const (
	// client -> service
	TypeStart     = "start"
	TypeResize    = "resize"
	TypeStdinEOF  = "stdin_eof"
	TypeInterrupt = "interrupt"
	TypeTerminate = "terminate"

	// service -> client
	TypeStarted = "started"
	TypeExit    = "exit"
	TypeError   = "error"
	TypeNotice  = "notice"
)

// Exit reasons reported by the service next to the exit code.
const (
	ReasonExited      = "exited"
	ReasonInterrupted = "interrupted"
	ReasonTerminated  = "terminated"
	ReasonTimeout     = "timeout"
	ReasonDeactivated = "deactivated"
	ReasonServiceExit = "service_exit"
)

// Exit codes synthesised by wudo itself (the child never produces these).
const (
	ExitInterrupted = 130
	ExitTerminated  = 143
	ExitTimeout     = 124
)

// Status is the JSON body of PathStatus.
type Status struct {
	Schema        int        `json:"schema"`
	Version       string     `json:"version"`
	PID           int        `json:"pid"`
	Instance      string     `json:"instance"`
	Started       time.Time  `json:"started"`
	Active        bool       `json:"active"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	SecondsLeft   int64      `json:"secondsLeft"`
	Sessions      int        `json:"sessions"`
	MaxSessions   int        `json:"maxSessions"`
	Elevated      bool       `json:"elevated"`
	CurrentUser   string     `json:"currentUser"`
	UptimeSeconds int64      `json:"uptimeSeconds"`
}

// StartRequest is the first control frame the client sends on PathRun.
type StartRequest struct {
	Type      string            `json:"type"`
	Schema    int               `json:"schema"`
	Argv      []string          `json:"argv"`
	Shell     bool              `json:"shell,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       []string          `json:"env,omitempty"`
	Tty       bool              `json:"tty"`
	Cols      int               `json:"cols,omitempty"`
	Rows      int               `json:"rows,omitempty"`
	TimeoutMs int64             `json:"timeoutMs,omitempty"`
	Client    ClientInfo        `json:"client"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// ClientInfo identifies the calling client in the service log.
type ClientInfo struct {
	Version string `json:"version"`
	PID     int    `json:"pid"`
	User    string `json:"user"`
	TTY     bool   `json:"tty"`
}

// Started is the service's acknowledgement that the child process is running.
type Started struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	PID       int    `json:"pid"`
	Mode      string `json:"mode"`    // "pty" or "pipe"
	Command   string `json:"command"` // fully resolved command line
	Cwd       string `json:"cwd"`
}

// Exit is the last control frame of a session.
type Exit struct {
	Type   string `json:"type"`
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

// Resize asks the service to change the pseudo console size.
type Resize struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// Simple is a control frame that only carries a type (stdin_eof, interrupt,
// terminate).
type Simple struct {
	Type string `json:"type"`
}

// Notice is a human readable hint from the service (never fatal).
type Notice struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Error is a fatal request error.
type Error struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes returned in Error.Code.
const (
	CodeInactive     = "service_inactive"
	CodeBusy         = "too_many_sessions"
	CodeBadRequest   = "bad_request"
	CodeSpawnFailed  = "spawn_failed"
	CodeUnsupported  = "unsupported"
	CodeShuttingDown = "shutting_down"
)

// PingTimeout returns how long a keepalive ping may wait for its pong. It is
// derived from the ping interval so that a short interval (tests, debugging)
// also means a short patience.
func PingTimeout(interval time.Duration) time.Duration {
	t := interval
	if t > 15*time.Second {
		t = 15 * time.Second
	}
	if t < time.Second {
		t = time.Second
	}
	return t
}

// EnvDuration reads a duration from the environment, falling back to def when
// the variable is missing or not a positive duration. It is a helper for the few
// timings that are worth tuning on slow or unusual setups.
func EnvDuration(name string, def time.Duration) time.Duration {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		return def
	}
	return d
}
