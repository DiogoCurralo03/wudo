// Command wudo runs a command with administrator privileges through the wudo
// service.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/italoalmeida0/wudo/internal/client"
	"github.com/italoalmeida0/wudo/internal/version"
)

const usage = `wudo - run commands with administrator privileges through the wudo service

Usage:
  wudo [options] <command> [args...]
  wudo [options] --shell "<command line>"

Options:
  -t, --tty             use a pseudo console (automatic on a terminal)
      --no-tty          never use a pseudo console, always plain pipes
      --shell           run the whole command line through cmd.exe
      --timeout <dur>   kill the command after this long (e.g. 30s, 5m)
      --cwd <dir>       working directory for the command (default: current)
      --status          show the state of the wudo service and exit
      --version         print the version and exit
      --verbose         print diagnostics on stderr
  -h, --help            show this help

Environment:
  WUDO_TTY=0|1          force pseudo console off or on
  WUDO_DIR=<dir>        use a different state folder (default C:\wudo)

The command runs with the privileges of the wudo service, which has to be
started as administrator and activated from its tray icon. Everything else
behaves like a local run: the working directory, the environment and the
terminal are forwarded, streamed output is unbuffered, and the exit code of the
command becomes the exit code of wudo.

Examples:
  wudo whoami /priv
  wudo -t powershell -NoLogo
  wudo --shell "ipconfig /flushdns && net stop wuauserv"
  wudo --timeout 30s netstat -ano
`

func main() {
	os.Exit(run(os.Args[1:]))
}

// options holds everything the command line can ask for.
type options struct {
	tty       bool
	ttyForced bool
	shell     bool
	timeout   time.Duration
	cwd       string
	verbose   bool
	status    bool
	version   bool
	help      bool
	rest      []string
}

func run(args []string) int {
	opt, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wudo: %v\n\n%s", err, usage)
		return 2
	}
	switch {
	case opt.help:
		fmt.Fprint(os.Stdout, usage)
		return 0
	case opt.version:
		fmt.Printf("wudo %s\n", version.String())
		return 0
	case opt.status:
		return printStatus()
	case len(opt.rest) == 0:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}

	// An interactive terminal wants a pseudo console unless the caller said
	// otherwise: that is what makes programs like edit.exe, less or a REPL work.
	if !opt.ttyForced && client.IsTerminal() {
		opt.tty = true
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The first Ctrl+C asks the service to interrupt the command, exactly like a
	// local Ctrl+C. A second one abandons it, which kills it server side.
	interrupts := make(chan struct{}, 1)
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)

	go func() {
		seen := 0
		for range sigs {
			seen++
			if seen == 1 {
				select {
				case interrupts <- struct{}{}:
				default:
				}
				continue
			}
			cancel()
			return
		}
	}()

	restoreOut := client.EnableStdoutVT()
	defer restoreOut()

	if opt.tty {
		restore, rawErr := client.MakeRaw()
		if rawErr != nil {
			fmt.Fprintf(os.Stderr, "wudo: cannot switch the terminal to raw mode: %v\n", rawErr)
		} else {
			defer restore()
		}
	}

	res, err := client.Run(ctx, client.Options{
		Argv:       opt.rest,
		Shell:      opt.shell,
		Cwd:        opt.cwd,
		Tty:        opt.tty,
		Timeout:    opt.timeout,
		Verbose:    opt.verbose,
		Interrupts: interrupts,
	})
	if err != nil {
		printRunError(err)
		return exitCodeForError(err)
	}
	if opt.verbose && res.Reason != "" && res.Reason != "exited" {
		fmt.Fprintf(os.Stderr, "wudo: command %s after %s\n", res.Reason, res.Elapsed.Round(time.Millisecond))
	}
	switch {
	case res.ExitCode < 0:
		return 1
	case res.ExitCode > 255:
		// Windows status codes do not fit in a process exit code; keep the
		// low byte so the value still round-trips through shells.
		return res.ExitCode & 0xff
	default:
		return res.ExitCode
	}
}

