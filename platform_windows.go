//go:build windows

package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
		CreationFlags: 0x08000000 | 0x00000001, // CREATE_NO_WINDOW | DETACHED_PROCESS
	}
}

// WindowsVersionInfo is the result of probing the OS for version + build.
// We use RtlGetVersion (ntdll) instead of the deprecated GetVersionEx
// because the latter lies about version numbers when the binary lacks a
// proper manifest. RtlGetVersion is the same API that `ver` and the
// Windows kernel itself use to report the real build.
type WindowsVersionInfo struct {
	Major       uint32
	Minor       uint32
	Build       uint32
	ProductName string
	IsWow64     bool
}

// String returns a human-friendly "Windows 10 22H2 (build 19045)" style label.
func (v WindowsVersionInfo) String() string {
	name := v.ProductName
	if name == "" {
		name = "Windows"
	}
	return fmt.Sprintf("%s %d.%d (build %d)", name, v.Major, v.Minor, v.Build)
}

// IsWindows10OrLater returns true if the running OS is Windows 10 or
// newer (build >= 10240, or Windows 11 which is build >= 22000).
// Returns false on Windows 7/8/8.1/Server 2008/2012.
func (v WindowsVersionInfo) IsWindows10OrLater() bool {
	if v.Major > 10 {
		return true
	}
	if v.Major == 10 && v.Build >= 10240 {
		return true
	}
	return false
}

// windowsVersion probes the OS version. It does NOT exit or panic —
// callers decide what to do (e.g., the main goroutine shows a clear
// error dialog and refuses to start on Windows 7/8).
func windowsVersion() WindowsVersionInfo {
	var info WindowsVersionInfo
	ntdll := windows.NewLazySystemDLL("ntdll.dll")
	proc := ntdll.NewProc("RtlGetVersion")
	type osversioninfoex struct {
		dwOSVersionInfoSize uint32
		dwMajorVersion      uint32
		dwMinorVersion      uint32
		dwBuildNumber       uint32
		dwPlatformId        uint32
		szCSDVersion        [128]uint16
		wServicePackMajor   uint16
		wServicePackMinor   uint16
		wSuiteMask          uint16
		wProductType        uint8
		wReserved           uint8
	}
	var v osversioninfoex
	v.dwOSVersionInfoSize = uint32(unsafe.Sizeof(v))
	r, _, _ := proc.Call(uintptr(unsafe.Pointer(&v)))
	if r == 0 {
		info.Major = v.dwMajorVersion
		info.Minor = v.dwMinorVersion
		info.Build = v.dwBuildNumber
	}
	// Read ProductName from registry (more reliable than version numbers
	// for distinguishing "Windows 10" vs "Windows 11" — same kernel
	// major, different product name). Best-effort: empty if unreadable.
	k32 := windows.NewLazySystemDLL("kernel32.dll")
	regOpen := k32.NewProc("RegOpenKeyExW")
	regClose := k32.NewProc("RegCloseKey")
	regQuery := k32.NewProc("RegQueryValueExW")
	const HKEY_LOCAL_MACHINE = 0x80000002
	subKey, _ := windows.UTF16PtrFromString(`SOFTWARE\Microsoft\Windows NT\CurrentVersion`)
	var hKey uintptr
	if ret, _, _ := regOpen.Call(HKEY_LOCAL_MACHINE, uintptr(unsafe.Pointer(subKey)), 0, 0x20019, uintptr(unsafe.Pointer(&hKey))); ret == 0 {
		defer regClose.Call(hKey)
		valName, _ := windows.UTF16PtrFromString("ProductName")
		var buf [128]uint16
		var bufSize uint32 = 256
		var regType uint32
		if ret, _, _ := regQuery.Call(hKey, uintptr(unsafe.Pointer(valName)), 0, uintptr(unsafe.Pointer(&regType)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bufSize))); ret == 0 {
			info.ProductName = windows.UTF16ToString(buf[:])
		}
	}
	// IsWow64: detect if 32-bit process on 64-bit OS
	var isWow64 bool
	procIsWow := k32.NewProc("IsWow64Process")
	var wow bool
	if ret, _, _ := procIsWow.Call(uintptr(0xffffffff), uintptr(unsafe.Pointer(&wow))); ret != 0 {
		isWow64 = wow
	}
	info.IsWow64 = isWow64
	return info
}

