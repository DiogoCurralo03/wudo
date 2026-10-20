// Package setup implements the machine changes the wudo installer and
// uninstaller perform: installing the binaries under the state folder, adding
// the client to the machine PATH, and registering the service so it starts with
// Windows already elevated and without a UAC prompt.
package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/italoalmeida0/wudo/internal/state"
	"github.com/italoalmeida0/wudo/internal/winproc"
)

// File names of the three programs that end up next to each other.
const (
	ClientName      = "wudo.exe"
	ServiceName     = "wudosvc.exe"
	UninstallerName = "wudo-uninstall.exe"

	// InstallLogName records what the installer did, so a failed run can be
	// diagnosed without reproducing it.
	InstallLogName = "install.log"
)

// Options controls Install.
type Options struct {
	// Root is the base folder, C:\wudo by default. Binaries go into its bin
	// subfolder, next to the runtime files the service publishes.
	Root string
	// BinDir overrides the folder the executables are copied to. It defaults to
	// Root\bin.
	BinDir string
	// Payload maps the file name to install to its content.
	Payload map[string][]byte
	// SkipPath leaves the machine PATH untouched.
	SkipPath bool
	// SkipAutostart does not register the logon task.
	SkipAutostart bool
	// StartNow launches the service right after a successful install. It goes
	// through the scheduled task, which is the only way to start it elevated
	// without another UAC prompt.
	StartNow bool
	// TaskUser optionally specifies the user account or SID that owns the logon
	// task. When empty, it defaults to the interactive desktop session user (or the
	// invoking user).
	TaskUser string
	// Print receives human readable progress lines.
	Print func(format string, args ...any)
}

// Result summarises an install.
type Result struct {
	Root      string
	BinDir    string
	PathAdded bool
	TaskName  string
	Started   bool
}

// elevated reports whether this process can make the machine wide changes. It is
// a variable so the tests can exercise the file handling without UAC; production
// code always leaves it alone.
var elevated = winproc.IsElevated

// AllowUnelevated skips the administrator check. It exists for the automated
// test script, which installs into a temporary folder with PATH and the logon
// task disabled, and it is only reachable through WUDO_INSTALL_TEST. The real
// installer checks for elevation in its entry point as well.
func AllowUnelevated() { elevated = func() bool { return true } }

