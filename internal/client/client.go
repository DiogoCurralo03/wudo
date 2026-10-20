// Package client implements the unprivileged side of wudo: it finds the running
// service, checks that it is active, and then runs exactly one command over a
// WebSocket while streaming stdin, stdout and stderr.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/italoalmeida0/wudo/internal/protocol"
	"github.com/italoalmeida0/wudo/internal/state"
	"github.com/italoalmeida0/wudo/internal/version"
	"github.com/italoalmeida0/wudo/internal/winproc"
)

// Errors returned to the caller.
var (
	// ErrNotInstalled means no service is known on this machine, either because
	// it never ran or because the endpoint file it left behind is stale.
	ErrNotInstalled = errors.New("wudo service is not running")
	// ErrNotActive means the service is running but currently disabled.
	ErrNotActive = errors.New("wudo service is not active")
	// ErrUnreachable means the service could not be contacted.
	ErrUnreachable = errors.New("wudo service is not reachable")
	// ErrAborted means the connection dropped before the command finished.
	ErrAborted = errors.New("connection to the wudo service was lost")
)

// Options describes one client invocation.
type Options struct {
	// Argv is the command and its arguments.
	Argv []string
	// Shell runs the joined Argv through cmd.exe.
	Shell bool
	// Cwd is the working directory the command must run in.
	Cwd string
	// Tty requests a pseudo console.
	Tty bool
	// Timeout kills the command service side after this long.
	Timeout time.Duration
	// Stdin, Stdout and Stderr default to the process streams.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Interrupts delivers user interrupts (Ctrl+C) while the command runs. Each
	// value asks the service to interrupt the command the way Ctrl+C would.
	Interrupts <-chan struct{}
	// Verbose prints diagnostics on stderr.
	Verbose bool
}

// Result is what one command run produced.
type Result struct {
	ExitCode int
	Reason   string
	Elapsed  time.Duration
	PID      int
	Session  string
	Mode     string
	Command  string
}

// Status queries the service for its activation state. The endpoint file is read
// on every call, so a restarted service is picked up immediately.
func Status(ctx context.Context) (*protocol.Status, *state.Endpoint, error) {
	ep, err := state.ReadEndpoint()
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return nil, nil, ErrNotInstalled
		}
		return nil, nil, err
	}
	st, err := fetchStatus(ctx, ep)
	if err != nil {
		if stale(ep) {
			// The service is gone but left its discovery file behind: clean up
			// that one file so the next run reports "not running" instead of
			// "unreachable". Another endpoint file elsewhere is left alone.
			state.RemoveEndpointAt(ep.Dir)
			return nil, ep, ErrNotInstalled
		}
		return nil, ep, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return st, ep, nil
}

func fetchStatus(ctx context.Context, ep *state.Endpoint) (*protocol.Status, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", ep.Port, protocol.PathStatus)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := statusClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP %d from the status endpoint", resp.StatusCode)
	}
	var st protocol.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

var statusClient = &http.Client{Timeout: 5 * time.Second}

// terminateGrace is how long the client waits for the exit report after it asked
// the service to terminate the command.
const terminateGrace = 3 * time.Second

// clientKeepAliveInterval is how often the client pings the service. A single
// failed ping stops the client, because nothing else would ever unblock it.
var clientKeepAliveInterval = protocol.EnvDuration("WUDO_KEEPALIVE", 15*time.Second)

// stale reports whether the process that published the endpoint is gone.
func stale(ep *state.Endpoint) bool {
	if ep.PID <= 0 {
		return true
	}
	return !winproc.ProcessAlive(uint32(ep.PID))
}

