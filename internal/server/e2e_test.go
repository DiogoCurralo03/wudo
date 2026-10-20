package server_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/italoalmeida0/wudo/internal/client"
	"github.com/italoalmeida0/wudo/internal/server"
	"github.com/italoalmeida0/wudo/internal/state"
)

// newTestServer starts a service on a loopback port. The endpoint file is
// written to a per-test directory so the tests never touch the real one.
func newTestServer(t *testing.T) *server.Server {
	t.Helper()
	t.Setenv(state.DirEnvVar, t.TempDir())
	srv := server.New(server.Config{})
	t.Cleanup(srv.Shutdown)
	if err := state.EnsureDir(); err != nil {
		t.Fatalf("ensure state dir: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start service: %v", err)
	}
	return srv
}

// TestRunPipes executes a command without a pseudo console and checks that
// stdout, stderr and the exit code all arrive intact.
func TestRunPipes(t *testing.T) {
	srv := newTestServer(t)
	srv.Activate(time.Minute)

	var stdout, stderr bytes.Buffer
	res, err := client.Run(context.Background(), client.Options{
		Argv:   []string{"cmd", "/c", "echo out-line & echo err-line 1>&2 & exit /b 7"},
		Shell:  true,
		Tty:    false,
		Cwd:    t.TempDir(),
		Stdin:  strings.NewReader(""),
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", res.ExitCode)
	}
	if !strings.Contains(stdout.String(), "out-line") {
		t.Errorf("stdout = %q, want it to contain out-line", stdout.String())
	}
	if !strings.Contains(stderr.String(), "err-line") {
		t.Errorf("stderr = %q, want it to contain err-line", stderr.String())
	}
}

// TestRunTty executes a command inside a pseudo console.
func TestRunTty(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a pseudo console")
	}
	srv := newTestServer(t)
	srv.Activate(time.Minute)

	var stdout bytes.Buffer
	res, err := client.Run(context.Background(), client.Options{
		Argv:   []string{"cmd", "/d", "/s", "/c", "echo tty-line & exit /b 3"},
		Shell:  true,
		Tty:    true,
		Cwd:    t.TempDir(),
		Stdin:  strings.NewReader(""),
		Stdout: &stdout,
		Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(stdout.String(), "tty-line") {
		t.Errorf("stdout = %q, want it to contain tty-line", stdout.String())
	}
}

// TestRunStdin feeds data to a child's standard input.
func TestRunStdin(t *testing.T) {
	srv := newTestServer(t)
	srv.Activate(time.Minute)

	var stdout bytes.Buffer
	res, err := client.Run(context.Background(), client.Options{
		Argv:   []string{"cmd", "/c", "sort"},
		Shell:  true,
		Cwd:    t.TempDir(),
		Stdin:  strings.NewReader("piped-input\r\n\x1a"),
		Stdout: &stdout,
		Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(stdout.String(), "piped-input") {
		t.Errorf("stdout = %q, want it to contain piped-input", stdout.String())
	}
}

// TestInactiveServiceRefusesCommands checks the activation gate.
func TestInactiveServiceRefusesCommands(t *testing.T) {
	newTestServer(t)

	_, err := client.Run(context.Background(), client.Options{
		Argv:   []string{"cmd", "/c", "echo nope"},
		Shell:  true,
		Cwd:    t.TempDir(),
		Stdin:  strings.NewReader(""),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("run succeeded, want an inactive service error")
	}
	if !strings.Contains(err.Error(), "not active") {
		t.Errorf("error = %v, want it to mention that the service is not active", err)
	}
}

// TestConcurrentCommands makes sure several commands run at the same time
// without interfering with each other.
func TestConcurrentCommands(t *testing.T) {
	srv := newTestServer(t)
	srv.Activate(time.Minute)

	const parallel = 6
	results := make(chan error, parallel)
	for i := 0; i < parallel; i++ {
		go func(i int) {
			var stdout bytes.Buffer
			res, err := client.Run(context.Background(), client.Options{
				Argv:   []string{"cmd", "/c", "ping -n 2 127.0.0.1 >nul & echo done"},
				Shell:  true,
				Cwd:    t.TempDir(),
				Stdin:  strings.NewReader(""),
				Stdout: &stdout,
				Stderr: &bytes.Buffer{},
			})
			if err != nil {
				results <- err
				return
			}
			if res.ExitCode != 0 {
				results <- err
				return
			}
			if !strings.Contains(stdout.String(), "done") {
				results <- err
				return
			}
			results <- nil
		}(i)
	}
	for i := 0; i < parallel; i++ {
		if err := <-results; err != nil {
			t.Fatalf("concurrent run failed: %v", err)
		}
	}
	waitForSessions(t, srv, 0)
}

// TestDeactivateKillsCommands checks that deactivation terminates running
// commands instead of leaving them behind.
func TestDeactivateKillsCommands(t *testing.T) {
	srv := newTestServer(t)
	srv.Activate(time.Minute)

	done := make(chan error, 1)
	go func() {
		_, err := client.Run(context.Background(), client.Options{
			Argv:   []string{"ping -n 30 127.0.0.1 >nul"},
			Shell:  true,
			Cwd:    t.TempDir(),
			Stdin:  strings.NewReader(""),
			Stdout: &bytes.Buffer{},
			Stderr: &bytes.Buffer{},
		})
		done <- err
	}()

	deadline := time.Now().Add(10 * time.Second)
	for srv.SessionCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if srv.SessionCount() == 0 {
		t.Fatal("command never registered")
	}

	srv.Deactivate()
	select {
	case err := <-done:
		if err != nil {
			t.Logf("run returned %v (expected when the command is killed)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("command survived deactivation")
	}
	waitForSessions(t, srv, 0)
}

// waitForSessions polls until the service reports want running commands. The
// unregister happens right after the client's exit message, so a short wait is
// always needed.
func waitForSessions(t *testing.T, srv *server.Server, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if srv.SessionCount() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("%d sessions registered, want %d", srv.SessionCount(), want)
}

func TestMain(m *testing.M) {
	code := m.Run()
	state.RemoveEndpoint()
	os.Exit(code)
}
