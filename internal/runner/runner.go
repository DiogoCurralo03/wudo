// Package runner starts one child process per wudo command, either inside a
// Windows pseudo console (interactive mode) or with plain pipes (non
// interactive mode), and exposes a uniform handle to it.
//
// Every child is created suspended, placed in its own job object and only then
// resumed. That guarantees that a process which dies instantly, and the whole
// tree it may spawn, can always be found and killed as a unit without touching
// any other session or the service itself.
package runner

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/italoalmeida0/wudo/internal/conpty"
	"github.com/italoalmeida0/wudo/internal/protocol"
	"github.com/italoalmeida0/wudo/internal/spawn"
	"github.com/italoalmeida0/wudo/internal/winproc"
)

// chunkSize is the size of one read from the child.
const chunkSize = 32 * 1024

// outputBuffer is how many output chunks may be queued before the reader
// goroutine has to wait for the consumer.
const outputBuffer = 1024

// drainQuiet is how long the runner keeps reading after the child exited
// before it tears the console down. ConPTY does not signal EOF when the child
// exits, so trailing output is collected by waiting for a short silence.
const drainQuiet = 150 * time.Millisecond

// drainMax bounds the total post-exit drain time.
const drainMax = 2 * time.Second

// Options describes the command to run.
type Options struct {
	// Argv is the command plus its arguments, exactly as the user typed them.
	Argv []string
	// Shell runs Argv joined by spaces as a single command line through cmd.exe.
	Shell bool
	// Cwd is the working directory; empty means the service's own directory.
	Cwd string
	// Env holds "NAME=value" overrides applied on top of the service
	// environment; nil means "inherit unchanged".
	Env []string
	// Tty requests a pseudo console instead of pipes.
	Tty bool
	// Cols and Rows size the pseudo console in character cells.
	Cols int
	Rows int
	// Timeout kills the command after this long; zero means no limit.
	Timeout time.Duration
	// ID identifies the session in the log and in the exit report.
	ID string
}

// Chunk is one piece of child output.
type Chunk struct {
	// Stderr reports whether the data came from the child's standard error.
	// Always false in TTY mode, where the console merges both streams.
	Stderr bool
	// Data is the payload; the slice is owned by the receiver.
	Data []byte
}

// ExitInfo describes how a command finished.
type ExitInfo struct {
	Code    int
	Reason  string
	Elapsed time.Duration
}

// Session is a running (or finished) child process.
type Session struct {
	ID      string
	PID     int
	Mode    string // "pty" or "pipe"
	Command string // resolved command line, for the log
	Cwd     string
	Started time.Time

	out      chan Chunk
	in       chan []byte
	done     chan struct{}
	abandon  chan struct{}
	readDone chan struct{}
	exitSeen chan struct{}
	exitMu   sync.Mutex
	exitInfo ExitInfo

	proc windows.Handle
	job  *winproc.Job
	pty  *conpty.Console
	inH  *conpty.Pipe
	outH *conpty.Pipe
	errH *conpty.Pipe

	closeOnce   sync.Once
	abandonOnce sync.Once
	writeMu     sync.Mutex
	resizeMu    sync.Mutex
	cols        int16
	rows        int16

	reasonMu sync.Mutex
	reason   string

	bytesRead atomic.Int64
	timedOut  atomic.Bool
	killed    atomic.Bool
	exited    atomic.Bool
}

