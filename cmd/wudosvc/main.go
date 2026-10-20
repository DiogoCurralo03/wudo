// Command wudosvc is the privileged wudo daemon. It lives in the Windows
// notification area, executes the commands requested by wudo.exe with
// administrator rights, and stays idle (and refusing every request) until the
// user activates it from its tray menu.
package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"github.com/italoalmeida0/wudo/internal/server"
	"github.com/italoalmeida0/wudo/internal/state"
	"github.com/italoalmeida0/wudo/internal/tray"
	"github.com/italoalmeida0/wudo/internal/version"
	"github.com/italoalmeida0/wudo/internal/winproc"
)

// instanceMutexName guards against two services fighting over the same port
// file. It lives in the local namespace because there is no cross-session use.
const instanceMutexName = `Local\wudo-service-instance`

func main() {
	os.Exit(run())
}

func run() int {
	mutex, alreadyRunning, err := acquireInstance()
	if err != nil {
		showFatal("wudo service", fmt.Sprintf("Cannot create the single instance guard:\n%v", err))
		return 1
	}
	if alreadyRunning {
		// Another instance owns the tray icon and the port file. Bring it into
		// the foreground instead of starting a second one.
		showFatal("wudo service",
			"Another wudo service is already running.\n\nUse its tray icon to activate or quit it.")
		return 0
	}
	defer windows.CloseHandle(mutex)

	if err := state.EnsureDir(); err != nil {
		showFatal("wudo service", fmt.Sprintf("Cannot create %s:\n%v", state.Dir(), err))
		return 1
	}

	logger, closeLog := openLog()
	defer closeLog()

	elevated := winproc.IsElevated()
	logger.Printf("wudo service %s starting (elevated=%v, user=%s)", version.String(), elevated, winproc.CurrentUser())

	svc := &service{
		logger:   logger,
		elevated: elevated,
	}
	if !elevated {
		logger.Printf("warning: the service is not elevated; commands will run with the current user's rights")
	}

	cfg := server.Config{Logger: logger}
	if idle, ok := idleTimeout(); ok {
		cfg.IdleTimeout = idle
		logger.Printf("idle timeout: the service deactivates itself after %s without commands", idle)
	}
	svc.srv = server.New(cfg)
	if err := svc.srv.Start(); err != nil {
		logger.Printf("failed to start: %v", err)
		showFatal("wudo service", fmt.Sprintf("The wudo service could not start:\n%v", err))
		return 1
	}

	if !elevated && !quiet() {
		// Starting without elevation is a mistake worth flagging once, but the
		// service still starts so the tray icon exists and the mistake is
		// visible in the status line.
		go showFatalWarning(
			"wudo service",
			"The wudo service was started without administrator privileges.\n\n"+
				"It will start inactive and its commands will NOT run elevated.\n"+
				"Close it from the tray icon and start it again with \"Run as administrator\".",
		)
	}

	// WUDO_AUTOACTIVATE lets an unattended setup (or the test suite) start the
	// service already active. It is only honoured when whoever launched the
	// service set it, so it does not widen the activation gate.
	if d, ok := autoActivate(); ok {
		svc.Activate(d)
		logger.Printf("activated at startup for %s (WUDO_AUTOACTIVATE)", d)
	}

	logger.Printf("service ready on port %d; activate it from the tray icon", svc.srv.Port())
	tray.Run(svc)

	svc.srv.Shutdown()
	logger.Printf("wudo service exited")
	return 0
}

// service adapts the server to what the tray needs.
type service struct {
	srv      *server.Server
	logger   *log.Logger
	elevated bool

	quitOnce sync.Once
}

// State implements tray.Controller.
func (s *service) State() tray.State {
	st := s.srv.Status()
	out := tray.State{
		Active:   st.Active,
		Sessions: st.Sessions,
		Elevated: s.elevated,
		Port:     s.srv.Port(),
		Version:  st.Version,
	}
	if st.ExpiresAt != nil {
		out.ExpiresAt = *st.ExpiresAt
	}
	return out
}

// Activate implements tray.Controller.
func (s *service) Activate(d time.Duration) {
	s.srv.Activate(d)
	s.logger.Printf("activated for %s from the tray menu", d)
}

// Deactivate implements tray.Controller.
func (s *service) Deactivate() {
	s.srv.Deactivate()
	s.logger.Printf("deactivated from the tray menu")
}

// Quit implements tray.Controller.
func (s *service) Quit() {
	s.quitOnce.Do(func() {
		s.logger.Printf("quit requested from the tray menu")
		s.srv.Shutdown()
	})
}

// LogPath implements tray.Controller.
func (s *service) LogPath() string { return state.LogPath() }

// Dir implements tray.Controller.
func (s *service) Dir() string { return state.Dir() }

// acquireInstance creates and takes the single instance mutex.
func acquireInstance() (windows.Handle, bool, error) {
	name, err := windows.UTF16PtrFromString(instanceMutexName)
	if err != nil {
		return 0, false, err
	}
	h, err := windows.CreateMutex(nil, false, name)
	if err == nil {
		return h, false, nil
	}
	// The wrapper reports a pre-existing mutex as ERROR_ALREADY_EXISTS while
	// still handing back a usable handle.
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return h, true, nil
	}
	return 0, false, err
}

// openLog opens the service log next to the port file, falling back to a
// discard writer when the file cannot be opened.
func openLog() (*log.Logger, func()) {
	if f, err := os.OpenFile(state.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		rotateIfBig(f)
		return log.New(io.MultiWriter(f), "", log.LstdFlags|log.Lmicroseconds), func() { f.Close() }
	}
	return log.New(io.Discard, "", 0), func() {}
}

// rotateIfBig truncates the log once it grows past a megabyte, so a long lived
// service does not fill the disk.
func rotateIfBig(f *os.File) {
	const limit = 1 << 20
	if info, err := f.Stat(); err == nil && info.Size() > limit {
		_ = f.Truncate(0)
		_, _ = f.Seek(0, io.SeekStart)
	}
}

// showFatal reports a startup problem the user has to see.
func showFatal(title, text string) {
	if quiet() {
		return
	}
	_ = winproc.MessageBox(title, text, winproc.MBOK|winproc.MBIconError|winproc.MBSetForeground)
}

// showFatalWarning reports a non fatal problem the user should notice.
func showFatalWarning(title, text string) {
	if quiet() {
		return
	}
	_ = winproc.MessageBox(title, text, winproc.MBOK|winproc.MBIconWarning|winproc.MBSetForeground)
}

// idleTimeout parses WUDO_IDLE_TIMEOUT, which deactivates the service after the
// given period without any command. An empty value disables it.
func idleTimeout() (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv("WUDO_IDLE_TIMEOUT"))
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// autoActivate parses WUDO_AUTOACTIVATE, which starts the service already
// active for the given duration.
func autoActivate() (time.Duration, bool) {
	raw := strings.TrimSpace(os.Getenv("WUDO_AUTOACTIVATE"))
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// quiet reports whether message boxes were disabled with WUDO_QUIET=1. It is
// used by the test suite and by unattended deployments.
func quiet() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WUDO_QUIET"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
