// Package winproc wraps the handful of Win32 facilities wudo needs from both
// the service and the client.
package winproc

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Job is a Windows job object used to keep every command and its descendants
// in one killable container. The job is created with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so as soon as the handle is closed every
// process still inside the job dies. A hung command can therefore never leak
// elevated processes, and it can never affect other sessions.
type Job struct {
	handle windows.Handle
}

// NewJob creates a job object with kill-on-close semantics.
func NewJob() (*Job, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return &Job{handle: h}, nil
}

// Assign puts a process (and, by inheritance, its future children) into the
// job. The handle must have PROCESS_SET_QUOTA and PROCESS_TERMINATE access,
// which is what CreateProcess returns.
func (j *Job) Assign(process windows.Handle) error {
	if j == nil || j.handle == 0 {
		return nil
	}
	if err := windows.AssignProcessToJobObject(j.handle, process); err != nil {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// Terminate kills every process in the job with the given exit code.
func (j *Job) Terminate(exitCode uint32) error {
	if j == nil || j.handle == 0 {
		return nil
	}
	if err := windows.TerminateJobObject(j.handle, exitCode); err != nil {
		return fmt.Errorf("TerminateJobObject: %w", err)
	}
	return nil
}

// Close releases the job handle, which also kills whatever is still inside.
func (j *Job) Close() error {
	if j == nil || j.handle == 0 {
		return nil
	}
	h := j.handle
	j.handle = 0
	return windows.CloseHandle(h)
}

// IsElevated reports whether the current process token is elevated.
func IsElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// CurrentUser returns DOMAIN\user for the current process token.
func CurrentUser() string {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return ""
	}
	return SidToAccountName(tu.User.Sid)
}

// SidToAccountName resolves a SID to DOMAIN\user form.
func SidToAccountName(sid *windows.SID) string {
	if sid == nil {
		return ""
	}
	account, domain, _, err := sid.LookupAccount("")
	if err != nil {
		return sid.String()
	}
	if domain == "" {
		return account
	}
	return domain + `\` + account
}

// MessageBox shows a modal message box owned by no window. It is used for
// startup problems the user must see even though the service has no console.
func MessageBox(title, text string, flags uint32) error {
	t, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return err
	}
	c, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return err
	}
	_, err = windows.MessageBox(0, t, c, flags)
	return err
}

// Message box flags used by wudo.
const (
	MBOK              = 0x00000000
	MBIconError       = 0x00000010
	MBIconWarning     = 0x00000030
	MBIconInformation = 0x00000040
	MBSetForeground   = 0x00010000
	MBTopmost         = 0x00040000
)

// ComSpec returns the command interpreter to use for shell style execution.
func ComSpec() string {
	if v, ok := windows.Getenv("ComSpec"); ok && v != "" {
		return v
	}
	return `C:\Windows\System32\cmd.exe`
}

// SafeDir returns dir when it names an existing directory, otherwise it falls
// back to the caller's current working directory.
func SafeDir(dir string) string {
	if dir == "" {
		return ""
	}
	clean := strings.TrimSpace(dir)
	if clean == "" {
		return ""
	}
	if info, err := statDir(clean); err == nil && info {
		return clean
	}
	return ""
}

// ProcessAlive reports whether a process with the given pid is still running.
func ProcessAlive(pid uint32) bool {
	if pid == 0 {
		return false
	}
	const processQueryLimitedInformation = 0x1000
	h, err := windows.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
