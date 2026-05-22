//go:build windows

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

const singletonMutexName = `Global\PunMonitorSingleton`

var singletonMutex windows.Handle

var punMonitorImageNames = []string{"PunMonitor.exe", "test_build.exe"}

// ensureSingleInstance — only one PunMonitor.exe on this machine.
// Always stops other copies first; --force waits longer after kill.
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
	// Stale mutex holder may have crashed — kill other PIDs and retry once
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
		cmd := exec.Command("taskkill", "/F", "/IM", img)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		_ = cmd.Run()
	}
}

func tryAcquireSingletonMutex() bool {
	name, err := windows.UTF16PtrFromString(singletonMutexName)
	if err != nil {
		return false
	}
	h, err := windows.CreateMutex(nil, true, name)
	if err != nil {
		return false
	}
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(h)
		return false
	}
	if singletonMutex != 0 {
		windows.CloseHandle(singletonMutex)
	}
	singletonMutex = h
	return true
}

func releaseSingleton() {
	if singletonMutex != 0 {
		windows.CloseHandle(singletonMutex)
		singletonMutex = 0
	}
	removePIDFile()
}

func killOtherPunMonitorProcesses(selfPID int) {
	if selfPID == 0 {
		selfPID = os.Getpid()
	}
	filter := fmt.Sprintf("PID ne %d", selfPID)
	for _, img := range punMonitorImageNames {
		cmd := exec.Command("taskkill", "/F", "/FI", filter, "/IM", img)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		_ = cmd.Run()
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
