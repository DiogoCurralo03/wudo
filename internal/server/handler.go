package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/italoalmeida0/wudo/internal/protocol"
	"github.com/italoalmeida0/wudo/internal/runner"
	"github.com/italoalmeida0/wudo/internal/winproc"
)

// handleRun upgrades the connection and executes one command.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET"})
		return
	}
	// A browser on the same machine would send an Origin header; the CLI never
	// does. Refusing known web origins keeps random pages from driving the
	// service, without introducing any authentication.
	if r.Header.Get("Origin") != "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin requests are not allowed"})
		return
	}
	if !s.IsActive() {
		writeJSON(w, http.StatusServiceUnavailable, protocol.Error{
			Type:    protocol.TypeError,
			Code:    protocol.CodeInactive,
			Message: "wudo service is not active",
		})
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Printf("websocket upgrade failed: %v", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// The first message must be the start request.
	req, err := readStart(ctx, conn)
	if err != nil {
		s.log.Printf("malformed start request from %s: %v", r.RemoteAddr, err)
		writeControl(ctx, conn, protocol.Error{
			Type:    protocol.TypeError,
			Code:    protocol.CodeBadRequest,
			Message: err.Error(),
		})
		return
	}
	if req.Schema != protocol.SchemaVersion {
		writeControl(ctx, conn, protocol.Error{
			Type:    protocol.TypeError,
			Code:    protocol.CodeBadRequest,
			Message: fmt.Sprintf("incompatible protocol version %d (service speaks %d)", req.Schema, protocol.SchemaVersion),
		})
		return
	}

	opts := runner.Options{
		Argv:  req.Argv,
		Shell: req.Shell,
		Cwd:   winproc.SafeDir(req.Cwd),
		Env:   req.Env,
		Tty:   req.Tty,
		Cols:  req.Cols,
		Rows:  req.Rows,
		ID:    newInstanceID(),
	}
	if req.TimeoutMs > 0 {
		opts.Timeout = time.Duration(req.TimeoutMs) * time.Millisecond
	}

	sess, err := runner.Start(opts)
	if err != nil {
		s.log.Printf("failed to start %q: %v", summarizeArgv(req.Argv), err)
		writeControl(ctx, conn, protocol.Error{
			Type:    protocol.TypeError,
			Code:    protocol.CodeSpawnFailed,
			Message: err.Error(),
		})
		return
	}
	if err := s.registerSession(sess); err != nil {
		sess.Terminate()
		writeControl(ctx, conn, protocol.Error{
			Type:    protocol.TypeError,
			Code:    protocol.CodeBusy,
			Message: err.Error(),
		})
		return
	}
	defer s.unregisterSession(sess.ID)

	s.log.Printf("session %s started (pid %d, %s, tty=%v) in %q: %s",
		sess.ID, sess.PID, sess.Mode, req.Tty, opts.Cwd, sess.Command)

	if err := writeControl(ctx, conn, protocol.Started{
		Type:      protocol.TypeStarted,
		SessionID: sess.ID,
		PID:       sess.PID,
		Mode:      sess.Mode,
		Command:   sess.Command,
		Cwd:       opts.Cwd,
	}); err != nil {
		sess.Terminate()
		return
	}

	// Reader side: client input and control messages.
	go s.readClient(ctx, cancel, conn, sess)

	// Keepalive: a client that disappears without closing the socket (cable
	// pulled, laptop suspended, process frozen) must not leave the command
	// running forever. A failed ping cancels the context, which tears the
	// session down exactly like a clean disconnect.
	go s.keepAlive(ctx, cancel, conn, sess)

	// Writer side: child output until the session ends.
	err = s.streamOutput(ctx, conn, sess)
	if err != nil {
		// The client went away: the command it asked for goes with it. Abandon
		// first, so the reader goroutines stop queueing instead of blocking on a
		// queue nobody drains any more.
		sess.Abandon()
		sess.Terminate()
	}
	info := sess.Wait()
	sess.Abandon()

	if err == nil {
		_ = writeControl(context.WithoutCancel(ctx), conn, protocol.Exit{
			Type:   protocol.TypeExit,
			Code:   info.Code,
			Reason: info.Reason,
		})
	}
	s.log.Printf("session %s finished: exit=%d reason=%s after %s",
		sess.ID, info.Code, info.Reason, info.Elapsed.Round(time.Millisecond))
	conn.Close(websocket.StatusNormalClosure, "done")
}

// defaultKeepAliveInterval is how often the service asks the client for a pong.
// Two missed pongs end the session, so a silently dropped connection is noticed
// in about twice this interval. WUDO_KEEPALIVE overrides it.
const defaultKeepAliveInterval = 15 * time.Second

// keepAliveInterval reports the ping interval currently in effect. It is read on
// every session start instead of at package initialisation, so the value can be
// changed (tests, unusual setups) after the process started.
func keepAliveInterval() time.Duration {
	return protocol.EnvDuration("WUDO_KEEPALIVE", defaultKeepAliveInterval)
}

// keepAlive pings the client until the connection ends.
func (s *Server) keepAlive(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, sess *runner.Session) {
	interval := keepAliveInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, protocol.PingTimeout(interval))
			err := conn.Ping(pingCtx)
			pingCancel()
			if err == nil {
				misses = 0
				continue
			}
			misses++
			s.log.Printf("session %s: keepalive failed (%d): %v", sess.ID, misses, err)
			if misses >= 2 {
				sess.SetReason(protocol.ReasonTerminated)
				cancel()
				return
			}
		}
	}
}

