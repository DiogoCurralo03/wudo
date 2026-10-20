package spawn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveFindsExecutable checks that a plain name is resolved through PATH.
func TestResolveFindsExecutable(t *testing.T) {
	r, err := Resolve("cmd.exe", "")
	if err != nil {
		t.Fatalf("resolve cmd.exe: %v", err)
	}
	if !strings.HasSuffix(strings.ToLower(r.Program), `\cmd.exe`) {
		t.Errorf("program = %q, want a path ending in \\cmd.exe", r.Program)
	}
	if r.Script {
		t.Errorf("cmd.exe was classified as a script")
	}
}

// TestResolveAddsExtension checks %PATHEXT% handling.
func TestResolveAddsExtension(t *testing.T) {
	r, err := Resolve("where", "")
	if err != nil {
		t.Fatalf("resolve where: %v", err)
	}
	if !strings.HasSuffix(strings.ToLower(r.Program), `\where.exe`) {
		t.Errorf("program = %q, want it to get the .exe extension", r.Program)
	}
}

// TestResolveUsesClientDirectory checks that a relative name is looked up in the
// caller's working directory first, not the service's.
func TestResolveUsesClientDirectory(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "hello.cmd")
	if err := os.WriteFile(script, []byte("@echo hello\r\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	r, err := Resolve("hello.cmd", dir)
	if err != nil {
		t.Fatalf("resolve hello.cmd: %v", err)
	}
	if !r.Script {
		t.Errorf("hello.cmd was not classified as a script")
	}
	if !strings.EqualFold(r.Program, script) {
		t.Errorf("program = %q, want %q", r.Program, script)
	}
}

// TestResolveMissingProgram checks the error for an unknown command.
func TestResolveMissingProgram(t *testing.T) {
	if _, err := Resolve("this-program-does-not-exist-42", t.TempDir()); err == nil {
		t.Fatal("resolve succeeded for a program that does not exist")
	}
}

// TestBuildCommandLineScriptQuoting checks the cmd.exe /d /s /c wrapping, which
// is what makes paths and arguments with spaces survive.
func TestBuildCommandLineScriptQuoting(t *testing.T) {
	dir := `C:\Program Files\nodejs`
	r := &Resolved{Program: dir + `\npm.cmd`, Script: true}

	got := BuildCommandLine(r, []string{"--version"})
	want := `C:\Windows\System32\cmd.exe /d /s /c ""C:\Program Files\nodejs\npm.cmd" --version"`
	if !strings.EqualFold(got, want) {
		t.Errorf("command line =\n  %q\nwant\n  %q", got, want)
	}
}

// TestBuildCommandLineQuotesArguments checks the normal quoting path.
func TestBuildCommandLineQuotesArguments(t *testing.T) {
	r := &Resolved{Program: `C:\Windows\System32\whoami.exe`}
	got := BuildCommandLine(r, []string{"/priv", "a b"})
	if !strings.Contains(got, `"a b"`) {
		t.Errorf("command line = %q, want the argument with a space to be quoted", got)
	}
	if !strings.HasPrefix(got, `C:\Windows\System32\whoami.exe /priv`) {
		t.Errorf("command line = %q, want it to start with the program and /priv", got)
	}
}
