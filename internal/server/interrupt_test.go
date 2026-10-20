package server_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/italoalmeida0/wudo/internal/client"
)

// TestInterruptPipeCommand checks that an interrupt request stops a command that
// runs without a pseudo console.
func TestInterruptPipeCommand(t *testing.T) {
	srv := newTestServer(t)
	srv.Activate(time.Minute)

	interrupts := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := client.Run(context.Background(), client.Options{
			Argv:       []string{"cmd", "/c", "ping -n 60 127.0.0.1 >nul"},
			Shell:      true,
			Cwd:        t.TempDir(),
			Stdin:      strings.NewReader(""),
			Stdout:     &bytes.Buffer{},
			Stderr:     &bytes.Buffer{},
			Interrupts: interrupts,
		})
		done <- err
	}()

	waitForSessions(t, srv, 1)
	interrupts <- struct{}{}

	select {
	case err := <-done:
		if err != nil {
			t.Logf("client returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the command survived the interrupt")
	}
	waitForSessions(t, srv, 0)
}

// TestInterruptPtyCommand checks that a pseudo console command can be
// interrupted, which is what a local Ctrl+C does.
func TestInterruptPtyCommand(t *testing.T) {
	srv := newTestServer(t)
	srv.Activate(time.Minute)

	interrupts := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := client.Run(context.Background(), client.Options{
			Argv:       []string{"cmd", "/d", "/c", "ping -n 60 127.0.0.1"},
			Shell:      true,
			Tty:        true,
			Cwd:        t.TempDir(),
			Stdin:      strings.NewReader(""),
			Stdout:     &bytes.Buffer{},
			Stderr:     &bytes.Buffer{},
			Interrupts: interrupts,
		})
		done <- err
	}()

	waitForSessions(t, srv, 1)
	interrupts <- struct{}{}

	select {
	case err := <-done:
		if err != nil {
			t.Logf("client returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the pseudo console command survived the interrupt")
	}
	waitForSessions(t, srv, 0)
}

// TestClientAbortKillsCommand checks that dropping the connection terminates the
// command instead of leaving an elevated process behind.
func TestClientAbortKillsCommand(t *testing.T) {
	srv := newTestServer(t)
	srv.Activate(time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := client.Run(ctx, client.Options{
			Argv:   []string{"cmd", "/c", "ping -n 60 127.0.0.1 >nul"},
			Shell:  true,
			Cwd:    t.TempDir(),
			Stdin:  strings.NewReader(""),
			Stdout: &bytes.Buffer{},
			Stderr: &bytes.Buffer{},
		})
		done <- err
	}()

	waitForSessions(t, srv, 1)
	cancel()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the client did not return after it was cancelled")
	}
	waitForSessions(t, srv, 0)
}
