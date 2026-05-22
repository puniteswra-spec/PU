//go:build linux

package main

import (
	"os"
	"os/exec"
	"strings"
)

func getOSVersion() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux (unknown)"
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
