package spawn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// ErrNotFound is returned when the program cannot be resolved on disk.
var ErrNotFound = errors.New("command not found")

// scriptExts are the extensions that Windows cannot launch directly and that
// therefore have to be handed to the command interpreter.
var scriptExts = map[string]bool{".cmd": true, ".bat": true}

// defaultPathExt is used when %PATHEXT% is not set for some reason.
const defaultPathExt = ".COM;.EXE;.BAT;.CMD"

// Resolved describes how a program is going to be executed.
type Resolved struct {
	// Program is the absolute path of the executable to hand to CreateProcess.
	Program string
	// Script is set when Program is a batch file that must run through cmd.exe.
	Script bool
	// Args is the argument vector, argv[0] replaced by the resolved program
	// for real executables, or the arguments after the script name.
	Args []string
}

// Resolve turns a program name into an absolute path, honouring the client's
// working directory and %PATH%/%PATHEXT% the same way cmd.exe would. cwd is the
// directory the command has to run in.
func Resolve(prog, cwd string) (*Resolved, error) {
	if strings.TrimSpace(prog) == "" {
		return nil, fmt.Errorf("%w: empty command", ErrNotFound)
	}

	explicitPath := filepath.IsAbs(prog) ||
		strings.ContainsAny(prog, `/\`) ||
		(len(prog) > 1 && prog[1] == ':')

	if explicitPath {
		target := prog
		if !filepath.IsAbs(target) && cwd != "" {
			target = filepath.Join(cwd, target)
		}
		if abs, ok := lookWithExt(target); ok {
			return &Resolved{
				Program: abs,
				Script:  scriptExts[strings.ToLower(filepath.Ext(abs))],
				Args:    []string{abs},
			}, nil
		}
		return nil, fmt.Errorf("%w: %s", ErrNotFound, prog)
	}

	dirs := searchDirs(cwd)
	for _, dir := range dirs {
		if abs, ok := lookWithExt(filepath.Join(dir, prog)); ok {
			return &Resolved{
				Program: abs,
				Script:  scriptExts[strings.ToLower(filepath.Ext(abs))],
				Args:    []string{abs},
			}, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, prog)
}

// searchDirs returns the directories CreateProcess would search, with the
// client's working directory taking the place of the service's own.
func searchDirs(cwd string) []string {
	var dirs []string
	seen := map[string]bool{}
	add := func(dir string) {
		if dir == "" {
			return
		}
		key := strings.ToLower(filepath.Clean(dir))
		if seen[key] {
			return
		}
		seen[key] = true
		dirs = append(dirs, dir)
	}

	add(cwd)
	if len(cwd) > 1 && cwd[1] == ':' {
		// The root of the client's drive is searched on Windows as well.
		add(string(cwd[:2]) + `\`)
	}
	sysRoot, _ := windows.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	add(filepath.Join(sysRoot, "System32"))
	add(sysRoot)
	pathVar, _ := windows.Getenv("PATH")
	for _, dir := range filepath.SplitList(pathVar) {
		add(dir)
	}
	return dirs
}

// lookWithExt reports whether path exists, trying %PATHEXT% when the name has
// no extension.
func lookWithExt(path string) (string, bool) {
	if ext := filepath.Ext(path); ext != "" {
		if isFile(path) {
			return path, true
		}
	}
	pathExt, _ := windows.Getenv("PATHEXT")
	if pathExt == "" {
		pathExt = defaultPathExt
	}
	for _, ext := range strings.Split(pathExt, ";") {
		ext = strings.TrimSpace(ext)
		if ext == "" {
			continue
		}
		candidate := path + ext
		if isFile(candidate) {
			return candidate, true
		}
		candidate = path + strings.ToLower(ext)
		if isFile(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// BuildCommandLine assembles the lpCommandLine string for CreateProcess.
//
// Regular executables get the standard CommandLineToArgvW quoting. Batch files
// are launched through the command interpreter, which has its own quoting
// rules; the /s switch makes cmd.exe strip exactly the outermost pair of quotes
// and keep everything else verbatim, which is what makes paths and arguments
// with spaces survive.
func BuildCommandLine(r *Resolved, args []string) string {
	if r.Script {
		inner := quoteCmdToken(r.Program)
		for _, a := range args {
			inner += " " + quoteCmdToken(a)
		}
		return composeShellCommandLine(inner)
	}
	argv := append([]string{r.Program}, args...)
	return windows.ComposeCommandLine(argv)
}

// composeShellCommandLine wraps a raw command inside cmd.exe /d /s /c "...".
func composeShellCommandLine(command string) string {
	comspec, _ := windows.Getenv("ComSpec")
	if comspec == "" {
		comspec = `C:\Windows\System32\cmd.exe`
	}
	return quoteCmdToken(comspec) + ` /d /s /c "` + command + `"`
}

// quoteCmdToken quotes one token for cmd.exe. Arguments that need no quoting
// are passed through untouched so that common cases look natural in the logs.
func quoteCmdToken(s string) string {
	if s == "" {
		return `""`
	}
	needs := strings.ContainsAny(s, " \t\"&|<>^()%!")
	if !needs {
		return s
	}
	// Inside double quotes cmd.exe only cares about embedded double quotes,
	// which are escaped by doubling them.
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// BuildShellCommandLine runs a raw command line through cmd.exe, as if the user
// had typed it into a command prompt.
func BuildShellCommandLine(command string) string {
	return composeShellCommandLine(command)
}
