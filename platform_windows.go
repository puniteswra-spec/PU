//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func getOSVersion() string {
	ver := windows.RtlGetVersion()
	if ver != nil {
		return fmt.Sprintf("Windows %d.%d build %d", ver.MajorVersion, ver.MinorVersion, ver.BuildNumber)
	}
	return "Windows (unknown version)"
}

func hideCmdWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func hideCmdWindowWithFlags(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
