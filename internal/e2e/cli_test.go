// Package e2e exercises the real wudo.exe binary against a real service.
package e2e_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/italoalmeida0/wudo/internal/server"
	"github.com/italoalmeida0/wudo/internal/state"
)

// clientBinary returns the path of the built client, building it on demand.
func clientBinary(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	bin := filepath.Join(root, "bin", "wudo.exe")
	if _, err := os.Stat(bin); err == nil {
		return bin
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go.exe"), "build", "-o", bin, "./cmd/wudo")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build client: %v\n%s", err, out)
	}
	return bin
}

// repoRoot walks up from the test's directory until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate the repository root from %s", dir)
		}
		dir = parent
	}
}

// startService runs a service in-process and points both sides at a private
// state directory so the real C:\wudo is never touched.
func startService(t *testing.T) (*server.Server, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(state.DirEnvVar, dir)
	srv := server.New(server.Config{})
	if err := srv.Start(); err != nil {
		t.Fatalf("start service: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	return srv, dir
}

// runClient executes the real client binary and returns stdout, stderr and the
// exit code.
func runClient(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	bin := clientBinary(t)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), state.DirEnvVar+"="+dir, "WUDO_TTY=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = strings.NewReader("")
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run client: %v (stderr: %s)", err, stderr.String())
		}
	}
	return stdout.String(), stderr.String(), code
}

func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

// TestCLIRunPipes checks the plain, non interactive path end to end.
func TestCLIRunPipes(t *testing.T) {
	srv, dir := startService(t)
	srv.Activate(time.Minute)

	stdout, stderr, code := runClient(t, dir, "--shell", "echo cli-out & echo cli-err 1>&2 & exit /b 9")
	if code != 9 {
		t.Errorf("exit code = %d, want 9 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "cli-out") {
		t.Errorf("stdout = %q, want it to contain cli-out", stdout)
	}
	if !strings.Contains(stderr, "cli-err") {
		t.Errorf("stderr = %q, want it to contain cli-err", stderr)
	}
}

// TestCLIRespectsWorkingDirectory checks that the client's cwd is used.
func TestCLIRespectsWorkingDirectory(t *testing.T) {
	srv, dir := startService(t)
	srv.Activate(time.Minute)

	work := t.TempDir()
	bin := clientBinary(t)
	cmd := exec.Command(bin, "cmd", "/c", "cd")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), state.DirEnvVar+"="+dir, "WUDO_TTY=0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.TrimSpace(string(out))
	if !strings.EqualFold(filepath.Clean(got), filepath.Clean(work)) {
		t.Errorf("cd reported %q, want %q", got, work)
	}
}

// TestCLIStatus reports the service state.
func TestCLIStatus(t *testing.T) {
	srv, dir := startService(t)

	stdout, _, code := runClient(t, dir, "--status")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "inactive") {
		t.Errorf("status = %q, want it to mention inactive", stdout)
	}

	srv.Activate(2 * time.Hour)
	stdout, _, code = runClient(t, dir, "--status")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "active") {
		t.Errorf("status = %q, want it to mention active", stdout)
	}
}

// TestCLINotActive prints the friendly message when the service is disabled.
func TestCLINotActive(t *testing.T) {
	_, dir := startService(t)

	stdout, stderr, code := runClient(t, dir, "cmd", "/c", "echo nope")
	if code == 0 {
		t.Errorf("exit code = 0, want a failure")
	}
	if !strings.Contains(strings.ToLower(stderr), "service not active") {
		t.Errorf("stderr = %q, want it to say the service is not active", stderr)
	}
	if strings.Contains(stdout, "nope") {
		t.Errorf("stdout = %q, the command should not have run", stdout)
	}
}

// TestCLIEnvIsForwarded verifies the caller's environment overrides reach the
// command.
func TestCLIEnvIsForwarded(t *testing.T) {
	srv, dir := startService(t)
	srv.Activate(time.Minute)

	bin := clientBinary(t)
	cmd := exec.Command(bin, "--shell", "echo %USERNAME%")
	cmd.Env = append(os.Environ(), state.DirEnvVar+"="+dir, "WUDO_TTY=0")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(out)), strings.ToLower(os.Getenv("USERNAME"))) {
		t.Errorf("output = %q, want it to contain %q", out, os.Getenv("USERNAME"))
	}
}

// TestCLIRunsCmdShim checks that a .cmd shim (like npm.cmd) is executed.
func TestCLIRunsCmdShim(t *testing.T) {
	if _, err := exec.LookPath("npm.cmd"); err != nil {
		t.Skip("npm.cmd is not installed")
	}
	srv, dir := startService(t)
	srv.Activate(time.Minute)

	stdout, stderr, code := runClient(t, dir, "npm.cmd", "--version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Errorf("stdout is empty, want the npm version")
	}
}