// Install copies the binaries, hardens the folder, updates PATH and registers the
// logon task. It must be called from an elevated process.
func Install(opts Options) (*Result, error) {
	if !elevated() {
		return nil, errors.New("installation needs administrator rights; start the installer with \"Run as administrator\"")
	}
	opts = withDefaults(opts)
	res := &Result{Root: opts.Root, BinDir: opts.BinDir, TaskName: TaskName}

	// A previous installation may still be running; its executable is locked, so
	// it has to go before the files can be replaced.
	stopped, err := StopService(opts.BinDir)
	if err != nil {
		// Not fatal: an old copy elsewhere is not our problem, and a locked file
		// will surface when the copy fails.
		opts.Printf("warning: could not stop a running service: %v", err)
	}
	if stopped > 0 {
		opts.Printf("stopped %d running wudo service instance(s)", stopped)
	}

	if err := os.MkdirAll(opts.BinDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", opts.BinDir, err)
	}
	// Only the standard folder gets the discovery ACL: a custom folder belongs to
	// whoever chose it, and locking it down would surprise them.
	if strings.EqualFold(filepath.Clean(opts.Root), filepath.Clean(state.DirPath)) {
		if err := state.HardenDir(opts.Root); err != nil {
			opts.Printf("warning: could not tighten the permissions of %s: %v", opts.Root, err)
		}
	}

	// Write the binaries. A .new file is written first and renamed into place, so
	// a failed copy never leaves a truncated executable behind.
	for _, name := range []string{ClientName, ServiceName, UninstallerName} {
		content, ok := opts.Payload[name]
		if !ok {
			return nil, fmt.Errorf("the installer does not carry %s", name)
		}
		target := filepath.Join(opts.BinDir, name)
		if err := writeAtomically(target, content); err != nil {
			return nil, err
		}
		opts.Printf("installed %s", target)
	}

	if !opts.SkipPath {
		added, err := AddToMachinePath(opts.BinDir)
		if err != nil {
			return nil, fmt.Errorf("add %s to PATH: %w", opts.BinDir, err)
		}
		res.PathAdded = added
		if added {
			opts.Printf("added %s to the machine PATH", opts.BinDir)
		} else {
			opts.Printf("%s is already in the machine PATH", opts.BinDir)
		}
	}

	if !opts.SkipAutostart {
		if err := RegisterTaskForUser(filepath.Join(opts.BinDir, ServiceName), opts.TaskUser); err != nil {
			return nil, fmt.Errorf("register the logon task: %w", err)
		}
		opts.Printf("registered the logon task %q (runs elevated, no UAC prompt)", TaskName)
	}

	if opts.StartNow {
		if err := RunTask(); err != nil {
			opts.Printf("warning: the service did not start automatically: %v", err)
			opts.Printf("start it from Task Scheduler, or run %s as administrator", filepath.Join(opts.BinDir, ServiceName))
		} else {
			// schtasks /Run triggers asynchronously; verify whether the service process actually came up.
			started := false
			for i := 0; i < 20; i++ {
				time.Sleep(150 * time.Millisecond)
				if ServiceRunning(opts.BinDir) {
					started = true
					break
				}
			}
			if started {
				res.Started = true
				opts.Printf("started the wudo service")
			} else {
				opts.Printf("warning: the logon task was triggered, but the service did not start")
				opts.Printf("start it from Task Scheduler, or run %s as administrator", filepath.Join(opts.BinDir, ServiceName))
			}
		}
	}
	return res, nil
}

// UninstallOptions controls Uninstall.
type UninstallOptions struct {
	Root string
	// BinDir overrides Root\bin.
	BinDir string
	// KeepData leaves the runtime files (log, endpoint) in place.
	KeepData bool
	// KeepPath does not touch the machine PATH.
	KeepPath bool
	// KeepAutostart does not remove the logon task.
	KeepAutostart bool
	// Dir is the state folder to clean; defaults to Root.
	Print func(format string, args ...any)
}

// UninstallResult summarises an uninstall.
type UninstallResult struct {
	RemovedFiles []string
	PathRemoved  bool
	TaskRemoved  bool
	DataKept     bool
	RebootNeeded bool
}

// Uninstall reverses Install.
func Uninstall(opts UninstallOptions) (*UninstallResult, error) {
	if !elevated() {
		return nil, errors.New("uninstalling needs administrator rights; start the uninstaller with \"Run as administrator\"")
	}
	opts = withUninstallDefaults(opts)
	res := &UninstallResult{DataKept: opts.KeepData}

	if !opts.KeepAutostart {
		removed, err := DeleteTask()
		switch {
		case err != nil:
			opts.Printf("warning: could not delete the logon task: %v", err)
		case removed:
			res.TaskRemoved = true
			opts.Printf("removed the logon task %q", TaskName)
		default:
			opts.Printf("the logon task %q was not registered", TaskName)
		}
	}

	stopped, err := StopService(opts.BinDir)
	if err != nil {
		opts.Printf("warning: could not stop the service: %v", err)
	}
	if stopped > 0 {
		opts.Printf("stopped %d running wudo service instance(s)", stopped)
	}

	if !opts.KeepPath {
		removed, err := RemoveFromMachinePath(opts.BinDir)
		if err != nil {
			opts.Printf("warning: could not update PATH: %v", err)
		} else if removed {
			res.PathRemoved = true
			opts.Printf("removed %s from the machine PATH", opts.BinDir)
		}
	}

	// Deleting the folder that holds the binaries is enough; a locked file (a
	// command still running from it) is scheduled for deletion at the next boot.
	for _, name := range []string{ClientName, ServiceName, UninstallerName} {
		path := filepath.Join(opts.BinDir, name)
		if err := os.Remove(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if scheduleDeleteOnReboot(path) == nil {
				res.RebootNeeded = true
				opts.Printf("scheduled %s for deletion at the next restart (it is in use)", path)
			} else {
				opts.Printf("warning: could not delete %s: %v", path, err)
			}
			continue
		}
		res.RemovedFiles = append(res.RemovedFiles, path)
		opts.Printf("removed %s", path)
	}
	_ = os.Remove(opts.BinDir)

	if !opts.KeepData {
		state.RemoveEndpointAt(opts.Root)
		for _, name := range []string{state.EndpointFileName, state.LogFileName, InstallLogName} {
			_ = os.Remove(filepath.Join(opts.Root, name))
		}
		if err := os.Remove(opts.Root); err == nil {
			opts.Printf("removed %s", opts.Root)
		} else if !errors.Is(err, os.ErrNotExist) {
			opts.Printf("warning: could not remove %s: %v", opts.Root, err)
		}
	}
	return res, nil
}