// enforceWindowsMinimumVersion checks that the OS is at least Windows 10.
// On older Windows (7/8/8.1), it shows a one-time error dialog (Windows)
// or prints to stderr (other OSes — the build tag already filters this
// function out for non-Windows) and refuses to continue, because the
// Go 1.25 runtime itself requires Windows 10 Anniversary Update or
// later. Returning here means the watchdog or shell will just relaunch
// us forever, so we BLOCK by entering an infinite sleep — the user
// sees the message box and can take action.
//
// Callers should invoke this in main() before doing anything else.
func enforceWindowsMinimumVersion() {
	v := windowsVersion()
	if v.IsWindows10OrLater() {
		llog("info", "OS: %s", v.String())
		return
	}
	msg := fmt.Sprintf(
		"PunMonitor requires Windows 10 or later.\n\n"+
			"Your OS: %s\n"+
			"(major=%d, build=%d)\n\n"+
			"The Go runtime (1.25) does not support older Windows.\n"+
			"Please upgrade to Windows 10, Windows 11, or Windows Server 2016+.\n\n"+
			"PunMonitor will now exit. (This message was shown because it is the only safe way to alert you — the program runs hidden.)",
		v.String(), v.Major, v.Build)
	llog("error", "OS TOO OLD: %s — refusing to start", v.String())
	// Use MessageBoxW so the user actually sees the message (we're a
	// hidden GUI process — they wouldn't otherwise know we exited).
	user32 := windows.NewLazySystemDLL("user32.dll")
	procMessageBox := user32.NewProc("MessageBoxW")
	titlePtr, _ := windows.UTF16PtrFromString("PunMonitor — Unsupported Windows Version")
	msgPtr, _ := windows.UTF16PtrFromString(msg)
	const MB_OK = 0x00000000
	const MB_ICONERROR = 0x00000010
	const MB_TOPMOST = 0x00040000
	procMessageBox.Call(0, uintptr(unsafe.Pointer(msgPtr)), uintptr(unsafe.Pointer(titlePtr)), MB_OK|MB_ICONERROR|MB_TOPMOST)
	// Sleep forever so the watchdog doesn't relaunch us — user must
	// fix the OS or the install will loop forever. This is the
	// "hassle-free" behavior: clear error, then stop, instead of
	// silently failing in a loop.
	for {
		time.Sleep(time.Hour)
	}
}

// platformStableMachineID returns the Windows MachineGuid, a unique
// per-install identifier that persists across reboots and survives clearing
// the PunMonitor settings file. Implemented via direct registry read so
// there's no external process spawn.
func platformStableMachineID() string {
	k32 := windows.NewLazyDLL("advapi32.dll")
	advOpen := k32.NewProc("RegOpenKeyExW")
	advQuery := k32.NewProc("RegQueryValueExW")
	advClose := k32.NewProc("RegCloseKey")

	subKey := `SOFTWARE\Microsoft\Cryptography`
	var hKey uintptr
	ret, _, _ := advOpen.Call(
		uintptr(0x80000002), // HKEY_LOCAL_MACHINE
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(subKey))),
		0, 0,
		uintptr(unsafe.Pointer(&hKey)),
	)
	if ret != 0 {
		return ""
	}
	defer advClose.Call(hKey)

	name := "MachineGuid"
	var dataBuf [256]uint16
	dataLen := uint32(len(dataBuf)) * 2
	var dtype uint32
	ret, _, _ = advQuery.Call(
		hKey,
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(name))),
		uintptr(0),
		uintptr(unsafe.Pointer(&dtype)),
		uintptr(unsafe.Pointer(&dataBuf[0])),
		uintptr(unsafe.Pointer(&dataLen)),
	)
	if ret != 0 || (dtype != 1 /*REG_SZ*/ && dtype != 2 /*REG_EXPAND_SZ*/) {
		return ""
	}
	// Trim trailing null + any padding
	guid := windows.UTF16ToString(dataBuf[:dataLen/2])
	guid = strings.TrimSpace(guid)
	if guid == "" {
		return ""
	}
	// 8-char prefix of SHA-1 keeps the ID short and human-readable
	sum := sha1.Sum([]byte(guid))
	return hex.EncodeToString(sum[:])[:8]
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