// Start launches the child described by opts.
func Start(opts Options) (*Session, error) {
	if len(opts.Argv) == 0 && !opts.Shell {
		return nil, errors.New("no command given")
	}

	var cmdline string
	if opts.Shell {
		cmdline = spawn.BuildShellCommandLine(strings.Join(opts.Argv, " "))
	} else {
		resolved, err := spawn.Resolve(opts.Argv[0], opts.Cwd)
		if err != nil {
			return nil, err
		}
		cmdline = spawn.BuildCommandLine(resolved, opts.Argv[1:])
	}

	s := &Session{
		ID:       opts.ID,
		Command:  cmdline,
		Cwd:      opts.Cwd,
		Started:  time.Now(),
		out:      make(chan Chunk, outputBuffer),
		in:       make(chan []byte, 256),
		done:     make(chan struct{}),
		abandon:  make(chan struct{}),
		readDone: make(chan struct{}),
		exitSeen: make(chan struct{}),
		cols:     int16(min(max(opts.Cols, 0), 500)),
		rows:     int16(min(max(opts.Rows, 0), 500)),
		reason:   protocol.ReasonExited,
	}

	job, err := winproc.NewJob()
	if err != nil {
		return nil, err
	}
	s.job = job

	if opts.Tty && conpty.Available() {
		s.Mode = "pty"
		err = s.startPty(cmdline, opts)
	} else {
		s.Mode = "pipe"
		err = s.startPipes(cmdline, opts)
	}
	if err != nil {
		s.releaseHandles()
		job.Close()
		return nil, err
	}

	go s.readLoop()
	go s.writeLoop()
	go s.waitLoop()
	if opts.Timeout > 0 {
		go s.timeoutLoop(opts.Timeout)
	}
	return s, nil
}

func (s *Session) startPty(cmdline string, opts Options) error {
	size := conpty.Size{Cols: s.cols, Rows: s.rows}
	if size.Cols <= 0 || size.Rows <= 0 {
		size = conpty.DefaultSize
	}
	c, err := conpty.New(size)
	if err != nil {
		return err
	}
	s.pty = c
	s.outH = c.Out
	// The write side of the child's stdin is owned by the session.
	s.inH = c.In
	s.cols, s.rows = size.Cols, size.Rows

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fmt.Errorf("attribute list: %w", err)
	}
	defer attrs.Delete()
	hpc := c.ConsoleHandle()
	// The attribute value has to be the handle itself, not a pointer to it.
	// Going through a pointer-to-pointer conversion keeps the value intact
	// without converting a uintptr straight to unsafe.Pointer.
	hpcValue := *(*unsafe.Pointer)(unsafe.Pointer(&hpc))
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, hpcValue, unsafe.Sizeof(hpc)); err != nil {
		return fmt.Errorf("attach pseudo console: %w", err)
	}

	si := &windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			// Present but with null handles: conhost fills them in with the
			// pseudo console ends instead of inheriting the service's console.
			Flags: windows.STARTF_USESTDHANDLES,
		},
		ProcThreadAttributeList: attrs.List(),
	}
	pi, err := s.createProcess(cmdline, opts, &si.StartupInfo)
	if err != nil {
		return err
	}
	s.finishStart(pi)
	return nil
}

func (s *Session) startPipes(cmdline string, opts Options) error {
	var inRead, inWrite, outRead, outWrite, errRead, errWrite windows.Handle
	opened := func(hs ...*windows.Handle) {
		for _, h := range hs {
			if *h != 0 && *h != windows.InvalidHandle {
				windows.CloseHandle(*h)
				*h = 0
			}
		}
	}
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		opened(&inRead, &inWrite)
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := windows.CreatePipe(&errRead, &errWrite, nil, 0); err != nil {
		opened(&inRead, &inWrite, &outRead, &outWrite)
		return fmt.Errorf("stderr pipe: %w", err)
	}

	childEnds := []windows.Handle{inRead, outWrite, errWrite}
	for _, h := range childEnds {
		if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			opened(&inRead, &inWrite, &outRead, &outWrite, &errRead, &errWrite)
			return fmt.Errorf("mark handles inheritable: %w", err)
		}
	}

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		opened(&inRead, &inWrite, &outRead, &outWrite, &errRead, &errWrite)
		return fmt.Errorf("attribute list: %w", err)
	}
	defer attrs.Delete()
	if err := attrs.Update(
		windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&childEnds[0]),
		uintptr(len(childEnds))*unsafe.Sizeof(childEnds[0]),
	); err != nil {
		opened(&inRead, &inWrite, &outRead, &outWrite, &errRead, &errWrite)
		return fmt.Errorf("restrict inherited handles: %w", err)
	}

	si := &windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:        uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags:     windows.STARTF_USESTDHANDLES,
			StdInput:  inRead,
			StdOutput: outWrite,
			StdErr:    errWrite,
		},
		ProcThreadAttributeList: attrs.List(),
	}
	pi, err := s.createProcess(cmdline, opts, &si.StartupInfo)
	// The child owns its three ends from now on, whether it started or not.
	opened(&inRead, &outWrite, &errWrite)
	if err != nil {
		opened(&inWrite, &outRead, &errRead)
		return err
	}

	s.inH = conpty.NewPipe(inWrite)
	s.outH = conpty.NewPipe(outRead)
	s.errH = conpty.NewPipe(errRead)
	s.finishStart(pi)
	return nil
}

