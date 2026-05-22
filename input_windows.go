//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

var (
	modUser32            = windows.NewLazyDLL("user32.dll")
	modKernel32          = windows.NewLazyDLL("kernel32.dll")
	procSetCursorPos     = modUser32.NewProc("SetCursorPos")
	procMouseEvent       = modUser32.NewProc("mouse_event")
	procKeybdEvent       = modUser32.NewProc("keybd_event")
	procGetConsoleWindow = modKernel32.NewProc("GetConsoleWindow")
)

const (
	mouseeventfLeftdown   = 0x0002
	mouseeventfLeftup     = 0x0004
	mouseeventfRightdown  = 0x0008
	mouseeventfRightup    = 0x0010
	mouseeventfMiddledown = 0x0020
	mouseeventfMiddleup   = 0x0040
	keyeventfKeyup        = 0x0002
)

func setupInputEvents() *InputEvent {
	return &InputEvent{
		MouseMove:  winMouseMove,
		MouseClick: winMouseClick,
		KeyPress:   winKeyPress,
		TypeText:   winTypeText,
	}
}

func winMouseMove(x, y int) {
	procSetCursorPos.Call(uintptr(x), uintptr(y))
}

func winMouseClick(x, y int, left bool) {
	if x > 0 || y > 0 {
		winMouseMove(x, y)
	}
	if left {
		procMouseEvent.Call(mouseeventfLeftdown, 0, 0, 0, 0)
		procMouseEvent.Call(mouseeventfLeftup, 0, 0, 0, 0)
	} else {
		procMouseEvent.Call(mouseeventfRightdown, 0, 0, 0, 0)
		procMouseEvent.Call(mouseeventfRightup, 0, 0, 0, 0)
	}
}

func winKeyPress(vk uint16) {
	procKeybdEvent.Call(uintptr(vk), 0, 0, 0)
	procKeybdEvent.Call(uintptr(vk), 0, keyeventfKeyup, 0)
}

func winTypeText(text string) {
	for _, r := range text {
		winKeyPress(uint16(r))
	}
}

func hideConsole() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd != 0 {
		showWindow := modUser32.NewProc("ShowWindow")
		showWindow.Call(hwnd, 0)
	}
}

func spawnSelfWatchdog() {
	// Optional watchdog hook; Phase 1 keeps a no-op stub.
	_, _ = os.Executable()
}
