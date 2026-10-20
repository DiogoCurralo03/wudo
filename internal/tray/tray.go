// Package tray renders the wudo service in the Windows notification area: it
// shows whether the service is active, how long it stays active and how many
// commands are running, and it lets the user activate the service for a fixed
// period or deactivate it again.
package tray

import (
	"fmt"
	"time"

	"fyne.io/systray"
)

// State is what the tray needs to render.
type State struct {
	// Active reports whether commands may run right now.
	Active bool
	// ExpiresAt is the activation deadline; zero when inactive.
	ExpiresAt time.Time
	// Sessions is the number of commands currently running.
	Sessions int
	// Elevated reports whether the service really has administrator rights.
	Elevated bool
	// Port is the loopback port the service listens on.
	Port int
	// Version is the service version shown in the menu.
	Version string
}

// Controller is implemented by the service behind the tray.
type Controller interface {
	// State returns the current state for rendering.
	State() State
	// Activate turns the service on for d.
	Activate(d time.Duration)
	// Deactivate turns the service off.
	Deactivate()
	// Quit stops the service.
	Quit()
	// LogPath returns the path of the log file, shown in the menu.
	LogPath() string
	// Dir returns the state directory in use.
	Dir() string
}

// Durations offered in the Activate submenu.
var Durations = []time.Duration{
	time.Hour,
	2 * time.Hour,
	3 * time.Hour,
	6 * time.Hour,
	12 * time.Hour,
	24 * time.Hour,
}

// refreshInterval is how often the countdown in the status line is redrawn.
const refreshInterval = time.Second

// Run shows the tray icon and blocks until it is closed. It must be called from
// the main goroutine: systray drives the Win32 message loop there.
func Run(ctl Controller) {
	systray.Run(func() { onReady(ctl) }, func() {})
}

// menuView groups the menu entries so the periodic refresh stays readable.
type menuView struct {
	ctl        Controller
	status     *systray.MenuItem
	sessions   *systray.MenuItem
	deactivate *systray.MenuItem
	about      *systray.MenuItem
	lastActive bool
	iconSet    bool
}

func onReady(ctl Controller) {
	systray.SetTooltip("wudo service")
	setIcon(false)

	view := &menuView{ctl: ctl}

	view.status = systray.AddMenuItem("Status", "Current wudo service state")
	view.status.Disable()

	activateMenu := systray.AddMenuItem("Activate", "Allow wudo to run commands with administrator rights")
	for _, d := range Durations {
		d := d
		item := activateMenu.AddSubMenuItem(durationLabel(d), fmt.Sprintf("Activate the service for %s", durationLabel(d)))
		go func() {
			for range item.ClickedCh {
				ctl.Activate(d)
				refreshView(view)
			}
		}()
	}

	systray.AddSeparator()
	view.sessions = systray.AddMenuItem("No commands running", "Commands currently executing")
	view.sessions.Disable()

	logItem := systray.AddMenuItem("Open log file", "Open the wudo service log in Notepad")
	go func() {
		for range logItem.ClickedCh {
			openLog(ctl.LogPath())
		}
	}()

	dirItem := systray.AddMenuItem("State folder", "Folder holding endpoint.json and the log")
	go func() {
		for range dirItem.ClickedCh {
			openFolder(ctl.Dir())
		}
	}()

	view.about = systray.AddMenuItem("Version", "Version, port and elevation of this service")
	view.about.Disable()

	systray.AddSeparator()
	view.deactivate = systray.AddMenuItem("Deactivate", "Stop accepting commands and terminate the running ones")
	go func() {
		for range view.deactivate.ClickedCh {
			ctl.Deactivate()
			refreshView(view)
		}
	}()

	quitItem := systray.AddMenuItem("Quit", "Stop the wudo service")
	go func() {
		for range quitItem.ClickedCh {
			ctl.Quit()
			systray.Quit()
		}
	}()

	refreshView(view)
	go func() {
		t := time.NewTicker(refreshInterval)
		defer t.Stop()
		for range t.C {
			refreshView(view)
		}
	}()
}

// refreshView re-renders every menu entry from the controller state.
func refreshView(v *menuView) {
	st := v.ctl.State()

	if !v.iconSet || st.Active != v.lastActive {
		setIcon(st.Active)
		v.lastActive = st.Active
		v.iconSet = true
	}

	v.status.SetTitle(statusText(st))
	if st.Active {
		v.deactivate.Enable()
	} else {
		v.deactivate.Disable()
	}

	switch st.Sessions {
	case 0:
		v.sessions.SetTitle("No commands running")
	case 1:
		v.sessions.SetTitle("1 command running")
	default:
		v.sessions.SetTitle(fmt.Sprintf("%d commands running", st.Sessions))
	}

	elevation := "elevated"
	if !st.Elevated {
		elevation = "NOT elevated"
	}
	v.about.SetTitle(fmt.Sprintf("wudo %s - port %d - %s", st.Version, st.Port, elevation))

	systray.SetTooltip(tooltipText(st))
}

func statusText(st State) string {
	if !st.Active {
		if !st.Elevated {
			return "Inactive (service is not elevated)"
		}
		return "Inactive"
	}
	if st.ExpiresAt.IsZero() {
		return "Active"
	}
	left := time.Until(st.ExpiresAt).Round(time.Second)
	if left < 0 {
		left = 0
	}
	return fmt.Sprintf("Active until %s (%s left)", st.ExpiresAt.Local().Format("15:04"), formatLeft(left))
}

func tooltipText(st State) string {
	if !st.Active {
		return fmt.Sprintf("wudo - inactive (port %d)", st.Port)
	}
	if st.ExpiresAt.IsZero() {
		return "wudo - active"
	}
	return fmt.Sprintf("wudo - active, %s left", formatLeft(time.Until(st.ExpiresAt)))
}

// formatLeft renders a duration as "2h 05m", "12m 30s" or "45s".
func formatLeft(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	s := int(d/time.Second) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func durationLabel(d time.Duration) string {
	hours := int(d / time.Hour)
	switch hours {
	case 1:
		return "For 1 hour"
	case 24:
		return "For 24 hours"
	default:
		return fmt.Sprintf("For %d hours", hours)
	}
}
