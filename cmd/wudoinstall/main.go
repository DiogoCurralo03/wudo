// Command wudoinstall installs wudo on this machine: it copies wudo.exe and
// wudosvc.exe to C:\wudo\bin, adds that folder to the machine PATH, and
// registers the logon task that starts the service already elevated and without
// a UAC prompt.
//
// The payload (client, service and uninstaller) is embedded at build time by
// scripts\build.ps1, so this single executable is the whole installer.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/italoalmeida0/wudo/internal/setup"
	"github.com/italoalmeida0/wudo/internal/state"
	"github.com/italoalmeida0/wudo/internal/version"
	"github.com/italoalmeida0/wudo/internal/winproc"
)

// payloadNames are the binaries the installer carries. The uninstaller is a copy
// of the installer itself (it understands --uninstall), so a single file is
// enough to ship.
var payloadNames = []string{setup.ClientName, setup.ServiceName}

const usage = `wudo installer

Installs wudo on this machine:
  * copies wudo.exe, wudosvc.exe and wudo-uninstall.exe to C:\wudo\bin
  * adds C:\wudo\bin to the machine PATH, so "wudo" works everywhere
  * registers the scheduled task "wudo-service", which starts the service at
    every logon already elevated and without a UAC prompt
  * optionally starts the service right away

Usage:
  wudoinstall.exe [options]

Options:
      --root <dir>     install folder (default C:\wudo)
      --no-path        do not modify the machine PATH
      --no-autostart   do not register the logon task
      --no-start       do not start the service after installing
      --yes            do not ask for confirmation
      --uninstall      remove wudo instead of installing it
      --keep-data      on uninstall, keep the log and the discovery file
      --version        print the version and exit
  -h, --help           show this help

The installer must be started as administrator: it changes machine wide settings.
`

func main() {
	args := os.Args[1:]
	// The installed copy is named wudo-uninstall.exe and defaults to removing
	// wudo, so a double click in Explorer does the expected thing.
	defaultUninstall := strings.HasPrefix(strings.ToLower(filepath.Base(os.Args[0])), "wudo-uninstall")
	os.Exit(run(args, defaultUninstall))
}

type options struct {
	root        string
	noPath      bool
	noAutostart bool
	noStart     bool
	assumeYes   bool
	uninstall   bool
	keepData    bool
	help        bool
	showVersion bool
}

func run(args []string, defaultUninstall bool) int {
	opt, err := parse(args, defaultUninstall)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wudoinstall: %v\n\n%s", err, usage)
		return 2
	}
	switch {
	case opt.help:
		fmt.Fprint(os.Stdout, usage)
		return 0
	case opt.showVersion:
		fmt.Printf("wudoinstall %s\n", version.String())
		return 0
	}

	// WUDO_INSTALL_TEST lets the automated test script install into a temporary
	// folder from an ordinary shell. It never appears in normal use.
	testing := os.Getenv("WUDO_INSTALL_TEST") == "1"
	if testing {
		setup.AllowUnelevated()
	}
	if !winproc.IsElevated() && !testing {
		fmt.Fprintln(os.Stderr, "wudoinstall: administrator rights are required.")
		fmt.Fprintln(os.Stderr, "wudoinstall: right click the installer and choose \"Run as administrator\".")
		return 1
	}

	opts := setup.Options{
		Root:          opt.root,
		SkipPath:      opt.noPath,
		SkipAutostart: opt.noAutostart,
		StartNow:      !opt.noStart,
		Print:         printf,
	}

	if opt.uninstall {
		// The log must not recreate the folder the uninstall just deleted.
		defer writeInstallLog(os.TempDir())
		return runUninstall(setup.UninstallOptions{
			Root:     opt.root,
			KeepData: opt.keepData,
			Print:    printf,
		}, opt.assumeYes)
	}

	defer writeInstallLog(opt.root)

	payload, err := loadPayload()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wudoinstall: %v\n", err)
		return 1
	}
	// The uninstaller is this very executable, copied next to the binaries.
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "wudoinstall: cannot read my own path: %v\n", err)
		return 1
	}
	selfBytes, err := os.ReadFile(self)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wudoinstall: cannot read %s: %v\n", self, err)
		return 1
	}
	payload[setup.UninstallerName] = selfBytes
	opts.Payload = payload

	fmt.Printf("wudo installer %s\n", version.Short())
	fmt.Printf("install folder: %s\n\n", setup.BinDirFor(opts.Root))
	if !opt.assumeYes && !confirm("Install wudo on this machine? [Y/n] ") {
		fmt.Println("cancelled.")
		return 1
	}

	res, err := setup.Install(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nwudoinstall: %v\n", err)
		return 1
	}

	fmt.Println()
	fmt.Println("wudo is installed.")
	fmt.Printf("  client:  %s\n", filepath.Join(res.BinDir, setup.ClientName))
	fmt.Printf("  service: %s\n", filepath.Join(res.BinDir, setup.ServiceName))
	if res.PathAdded {
		fmt.Println("  PATH:    updated - open a new terminal to use \"wudo\"")
	} else {
		fmt.Printf("  PATH:    %s was already there\n", res.BinDir)
	}
	if res.TaskName != "" {
		fmt.Printf("  logon:   task %q starts the service elevated, with no UAC prompt\n", res.TaskName)
	}
	fmt.Println()
	if res.Started {
		fmt.Println("The service is running. It starts INACTIVE: right click the tray icon")
		fmt.Println("and choose Activate to allow wudo commands for a while.")
	} else {
		fmt.Printf("Start it now with: schtasks /Run /TN %s\n", res.TaskName)
	}
	fmt.Println("Uninstall any time with wudo-uninstall.exe (or wudoinstall.exe --uninstall).")
	return 0
}

