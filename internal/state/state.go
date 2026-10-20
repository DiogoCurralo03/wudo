// Package state knows where wudo keeps its runtime files and how they are
// shared between the elevated service and the unprivileged client.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"github.com/italoalmeida0/wudo/internal/winproc"
)

// SchemaVersion is bumped whenever the on-disk layout or the wire protocol
// changes in a way that makes old clients incompatible.
const SchemaVersion = 1

// DirPath is the folder on the local C: drive that wudo creates on startup and
// where it publishes the port of the running service.
const DirPath = `C:\wudo`

// EndpointFileName holds the discovery information for the running service.
const EndpointFileName = "endpoint.json"

// LogFileName is the service log file, kept next to the endpoint file.
const LogFileName = "wudosvc.log"

// DirEnvVar overrides the state directory. It exists so that tests and
// portable setups can use a different folder without touching the real one.
const DirEnvVar = "WUDO_DIR"

// dirSDDL describes the discretionary ACL applied to DirPath:
//
//	SYSTEM and Administrators get full control, authenticated users only get
//	read + execute: they must be able to read endpoint.json, nothing else.
//	The "P" prefix makes the DACL protected, so no inherited ACEs sneak in
//	from C:\ and grants nobody else write access to the discovery file.
const dirSDDL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200A9;;;AU)"

var (
	resolveOnce sync.Once
	activeDir   string
)

// Endpoint is the content of endpoint.json. It only carries discovery data:
// whether the service is currently active is answered by the service itself
// over HTTP, because it changes over time.
type Endpoint struct {
	Schema   int       `json:"schema"`
	Port     int       `json:"port"`
	PID      int       `json:"pid"`
	Instance string    `json:"instance"`
	Version  string    `json:"version"`
	User     string    `json:"user"`
	Elevated bool      `json:"elevated"`
	Dir      string    `json:"dir"`
	Started  time.Time `json:"started"`
}

// PrimaryDir is the preferred state directory.
func PrimaryDir() string {
	if v := os.Getenv(DirEnvVar); v != "" {
		return v
	}
	return DirPath
}

// FallbackDir is used when the privileged location cannot be written to, which
// happens when the service is started without administrator rights.
func FallbackDir() string {
	if v := os.Getenv(DirEnvVar); v != "" {
		return v
	}
	base, err := os.UserCacheDir() // %LOCALAPPDATA%
	if err != nil || base == "" {
		return filepath.Join(os.TempDir(), "wudo")
	}
	return filepath.Join(base, "wudo")
}

// CandidateDirs returns every directory an endpoint file may live in, most
// privileged first.
func CandidateDirs() []string {
	primary, fallback := PrimaryDir(), FallbackDir()
	if filepath.Clean(primary) == filepath.Clean(fallback) {
		return []string{primary}
	}
	return []string{primary, fallback}
}

// Dir returns the state directory in use. Before Resolve or EnsureDir ran, it
// reports the primary directory.
func Dir() string {
	if activeDir != "" {
		return activeDir
	}
	return PrimaryDir()
}

// EndpointPath returns the full path of endpoint.json.
func EndpointPath() string { return filepath.Join(Dir(), EndpointFileName) }

// LogPath returns the full path of the service log file.
func LogPath() string { return filepath.Join(Dir(), LogFileName) }

// processAlive reports whether pid belongs to a running process.
func processAlive(pid uint32) bool { return winproc.ProcessAlive(pid) }

// Resolve picks the directory the service should use and creates it: the
// privileged one when possible, the per-user one otherwise.
func Resolve() (string, error) {
	var errs []error
	for _, dir := range CandidateDirs() {
		if err := ensureDirAt(dir); err != nil {
			errs = append(errs, err)
			continue
		}
		activeDir = dir
		return dir, nil
	}
	return "", errors.Join(errs...)
}

// EnsureDir makes sure the state directory in use exists.
func EnsureDir() error {
	_, err := Resolve()
	return err
}

// ensureDirAt creates dir and, when it is the privileged location, replaces its
// ACL with dirSDDL.
func ensureDirAt(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if filepath.Clean(dir) != filepath.Clean(DirPath) {
		// A custom directory belongs to whoever created it; leave the ACL alone.
		return nil
	}
	// Failing to harden the directory is not fatal: the endpoint file is still
	// usable, the folder just keeps whatever ACL C:\ hands down.
	_ = applyDirACL(dir)
	// Verify that the file can really be replaced in this directory.
	probe := filepath.Join(dir, EndpointFileName+".probe")
	if err := os.WriteFile(probe, []byte("{}"), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dir, err)
	}
	_ = os.Remove(probe)
	return nil
}

// HardenDir applies the discovery folder ACL to path. The installer calls it for
// the folder it creates, and the service calls it through EnsureDir. Failing is
// not fatal for the caller: the folder stays usable with the inherited ACL.
func HardenDir(path string) error { return applyDirACL(path) }

// applyDirACL replaces the directory DACL with dirSDDL.
func applyDirACL(path string) error {
	sd, err := windows.SecurityDescriptorFromString(dirSDDL)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	)
}

// ErrNotFound is returned by ReadEndpoint when no endpoint file exists.
var ErrNotFound = errors.New("wudo endpoint file not found")

// ReadEndpoint loads endpoint.json and remembers the directory it came from.
//
// Two directories can hold an endpoint file at the same time (an elevated
// service publishes into the privileged folder, a non elevated one into the
// per-user folder). The first candidate whose service process is still alive
// wins; if none is alive, the first readable file is returned so the caller can
// report a stale endpoint and clean it up.
func ReadEndpoint() (*Endpoint, error) {
	var (
		first    *Endpoint
		firstDir string
		parseErr error
	)
	for _, dir := range CandidateDirs() {
		path := filepath.Join(dir, EndpointFileName)
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		var ep Endpoint
		if err := json.Unmarshal(data, &ep); err != nil {
			parseErr = fmt.Errorf("parse %s: %w", path, err)
			continue
		}
		if ep.Port <= 0 || ep.Port > 65535 {
			parseErr = fmt.Errorf("invalid port %d in %s", ep.Port, path)
			continue
		}
		ep.Dir = dir
		if processAlive(uint32(ep.PID)) {
			activeDir = dir
			return &ep, nil
		}
		if first == nil {
			first, firstDir = &ep, dir
		}
	}
	if first != nil {
		activeDir = firstDir
		return first, nil
	}
	if parseErr != nil {
		return nil, parseErr
	}
	return nil, ErrNotFound
}

// RemoveEndpointAt deletes the endpoint file in one specific directory.
func RemoveEndpointAt(dir string) {
	if dir == "" {
		return
	}
	_ = os.Remove(filepath.Join(dir, EndpointFileName))
}

// WriteEndpoint atomically publishes endpoint.json in the active directory.
func WriteEndpoint(ep *Endpoint) error {
	dir, err := Resolve()
	if err != nil {
		return err
	}
	ep.Dir = dir
	data, err := json.MarshalIndent(ep, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(dir, EndpointFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	// Windows refuses to rename over an existing file until Go maps it to
	// MoveFileEx with MOVEFILE_REPLACE_EXISTING, which os.Rename does.
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// RemoveEndpoint deletes endpoint.json from every candidate directory,
// ignoring missing files.
func RemoveEndpoint() {
	for _, dir := range CandidateDirs() {
		RemoveEndpointAt(dir)
	}
}