// Run executes one command through the service and returns its result.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}

	ep, err := state.ReadEndpoint()
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return nil, ErrNotInstalled
		}
		return nil, err
	}

	// Ask before opening the WebSocket so an inactive service produces a clear
	// message instead of a failed upgrade.
	st, _, err := Status(ctx)
	if err != nil {
		return nil, err
	}
	if !st.Active {
		return nil, ErrNotActive
	}
	if opts.Verbose {
		fmt.Fprintf(opts.Stderr, "wudo: service pid %d on port %d (%s)\n", ep.PID, ep.Port, describeStatus(st))
	}

	cols, rows := 0, 0
	if opts.Tty {
		cols, rows = consoleSize()
	}

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	url := fmt.Sprintf("ws://127.0.0.1:%d%s", ep.Port, protocol.PathRun)
	conn, resp, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{
		HTTPClient:      &http.Client{},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusServiceUnavailable {
			return nil, ErrNotActive
		}
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)

	cwd := opts.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	start := protocol.StartRequest{
		Type:      protocol.TypeStart,
		Schema:    protocol.SchemaVersion,
		Argv:      opts.Argv,
		Shell:     opts.Shell,
		Cwd:       cwd,
		Env:       clientEnv(),
		Tty:       opts.Tty,
		Cols:      cols,
		Rows:      rows,
		TimeoutMs: opts.Timeout.Milliseconds(),
		Client: protocol.ClientInfo{
			Version: version.Short(),
			PID:     os.Getpid(),
			User:    winproc.CurrentUser(),
			TTY:     opts.Tty,
		},
	}
	if err := writeJSON(ctx, conn, start); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}

	// The first answer is either the acknowledgement or a fatal error.
	ackCtx, ackCancel := context.WithTimeout(ctx, 60*time.Second)
	typ, data, err := conn.Read(ackCtx)
	ackCancel()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	started, err := decodeStarted(typ, data)
	if err != nil {
		return nil, err
	}
	if opts.Verbose {
		fmt.Fprintf(opts.Stderr, "wudo: session %s, pid %d, mode %s, cwd %s\n",
			started.SessionID, started.PID, started.Mode, started.Cwd)
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	var writeMu sync.Mutex
	writeFrame := func(kind websocket.MessageType, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		c, cancel := context.WithTimeout(runCtx, 30*time.Second)
		defer cancel()
		return conn.Write(c, kind, payload)
	}

	result := &Result{
		PID:     started.PID,
		Session: started.SessionID,
		Mode:    started.Mode,
		Command: started.Command,
	}
	exitDone := make(chan struct{})
	var exitOnce sync.Once

	// Reader: child output plus the final exit message.
	readErr := make(chan error, 1)
	go func() {
		for {
			typ, data, err := conn.Read(runCtx)
			if err != nil {
				readErr <- err
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if len(data) == 0 {
					continue
				}
				switch data[0] {
				case protocol.ChanStdout:
					_, _ = opts.Stdout.Write(data[1:])
				case protocol.ChanStderr:
					_, _ = opts.Stderr.Write(data[1:])
				}
			case websocket.MessageText:
				var envelope struct {
					Type   string `json:"type"`
					Code   int    `json:"code"`
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal(data, &envelope); err != nil {
					continue
				}
				switch envelope.Type {
				case protocol.TypeExit:
					result.ExitCode = envelope.Code
					result.Reason = envelope.Reason
					exitOnce.Do(func() { close(exitDone) })
					return
				case protocol.TypeError, protocol.TypeNotice:
					var msg protocol.Error
					_ = json.Unmarshal(data, &msg)
					fmt.Fprintf(opts.Stderr, "wudo: %s\n", msg.Message)
				}
			}
		}
	}()

	// Stdin pump. A nil Interrupts channel simply leaves the case disabled.
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := opts.Stdin.Read(buf)
			if n > 0 {
				frame := make([]byte, n+1)
				frame[0] = protocol.ChanStdin
				copy(frame[1:], buf[:n])
				if werr := writeFrame(websocket.MessageBinary, frame); werr != nil {
					return
				}
			}
			if err != nil {
				// The caller's stdin reached EOF: tell the child, but keep the
				// session open so its output is still delivered.
				_ = writeFrame(websocket.MessageText, mustJSON(protocol.Simple{Type: protocol.TypeStdinEOF}))
				return
			}
		}
	}()

	// Console size changes (interactive terminals only).
	if opts.Tty {
		go watchResize(runCtx, writeFrame)
	}

	// Keepalive: if the service disappears without closing the socket, the ping
	// fails and the client stops instead of waiting forever.
	keepAliveFailed := make(chan struct{})
	go func() {
		ticker := time.NewTicker(clientKeepAliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				pingCtx, pingCancel := context.WithTimeout(runCtx, protocol.PingTimeout(clientKeepAliveInterval))
				err := conn.Ping(pingCtx)
				pingCancel()
				if err != nil {
					close(keepAliveFailed)
					return
				}
			}
		}
	}()

