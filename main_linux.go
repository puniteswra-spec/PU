//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func startPopupKiller(ctx context.Context) {}

func copyToSystemLocation() (string, error) {
	src, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("get executable: %w", err)
	}

	destDir := filepath.Join(os.Getenv("HOME"), ".local", "bin")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", fmt.Errorf("create dest dir: %w", err)
	}

	base := filepath.Base(src)
	dest := filepath.Join(destDir, base)

	srcData, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("read source: %w", err)
	}
	if err := os.WriteFile(dest, srcData, 0755); err != nil {
		return "", fmt.Errorf("write dest: %w", err)
	}

	llog("info", "copied to %s", dest)
	return dest, nil
}

func applyUpdate(downloadURL string) {
	llog("info", "update: downloading %s", downloadURL)
	tmpPath := filepath.Join(os.TempDir(), "rmon_update")
	if err := downloadFile(downloadURL, tmpPath); err != nil {
		llog("error", "update download: %v", err)
		return
	}
	if err := os.Chmod(tmpPath, 0755); err != nil {
		llog("error", "update chmod: %v", err)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		llog("error", "update: %v", err)
		return
	}
	// Write a shell script that waits, copies, and restarts
	script := fmt.Sprintf(`#!/bin/sh
sleep 2
kill -9 %d 2>/dev/null
sleep 1
cp "%s" "%s"
chmod +x "%s"
exec "%s"
`, os.Getpid(), tmpPath, exe, exe, exe)
	scriptPath := filepath.Join(os.TempDir(), "rmon_update.sh")
	os.WriteFile(scriptPath, []byte(script), 0755)
	llog("info", "update: applying, restarting...")
	exec.Command("/bin/sh", "-c", scriptPath).Start()
	os.Exit(0)
}
