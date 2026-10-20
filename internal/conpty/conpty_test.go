// Package conpty_test verifies the pseudo console wrapper on this machine. The
// tests are skipped automatically when ConPTY is unavailable.
package conpty_test

import (
	"strings"
	"sync"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/italoalmeida0/wudo/internal/conpty"
)

// TestAvailable reports whether ConPTY can be used here.
func TestAvailable(t *testing.T) {
	if !conpty.Available() {
		t.Skip("ConPTY is not available on this Windows version")
	}
}

// TestPipeRoundTrip writes to a pipe and reads the same bytes back.
func TestPipeRoundTrip(t *testing.T) {
	var r, w windows.Handle
	if err := windows.CreatePipe(&r, &w, nil, 0); err != nil {
		t.Fatalf("CreatePipe: %v", err)
	}
	out := conpty.NewPipe(r)
	in := conpty.NewPipe(w)
	defer out.Close()
	defer in.Close()

	payload := []byte("hello pipe")
	if n, err := in.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	buf := make([]byte, 64)
	n, err := out.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Errorf("read %q, want %q", buf[:n], payload)
	}
}

// TestPseudoConsoleRunsChild starts cmd.exe inside a pseudo console and checks
// that its output and exit code arrive.
func TestPseudoConsoleRunsChild(t *testing.T) {
	if !conpty.Available() {
		t.Skip("ConPTY is not available on this Windows version")
	}
	if testing.Short() {
		t.Skip("spawns a child process")
	}

	c, err := conpty.New(conpty.Size{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatalf("create pseudo console: %v", err)
	}

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		t.Fatalf("attribute list: %v", err)
	}
	defer attrs.Delete()
	hpc := c.ConsoleHandle()
	hpcValue := *(*unsafe.Pointer)(unsafe.Pointer(&hpc))
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, hpcValue, unsafe.Sizeof(hpc)); err != nil {
		t.Fatalf("attach pseudo console: %v", err)
	}

	si := &windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:    uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags: windows.STARTF_USESTDHANDLES,
		},
		ProcThreadAttributeList: attrs.List(),
	}
	cmdline, err := windows.UTF16PtrFromString(`C:\Windows\System32\cmd.exe /d /s /c "echo pty-child-ok & exit /b 5"`)
	if err != nil {
		t.Fatalf("utf16: %v", err)
	}
	pi := new(windows.ProcessInformation)
	if err := windows.CreateProcess(nil, cmdline, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		nil, nil, &si.StartupInfo, pi); err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)

	var mu sync.Mutex
	var out []byte
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := c.Out.Read(buf)
			if n > 0 {
				mu.Lock()
				out = append(out, buf[:n]...)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	if _, err := windows.WaitForSingleObject(pi.Process, 15000); err != nil {
		t.Fatalf("wait: %v", err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
		t.Fatalf("exit code: %v", err)
	}
	// Closing the console is what ends the output stream.
	c.Close()
	wg.Wait()

	if code != 5 {
		t.Errorf("exit code = %d, want 5", code)
	}
	mu.Lock()
	text := string(out)
	mu.Unlock()
	if !strings.Contains(text, "pty-child-ok") {
		t.Errorf("output = %q, want it to contain pty-child-ok", text)
	}
}
