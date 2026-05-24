//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func newHiddenCmd(cmd *exec.Cmd) {}
func hideConsole()                  {}

func watchdogSingleton() bool {
	lockFile := filepath.Join(os.TempDir(), "PunMonitorWatchdog.lock")
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err == nil {
		fmt.Fprintf(f, "%d", os.Getpid())
		f.Close()
		return true
	}
	// Lock file exists; check if it's stale
	data, err := os.ReadFile(lockFile)
	if err == nil {
		s := strings.TrimSpace(string(data))
		if s == "" {
			os.Remove(lockFile)
			return watchdogSingleton()
		}
		if pid, err := strconv.Atoi(s); err == nil && pid > 0 {
			if isProcessRunning(pid) {
				// Another live instance holds the lock
				return false
			}
			// Stale lock file, remove and retry
			os.Remove(lockFile)
			return watchdogSingleton()
		}
		// Unparseable content, treat as stale
		os.Remove(lockFile)
		return watchdogSingleton()
	}
	// Could not read lock file; assume locked
	return false
}

func singleton() bool {
	lockFile := filepath.Join(os.TempDir(), "PunMonitor.lock")
	f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err == nil {
		fmt.Fprintf(f, "%d", os.Getpid())
		f.Close()
		return true
	}
	// Lock file exists; check if it's stale
	data, err := os.ReadFile(lockFile)
	if err == nil {
		s := strings.TrimSpace(string(data))
		if pid, err := strconv.Atoi(s); err == nil && pid > 0 {
			if isProcessRunning(pid) {
				// Another live instance holds the lock
				return false
			}
			// Stale lock file, remove and retry
			os.Remove(lockFile)
			return singleton()
		}
	}
	// Could not read or parse lock file; assume locked
	return false
}

func isProcessRunning(pid int) bool {
	// On Unix, send signal 0 to check existence
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if err == syscall.ESRCH {
		return false
	}
	// EPERM means process exists but no permission to signal
	if err == syscall.EPERM {
		return true
	}
	return false
}

func systemBootTimeMS() int64 {
	return 0
}

type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

type IdleDetector struct {
	threshold time.Duration
	callback  func(idle bool)
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

func NewIdleDetector(threshold time.Duration, callback func(idle bool)) *IdleDetector {
	return &IdleDetector{
		threshold: threshold,
		callback:  callback,
		stopCh:    make(chan struct{}),
	}
}

func (id *IdleDetector) Start(ctx context.Context) {
	id.wg.Add(1)
	go func() {
		defer id.wg.Done()
		<-ctx.Done()
	}()
}

func (id *IdleDetector) Stop() {
	close(id.stopCh)
	id.wg.Wait()
}

func winMouseMove(x, y int)  {}
func winMouseClick(x, y int, left bool) {}
func winKeyPress(vk uint16)  {}
func winTypeText(text string) {}

func setupAutostart() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	launchDir := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")
	os.MkdirAll(launchDir, 0755)
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.punmonitor.watchdog</string>
    <key>ProgramArguments</key>
    <array>
        <string>` + exe + `</string>
        <string>--watchdog</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ProcessType</key>
    <string>Background</string>
</dict>
</plist>`
	plistPath := filepath.Join(launchDir, "com.punmonitor.watchdog.plist")
	os.WriteFile(plistPath, []byte(plist), 0644)
	exec.Command("launchctl", "load", plistPath).Run()
	llog("info", "Autostart installed via LaunchAgent")
}

func removeAutostart() {
	plistPath := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", "com.punmonitor.watchdog.plist")
	exec.Command("launchctl", "unload", plistPath).Run()
	os.Remove(plistPath)
	llog("info", "Autostart removed")
}

func cleanDuplicateAutostartEntries() {}

func ensureSingleInstance(replaceExisting bool) bool { return true }
func killAllPunMonitorImages()                       {}
func tryAcquireSingletonMutex() bool                 { return true }
func releaseSingleton()                              {}
func killOtherPunMonitorProcesses(selfPID int)        {}
func writePIDFile()                                   {}
func removePIDFile()                                  {}
func isPortInUse(port int) bool                       { return false }
func updateSystemInfoFromActivity(info map[string]string) {}
