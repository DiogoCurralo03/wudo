package tray

import (
	_ "embed"
	"os/exec"

	"fyne.io/systray"
)

// The tray icons live in the binary, so the service stays a single file.
//
//go:embed assets/inactive.ico
var inactiveIcon []byte

//go:embed assets/active.ico
var activeIcon []byte

// setIcon switches the notification area icon between the inactive and the
// active artwork. systray on Windows wants .ico bytes and caches them by hash,
// so the two states are distinct files.
func setIcon(active bool) {
	if active {
		systray.SetIcon(activeIcon)
		return
	}
	systray.SetIcon(inactiveIcon)
}

// openLog asks the shell to open the log file in Notepad.
func openLog(path string) {
	if path == "" {
		return
	}
	_ = exec.Command("notepad.exe", path).Start()
}

// openFolder opens the state directory in File Explorer.
func openFolder(dir string) {
	if dir == "" {
		return
	}
	_ = exec.Command("explorer.exe", dir).Start()
}
