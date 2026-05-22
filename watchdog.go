//go:build windows

package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Watchdog monitors the main PunMonitor process and restarts it if killed.
// Only manager.cmd or Dashboard can stop the watchdog.
func startWatchdog() {
	selfPID := os.Getpid()

	// If we're the watchdog, monitor the child
	if os.Getenv("PUNMONITOR_WATCHDOG") == "1" {
		targetPID, _ := strconv.Atoi(os.Getenv("PUNMONITOR_TARGET_PID"))
		if targetPID == 0 {
			return
		}
		go watchdogLoop(targetPID)
		return
	}

	// If we're the main process, spawn watchdog
	go spawnWatchdog(selfPID)
}

func spawnWatchdog(targetPID int) {
	// Ensure any existing watchdog is stopped (orphaned watchdog may be monitoring a previous instance)
	stopWatchdog()
	time.Sleep(1 * time.Second) // give it time to exit

	exe, err := os.Executable()
	if err != nil {
		return
	}

	// Copy watchdog to a hidden location with a different name to survive Task Manager kills
	watchdogDir := filepath.Join(os.Getenv("APPDATA"), "PunMonitor")
	os.MkdirAll(watchdogDir, 0755)
	watchdogPath := filepath.Join(watchdogDir, "PunMonitor-watchdog.exe")

	// Copy executable if not already done or if source is newer
	srcInfo, _ := os.Stat(exe)
	dstInfo, dstErr := os.Stat(watchdogPath)
	needCopy := dstErr != nil
	if !needCopy && srcInfo != nil {
		needCopy = srcInfo.ModTime().After(dstInfo.ModTime())
	}
	if needCopy {
		src, err := os.Open(exe)
		if err != nil {
			return
		}
		defer src.Close()
		dst, err := os.Create(watchdogPath)
		if err != nil {
			return
		}
		io.Copy(dst, src)
		dst.Close()
	}

	// Check if watchdog is already running
	watchdogPIDFile := filepath.Join(dataDir(), "watchdog.pid")
	if data, err := os.ReadFile(watchdogPIDFile); err == nil {
		if pid, _ := strconv.Atoi(string(data)); pid > 0 {
			// Check if process is still running
			proc, err := os.FindProcess(pid)
			if err == nil && proc != nil {
				// Try to signal it
				if proc.Signal(syscall.Signal(0)) == nil {
					return // Watchdog already running
				}
			}
		}
	}

	cmd := exec.Command(watchdogPath)
	cmd.Env = append(os.Environ(),
		"PUNMONITOR_WATCHDOG=1",
		"PUNMONITOR_TARGET_PID="+strconv.Itoa(targetPID),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
	if err := cmd.Start(); err != nil {
		return
	}

	// Write watchdog PID
	os.WriteFile(watchdogPIDFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0644)

	// Cleanup watchdog PID on exit
	go func() {
		cmd.Wait()
		os.Remove(watchdogPIDFile)
	}()
}

func watchdogLoop(targetPID int) {
	watchdogPIDFile := filepath.Join(dataDir(), "watchdog.pid")
	stopFile := filepath.Join(dataDir(), "watchdog.stop")

	// Write our PID
	os.WriteFile(watchdogPIDFile, []byte(strconv.Itoa(os.Getpid())), 0644)
	defer os.Remove(watchdogPIDFile)

	exe, _ := os.Executable()

	for {
		// Check for stop signal
		if _, err := os.Stat(stopFile); err == nil {
			os.Remove(stopFile)
			os.Remove(watchdogPIDFile)
			return
		}

		// Check if target process is alive
		proc, err := os.FindProcess(targetPID)
		alive := false
		if err == nil && proc != nil {
			if proc.Signal(syscall.Signal(0)) == nil {
				alive = true
			}
		}

		if !alive {
			// Process is dead, wait 60 seconds then restart
			llog("info", "watchdog: target process %d dead, restarting in 60s...", targetPID)
			time.Sleep(60 * time.Second)

			// Check again for stop signal
			if _, err := os.Stat(stopFile); err == nil {
				os.Remove(stopFile)
				os.Remove(watchdogPIDFile)
				return
			}

			// Restart the process
			cmd := exec.Command(exe)
			cmd.SysProcAttr = &syscall.SysProcAttr{
				HideWindow:    true,
				CreationFlags: 0x08000000,
			}
			if err := cmd.Start(); err == nil {
				llog("info", "watchdog: restarted process (PID %d)", cmd.Process.Pid)
				targetPID = cmd.Process.Pid
			} else {
				llog("error", "watchdog: failed to restart: %v", err)
				time.Sleep(30 * time.Second)
			}
		}

		time.Sleep(5 * time.Second)
	}
}

func stopWatchdog() {
	stopFile := filepath.Join(dataDir(), "watchdog.stop")
	os.WriteFile(stopFile, []byte("1"), 0644)
	llog("info", "watchdog stop signal sent")
}