// createProcess builds the environment block, starts the child suspended,
// attaches it to the job object and resumes it.
func (s *Session) createProcess(cmdline string, opts Options, si *windows.StartupInfo) (*windows.ProcessInformation, error) {
	cmdlineUTF16, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, err
	}

	var dir *uint16
	if opts.Cwd != "" {
		if dir, err = windows.UTF16PtrFromString(opts.Cwd); err != nil {
			return nil, err
		}
	}

	envBlock, err := buildEnvBlock(opts.Env)
	if err != nil {
		return nil, err
	}

	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT |
		windows.CREATE_UNICODE_ENVIRONMENT |
		windows.CREATE_SUSPENDED)
	inherit := false
	if s.Mode == "pipe" {
		// No console of its own: a console application would otherwise flash a
		// window on the interactive desktop.
		flags |= windows.CREATE_NO_WINDOW
		inherit = true
	}

	pi := new(windows.ProcessInformation)
	if err := windows.CreateProcess(nil, cmdlineUTF16, nil, nil, inherit, flags, envBlock, dir, si, pi); err != nil {
		return nil, fmt.Errorf("CreateProcess: %w", err)
	}

	if err := s.job.Assign(pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Thread)
		windows.CloseHandle(pi.Process)
		return nil, err
	}

	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		s.job.Terminate(1)
		windows.CloseHandle(pi.Thread)
		windows.CloseHandle(pi.Process)
		return nil, fmt.Errorf("ResumeThread: %w", err)
	}
	return pi, nil
}

func (s *Session) finishStart(pi *windows.ProcessInformation) {
	s.proc = pi.Process
	s.PID = int(pi.ProcessId)
	windows.CloseHandle(pi.Thread)
}

// Output is the channel the child's output is delivered on. It is closed once
// the child is gone and every byte has been handed over.
func (s *Session) Output() <-chan Chunk { return s.out }

// Wait blocks until the child is gone and every byte of its output has been
// handed over, then returns how it finished. It can be called repeatedly and
// from several goroutines.
func (s *Session) Wait() ExitInfo {
	<-s.exitSeen
	s.exitMu.Lock()
	defer s.exitMu.Unlock()
	return s.exitInfo
}

// Done is closed when the session has been fully torn down.
func (s *Session) Done() <-chan struct{} { return s.done }

// Abandon tells the session that nobody is interested in its output any more.
// The reader goroutines stop queueing data instead of blocking forever.
func (s *Session) Abandon() {
	s.abandonOnce.Do(func() { close(s.abandon) })
}

// waitLoop waits for the child, drains trailing output, releases resources and
// publishes the exit information.
func (s *Session) waitLoop() {
	windows.WaitForSingleObject(s.proc, windows.INFINITE)
	var code uint32
	if err := windows.GetExitCodeProcess(s.proc, &code); err != nil {
		code = 1
	}
	s.exited.Store(true)

	// ConPTY keeps the output pipe open after the child exits, so wait for the
	// stream to go quiet and then tear the console down, which also unblocks
	// the reader goroutines.
	s.drain()
	s.closeQuietly()
	<-s.readDone

	info := ExitInfo{Code: int(code), Reason: s.exitReason(), Elapsed: time.Since(s.Started)}
	if s.timedOut.Load() {
		info.Code = protocol.ExitTimeout
		info.Reason = protocol.ReasonTimeout
	}
	if s.killed.Load() && info.Reason == protocol.ReasonExited {
		info.Reason = protocol.ReasonTerminated
	}
	s.exitMu.Lock()
	s.exitInfo = info
	s.exitMu.Unlock()
	close(s.exitSeen)
}

