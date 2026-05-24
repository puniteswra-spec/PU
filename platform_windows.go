//go:build windows

package main

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func newHiddenCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func watchdogSingleton() bool {
	h, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(`Global\PunMonitorWatchdog`))
	if err != nil {
		return false
	}
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(h)
		return false
	}
	return h != 0
}

func singleton() bool {
	h, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(`Global\PunMonitorSingleton`))
	if err != nil {
		llog("error", "Failed to create mutex: %v", err)
		return false
	}
	if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(h)
		return false
	}
	return h != 0
}

func systemBootTimeMS() int64 {
	k32 := windows.NewLazyDLL("kernel32.dll")
	getTick := k32.NewProc("GetTickCount64")
	r, _, _ := getTick.Call()
	if r == 0 {
		return 0
	}
	return time.Now().Add(-time.Duration(r) * time.Millisecond).UnixMilli()
}

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	GetTickCount64       = kernel32.NewProc("GetTickCount64")
	GetLastInputInfo     = user32.NewProc("GetLastInputInfo")
	RegisterHotKey       = user32.NewProc("RegisterHotKey")
	UnregisterHotKey     = user32.NewProc("UnregisterHotKey")
	GetMessage           = user32.NewProc("GetMessageW")
	TranslateMessage     = user32.NewProc("TranslateMessage")
	DispatchMessage      = user32.NewProc("DispatchMessageW")
	PostQuitMessage      = user32.NewProc("PostQuitMessage")
	CreateMutexW         = kernel32.NewProc("CreateMutexW")
	ReleaseMutex         = kernel32.NewProc("ReleaseMutex")
	OpenMutexW           = kernel32.NewProc("OpenMutexW")
	CloseHandle          = kernel32.NewProc("CloseHandle")
	CreateProcessW       = kernel32.NewProc("CreateProcessW")
	WaitForSingleObject  = kernel32.NewProc("WaitForSingleObject")
	TerminateProcess     = kernel32.NewProc("TerminateProcess")
	GetModuleFileNameExW = windows.NewLazySystemDLL("psapi.dll").NewProc("GetModuleFileNameExW")
	EnumProcesses        = windows.NewLazySystemDLL("psapi.dll").NewProc("EnumProcesses")
	OpenProcess          = kernel32.NewProc("OpenProcess")
)

type lastInputInfo struct {
	cbSize uint32
	dwTime uint32
}

type IdleDetector struct {
	threshold time.Duration
	callback  func(idle bool)
	stopCh    chan struct{}
	wg        sync.WaitGroup
}

func NewIdleDetector(threshold time.Duration, callback func(idle bool)) *IdleDetector {
	return &IdleDetector{
		threshold: threshold,
		callback:  callback,
		stopCh:    make(chan struct{}),
	}
}

func (id *IdleDetector) Start(ctx context.Context) {
	id.wg.Add(1)
	go id.loop(ctx)
}

func (id *IdleDetector) Stop() {
	close(id.stopCh)
	id.wg.Wait()
}

func (id *IdleDetector) loop(ctx context.Context) {
	defer id.wg.Done()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	var wasIdle bool
	for {
		select {
		case <-ticker.C:
			var lii lastInputInfo
			lii.cbSize = uint32(unsafe.Sizeof(lii))
			ret, _, _ := GetLastInputInfo.Call(uintptr(unsafe.Pointer(&lii)))
			if ret == 0 {
				continue
			}
			tick, _, _ := GetTickCount64.Call()
			idleTime := time.Duration(uint64(tick) - uint64(lii.dwTime)) * time.Millisecond
			idle := idleTime > id.threshold
			if idle != wasIdle {
				id.callback(idle)
				wasIdle = idle
			}
		case <-ctx.Done():
			return
		}
	}
}

func winMouseMove(x, y int)  {}
func winMouseClick(x, y int, left bool) {}
func winKeyPress(vk uint16)  {}
func winTypeText(text string) {}

const autostartKeyName = "PunMonitor"

func setupAutostart() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	path := exe + ` --watchdog`
	k32 := windows.NewLazyDLL("advapi32.dll")
	regSetValue := k32.NewProc("RegSetKeyValueW")
	regSetValue.Call(
		uintptr(0x80000001), // HKEY_CURRENT_USER
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(`Software\Microsoft\Windows\CurrentVersion\Run`))),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("PunMonitor"))),
		uintptr(1), // REG_SZ
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(path))),
		uintptr(len(path)*2),
	)
	llog("info", "Autostart installed: %s", path)
}

func removeAutostart() {
	k32 := windows.NewLazyDLL("advapi32.dll")
	regDelete := k32.NewProc("RegDeleteKeyValueW")
	regDelete.Call(
		uintptr(0x80000001),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(`Software\Microsoft\Windows\CurrentVersion\Run`))),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("PunMonitor"))),
	)
	llog("info", "Autostart removed")
}

func cleanDuplicateAutostartEntries() {}

var (
	singletonMutexName   = `Global\PunMonitorSingleton`
	singletonMutex       windows.Handle
	punMonitorImageNames = []string{"PunMonitor.exe", "test_build.exe"}
)

func ensureSingleInstance(replaceExisting bool) bool { return true }
func killAllPunMonitorImages()                       {}
func tryAcquireSingletonMutex() bool                 { return true }
func releaseSingleton()                              {}
func killOtherPunMonitorProcesses(selfPID int)        {}
func writePIDFile()                                   {}
func removePIDFile()                                  {}
func isPortInUse(port int) bool                       { return false }
func updateSystemInfoFromActivity(info map[string]string) {}
