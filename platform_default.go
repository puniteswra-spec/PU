//go:build !windows && !darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func newHiddenCmd(cmd *exec.Cmd) {}
func hideConsole()                  {}

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

func winMouseMove(x, y int)          {}
func winMouseClick(x, y int, left bool) {}
func winKeyPress(vk uint16)          {}
func winTypeText(text string)        {}

func setupAutostart() {}
func removeAutostart() {}

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