func (s *Session) exitReason() string {
	s.reasonMu.Lock()
	defer s.reasonMu.Unlock()
	if s.reason == "" {
		return protocol.ReasonExited
	}
	return s.reason
}

// SetReason records why the session is being stopped (interrupted, terminated,
// ...). It only takes effect when no stronger reason was recorded before.
func (s *Session) SetReason(reason string) { s.setReason(reason) }

func (s *Session) setReason(reason string) {
	if s.exited.Load() {
		return
	}
	s.reasonMu.Lock()
	if s.reason == protocol.ReasonExited || s.reason == "" {
		s.reason = reason
	}
	s.reasonMu.Unlock()
}

// drain waits until the child's output stops arriving, with a hard upper bound.
func (s *Session) drain() {
	deadline := time.Now().Add(drainMax)
	last := s.bytesRead.Load()
	lastChange := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
		cur := s.bytesRead.Load()
		if cur != last {
			last = cur
			lastChange = time.Now()
			continue
		}
		// A full queue means the consumer is just slow: keep waiting so that no
		// output is lost.
		if len(s.out) == cap(s.out) {
			lastChange = time.Now()
			continue
		}
		if time.Since(lastChange) >= drainQuiet {
			return
		}
	}
}

func (s *Session) readLoop() {
	defer close(s.readDone)
	defer close(s.out)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.pump(s.outH, false)
	}()
	if s.errH != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.pump(s.errH, true)
		}()
	}
	wg.Wait()
}