// parseArgs consumes wudo's own options and returns the command to run.
func parseArgs(args []string) (*options, error) {
	opt := &options{}

	// WUDO_TTY lets callers force the mode without touching the command line.
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WUDO_TTY"))) {
	case "1", "true", "yes", "on":
		opt.tty = true
		opt.ttyForced = true
	case "0", "false", "no", "off":
		opt.tty = false
		opt.ttyForced = true
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			opt.rest = args[i+1:]
			return opt, nil
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			opt.rest = args[i:]
			return opt, nil
		}
		name, value, hasValue := strings.Cut(a, "=")
		switch name {
		case "-h", "--help", "/?":
			opt.help = true
			return opt, nil
		case "--version", "-V":
			opt.version = true
			return opt, nil
		case "--status":
			opt.status = true
			return opt, nil
		case "-t", "--tty":
			opt.tty = true
			opt.ttyForced = true
		case "--no-tty":
			opt.tty = false
			opt.ttyForced = true
		case "--shell":
			opt.shell = true
		case "--verbose", "-v":
			opt.verbose = true
		case "--timeout":
			if !hasValue {
				if i+1 >= len(args) {
					return nil, errors.New("--timeout needs a value, e.g. --timeout 30s")
				}
				i++
				value = args[i]
			}
			d, err := parseDuration(value)
			if err != nil {
				return nil, err
			}
			opt.timeout = d
		case "--cwd":
			if !hasValue {
				if i+1 >= len(args) {
					return nil, errors.New("--cwd needs a directory")
				}
				i++
				value = args[i]
			}
			opt.cwd = value
		default:
			return nil, fmt.Errorf("unknown option %q (use -- to pass it to the command)", a)
		}
	}
	return opt, nil
}

func parseDuration(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return 0, fmt.Errorf("invalid timeout %q", s)
		}
		return d, nil
	}
	// A bare number means seconds, like timeout(1).
	if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second, nil
	}
	return 0, fmt.Errorf("invalid duration %q (use forms like 30s, 5m, 1h)", s)
}

func printStatus() int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, ep, err := client.Status(ctx)
	if err != nil {
		printRunError(err)
		return exitCodeForError(err)
	}
	fmt.Printf("service:  running (pid %d, port %d, version %s)\n", st.PID, ep.Port, st.Version)
	fmt.Printf("folder:   %s\n", ep.Dir)
	fmt.Printf("account:  %s%s\n", st.CurrentUser, elevationSuffix(st.Elevated))
	fmt.Printf("uptime:   %s\n", (time.Duration(st.UptimeSeconds) * time.Second).Truncate(time.Second))
	if st.Active {
		if st.ExpiresAt != nil {
			fmt.Printf("status:   active until %s (%s left)\n",
				st.ExpiresAt.Local().Format("2006-01-02 15:04:05"),
				(time.Duration(st.SecondsLeft) * time.Second).Truncate(time.Second))
		} else {
			fmt.Printf("status:   active\n")
		}
	} else {
		fmt.Printf("status:   inactive - activate it from the wudo tray icon\n")
	}
	fmt.Printf("commands: %d running (limit %d)\n", st.Sessions, st.MaxSessions)
	return 0
}

func elevationSuffix(elevated bool) string {
	if elevated {
		return " (elevated)"
	}
	return " (NOT elevated - commands will not run with administrator rights)"
}

func printRunError(err error) {
	switch {
	case errors.Is(err, client.ErrNotInstalled):
		fmt.Fprintln(os.Stderr, "wudo: service not active")
		fmt.Fprintln(os.Stderr, "wudo: no wudo service is running. Start wudosvc.exe as administrator.")
	case errors.Is(err, client.ErrNotActive):
		fmt.Fprintln(os.Stderr, "wudo: service not active")
		fmt.Fprintln(os.Stderr, "wudo: right-click the wudo tray icon and choose Activate.")
	case errors.Is(err, client.ErrUnreachable):
		fmt.Fprintf(os.Stderr, "wudo: service not reachable: %v\n", err)
		fmt.Fprintln(os.Stderr, "wudo: restart wudosvc.exe as administrator.")
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(os.Stderr, "wudo: aborted")
	default:
		fmt.Fprintf(os.Stderr, "wudo: %v\n", err)
	}
}

func exitCodeForError(err error) int {
	switch {
	case errors.Is(err, context.Canceled):
		return 130
	default:
		return 1
	}
}
