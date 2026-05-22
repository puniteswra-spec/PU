//go:build windows

package main

import (
	"context"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func systemBootTimeMS() int64 {
	k32 := windows.NewLazyDLL("kernel32.dll")
	getTick := k32.NewProc("GetTickCount64")
	r, _, _ := getTick.Call()
	if r == 0 {
		return 0
	}
	return time.Now().Add(-time.Duration(r) * time.Millisecond).UnixMilli()
}

type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

type IdleDetector struct {
	threshold time.Duration
	callback  func(bool)
	cancel    context.CancelFunc
	wasIdle   bool
}

func NewIdleDetector(threshold time.Duration, callback func(idle bool)) *IdleDetector {
	return &IdleDetector{threshold: threshold, callback: callback}
}

func (id *IdleDetector) Start(ctx context.Context) {
	ctx, id.cancel = context.WithCancel(ctx)
	go id.loop(ctx)
}

func (id *IdleDetector) Stop() {
	if id.cancel != nil {
		id.cancel()
	}
}

func (id *IdleDetector) loop(ctx context.Context) {
	user32 := windows.NewLazyDLL("user32.dll")
	getLastInput := user32.NewProc("GetLastInputInfo")
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			var lii lastInputInfo
			lii.cbSize = uint32(unsafe.Sizeof(lii))
			r, _, _ := getLastInput.Call(uintptr(unsafe.Pointer(&lii)))
			if r == 0 {
				continue
			}
			k32 := windows.NewLazyDLL("kernel32.dll")
			tick64, _, _ := k32.NewProc("GetTickCount64").Call()
			idleMs := uint64(tick64) - uint64(lii.dwTime)
			idle := time.Duration(idleMs) * time.Millisecond
			isIdle := idle >= id.threshold
			if isIdle != id.wasIdle {
				id.wasIdle = isIdle
				if id.callback != nil {
					id.callback(isIdle)
				}
			}
		}
	}
}
