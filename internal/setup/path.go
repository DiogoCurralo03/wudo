package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// environmentKey is where the machine wide PATH lives.
const environmentKey = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`

// AddToMachinePath appends dir to the machine PATH when it is not there yet. It
// reports whether the value changed.
func AddToMachinePath(dir string) (bool, error) {
	dir = cleanDir(dir)
	if dir == "" {
		return false, errors.New("empty directory")
	}
	current, valueType, err := readMachinePath()
	if err != nil {
		return false, err
	}
	updated, changed := addToPathValue(current, dir)
	if !changed {
		return false, nil
	}
	if err := writeMachinePath(updated, valueType); err != nil {
		return false, err
	}
	broadcastEnvironmentChange()
	return true, nil
}

// RemoveFromMachinePath drops dir from the machine PATH. It reports whether the
// value changed.
func RemoveFromMachinePath(dir string) (bool, error) {
	dir = cleanDir(dir)
	if dir == "" {
		return false, errors.New("empty directory")
	}
	current, valueType, err := readMachinePath()
	if err != nil {
		return false, err
	}
	updated, removed := removeFromPathValue(current, dir)
	if !removed {
		return false, nil
	}
	if err := writeMachinePath(updated, valueType); err != nil {
		return false, err
	}
	broadcastEnvironmentChange()
	return true, nil
}

// addToPathValue appends dir to a PATH value. It reports whether the value
// changed, so an installer run twice is silent the second time.
func addToPathValue(current, dir string) (string, bool) {
	dir = cleanDir(dir)
	if dir == "" {
		return current, false
	}
	if pathContains(current, dir) {
		return current, false
	}
	updated := current
	if updated != "" && !strings.HasSuffix(updated, ";") {
		updated += ";"
	}
	return updated + dir, true
}

// removeFromPathValue drops every spelling of dir from a PATH value.
func removeFromPathValue(current, dir string) (string, bool) {
	dir = cleanDir(dir)
	if dir == "" {
		return current, false
	}
	entries := strings.Split(current, ";")
	kept := make([]string, 0, len(entries))
	removed := false
	for _, entry := range entries {
		if sameDir(entry, dir) {
			removed = true
			continue
		}
		kept = append(kept, entry)
	}
	if !removed {
		return current, false
	}
	return strings.Join(kept, ";"), true
}

// pathContains reports whether dir is already an entry of the PATH value.
func pathContains(current, dir string) bool {
	for _, entry := range strings.Split(current, ";") {
		if sameDir(entry, dir) {
			return true
		}
	}
	return false
}

// sameDir compares two PATH entries, ignoring quotes, trailing separators,
// environment variables and letter case, because all of those are common ways to
// write the same folder.
func sameDir(entry, dir string) bool {
	a := cleanDir(os.ExpandEnv(entry))
	b := cleanDir(dir)
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(a, b)
}

// MachinePath returns the machine wide PATH as it is stored.
func MachinePath() (string, error) {
	current, _, err := readMachinePath()
	return current, err
}

// readMachinePath reads PATH and the registry type it uses. The type matters:
// a PATH of type REG_SZ stops expanding %SystemRoot%, so it is preserved.
func readMachinePath() (string, uint32, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, environmentKey, registry.QUERY_VALUE)
	if err != nil {
		return "", 0, err
	}
	defer key.Close()
	value, valueType, err := key.GetStringValue("Path")
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", registry.EXPAND_SZ, nil
		}
		return "", 0, err
	}
	return value, valueType, nil
}

// writeMachinePath stores PATH, keeping its original type.
func writeMachinePath(value string, valueType uint32) error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, environmentKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if valueType == registry.SZ {
		return key.SetStringValue("Path", value)
	}
	return key.SetExpandStringValue("Path", value)
}

// broadcastEnvironmentChange tells the running programs that the environment
// changed, so a new shell picks the new PATH up without signing out.
func broadcastEnvironmentChange() { broadcastSettingChange("Environment") }

// RegisterTask makes the service start with Windows, already elevated and
// without asking for UAC consent.
//
// A scheduled task is the only supported way to do that: it stores a principal
// with the highest run level, so the Task Scheduler launches the executable
// elevated directly. There is no consent prompt because nothing is elevated at
// launch time - the task was created with the consent the installer already had.
func RegisterTask(servicePath string) error {
	servicePath = cleanDir(servicePath)
	if servicePath == "" {
		return errors.New("no service path")
	}
	// Preferred: a full task definition. It pins the principal to the account
	// running the installer with an interactive token and the highest run level,
	// which is exactly the "elevated tray application" combination and needs no
	// password.
	xmlErr := registerTaskFromXML(servicePath)
	if xmlErr == nil {
		return nil
	}
	// Fallback: let schtasks build the definition. It is less precise but always
	// produces something the Task Scheduler accepts.
	if err := registerTaskFromFlags(servicePath); err != nil {
		return fmt.Errorf("%v (task definition also rejected: %v)", err, xmlErr)
	}
	return nil
}

// registerTaskFromXML imports the XML definition staged in the temp folder.
func registerTaskFromXML(servicePath string) error {
	// schtasks reads UTF-16 when the file starts with a BOM.
	if err := os.WriteFile(taskXMLPath(), encodeUTF16LE(taskXML(servicePath)), 0o644); err != nil {
		return err
	}
	defer os.Remove(taskXMLPath())
	_, err := runTaskScheduler("/Create", "/TN", TaskName, "/XML", taskXMLPath(), "/F")
	return err
}

// registerTaskFromFlags creates the task with plain command line switches.
func registerTaskFromFlags(servicePath string) error {
	_, err := runTaskScheduler(
		"/Create",
		"/TN", TaskName,
		"/TR", servicePath,
		"/SC", "ONLOGON",
		"/RL", "HIGHEST",
		"/F",
	)
	return err
}

// DeleteTask removes the logon task. It reports whether a task was actually
// removed, so the caller does not claim to have deleted something that was never
// there.
func DeleteTask() (bool, error) {
	if !TaskExists() {
		return false, nil
	}
	if _, err := runTaskScheduler("/Delete", "/TN", TaskName, "/F"); err != nil {
		return false, err
	}
	return true, nil
}

// TaskExists reports whether the logon task is registered.
func TaskExists() bool {
	_, err := runTaskScheduler("/Query", "/TN", TaskName)
	return err == nil
}

// RunTask starts the service through the task, which is what makes it start
// elevated without a prompt.
func RunTask() error {
	_, err := runTaskScheduler("/Run", "/TN", TaskName)
	return err
}

// taskXMLPath is where the task definition is staged before registration.
func taskXMLPath() string { return filepath.Join(os.TempDir(), "wudo-task.xml") }

// taskXML builds the Task Scheduler definition.
//
// LogonType InteractiveToken with the invoking user's SID means "run in that
// user's session", which is what a tray icon needs. At logon the same task
// starts the service for the user who is logging in.
func taskXML(servicePath string) string {
	sid := currentUserSID()
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>wudo service - starts elevated at logon so that wudo commands can run as administrator.</Description>
    <URI>\%s</URI>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>%s</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
    </Exec>
  </Actions>
</Task>
`, TaskName, sid, sid, xmlEscape(servicePath))
}

// xmlEscape escapes the characters that may appear in a Windows path.
func xmlEscape(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
	)
	return replacer.Replace(s)
}

// currentUserSID returns the SID of the account running the installer.
func currentUserSID() string {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || tu.User.Sid == nil {
		return ""
	}
	return tu.User.Sid.String()
}
