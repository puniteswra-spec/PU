//go:build linux

package main

func setupInputEvents() *InputEvent {
	return &InputEvent{
		MouseMove:        func(x, y int) {},
		MouseClick:       func(x, y int, left bool) {},
		MouseMiddleClick: func(x, y int) {},
		KeyPress:         func(vk uint16) {},
		TypeText:         func(text string) {},
	}
}

func hideConsole() {}

func spawnSelfWatchdog() {}