// readStart reads and decodes the opening control frame.
func readStart(ctx context.Context, conn *websocket.Conn) (*protocol.StartRequest, error) {
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	typ, data, err := conn.Read(readCtx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, errors.New("the first message must be the JSON start request")
	}
	var req protocol.StartRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("invalid start request: %w", err)
	}
	if req.Type != protocol.TypeStart {
		return nil, fmt.Errorf("expected a %q message, got %q", protocol.TypeStart, req.Type)
	}
	return &req, nil
}

// streamOutput copies the child's output to the client until the output channel
// closes, which happens once the child is gone and every byte has been read.
func (s *Server) streamOutput(ctx context.Context, conn *websocket.Conn, sess *runner.Session) error {
	out := sess.Output()
	for {
		select {
		case chunk, ok := <-out:
			if !ok {
				return nil
			}
			channel := byte(protocol.ChanStdout)
			if chunk.Stderr {
				channel = protocol.ChanStderr
			}
			frame := make([]byte, len(chunk.Data)+1)
			frame[0] = channel
			copy(frame[1:], chunk.Data)
			writeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := conn.Write(writeCtx, websocket.MessageBinary, frame)
			cancel()
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// readClient applies client input and control messages to the session.
func (s *Server) readClient(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, sess *runner.Session) {
	defer cancel()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			// The client vanished: the command it asked for goes with it.
			sess.SetReason(protocol.ReasonTerminated)
			sess.Terminate()
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if len(data) == 0 {
				continue
			}
			switch data[0] {
			case protocol.ChanStdin, protocol.ChanStderrAlias:
				sess.WriteStdin(data[1:])
			default:
				s.log.Printf("session %s: ignoring unknown binary channel %d", sess.ID, data[0])
			}
		case websocket.MessageText:
			s.handleControl(conn, sess, data)
		default:
			// No other message types are part of the protocol.
		}
	}
}

func (s *Server) handleControl(conn *websocket.Conn, sess *runner.Session, data []byte) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		s.log.Printf("session %s: invalid control frame: %v", sess.ID, err)
		return
	}
	switch envelope.Type {
	case protocol.TypeResize:
		var msg protocol.Resize
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
		if err := sess.Resize(msg.Cols, msg.Rows); err != nil {
			s.log.Printf("session %s: resize to %dx%d failed: %v", sess.ID, msg.Cols, msg.Rows, err)
		}
	case protocol.TypeStdinEOF:
		sess.CloseStdin()
	case protocol.TypeInterrupt:
		s.log.Printf("session %s: interrupt requested by client", sess.ID)
		sess.Interrupt()
	case protocol.TypeTerminate:
		s.log.Printf("session %s: terminate requested by client", sess.ID)
		sess.SetReason(protocol.ReasonTerminated)
		sess.Terminate()
	default:
		s.log.Printf("session %s: unknown control message %q", sess.ID, envelope.Type)
	}
}

// writeControl sends one JSON control frame.
func writeControl(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, data)
}