// writeAtomically replaces target with content, tolerating the case where target
// is the file this process is running from: Windows keeps it open as an image, so
// the running copy is renamed out of the way first and removed afterwards.
func writeAtomically(target string, content []byte) error {
	tmp := target + ".new"
	if err := os.WriteFile(tmp, content, 0o755); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	if err := os.Rename(tmp, target); err == nil {
		return nil
	}

	// Retry once through a rename of the existing file. This is the path used
	// when the uninstaller is installed by a copy of itself.
	old := target + ".old"
	_ = os.Remove(old)
	if err := os.Rename(target, old); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install %s: the file is in use", target)
	}
	if err := os.Rename(tmp, target); err != nil {
		// Put the original back, so a failed upgrade keeps a working install.
		_ = os.Rename(old, target)
		_ = os.Remove(tmp)
		return fmt.Errorf("install %s: %w", target, err)
	}
	if err := os.Remove(old); err != nil {
		// The old copy is this process's own image: schedule it for deletion.
		_ = scheduleDeleteOnReboot(old)
	}
	return nil
}

// BinDirFor returns the folder the binaries are installed to for a given root.
func BinDirFor(root string) string {
	if root == "" {
		root = state.DirPath
	}
	return filepath.Join(root, "bin")
}

// withDefaults fills in the folders and the printer.
func withDefaults(opts Options) Options {
	if opts.Root == "" {
		opts.Root = state.DirPath
	}
	if opts.BinDir == "" {
		opts.BinDir = BinDirFor(opts.Root)
	}
	if opts.Print == nil {
		opts.Print = func(format string, args ...any) { fmt.Fprintf(os.Stdout, format+"\n", args...) }
	}
	return opts
}

// Printf reports progress through the configured printer.
func (o Options) Printf(format string, args ...any) { o.Print(format, args...) }

func withUninstallDefaults(opts UninstallOptions) UninstallOptions {
	if opts.Root == "" {
		opts.Root = state.DirPath
	}
	if opts.BinDir == "" {
		opts.BinDir = BinDirFor(opts.Root)
	}
	if opts.Print == nil {
		opts.Print = func(format string, args ...any) { fmt.Fprintf(os.Stdout, format+"\n", args...) }
	}
	return opts
}

// Printf reports progress through the configured printer.
func (o UninstallOptions) Printf(format string, args ...any) { o.Print(format, args...) }

// cleanDir normalises a folder for comparisons and for the PATH entry.
func cleanDir(dir string) string {
	dir = strings.TrimSpace(dir)
	dir = strings.Trim(dir, `"`)
	if dir == "" {
		return ""
	}
	return filepath.Clean(dir)
}
