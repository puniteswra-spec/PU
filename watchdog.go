package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func startWatchdog() {
	selfPID := os.Getpid()

	if os.Getenv("PUNMONITOR_WATCHDOG") == "1" {
		targetPID, _ := strconv.Atoi(os.Getenv("PUNMONITOR_TARGET_PID"))
		if targetPID == 0 {
			return
		}
		go watchdogLoop(targetPID)
		return
	}

	go spawnWatchdog(selfPID)
}

func spawnWatchdog(targetPID int) {
	stopWatchdog()
	time.Sleep(1 * time.Second)

	exe, err := os.Executable()
	if err != nil {
		return
	}

	watchdogDir := dataDir()
	os.MkdirAll(watchdogDir, 0755)

	ext := filepath.Ext(exe)
	watchdogBase := strings.TrimSuffix(filepath.Base(exe), ext) + "-watchdog" + ext
	watchdogPath := filepath.Join(watchdogDir, watchdogBase)

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

	watchdogPIDFile := filepath.Join(dataDir(), "watchdog.pid")
	if data, err := os.ReadFile(watchdogPIDFile); err == nil {
		if pid, _ := strconv.Atoi(string(data)); pid > 0 {
			proc, err := os.FindProcess(pid)
			if err == nil && proc != nil {
				if proc.Signal(syscall.Signal(0)) == nil {
					return
				}
			}
		}
	}

	cmd := exec.Command(watchdogPath)
	cmd.Env = append(os.Environ(),
		"PUNMONITOR_WATCHDOG=1",
		"PUNMONITOR_TARGET_PID="+strconv.Itoa(targetPID),
	)
	hideCmdWindowWithFlags(cmd)
	if err := cmd.Start(); err != nil {
		return
	}

	os.WriteFile(watchdogPIDFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0644)

	go func() {
		cmd.Wait()
		os.Remove(watchdogPIDFile)
	}()
}

func watchdogLoop(targetPID int) {
	watchdogPIDFile := filepath.Join(dataDir(), "watchdog.pid")
	stopFile := filepath.Join(dataDir(), "watchdog.stop")

	os.WriteFile(watchdogPIDFile, []byte(strconv.Itoa(os.Getpid())), 0644)
	defer os.Remove(watchdogPIDFile)

	exe, _ := os.Executable()

	for {
		if _, err := os.Stat(stopFile); err == nil {
			os.Remove(stopFile)
			os.Remove(watchdogPIDFile)
			return
		}

		proc, err := os.FindProcess(targetPID)
		alive := false
		if err == nil && proc != nil {
			if proc.Signal(syscall.Signal(0)) == nil {
				alive = true
			}
		}

		if !alive {
			llog("info", "watchdog: target process %d dead, restarting in 60s...", targetPID)
			time.Sleep(60 * time.Second)

			if _, err := os.Stat(stopFile); err == nil {
				os.Remove(stopFile)
				os.Remove(watchdogPIDFile)
				return
			}

			cmd := exec.Command(exe)
			hideCmdWindowWithFlags(cmd)
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