waitLoop:
	for {
		select {
		case <-exitDone:
			break waitLoop
		case err := <-readErr:
			// The connection ended without an exit message.
			if err != nil && ctx.Err() == nil {
				return result, fmt.Errorf("%w: %v", ErrAborted, err)
			}
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			break waitLoop
		case <-opts.Interrupts:
			// Forward the interrupt and keep waiting: the command decides
			// whether it wants to stop.
			_ = writeFrame(websocket.MessageText, mustJSON(protocol.Simple{Type: protocol.TypeInterrupt}))
			fmt.Fprintln(opts.Stderr)
			if opts.Verbose {
				fmt.Fprintln(opts.Stderr, "wudo: interrupt forwarded to the command")
			}
			// The channel is drained by the caller after the first signal, so
			// this case will not fire again.
		case <-keepAliveFailed:
			// The exit message may have arrived just before the connection went
			// quiet; that is a normal finish, not an abort.
			select {
			case <-exitDone:
				break waitLoop
			default:
			}
			runCancel()
			return result, fmt.Errorf("%w: the service stopped answering", ErrAborted)
		case <-ctx.Done():
			// The caller gave up (second Ctrl+C): ask the service to terminate
			// the command and give it a moment to report the exit code.
			_ = writeFrame(websocket.MessageText, mustJSON(protocol.Simple{Type: protocol.TypeTerminate}))
			select {
			case <-exitDone:
			case <-readErr:
			case <-time.After(terminateGrace):
			}
			runCancel()
			select {
			case <-stdinDone:
			case <-time.After(200 * time.Millisecond):
			}
			return result, context.Canceled
		}
	}

	runCancel()
	_ = conn.Close(websocket.StatusNormalClosure, "done")
	// The stdin pump may still be blocked reading the terminal; do not wait for
	// it, or an interactive session would hang until the next key press.
	select {
	case <-stdinDone:
	case <-time.After(200 * time.Millisecond):
	}
	return result, nil
}

// decodeStarted parses the first control frame and turns service errors into
// friendly Go errors.
func decodeStarted(typ websocket.MessageType, data []byte) (*protocol.Started, error) {
	if typ != websocket.MessageText {
		return nil, fmt.Errorf("unexpected first frame from service (type %d)", typ)
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("invalid first frame from service: %w", err)
	}
	switch envelope.Type {
	case protocol.TypeStarted:
		var st protocol.Started
		if err := json.Unmarshal(data, &st); err != nil {
			return nil, err
		}
		return &st, nil
	case protocol.TypeError:
		var e protocol.Error
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		switch e.Code {
		case protocol.CodeInactive:
			return nil, ErrNotActive
		case protocol.CodeShuttingDown:
			return nil, fmt.Errorf("%s", e.Message)
		default:
			return nil, errors.New(e.Message)
		}
	default:
		return nil, fmt.Errorf("unexpected message %q from service", envelope.Type)
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, data)
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"type":"noop"}`)
	}
	return data
}

// envKeys are the variables forwarded from the caller so that an elevated
// command behaves like one started in the caller's shell.
var envKeys = []string{
	"USERNAME", "USERDOMAIN", "USERPROFILE", "HOMEDRIVE", "HOMEPATH",
	"APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "PATHEXT", "PATH",
}

// clientEnv collects those variables from the client environment.
func clientEnv() []string {
	out := make([]string, 0, len(envKeys))
	for _, k := range envKeys {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

func describeStatus(st *protocol.Status) string {
	if !st.Active {
		return "inactive"
	}
	if st.ExpiresAt != nil {
		return fmt.Sprintf("active for another %s", (time.Duration(st.SecondsLeft) * time.Second).Truncate(time.Second))
	}
	return "active"
}