// monitorAlreadyRunning checks if a main PunMonitor process is already running by
// attempting to grab the singleton mutex. If the mutex already exists (whether
// via GetLastError or via the returned error), another instance is running.
// Used by the watchdog to avoid restart loops.
func monitorAlreadyRunning() bool {
	h, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(`Global\PunMonitorSingleton`))
	if h != 0 {
		defer windows.CloseHandle(h)
	}
	// CreateMutex returns ERROR_ALREADY_EXISTS as an error when the mutex exists.
	if err == windows.ERROR_ALREADY_EXISTS || err == syscall.Errno(0xB7) {
		return true
	}
	return false
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
	// Fallback: native calculation in metrics_windows.go
	if t := nativeBootTimeMS(); t > 0 {
		return t
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
			idleTime := time.Duration(uint64(tick)-uint64(lii.dwTime)) * time.Millisecond
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

// isWindowsAdmin returns true if the current process is elevated (admin).
// schtasks /RL HIGHEST requires admin; calling it without admin flashes a
// console window AND fails with "Access is denied", so we skip it entirely
// for non-admin users. The HKCU Run registry entry works without admin.
func isWindowsAdmin() bool {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return false
	}
	defer token.Close()
	var elevationType uint32
	var outLen uint32
	err = windows.GetTokenInformation(token, windows.TokenElevationType, (*byte)(unsafe.Pointer(&elevationType)), uint32(unsafe.Sizeof(elevationType)), &outLen)
	if err != nil {
		return false
	}
	return elevationType == 2 // TokenElevationTypeFull
}

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
	llog("info", "Autostart installed: %s (HKCU Run only — no schtasks to avoid CMD popup)", path)
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
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", psCmd)
	newHiddenCmd(cmd)
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

