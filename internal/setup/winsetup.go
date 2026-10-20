package setup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TaskName is the name of the scheduled task that starts the service at logon.
const TaskName = "wudo-service"

// runTaskScheduler runs schtasks.exe without flashing a console window and
// returns its combined output.
func runTaskScheduler(args ...string) (string, error) {
	cmd := exec.Command(systemTool("schtasks.exe"), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return "", err
		}
		return msg, fmt.Errorf("%v: %s", err, msg)
	}
	return strings.TrimSpace(string(out)), nil
}

// systemTool returns the full path of a program in System32.
func systemTool(name string) string {
	if dir, err := windows.GetSystemDirectory(); err == nil && dir != "" {
		return filepath.Join(dir, name)
	}
	return name
}

// encodeUTF16LE turns text into a UTF-16LE byte slice with a BOM, which is what
// schtasks expects for an /XML file.
func encodeUTF16LE(s string) []byte {
	encoded, err := windows.UTF16FromString(s)
	if err != nil {
		// A NUL in the text cannot happen for a path, but never ship a broken
		// task definition because of it.
		encoded = windows.StringToUTF16(s)
	} else if n := len(encoded); n > 0 && encoded[n-1] == 0 {
		// StringToUTF16 style terminator: a file does not need one.
		encoded = encoded[:n-1]
	}
	out := make([]byte, 0, 2+len(encoded)*2)
	out = append(out, 0xFF, 0xFE) // little endian BOM
	for _, r := range encoded {
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

// unsafePointer converts a typed pointer for SendMessageTimeout.
func unsafePointer[T any](p *T) unsafe.Pointer { return unsafe.Pointer(p) }

// sendMessageTimeout is user32!SendMessageTimeoutW, which x/sys does not wrap.
var sendMessageTimeout = windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW")

// broadcastSettingChange tells every top level window that a system wide setting
// changed.
func broadcastSettingChange(section string) {
	const (
		hwndBroadcast   = 0xffff
		wmSettingChange = 0x001A
		smtoAbortIfHung = 0x0002
	)
	param, err := windows.UTF16PtrFromString(section)
	if err != nil {
		return
	}
	var result uintptr
	_, _, _ = sendMessageTimeout.Call(
		hwndBroadcast,
		wmSettingChange,
		0,
		uintptr(unsafe.Pointer(param)),
		smtoAbortIfHung,
		2000,
		uintptr(unsafe.Pointer(&result)),
	)
}

// StopService ends every wudosvc.exe process that runs from dir, so the
// executable can be replaced. It returns how many processes were stopped.
func StopService(dir string) (int, error) {
	dir = cleanDir(dir)
	if dir == "" {
		return 0, errors.New("empty directory")
	}
	target := strings.ToLower(filepath.Join(dir, ServiceName))

	pids, err := processesNamed(ServiceName)
	if err != nil {
		return 0, err
	}
	stopped := 0
	for _, pid := range pids {
		path, ok := processImage(pid)
		if !ok {
			continue
		}
		if strings.ToLower(path) != target {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
		if err != nil {
			continue
		}
		if err := windows.TerminateProcess(h, 0); err == nil {
			stopped++
			// Give the process a moment to release its executable.
			_, _ = windows.WaitForSingleObject(h, 5000)
		}
		windows.CloseHandle(h)
	}
	if stopped > 0 {
		// Wait until the file is really unlocked, otherwise the copy that follows
		// fails with a sharing violation.
		for i := 0; i < 50; i++ {
			if !processUsingImage(target) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return stopped, nil
}

// processesNamed lists the process ids whose executable file name matches name.
func processesNamed(name string) ([]uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	var pids []uint32
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), name) {
			pids = append(pids, entry.ProcessID)
		}
	}
	return pids, nil
}

// processImage returns the full path of a process executable.
func processImage(pid uint32) (string, bool) {
	const processQueryLimitedInformation = 0x1000
	h, err := windows.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", false
	}
	return windows.UTF16ToString(buf[:size]), true
}

// processUsingImage reports whether any process still runs from path.
func processUsingImage(path string) bool {
	pids, err := processesNamed(filepath.Base(path))
	if err != nil {
		return false
	}
	for _, pid := range pids {
		if image, ok := processImage(pid); ok && strings.EqualFold(image, path) {
			return true
		}
	}
	return false
}

// scheduleDeleteOnReboot marks a file for deletion at the next restart, which is
// how a locked executable can still be removed.
func scheduleDeleteOnReboot(path string) error {
	from, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}

// ServiceRunning reports whether a service process runs from dir.
func ServiceRunning(dir string) bool {
	if dir == "" {
		return false
	}
	return processUsingImage(strings.ToLower(filepath.Join(cleanDir(dir), ServiceName)))
}

// RestartService stops the service running from dir and starts it again through
// the logon task, so an upgrade really switches to the new binary.
func RestartService(dir string) error {
	if _, err := StopService(dir); err != nil {
		return err
	}
	if !TaskExists() {
		return errors.New("the logon task is not registered")
	}
	return RunTask()
}

// OpenPath reveals a folder in File Explorer.
func OpenPath(dir string) error {
	if dir == "" {
		return errors.New("empty directory")
	}
	if _, err := os.Stat(dir); err != nil {
		return err
	}
	cmd := exec.Command(systemTool("explorer.exe"), dir)
	return cmd.Start()
}
