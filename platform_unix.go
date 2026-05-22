//go:build !windows

package main

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

func getOSVersion() string {
	if runtime.GOOS == "darwin" {
		data, err := os.ReadFile("/System/Library/CoreServices/SystemVersion.plist")
		if err == nil {
			content := string(data)
			if i := strings.Index(content, "<key>ProductVersion</key>"); i >= 0 {
				rest := content[i:]
				if j := strings.Index(rest, "<string>"); j >= 0 {
					rest = rest[j+8:]
					if k := strings.Index(rest, "</string>"); k >= 0 {
						return "macOS " + rest[:k]
					}
				}
			}
		}
		// Fallback to sw_vers
		data, err = exec.Command("sw_vers", "-productVersion").Output()
		if err == nil {
			return "macOS " + strings.TrimSpace(string(data))
		}
		return "macOS"
	}

	// Linux
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	var name, version string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			v := strings.Trim(line[len("PRETTY_NAME="):], `"`)
			return "Linux " + v
		}
		if strings.HasPrefix(line, "NAME=") {
			name = strings.Trim(line[len("NAME="):], `"`)
		}
		if strings.HasPrefix(line, "VERSION_ID=") {
			version = strings.Trim(line[len("VERSION_ID="):], `"`)
		}
	}
	if name != "" && version != "" {
		return "Linux " + name + " " + version
	}
	if name != "" {
		return "Linux " + name
	}
	return "Linux"
}

func hideCmdWindow(cmd *exec.Cmd) {}
func hideCmdWindowWithFlags(cmd *exec.Cmd) {}
