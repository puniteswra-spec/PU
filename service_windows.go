//go:build windows
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// installService installs the current executable as a Windows Service named PunMonitor.
// The service is configured to start automatically and restart on failure.
func installService() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("executable path: %w", err)
	}
	serviceName := "PunMonitor"

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to SCM: %w", err)
	}
	defer m.Disconnect()

	// If service already exists, remove it first to update config
	s, err := m.OpenService(serviceName)
	if err == nil {
		s.Close()
		_ = s.Delete()
		time.Sleep(2 * time.Second)
	}

	s, err = m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName:  "PunMonitor Service",
		Description:  "PunMonitor remote monitoring agent/server with auto-restart",
		StartType:    mgr.StartAutomatic,
		ErrorControl: mgr.ErrorNormal,
	})
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer s.Close()

	// Configure recovery: restart on first and second failure, reboot on third
	err = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ComputerReboot, Delay: 0},
	}, 60)
	if err != nil {
		return fmt.Errorf("set recovery actions: %w", err)
	}

	llog("info", "Service '%s' installed successfully.", serviceName)
	return nil
}

// removeService removes the Windows Service named PunMonitor.
func removeService() error {
	serviceName := "PunMonitor"
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		if windowsErr, ok := err.(syscall.Errno); ok && windowsErr == windows.ERROR_SERVICE_DOES_NOT_EXIST {
			llog("info", "Service '%s' not found.", serviceName)
			return nil
		}
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()

	err = s.Delete()
	if err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	llog("info", "Service '%s' removed successfully.", serviceName)
	return nil
}

// runService is called when the executable is launched as a Windows Service.
func runService() {
	err := svc.Run("PunMonitor", &punmonitorService{})
	if err != nil {
		llog("error", "service failed: %v", err)
	}
}

// punmonitorService implements the svc.Handler interface for the Windows Service.
type punmonitorService struct{}

// Execute is called by the Service Control Manager to start, stop, etc.
func (m *punmonitorService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (svcSpecificEC bool, exitCode uint32) {
	changes <- svc.Status{State: svc.StartPending}

	// Run supervision loop in background
	go m.runSupervisionLoop()

	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	llog("info", "Service started.")

	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			llog("info", "Service stopping.")
			changes <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
	return false, 0
}

// runSupervisionLoop starts and monitors the main PunMonitor process.
func (m *punmonitorService) runSupervisionLoop() {
	ensureBinaryRelocated()

	watchdogExe, err := os.Executable()
	if err != nil {
		wlog("Failed to get executable path: %v", err)
		return
	}
	permPath := filepath.Join(binDir(), filepath.Base(watchdogExe))
	if _, err := os.Stat(permPath); err == nil {
		watchdogExe = permPath
	}
	mainExe := watchdogExe

	go func() {
		for {
			time.Sleep(10 * time.Second)
			writeWatchdogHeartbeat()
		}
	}()

	var consecutiveRestarts int
	var monitorStartTime time.Time

	for {
		if monitorAlreadyRunning() {
			wlog("Monitor already running — watching for exit every 5s")
			consecutiveRestarts++
			for monitorAlreadyRunning() {
				time.Sleep(5 * time.Second)
			}
			wlog("Monitor exited — respawning")
			consecutiveRestarts = 0
			continue
		}

		wlog("Starting monitor from %s", mainExe)
		consecutiveRestarts++
		if consecutiveRestarts > 3 {
			backoff := time.Duration(consecutiveRestarts) * 10 * time.Second
			if backoff > 2*time.Minute {
				backoff = 2 * time.Minute
			}
			wlog("Watchdog: %d consecutive restarts, backing off %s", consecutiveRestarts, backoff)
			time.Sleep(backoff)
		}

		cmd := exec.Command(mainExe)
		newHiddenCmd(cmd)

		if err := cmd.Start(); err != nil {
			wlog("Failed to start monitor: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		wlog("Monitor PID: %d", cmd.Process.Pid)
		consecutiveRestarts = 0
		monitorStartTime = time.Now()

		if err := cmd.Wait(); err != nil {
			wlog("Monitor exited with error: %v", err)
		} else {
			wlog("Monitor exited cleanly")
		}
		if time.Since(monitorStartTime) < 5*time.Second {
			consecutiveRestarts++
			wlog("Watchdog: monitor died quickly — sleeping 30s")
			time.Sleep(30 * time.Second)
		} else {
			time.Sleep(3 * time.Second)
		}
	}
}

// isServiceMode is set to true when the process is launched by the Windows Service
// Control Manager. Used by crypto_windows.go to switch to machine-level DPAPI.
var isServiceMode bool

// detectAndRunService checks if the process was launched by the Windows Service
// Control Manager. If so, it starts the service and returns true.
func detectAndRunService() bool {
	isService, _ := svc.IsWindowsService()
	if !isService {
		return false
	}
	isServiceMode = true
	runService()
	return true
}
