//go:build !windows && !darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func newHiddenCmd(cmd *exec.Cmd)   {}
func newDetachedCmd(cmd *exec.Cmd) {}
func hideConsole()                 {}
func addDefenderExclusion()        {}

func watchdogSingleton() bool {
	lockFile := filepath.Join(os.TempDir(), "PunMonitorWatchdog.lock")
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func singleton() bool {
	lockFile := filepath.Join(os.TempDir(), "PunMonitor.lock")
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func systemBootTimeMS() int64 { return 0 }

type IdleDetector struct{}

func NewIdleDetector(threshold time.Duration, callback func(idle bool)) *IdleDetector {
	return &IdleDetector{}
}

func (id *IdleDetector) Start(ctx context.Context) {}
func (id *IdleDetector) Stop()                     {}
func (id *IdleDetector) loop(ctx context.Context)  {}

func winMouseMove(x, y int)             {}
func winMouseClick(x, y int, left bool) {}
func winKeyPress(vk uint16)             {}
func winTypeText(text string)           {}

func setupAutostart()  {}
func removeAutostart() {}

func monitorAlreadyRunning() bool { return false }

func cleanDuplicateAutostartEntries() {}

// platformStableMachineID returns "" on non-Windows, non-Darwin (Linux).
// Caller falls back to SHA-1 of hostname.
func platformStableMachineID() string {
	return ""
}

func ensureSingleInstance(replaceExisting bool) bool      { return true }
func killAllPunMonitorImages()                            {}
func tryAcquireSingletonMutex() bool                      { return true }
func releaseSingleton()                                   {}
func killOtherPunMonitorProcesses(selfPID int)            {}
func writePIDFile()                                       {}
func removePIDFile()                                      {}
func isPortInUse(port int) bool                           { return false }
func updateSystemInfoFromActivity(info map[string]string) {}

func getIdleDuration() time.Duration { return 0 }

func hideFile(path string) string {
	dir := filepath.Dir(path)
	name := filepath.Base(path)
	if len(name) > 0 && name[0] == '.' {
		return path
	}
	hidden := filepath.Join(dir, "."+name)
	os.Rename(path, hidden)
	return hidden
}

func detectAndRunService() bool { return false }

var isServiceMode bool

// Windows-specific stubs for non-Windows platforms
func enforceWindowsMinimumVersion() {}

// Service stubs for non-Windows platforms
func installService() error         { return nil }
func removeService() error          { return nil }
func copySettingsToProgramData()    {}
