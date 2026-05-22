//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const autostartKeyName = "PunMonitor"

func setupAutostart() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	exe, _ = filepath.Abs(exe)
	// Single instance: only launch if not already running (mutex check in app)
	cmd := `"` + exe + `"`
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`,
		registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.SetStringValue(autostartKeyName, cmd)
	llog("info", "autostart enabled: %s", exe)
}

func removeAutostart() {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`,
		registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.DeleteValue(autostartKeyName)
}

// cleanDuplicateAutostartEntries removes legacy Run key names
func cleanDuplicateAutostartEntries() {
	names := []string{"RemoteMonitor", "SystemHelper", "PunMonitorHelper"}
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`,
		registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	for _, n := range names {
		val, _, err := k.GetStringValue(n)
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(val), "punmonitor") {
			_ = k.DeleteValue(n)
		}
	}
}
