package client

import (
	"context"
	"os"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sys/windows"
	"golang.org/x/term"

	"github.com/italoalmeida0/wudo/internal/protocol"
)

// consoleSize returns the size of the client's terminal, falling back to the
// classic 120x30 when there is no interactive console.
func consoleSize() (cols, rows int) {
	out := windows.Handle(os.Stdout.Fd())
	if c, r, err := consoleSizeOf(out); err == nil && c > 0 && r > 0 {
		return c, r
	}
	in := windows.Handle(os.Stdin.Fd())
	if c, r, err := consoleSizeOf(in); err == nil && c > 0 && r > 0 {
		return c, r
	}
	return 120, 30
}

func consoleSizeOf(h windows.Handle) (int, int, error) {
	if !isConsole(h) {
		return 0, 0, os.ErrInvalid
	}
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(h, &info); err != nil {
		return 0, 0, err
	}
	c := int(info.Window.Right-info.Window.Left) + 1
	r := int(info.Window.Bottom-info.Window.Top) + 1
	return c, r, nil
}

func isConsole(h windows.Handle) bool {
	var mode uint32
	return windows.GetConsoleMode(h, &mode) == nil
}

// IsTerminal reports whether the client's stdin is an interactive console.
func IsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// MakeRaw switches the client's terminal to raw mode and enables Win32 virtual
// terminal input, so keystrokes (including arrow keys and function keys) reach
// the remote console as ANSI sequences.
func MakeRaw() (restore func(), err error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return func() {}, nil
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() { term.Restore(fd, state) }, nil
}

// EnableStdoutVT turns on ANSI escape processing for the client's console so the
// remote console's output renders correctly.
func EnableStdoutVT() func() {
	h := windows.Handle(os.Stdout.Fd())
	if !isConsole(h) {
		return func() {}
	}
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return func() {}
	}
	const enableVirtualTerminalProcessing = 0x0004
	if mode&enableVirtualTerminalProcessing != 0 {
		return func() {}
	}
	if err := windows.SetConsoleMode(h, mode|enableVirtualTerminalProcessing); err != nil {
		return func() {}
	}
	return func() { windows.SetConsoleMode(h, mode) }
}

// watchResize polls the console size and reports changes.
func watchResize(ctx context.Context, write func(websocket.MessageType, []byte) error) {
	cols, rows := consoleSize()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c, r := consoleSize()
			if c == cols && r == rows {
				continue
			}
			cols, rows = c, r
			msg := protocol.Resize{Type: protocol.TypeResize, Cols: cols, Rows: rows}
			if err := write(websocket.MessageText, mustJSON(msg)); err != nil {
				return
			}
		}
	}
}
