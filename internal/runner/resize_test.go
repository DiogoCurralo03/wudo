package runner_test

import (
	"strings"
	"testing"
	"time"

	"github.com/italoalmeida0/wudo/internal/runner"
)

// TestResizeReachesChild checks the whole resize path: the session is created
// with one console size, resized afterwards, and the child is asked to report
// what it sees.
func TestResizeReachesChild(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a pseudo console")
	}
	s, err := runner.Start(runner.Options{
		Argv: []string{`C:\Windows\System32\cmd.exe`, "/d", "/k"},
		Tty:  true,
		Cols: 100,
		Rows: 30,
		ID:   "resize",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if s.Mode != "pty" {
		s.Terminate()
		t.Skipf("pseudo console not available (mode %q)", s.Mode)
	}

	// Let the interpreter print its banner before touching the console.
	time.Sleep(700 * time.Millisecond)
	if err := s.Resize(132, 43); err != nil {
		t.Fatalf("resize: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	s.WriteStdin([]byte("mode con\r\n"))
	time.Sleep(500 * time.Millisecond)
	s.WriteStdin([]byte("exit\r\n"))

	info := s.Wait()
	var out strings.Builder
	for chunk := range s.Output() {
		out.Write(chunk.Data)
	}
	if info.Code != 0 {
		t.Logf("child exited with %d (%s)", info.Code, info.Reason)
	}

	got := out.String()
	if !strings.Contains(got, "132") {
		t.Errorf("child did not report the new column count:\n%q", got)
	}
	if !strings.Contains(got, "43") {
		t.Errorf("child did not report the new line count:\n%q", got)
	}
}

// TestResizeIsNoopInPipeMode checks that resizing a pipe session is harmless.
func TestResizeIsNoopInPipeMode(t *testing.T) {
	s, err := runner.Start(runner.Options{
		Argv: []string{"cmd", "/c", "echo hi"},
		Tty:  false,
		ID:   "pipe-resize",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.Resize(132, 43); err != nil {
		t.Errorf("resize in pipe mode returned %v, want nil", err)
	}
	s.Wait()
	for range s.Output() {
	}
}
