// Package conpty is a small, self-contained Windows pseudo console (ConPTY)
// wrapper built directly on golang.org/x/sys/windows.
//
// It deliberately does not hide the process handles: wudo needs to create the
// child suspended, put it in a job object and only then let it run, so the
// create/resume steps stay in the caller's hands.
package conpty

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrUnsupported is returned when the running Windows build has no ConPTY.
var ErrUnsupported = errors.New("conhost pseudo console API is not available on this Windows version (Windows 10 1809 or newer is required)")

var (
	modKernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procCreatePseudoConsole = modKernel32.NewProc("CreatePseudoConsole")
	procResizePseudoConsole = modKernel32.NewProc("ResizePseudoConsole")
	procClosePseudoConsole  = modKernel32.NewProc("ClosePseudoConsole")
)

// Available reports whether ConPTY can be used on this machine.
func Available() bool {
	return procCreatePseudoConsole.Find() == nil &&
		procResizePseudoConsole.Find() == nil &&
		procClosePseudoConsole.Find() == nil
}

func availableOrErr() error {
	if !Available() {
		return ErrUnsupported
	}
	return nil
}

// HPCON is the pseudo console handle returned by CreatePseudoConsole.
type HPCON windows.Handle

// Console is a pseudo console plus the pair of pipes that carry the child's
// input and output.
//
// Data flow:
//
//	Console.In     -> the child's stdin
//	Console.Out    <- the child's stdout/stderr (merged by conhost)
type Console struct {
	hpc HPCON

	// In is the write end the caller uses to feed the child's stdin.
	In *Pipe
	// Out is the read end the caller uses to read the child's output.
	Out *Pipe

	closeOnce sync.Once
}

// Size is the character-cell size of the pseudo console.
type Size struct {
	Cols int16
	Rows int16
}

func (s Size) coord() windows.Coord { return windows.Coord{X: s.Cols, Y: s.Rows} }

// DefaultSize is used when the caller does not know the real terminal size.
var DefaultSize = Size{Cols: 120, Rows: 30}

// Pipe wraps a Win32 pipe handle with io.ReadWriteCloser-ish helpers.
type Pipe struct {
	h    windows.Handle
	once sync.Once
}

// NewPipe wraps an already open Win32 handle.
func NewPipe(h windows.Handle) *Pipe { return &Pipe{h: h} }

// Handle exposes the raw Win32 handle.
func (p *Pipe) Handle() windows.Handle { return p.h }

// Read reads from the pipe. For the output side of a pseudo console the read
// only returns once the child produced data; it unblocks with an error after
// ClosePseudoConsole tore the console down.
func (p *Pipe) Read(b []byte) (int, error) {
	var n uint32
	err := windows.ReadFile(p.h, b, &n, nil)
	if n > 0 {
		return int(n), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// Write writes to the pipe.
func (p *Pipe) Write(b []byte) (int, error) {
	var written uint32
	err := windows.WriteFile(p.h, b, &written, nil)
	if written > 0 {
		return int(written), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// Close closes the underlying handle exactly once.
func (p *Pipe) Close() error {
	if p == nil || p.h == 0 || p.h == windows.InvalidHandle {
		return nil
	}
	var err error
	p.once.Do(func() {
		err = windows.CloseHandle(p.h)
		p.h = 0
	})
	return err
}

// New creates a pseudo console of the given size and the pipes that back it.
func New(size Size) (*Console, error) {
	if err := availableOrErr(); err != nil {
		return nil, err
	}
	if size.Cols <= 0 {
		size.Cols = DefaultSize.Cols
	}
	if size.Rows <= 0 {
		size.Rows = DefaultSize.Rows
	}

	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("CreatePipe (stdin): %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		return nil, fmt.Errorf("CreatePipe (stdout): %w", err)
	}

	var hpc windows.Handle
	r, _, e := procCreatePseudoConsole.Call(
		coordToUintptr(size.coord()),
		uintptr(inRead),
		uintptr(outWrite),
		0,
		uintptr(unsafe.Pointer(&hpc)),
	)
	if int32(r) != 0 {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		windows.CloseHandle(outWrite)
		return nil, fmt.Errorf("CreatePseudoConsole: 0x%x (%v)", r, e)
	}

	// The console owns its copies of the child ends now.
	windows.CloseHandle(inRead)
	windows.CloseHandle(outWrite)

	return &Console{
		hpc: HPCON(hpc),
		In:  &Pipe{h: inWrite},
		Out: &Pipe{h: outRead},
	}, nil
}

// ConsoleHandle returns the raw pseudo console handle.
func (c *Console) ConsoleHandle() windows.Handle { return windows.Handle(c.hpc) }

// Resize changes the pseudo console size and notifies the attached child.
func (c *Console) Resize(size Size) error {
	if c == nil || c.hpc == 0 {
		return errors.New("conpty: console already closed")
	}
	if err := availableOrErr(); err != nil {
		return err
	}
	if size.Cols <= 0 || size.Rows <= 0 {
		return fmt.Errorf("conpty: invalid size %dx%d", size.Cols, size.Rows)
	}
	r, _, e := procResizePseudoConsole.Call(uintptr(c.hpc), coordToUintptr(size.coord()))
	if int32(r) != 0 {
		return fmt.Errorf("ResizePseudoConsole: 0x%x (%v)", r, e)
	}
	return nil
}

// Close tears the pseudo console down, which terminates the attached child and
// makes reads on Out fail. Closing the console does not close the pipes; the
// caller still owns In and Out.
func (c *Console) Close() error {
	if c == nil || c.hpc == 0 {
		return nil
	}
	var err error
	c.closeOnce.Do(func() {
		if availableOrErr() == nil {
			procClosePseudoConsole.Call(uintptr(c.hpc))
		}
		c.hpc = 0
	})
	return err
}

func coordToUintptr(c windows.Coord) uintptr {
	return uintptr(uint32(uint16(c.X)) | uint32(uint16(c.Y))<<16)
}
