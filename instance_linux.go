//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

const singletonMutexName = "PunMonitorSingleton"

var (
	singletonLock      *os.File
	punMonitorImageNames = []string{"PunMonitor", "punmonitor"}
)

func ensureSingleInstance(replaceExisting bool) bool {
	self := os.Getpid()
	killOtherPunMonitorProcesses(self)
	wait := 600 * time.Millisecond
	if replaceExisting {
		killAllPunMonitorImages()
		wait = 1500 * time.Millisecond
	}
	time.Sleep(wait)
	if tryAcquireSingletonMutex() {
		writePIDFile()
		llog("info", "single instance active (pid %d)", self)
		return true
	}
	killOtherPunMonitorProcesses(self)
	time.Sleep(1200 * time.Millisecond)
	if tryAcquireSingletonMutex() {
		writePIDFile()
		llog("info", "single instance acquired after cleanup (pid %d)", self)
		return true
	}
	llog("info", "PunMonitor already running — exiting (single instance only)")
	return false
}

func killAllPunMonitorImages() {
	for _, img := range punMonitorImageNames {
		cmd := exec.Command("pkill", "-9", "-f", img)
		_ = cmd.Run()
	}
}

func tryAcquireSingletonMutex() bool {
	lockDir := filepath.Join(dataDir())
	os.MkdirAll(lockDir, 0755)
	lockPath := filepath.Join(lockDir, "punmonitor.lock")

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return false
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return false
	}

	if singletonLock != nil {
		singletonLock.Close()
	}
	singletonLock = f
	return true
}

func releaseSingleton() {
	if singletonLock != nil {
		unix.Flock(int(singletonLock.Fd()), unix.LOCK_UN)
		singletonLock.Close()
		singletonLock = nil
	}
	removePIDFile()
}

func killOtherPunMonitorProcesses(selfPID int) {
	if selfPID == 0 {
		selfPID = os.Getpid()
	}
	for _, img := range punMonitorImageNames {
		// Use pgrep + grep to exclude self PID, then kill
		kill := exec.Command("sh", "-c",
			fmt.Sprintf("pgrep -f '%s' | grep -v '^%d$' | xargs -r kill -9 2>/dev/null", img, selfPID))
		_ = kill.Run()
	}
}

func writePIDFile() {
	path := filepath.Join(dataDir(), "punmonitor.pid")
	_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0644)
}

func removePIDFile() {
	_ = os.Remove(filepath.Join(dataDir(), "punmonitor.pid"))
}

func isPortInUse(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return true
	}
	ln.Close()
	return false
}
