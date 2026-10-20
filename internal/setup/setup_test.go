package setup

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// allowElevated makes the package believe it runs with administrator rights, so
// the file handling can be tested without UAC.
func allowElevated(t *testing.T) {
	t.Helper()
	original := elevated
	elevated = func() bool { return true }
	t.Cleanup(func() { elevated = original })
}

// fakePayload returns three small files that stand in for the binaries.
func fakePayload() map[string][]byte {
	return map[string][]byte{
		ClientName:      []byte("client-binary"),
		ServiceName:     []byte("service-binary"),
		UninstallerName: []byte("uninstaller-binary"),
	}
}

// quietOptions builds options that touch nothing machine wide.
func quietOptions(t *testing.T, root string) Options {
	t.Helper()
	var mu sync.Mutex
	return Options{
		Root:          root,
		Payload:       fakePayload(),
		SkipPath:      true,
		SkipAutostart: true,
		Print: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			t.Logf(format, args...)
		},
	}
}

// TestInstallWritesBinaries checks the file layout and contents.
func TestInstallWritesBinaries(t *testing.T) {
	allowElevated(t)
	root := t.TempDir()

	res, err := Install(quietOptions(t, root))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	wantBin := filepath.Join(root, "bin")
	if res.BinDir != wantBin {
		t.Errorf("BinDir = %q, want %q", res.BinDir, wantBin)
	}
	for name, want := range fakePayload() {
		path := filepath.Join(wantBin, name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != string(want) {
			t.Errorf("%s contains %q, want %q", path, got, want)
		}
	}
}

// TestInstallReplacesRunningBinaries checks that an upgrade overwrites the files
// and that no .new leftover is kept.
func TestInstallReplacesRunningBinaries(t *testing.T) {
	allowElevated(t)
	root := t.TempDir()
	if _, err := Install(quietOptions(t, root)); err != nil {
		t.Fatalf("first install: %v", err)
	}

	opts := quietOptions(t, root)
	opts.Payload = map[string][]byte{
		ClientName:      []byte("client-v2"),
		ServiceName:     []byte("service-v2"),
		UninstallerName: []byte("uninstaller-v2"),
	}
	if _, err := Install(opts); err != nil {
		t.Fatalf("second install: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "bin", ClientName))
	if err != nil {
		t.Fatalf("read client: %v", err)
	}
	if string(got) != "client-v2" {
		t.Errorf("client contains %q, want the new binary", got)
	}
	entries, err := os.ReadDir(filepath.Join(root, "bin"))
	if err != nil {
		t.Fatalf("read bin: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".new") {
			t.Errorf("staging file %s was left behind", e.Name())
		}
	}
}

// TestInstallNeedsEveryBinary checks the payload validation.
func TestInstallNeedsEveryBinary(t *testing.T) {
	allowElevated(t)
	opts := quietOptions(t, t.TempDir())
	delete(opts.Payload, ServiceName)
	if _, err := Install(opts); err == nil {
		t.Fatal("install succeeded without the service binary")
	}
}

// TestInstallRefusesWithoutElevation checks the guard.
func TestInstallRefusesWithoutElevation(t *testing.T) {
	original := elevated
	elevated = func() bool { return false }
	t.Cleanup(func() { elevated = original })

	if _, err := Install(quietOptions(t, t.TempDir())); err == nil {
		t.Fatal("install succeeded without administrator rights")
	} else if !strings.Contains(strings.ToLower(err.Error()), "administrator") {
		t.Errorf("error = %v, want it to mention administrator rights", err)
	}
}

// TestUninstallRemovesEverything checks the reverse operation.
func TestUninstallRemovesEverything(t *testing.T) {
	allowElevated(t)
	root := t.TempDir()
	if _, err := Install(quietOptions(t, root)); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Simulate the runtime files the service publishes.
	logPath := filepath.Join(root, "wudosvc.log")
	if err := os.WriteFile(logPath, []byte("log"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	res, err := Uninstall(UninstallOptions{
		Root:          root,
		KeepPath:      true,
		KeepAutostart: true,
	})
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if len(res.RemovedFiles) != 3 {
		t.Errorf("removed %d files, want 3: %v", len(res.RemovedFiles), res.RemovedFiles)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("%s still exists", root)
	}
}

// TestUninstallKeepData checks that the log survives when asked.
func TestUninstallKeepData(t *testing.T) {
	allowElevated(t)
	root := t.TempDir()
	if _, err := Install(quietOptions(t, root)); err != nil {
		t.Fatalf("install: %v", err)
	}
	logPath := filepath.Join(root, "wudosvc.log")
	if err := os.WriteFile(logPath, []byte("log"), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}

	if _, err := Uninstall(UninstallOptions{
		Root:          root,
		KeepData:      true,
		KeepPath:      true,
		KeepAutostart: true,
	}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("the log was removed even though KeepData was set: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", ClientName)); !os.IsNotExist(err) {
		t.Error("the client was not removed")
	}
}

// TestUninstallWithoutElevation checks the guard.
func TestUninstallWithoutElevation(t *testing.T) {
	original := elevated
	elevated = func() bool { return false }
	t.Cleanup(func() { elevated = original })

	if _, err := Uninstall(UninstallOptions{Root: t.TempDir()}); err == nil {
		t.Fatal("uninstall succeeded without administrator rights")
	}
}
