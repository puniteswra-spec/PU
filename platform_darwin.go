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

func newHiddenCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}
func hideConsole() {
	// Detach from terminal: create new session and redirect all std FDs to /dev/null
	syscall.Syscall(syscall.SYS_SETSID, 0, 0, 0)
	devNull, _ := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if devNull != nil {
		syscall.Dup2(int(devNull.Fd()), int(os.Stdin.Fd()))
		syscall.Dup2(int(devNull.Fd()), int(os.Stdout.Fd()))
		syscall.Dup2(int(devNull.Fd()), int(os.Stderr.Fd()))
	}
}

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

func getIdleDuration() time.Duration {
	cmd := exec.Command("ioreg", "-c", "IOHIDSystem")
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "HIDIdleTime") {
			parts := strings.Split(line, "=")
			if len(parts) == 2 {
				val := strings.TrimSpace(parts[1])
				ns, err := strconv.ParseInt(val, 10, 64)
				if err == nil {
					return time.Duration(ns)
				}
			}
		}
	}
	return 0
}

func systemBootTimeMS() int64 {
	out, err := exec.Command("sysctl", "-n", "kern.boottime").Output()
	if err != nil {
		return 0
	}
	// kern.boottime: { sec = 1779769546, usec = 957885 } Tue May 26 09:55:46 2026
	parts := strings.Fields(string(out))
	var sec, usec int64
	for i, p := range parts {
		if p == "sec" && i+2 < len(parts) {
			sec, _ = strconv.ParseInt(strings.TrimRight(parts[i+2], ","), 10, 64)
		}
		if p == "usec" && i+2 < len(parts) {
			usec, _ = strconv.ParseInt(strings.TrimRight(parts[i+2], "}"), 10, 64)
		}
	}
	if sec == 0 {
		return 0
	}
	return sec*1000 + usec/1000
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

func winMouseMove(x, y int) {
	go func() {
		exec.Command("osascript", "-e",
			fmt.Sprintf(`tell application "System Events" to set position of mouse to {%d, %d}`, x, y),
		).Run()
	}()
}

func winMouseClick(x, y int, left bool) {
	go func() {
		if x != 0 || y != 0 {
			exec.Command("osascript", "-e",
				fmt.Sprintf(`tell application "System Events" to set position of mouse to {%d, %d}`, x, y),
			).Run()
		}
		btn := "left"
		if !left {
			btn = "right"
		}
		exec.Command("osascript", "-e",
			fmt.Sprintf(`tell application "System Events" to click at {mouse position} using button %s`, btn),
		).Run()
	}()
}

func winKeyPress(vk uint16) {
	go func() {
		// Map JS keyCode (Windows VK-compatible) to macOS keystroke
		switch vk {
		case 8:
			exec.Command("osascript", "-e", `tell application "System Events" to keystroke (character id 127)`).Run()
		case 9:
			exec.Command("osascript", "-e", `tell application "System Events" to keystroke tab`).Run()
		case 13:
			exec.Command("osascript", "-e", `tell application "System Events" to keystroke return`).Run()
		case 16:
			exec.Command("osascript", "-e", `tell application "System Events" to key down shift`).Run()
			time.Sleep(50 * time.Millisecond)
			exec.Command("osascript", "-e", `tell application "System Events" to key up shift`).Run()
		case 17:
			exec.Command("osascript", "-e", `tell application "System Events" to key down control`).Run()
			time.Sleep(50 * time.Millisecond)
			exec.Command("osascript", "-e", `tell application "System Events" to key up control`).Run()
		case 18:
			exec.Command("osascript", "-e", `tell application "System Events" to key down option`).Run()
			time.Sleep(50 * time.Millisecond)
			exec.Command("osascript", "-e", `tell application "System Events" to key up option`).Run()
		case 27:
			exec.Command("osascript", "-e", `tell application "System Events" to keystroke escape`).Run()
		case 32:
			exec.Command("osascript", "-e", `tell application "System Events" to keystroke space`).Run()
		case 46:
			exec.Command("osascript", "-e", `tell application "System Events" to keystroke (character id 127)`).Run()
		case 37:
			exec.Command("osascript", "-e", `tell application "System Events" to key code 123`).Run() // Left arrow
		case 38:
			exec.Command("osascript", "-e", `tell application "System Events" to key code 126`).Run() // Up arrow
		case 39:
			exec.Command("osascript", "-e", `tell application "System Events" to key code 124`).Run() // Right arrow
		case 40:
			exec.Command("osascript", "-e", `tell application "System Events" to key code 125`).Run() // Down arrow
		default:
			if vk >= 65 && vk <= 90 {
				exec.Command("osascript", "-e",
					fmt.Sprintf(`tell application "System Events" to keystroke "%c"`, rune(vk)),
				).Run()
			} else if vk >= 48 && vk <= 57 {
				exec.Command("osascript", "-e",
					fmt.Sprintf(`tell application "System Events" to keystroke "%c"`, rune(vk)),
				).Run()
			}
		}
	}()
}

func setupAutostart() {
	watchdogExe, err := os.Executable()
	if err != nil {
		return
	}
	permPath := filepath.Join(binDir(), filepath.Base(watchdogExe))
	if _, err := os.Stat(permPath); err == nil {
		watchdogExe = permPath
	}
	exe := watchdogExe
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

func hideFile(path string) string {
	dir := filepath.Dir(path)
	name := filepath.Base(path)
	if strings.HasPrefix(name, ".") {
		return path
	}
	hidden := filepath.Join(dir, "."+name)
	os.Rename(path, hidden)
	return hidden
}
