// Package server implements the privileged side of wudo: an HTTP API bound to
// the loopback interface that the client talks to.
//
// The server owns the activation state (the "service is active until ..."
// clock) and the list of running commands. Deactivating or quitting the service
// terminates every command it started, but each command also lives in its own
// job object, so one stuck child can never take the daemon (or another command)
// down with it.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/italoalmeida0/wudo/internal/protocol"
	"github.com/italoalmeida0/wudo/internal/runner"
	"github.com/italoalmeida0/wudo/internal/state"
	"github.com/italoalmeida0/wudo/internal/version"
	"github.com/italoalmeida0/wudo/internal/winproc"
)

// MaxSessions is the hard ceiling of concurrently running commands.
const MaxSessions = 64

// Config configures a Server.
type Config struct {
	// MaxSessions overrides MaxSessions when non-zero.
	MaxSessions int
	// IdleTimeout deactivates the service after this long without any command.
	// Zero disables the idle timer.
	IdleTimeout time.Duration
	// Logger receives the service log; nil disables logging.
	Logger *log.Logger
}

// Server is the wudo service.
type Server struct {
	cfg Config
	log *log.Logger

	mu        sync.Mutex
	active    bool
	expiresAt time.Time
	instance  string
	started   time.Time
	sessions  map[string]*runner.Session
	idleTimer *time.Timer

	httpSrv  *http.Server
	listener net.Listener
	port     int

	closed   bool
	doneCh   chan struct{}
	doneOnce sync.Once
}

// New creates a server; call Start to bind the port.
func New(cfg Config) *Server {
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = MaxSessions
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.New(discard{}, "", 0)
	}
	return &Server{
		cfg:      cfg,
		log:      logger,
		instance: newInstanceID(),
		started:  time.Now(),
		sessions: map[string]*runner.Session{},
		doneCh:   make(chan struct{}),
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func newInstanceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Instance returns the random identifier of this service run.
func (s *Server) Instance() string { return s.instance }

// Start binds a loopback port, starts serving and publishes endpoint.json.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("bind loopback port: %w", err)
	}
	s.listener = ln
	s.port = ln.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc(protocol.PathStatus, s.handleStatus)
	mux.HandleFunc(protocol.PathRun, s.handleRun)
	mux.HandleFunc("/", s.notFound)

	s.httpSrv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No read/write timeouts on purpose: a WebSocket connection carries a
		// long lived, possibly idle, command session.
	}

	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Printf("http server stopped: %v", err)
		}
	}()

	ep := &state.Endpoint{
		Schema:   state.SchemaVersion,
		Port:     s.port,
		PID:      osGetpid(),
		Instance: s.instance,
		Version:  version.Short(),
		User:     winproc.CurrentUser(),
		Elevated: winproc.IsElevated(),
		Started:  s.started,
	}
	if err := state.WriteEndpoint(ep); err != nil {
		s.Shutdown()
		return fmt.Errorf("publish endpoint: %w", err)
	}
	s.log.Printf("service listening on 127.0.0.1:%d (pid %d, instance %s)", s.port, ep.PID, s.instance)
	return nil
}

// Port returns the bound port (valid after Start).
func (s *Server) Port() int { return s.port }

// Done is closed when the server shut down.
func (s *Server) Done() <-chan struct{} { return s.doneCh }

// Shutdown stops the HTTP server, kills every running command and removes the
// endpoint file.
func (s *Server) Shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	sessions := make([]*runner.Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()

	for _, sess := range sessions {
		sess.Terminate()
	}

	if s.httpSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.httpSrv.Shutdown(ctx)
		cancel()
	}
	state.RemoveEndpoint()
	s.log.Printf("service stopped")
	s.doneOnce.Do(func() { close(s.doneCh) })
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown endpoint"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// Status reports the current activation state.
func (s *Server) Status() protocol.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
}

func (s *Server) statusLocked() protocol.Status {
	st := protocol.Status{
		Schema:        protocol.SchemaVersion,
		Version:       version.Short(),
		PID:           osGetpid(),
		Instance:      s.instance,
		Started:       s.started,
		Active:        s.active,
		Sessions:      len(s.sessions),
		MaxSessions:   s.cfg.MaxSessions,
		Elevated:      winproc.IsElevated(),
		CurrentUser:   winproc.CurrentUser(),
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
	}
	if s.active && !s.expiresAt.IsZero() {
		exp := s.expiresAt
		st.ExpiresAt = &exp
		left := int64(time.Until(exp).Seconds())
		if left < 0 {
			left = 0
		}
		st.SecondsLeft = left
	}
	return st
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET"})
		return
	}
	s.expireIfNeeded()
	writeJSON(w, http.StatusOK, s.Status())
}

// Activate turns the service on for the given duration.
func (s *Server) Activate(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		// Shutting down: activating again would be a lie.
		return
	}
	s.active = true
	s.expiresAt = time.Now().Add(d)
	s.resetIdleLocked()
	s.log.Printf("activated until %s", s.expiresAt.Format(time.RFC3339))
}

// Deactivate turns the service off and terminates every running command.
func (s *Server) Deactivate() {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return
	}
	s.active = false
	s.expiresAt = time.Time{}
	sessions := make([]*runner.Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.mu.Unlock()

	for _, sess := range sessions {
		sess.Terminate()
	}
	s.log.Printf("deactivated; terminated %d running command(s)", len(sessions))
}

// IsActive reports whether commands may be executed right now.
func (s *Server) IsActive() bool {
	s.expireIfNeeded()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// ExpiresAt returns the activation deadline, or the zero time when inactive.
func (s *Server) ExpiresAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return time.Time{}
	}
	return s.expiresAt
}

func (s *Server) expireIfNeeded() {
	s.mu.Lock()
	if s.active && !s.expiresAt.IsZero() && time.Now().After(s.expiresAt) {
		s.active = false
		s.expiresAt = time.Time{}
		s.log.Printf("activation expired")
	}
	s.mu.Unlock()
}

// resetIdleLocked re-arms the idle timer that deactivates an unused service.
func (s *Server) resetIdleLocked() {
	if s.cfg.IdleTimeout <= 0 || !s.active {
		return
	}
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	s.idleTimer = time.AfterFunc(s.cfg.IdleTimeout, func() {
		s.mu.Lock()
		idle := s.active && len(s.sessions) == 0
		s.mu.Unlock()
		if idle {
			s.log.Printf("no command for %s; deactivating", s.cfg.IdleTimeout)
			s.Deactivate()
		}
	})
}

// SessionCount returns the number of running commands.
func (s *Server) SessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *Server) registerSession(sess *runner.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("service is shutting down")
	}
	if !s.active {
		return errNotActive
	}
	if len(s.sessions) >= s.cfg.MaxSessions {
		return errTooManySessions
	}
	s.sessions[sess.ID] = sess
	s.resetIdleLocked()
	return nil
}

func (s *Server) unregisterSession(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.resetIdleLocked()
	s.mu.Unlock()
}

var (
	errNotActive       = errors.New("service is not active")
	errTooManySessions = fmt.Errorf("too many concurrent commands (limit %d)", MaxSessions)
)

// summarizeArgv renders a command for the log without dumping huge arguments.
func summarizeArgv(argv []string) string {
	s := strings.Join(argv, " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