func (s *Session) pump(p *conpty.Pipe, stderr bool) {
	if p == nil {
		return
	}
	defer p.Close()
	buf := make([]byte, chunkSize)
	for {
		n, err := p.Read(buf)
		if n > 0 {
			s.bytesRead.Add(int64(n))
			data := make([]byte, n)
			copy(data, buf[:n])
			// Only an explicit abandonment may drop data: tearing the session
			// down (s.done) must never discard output that was already read.
			select {
			case s.out <- Chunk{Stderr: stderr, Data: data}:
			case <-s.abandon:
				return
			}
		}
		if err != nil {
			return
		}
		if n == 0 {
			// Never spin on an empty read.
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// writeLoop serializes everything that reaches the child's standard input. A
// nil slice is the "close the pipe" sentinel, which keeps the end of input
// strictly after the data that was queued before it.
func (s *Session) writeLoop() {
	for {
		select {
		case data := <-s.in:
			s.writeMu.Lock()
			if s.inH != nil {
				if data == nil {
					s.inH.Close()
					s.inH = nil
				} else {
					s.inH.Write(data)
				}
			}
			s.writeMu.Unlock()
		case <-s.done:
			return
		}
	}
}

func (s *Session) timeoutLoop(d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		s.timedOut.Store(true)
		s.Terminate()
	case <-s.done:
	}
}

// WriteStdin feeds the child's standard input. The data is copied, so the
// caller may reuse its buffer immediately.
func (s *Session) WriteStdin(data []byte) {
	if len(data) == 0 || s.exited.Load() {
		return
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case s.in <- cp:
	case <-s.done:
	}
}

// CloseStdin signals end of input to the child.
//
// In pipe mode the write end is closed, which is how a child reading from a pipe
// sees EOF. In TTY mode the input channel of a pseudo console cannot be closed
// without killing the whole console session, so an end of input is simply
// ignored: an interactive program keeps running until it decides to exit, which
// is exactly what happens when a local terminal stops sending keystrokes.
func (s *Session) CloseStdin() {
	if s.Mode != "pipe" {
		return
	}
	select {
	case s.in <- nil:
	case <-s.done:
	}
}

// Resize changes the pseudo console size. It is a no-op in pipe mode.
func (s *Session) Resize(cols, rows int) error {
	if s.Mode != "pty" || s.pty == nil {
		return nil
	}
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("invalid console size %dx%d", cols, rows)
	}
	s.resizeMu.Lock()
	defer s.resizeMu.Unlock()
	if err := s.pty.Resize(conpty.Size{Cols: int16(cols), Rows: int16(rows)}); err != nil {
		return err
	}
	s.cols, s.rows = int16(cols), int16(rows)
	return nil
}

// Interrupt delivers Ctrl+C semantics. On a pseudo console the ETX character is
// written to the input, which conhost turns into a console control event, so the
// child can handle or ignore it exactly like a local Ctrl+C. In pipe mode there
// is no such channel, so the job is terminated instead.
func (s *Session) Interrupt() {
	s.setReason(protocol.ReasonInterrupted)
	if s.Mode == "pty" {
		s.writeMu.Lock()
		if s.inH != nil {
			s.inH.Write([]byte{0x03})
		}
		s.writeMu.Unlock()
		go func() {
			select {
			case <-time.After(3 * time.Second):
				if !s.exited.Load() {
					s.forceKill(protocol.ExitInterrupted)
				}
			case <-s.done:
			}
		}()
		return
	}
	s.forceKill(protocol.ExitInterrupted)
}

// Terminate kills every process in the session's job object.
func (s *Session) Terminate() { s.forceKill(protocol.ExitTerminated) }

func (s *Session) forceKill(code uint32) {
	s.killed.Store(true)
	if s.job != nil {
		s.job.Terminate(code)
	}
	if s.proc != 0 {
		windows.TerminateProcess(s.proc, code)
	}
}

// closeQuietly releases every resource the session owns, exactly once. Closing
// the pseudo console kills whatever is still attached to it, and closing the job
// object kills anything else the child spawned.
func (s *Session) closeQuietly() {
	s.closeOnce.Do(func() {
		if s.pty != nil {
			s.pty.Close()
		}
		// Close the input pipe directly: the writer goroutine is about to stop
		// and may never consume a queued close sentinel.
		s.writeMu.Lock()
		if s.inH != nil {
			s.inH.Close()
			s.inH = nil
		}
		s.writeMu.Unlock()
		if s.proc != 0 {
			windows.CloseHandle(s.proc)
			s.proc = 0
		}
		if s.job != nil {
			s.job.Close()
		}
		close(s.done)
	})
}

// releaseHandles closes the pipe handles when the child never started.
func (s *Session) releaseHandles() {
	if s.pty != nil {
		s.pty.Close()
	}
	for _, p := range []*conpty.Pipe{s.inH, s.outH, s.errH} {
		if p != nil {
			p.Close()
		}
	}
}

// buildEnvBlock renders the environment block handed to the child: the
// service's own environment with the client's variables applied on top, so the
// command behaves as if it had been launched from the caller's shell.
func buildEnvBlock(clientEnv []string) (*uint16, error) {
	env := map[string]string{}
	var order []string
	add := func(kv string) {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			return
		}
		if _, seen := env[name]; !seen {
			order = append(order, name)
		}
		env[name] = value
	}
	for _, kv := range os.Environ() {
		add(kv)
	}
	for _, kv := range clientEnv {
		add(kv)
	}

	// System variables must describe the elevated environment, never the
	// caller's, or an elevated process would load the wrong DLLs.
	for _, name := range []string{"SystemRoot", "windir", "SystemDrive", "ComSpec"} {
		sysValue, _ := windows.Getenv(name)
		if name == "ComSpec" && sysValue == "" {
			sysValue = winproc.ComSpec()
		}
		if sysValue == "" {
			continue
		}
		if _, seen := env[name]; !seen {
			order = append(order, name)
		}
		env[name] = sysValue
	}

	sort.SliceStable(order, func(i, j int) bool {
		return strings.ToUpper(order[i]) < strings.ToUpper(order[j])
	})

	block := make([]uint16, 0, 4096)
	for _, name := range order {
		// StringToUTF16 already appends a terminator, which is exactly the
		// separator between two environment entries. Adding another one would
		// end the block after the first variable.
		block = append(block, windows.StringToUTF16(name+"="+env[name])...)
	}
	block = append(block, 0)
	if len(block) == 1 {
		return nil, nil
	}
	return &block[0], nil
}