// runUninstall removes an installation.
func runUninstall(opts setup.UninstallOptions, assumeYes bool) int {
	fmt.Println("wudo uninstaller")
	if !assumeYes && !confirm("Remove wudo from this machine? [y/N] ") {
		fmt.Println("cancelled.")
		return 1
	}
	res, err := setup.Uninstall(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nwudoinstall: %v\n", err)
		return 1
	}
	fmt.Println()
	fmt.Println("wudo is uninstalled.")
	if res.TaskRemoved {
		fmt.Println("  the logon task was removed")
	}
	if res.PathRemoved {
		fmt.Printf("  %s was removed from the machine PATH - open a new terminal\n", setup.BinDirFor(opts.Root))
	}
	if res.DataKept {
		fmt.Printf("  the log and the discovery file were kept in %s\n", opts.Root)
	}
	if res.RebootNeeded {
		fmt.Println("  a file was still in use and will be deleted at the next restart")
	}
	return 0
}

// parse reads the command line.
func parse(args []string, defaultUninstall bool) (*options, error) {
	opt := &options{}
	fs := flag.NewFlagSet("wudoinstall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opt.root, "root", state.DirPath, "install folder")
	fs.BoolVar(&opt.noPath, "no-path", false, "do not modify PATH")
	fs.BoolVar(&opt.noAutostart, "no-autostart", false, "do not register the logon task")
	fs.BoolVar(&opt.noStart, "no-start", false, "do not start the service")
	fs.BoolVar(&opt.assumeYes, "yes", false, "do not ask for confirmation")
	fs.BoolVar(&opt.uninstall, "uninstall", false, "remove wudo")
	fs.BoolVar(&opt.keepData, "keep-data", false, "keep the log on uninstall")
	fs.BoolVar(&opt.help, "help", false, "show the help")
	fs.BoolVar(&opt.help, "h", false, "show the help")
	fs.BoolVar(&opt.showVersion, "version", false, "show the version")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	// Registering a flag resets it to its default, so the "installed as
	// wudo-uninstall.exe means uninstall" default is applied after parsing.
	if defaultUninstall && !flagSet(fs, "uninstall") {
		opt.uninstall = true
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	opt.root = strings.TrimSpace(opt.root)
	return opt, nil
}

// flagSet reports whether the flag was given on the command line.
func flagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// confirm asks a yes/no question on the console.
func confirm(question string) bool {
	fmt.Print(question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		// No console to read from: treat it as accepted, the caller can still use
		// --yes to be explicit.
		return true
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "" || line == "y" || line == "yes"
}

// printf is the progress printer handed to the setup package. It writes to the
// console and remembers the lines for install.log, so a failed run can be
// diagnosed afterwards.
func printf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	fmt.Println(line)
	logMu.Lock()
	logLines = append(logLines, line)
	logMu.Unlock()
}

var (
	logMu    sync.Mutex
	logLines []string
)

// writeInstallLog stores the buffered lines next to the runtime files. It runs
// when the program is about to exit and never keeps the file open: the
// uninstaller deletes the very folder the log lives in.
func writeInstallLog(root string) {
	logMu.Lock()
	lines := logLines
	logMu.Unlock()
	if len(lines) == 0 {
		return
	}
	if root == "" {
		root = state.DirPath
	}
	path := filepath.Join(root, setup.InstallLogName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		path = filepath.Join(os.TempDir(), setup.InstallLogName)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "\n=== wudo %s ===\n", time.Now().Format("2006-01-02 15:04:05"))
	for _, line := range lines {
		fmt.Fprintln(f, line)
	}
}
