//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

const autostartKeyName = "PunMonitor"

func autostartDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "autostart")
}

func setupAutostart() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	exe, _ = filepath.Abs(exe)
	dir := autostartDir()
	if dir == "" {
		return
	}
	os.MkdirAll(dir, 0755)

	desktop := `[Desktop Entry]
Type=Application
Name=PunMonitor
Exec=` + exe + `
Terminal=false
X-GNOME-Autostart-enabled=true
`
	path := filepath.Join(dir, "punmonitor.desktop")
	if err := os.WriteFile(path, []byte(desktop), 0755); err != nil {
		llog("error", "autostart: failed to write desktop file: %v", err)
		return
	}
	llog("info", "autostart enabled: %s", path)
}

func removeAutostart() {
	dir := autostartDir()
	if dir == "" {
		return
	}
	_ = os.Remove(filepath.Join(dir, "punmonitor.desktop"))
}

func cleanDuplicateAutostartEntries() {
	names := []string{"RemoteMonitor", "SystemHelper", "PunMonitorHelper"}
	dir := autostartDir()
	if dir == "" {
		return
	}
	for _, n := range names {
		path := filepath.Join(dir, n+".desktop")
		if _, err := os.Stat(path); err == nil {
			os.Remove(path)
		}
	}
}
