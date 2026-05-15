//go:build !windows && !darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func hideCmd(cmd *exec.Cmd) {}

func hideConsole() {}

func screenSize() (int, int) { return 1920, 1080 }

func moveMouse(x, y int) {}

func clickMouse(x, y int, right bool) {}

func pressKey(key string) {}

func bootTime() time.Time { return time.Now() }

func getIdleSeconds() int { return 0 }

func osUptime() int { return 0 }

func preventDuplicate() {
	lockFile := filepath.Join(dataDir(), "agent.lock")
	os.WriteFile(lockFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644)
}

func setupAutostart() {}

func receivedDir() string {
	return filepath.Join(dataDir(), "received")
}

func startActivityLogger() {}