// cleanDuplicateAutostartEntries removes legacy autostart entries from prior
// project iterations that point to .bat / .vbs / old binary locations and
// cause periodic cmd/powershell/wscript popups. Runs once at startup; safe to
// re-run.
func cleanDuplicateAutostartEntries() {
	k32 := windows.NewLazyDLL("advapi32.dll")
	advOpen := k32.NewProc("RegOpenKeyExW")
	advEnum := k32.NewProc("RegEnumValueW")
	advQuery := k32.NewProc("RegQueryValueExW")
	advDelete := k32.NewProc("RegDeleteKeyValueW")
	advClose := k32.NewProc("RegCloseKey")

	// Legacy entry names that other iterations of this project (or malware
	// imitating it) used to install themselves. We own "PunMonitor" and
	// "PunMonitorWatchdog" only; remove anything else.
	legacyNames := map[string]bool{
		"SystemMonitor":       true,
		"SystemHelper":        true,
		"WindowsUpdateHelper": true,
		"PunMonitorServer":    true,
		"PunMonitorAgent":     true,
		"RemoteMonitor":       true,
		"RemoteMonitorAgent":  true,
		"SystemWatchdog":      true,
	}

	// Paths / fragments that, if present in a Run value, mean it's NOT our
	// current PunMonitor and should be removed.
	legacyPathFragments := []string{
		`P:\Opencode\RemoteMonitor-Merged`,
		`RemoteMonitor-Merged_webRTC`,
		`\AppData\Roaming\SystemHelper\`,
		`\AppData\Roaming\Microsoft\SystemHelper\`,
		`\AppData\Roaming\RemoteMonitor\`,
		`\AppData\Roaming\WindowsUpdate\`,
		`\AppData\Local\RemoteMonitor\`,
		`watchdog.bat`,
		`watchdog.vbs`,
	}

	subKey := `Software\Microsoft\Windows\CurrentVersion\Run`
	var hKey uintptr
	ret, _, _ := advOpen.Call(
		uintptr(0x80000001), // HKEY_CURRENT_USER
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(subKey))),
		0, 0, // reserved, samDesired
		uintptr(unsafe.Pointer(&hKey)),
	)
	if ret != 0 {
		// Try HKLM as well
		ret, _, _ = advOpen.Call(
			uintptr(0x80000002), // HKEY_LOCAL_MACHINE
			uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(subKey))),
			0, 0,
			uintptr(unsafe.Pointer(&hKey)),
		)
		if ret != 0 {
			return
		}
	}
	defer advClose.Call(hKey)

	// Enumerate values
	var nameBuf [256]uint16
	var dataBuf [2048]uint16
	removed := []string{}
	for i := uint32(0); ; i++ {
		nameLen := uint32(len(nameBuf))
		dataLen := uint32(len(dataBuf)) * 2
		var dtype uint32
		ret, _, _ := advEnum.Call(
			hKey,
			uintptr(i),
			0, // lpValueName
			uintptr(unsafe.Pointer(&nameBuf[0])),
			uintptr(unsafe.Pointer(&nameLen)),
			0, // lpReserved
			uintptr(unsafe.Pointer(&dtype)),
			uintptr(unsafe.Pointer(&dataBuf[0])),
			uintptr(unsafe.Pointer(&dataLen)),
		)
		if ret != 0 {
			break // ERROR_NO_MORE_ITEMS or similar
		}
		name := windows.UTF16ToString(nameBuf[:nameLen])
		// Skip our own entries
		if name == "PunMonitor" || name == "PunMonitorWatchdog" {
			continue
		}
		// Skip if we can't read the data
		if ret, _, _ := advQuery.Call(
			hKey,
			uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(name))),
			uintptr(0), // lpReserved must be NULL
			uintptr(unsafe.Pointer(&dataBuf[0])),
			uintptr(unsafe.Pointer(&dataLen)),
		); ret == 0 {
			value := windows.UTF16ToString(dataBuf[:dataLen/2])
			shouldRemove := false
			if legacyNames[name] {
				shouldRemove = true
			}
			if !shouldRemove {
				for _, frag := range legacyPathFragments {
					if containsFold(value, frag) {
						shouldRemove = true
						break
					}
				}
			}
			if shouldRemove {
				advDelete.Call(
					uintptr(0x80000001),
					uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(subKey))),
					uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(name))),
				)
				removed = append(removed, fmt.Sprintf("%s = %s", name, value))
			}
		}
	}
	if len(removed) > 0 {
		llog("info", "cleanDuplicateAutostartEntries: removed %d legacy entries: %v", len(removed), removed)
		// Best-effort: also kill any processes those entries spawned.
		// Match by path fragments, not by name (some may be legitimate).
		killLegacyProcesses()
	}
}

// containsFold is a case-insensitive substring check (we can't use strings
// package in platform-specific files without pulling it in; but Go's strings
// IS available — use it for clarity).
func containsFold(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(s) < len(substr) {
		return false
	}
	// Quick lowercase comparison
	a := []rune(s)
	b := []rune(substr)
	for i := 0; i+len(b) <= len(a); i++ {
		match := true
		for j := 0; j < len(b); j++ {
			ca, cb := a[i+j], b[j]
			if ca >= 'A' && ca <= 'Z' {
				ca += 32
			}
			if cb >= 'A' && cb <= 'Z' {
				cb += 32
			}
			if ca != cb {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// killLegacyProcesses terminates processes whose executable path matches the
// legacy fragments — the SystemHelper.exe / watchdog.vbs chain that the
// removed autostart entries used to spawn. Runs best-effort, no error
// reporting.
func killLegacyProcesses() {
	// Use tasklist to find processes whose image path contains legacy
	// fragments. We can't read /proc/<pid>/exe on Windows, so we rely on
	// the running process list and stop processes by PID.
	//
	// Simpler approach: kill well-known legacy process names if they exist.
	legacyNames := []string{"SystemHelper", "RemoteMonitorAgent", "RemoteMonitorServer"}
	for _, name := range legacyNames {
		// taskkill /F /IM <name>
		exe, _ := exec.LookPath("taskkill")
		if exe == "" {
			continue
		}
		cmd := exec.Command(exe, "/F", "/IM", name+".exe")
		newHiddenCmd(cmd)
		_ = cmd.Run()
	}

	// Also clean the Windows Startup folder (legacy .vbs / .lnk / .bat
	// shortcuts that trigger WSH popups on every login).
	cleanLegacyStartupFolder()
}

// cleanLegacyStartupFolder removes .vbs / .lnk / .bat / .cmd / .ps1 / .exe
// files from both the user and all-users Startup folders whose names match
// legacy patterns (SystemHelper, RemoteMonitor, etc.). Runs best-effort.
func cleanLegacyStartupFolder() {
	startupDirs := []string{
		filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs\Startup`),
		`C:\ProgramData\Microsoft\Windows\Start Menu\Programs\Startup`,
	}
	legacyPattern := regexp.MustCompile(`(?i)^(SystemHelper|RemoteMonitor|SystemMonitor|RemoteHelper|WindowsUpdate|PunMonitorServer|PunMonitorAgent|SystemWatchdog)`)

	for _, dir := range startupDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			ext := strings.ToLower(filepath.Ext(name))
			if ext != ".vbs" && ext != ".lnk" && ext != ".bat" && ext != ".cmd" && ext != ".ps1" && ext != ".exe" {
				continue
			}
			if legacyPattern.MatchString(name) {
				full := filepath.Join(dir, name)
				if err := os.Remove(full); err == nil {
					llog("info", "Removed legacy Startup item: %s", full)
				}
			}
		}
	}
}

var (
	singletonMutexName   = `Global\PunMonitorSingleton`
	singletonMutex       windows.Handle
	punMonitorImageNames = []string{"PunMonitor.exe", "test_build.exe"}
)

func ensureSingleInstance(replaceExisting bool) bool      { return true }
func killAllPunMonitorImages()                            {}
func tryAcquireSingletonMutex() bool                      { return true }
func releaseSingleton()                                   {}
func killOtherPunMonitorProcesses(selfPID int)            {}
func writePIDFile()                                       {}
func removePIDFile()                                      {}
func isPortInUse(port int) bool                           { return false }
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
