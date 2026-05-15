//go:build windows

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	INPUT_MOUSE           = 0
	INPUT_KEYBOARD        = 1
	MOUSEEVENTF_MOVE      = 0x0001
	MOUSEEVENTF_LEFTDOWN  = 0x0002
	MOUSEEVENTF_LEFTUP    = 0x0004
	MOUSEEVENTF_RIGHTDOWN = 0x0008
	MOUSEEVENTF_RIGHTUP   = 0x0010
	MOUSEEVENTF_ABSOLUTE  = 0x8000
	KEYEVENTF_KEYUP       = 0x0002
)

var (
	user32            = windows.NewLazySystemDLL("user32.dll")
	procSendInput     = user32.NewProc("SendInput")
	procGetDC         = user32.NewProc("GetDC")
	procReleaseDC     = user32.NewProc("ReleaseDC")
	procGetLastInputInfo = user32.NewProc("GetLastInputInfo")
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procGetTickCount  = kernel32.NewProc("GetTickCount")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	gdi32             = windows.NewLazySystemDLL("gdi32.dll")
	procGetDeviceCaps = gdi32.NewProc("GetDeviceCaps")
)

func hideCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func hideConsole() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd != 0 {
		user32.NewProc("ShowWindow").Call(hwnd, 0)
	}
}

func screenSize() (int, int) {
	dc, _, _ := procGetDC.Call(0)
	if dc == 0 { return 1920, 1080 }
	w, _, _ := procGetDeviceCaps.Call(dc, 8)
	h, _, _ := procGetDeviceCaps.Call(dc, 10)
	procReleaseDC.Call(0, dc)
	sw, sh := int(int32(w)), int(int32(h))
	if sw <= 0 || sh <= 0 { return 1920, 1080 }
	return sw, sh
}

func makeMouseInput(absX, absY, flags uint32) []byte {
	b := make([]byte, 40)
	binary.LittleEndian.PutUint32(b[0:4], INPUT_MOUSE)
	binary.LittleEndian.PutUint32(b[8:12], absX)
	binary.LittleEndian.PutUint32(b[12:16], absY)
	binary.LittleEndian.PutUint32(b[20:24], flags)
	return b
}

func makeKeyboardInput(vk uint16, flags uint32) []byte {
	b := make([]byte, 40)
	binary.LittleEndian.PutUint32(b[0:4], INPUT_KEYBOARD)
	binary.LittleEndian.PutUint16(b[8:10], vk)
	binary.LittleEndian.PutUint32(b[12:16], flags)
	return b
}

func moveMouse(x, y int) {
	sw, sh := screenSize()
	b := makeMouseInput(uint32(x*65535/sw), uint32(y*65535/sh), MOUSEEVENTF_MOVE|MOUSEEVENTF_ABSOLUTE)
	procSendInput.Call(1, uintptr(unsafe.Pointer(&b[0])), 40)
}

func clickMouse(x, y int, right bool) {
	sw, sh := screenSize()
	absX, absY := uint32(x*65535/sw), uint32(y*65535/sh)
	moveMouse(x, y)
	f, fu := uint32(MOUSEEVENTF_LEFTDOWN), uint32(MOUSEEVENTF_LEFTUP)
	if right { f, fu = MOUSEEVENTF_RIGHTDOWN, MOUSEEVENTF_RIGHTUP }
	d := makeMouseInput(absX, absY, f|MOUSEEVENTF_ABSOLUTE)
	u := makeMouseInput(absX, absY, fu|MOUSEEVENTF_ABSOLUTE)
	procSendInput.Call(1, uintptr(unsafe.Pointer(&d[0])), 40)
	procSendInput.Call(1, uintptr(unsafe.Pointer(&u[0])), 40)
}

func pressKey(key string) {
	if len(key) != 1 { return }
	v := uint16(key[0])
	if key[0] >= 'a' && key[0] <= 'z' { v -= 32 }
	d := makeKeyboardInput(v, 0)
	u := makeKeyboardInput(v, KEYEVENTF_KEYUP)
	procSendInput.Call(1, uintptr(unsafe.Pointer(&d[0])), 40)
	procSendInput.Call(1, uintptr(unsafe.Pointer(&u[0])), 40)
}

func bootTime() time.Time {
	t, _, _ := procGetTickCount.Call()
	return time.Now().Add(-time.Duration(t) * time.Millisecond)
}

func getIdleSeconds() int {
	type LASTINPUTINFO struct {
		CbSize uint32
		DwTime uint32
	}
	var info LASTINPUTINFO
	info.CbSize = uint32(unsafe.Sizeof(info))
	ret, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&info)))
	if ret == 0 { return 0 }
	tick, _, _ := procGetTickCount.Call()
	diff := uint32(tick) - info.DwTime
	return int(diff / 1000)
}

func osUptime() int {
	t, _, _ := procGetTickCount.Call()
	return int(t / 60000)
}

func preventDuplicate() {
	myPID := os.Getpid()
	exe, _ := os.Executable()
	exeName := filepath.Base(exe)
	// Find and kill only OTHER instances with the same name
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		"Get-Process -Name '"+strings.TrimSuffix(exeName, ".exe")+"' -ErrorAction SilentlyContinue | Where-Object { $_.Id -ne "+fmt.Sprintf("%d", myPID)+" } | Stop-Process -Force")
	hideCmd(cmd)
	_ = cmd.Run()
	lockFile := filepath.Join(dataDir(), "agent.lock")
	os.WriteFile(lockFile, []byte(fmt.Sprintf("%d", myPID)), 0644)
}

func setupAutostart() {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	watchdogPath := filepath.Join(dir, "watchdog.bat")
	watchdog := `@echo off
:loop
tasklist | find "SystemHelper" >nul
if errorlevel 1 start "" "` + exe + `"
timeout /t 120 /nobreak >nul
goto loop`
	os.WriteFile(watchdogPath, []byte(watchdog), 0644)
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil { log("Registry: " + err.Error()); return }
	defer k.Close()
	k.SetStringValue("SystemMonitor", watchdogPath)
	log("Watchdog installed (auto-restarts every 2 min)")
}



func receivedDir() string {
	return filepath.Join("C:\\", "ProgramData", "SystemHelper", "received")
}

func startActivityLogger() {
	bt := bootTime()
	logEventDate("STARTED (boot: " + bt.Format("15:04") + ")")
	go func() {
		lastIdle := 0
		lastLog := 0
		statusTick := 0
		for {
			idle := getIdleSeconds()
			now := time.Now()
			if idle > 300 && lastIdle < 300 {
				idlePeriodStart = now
				activeDuration := now.Sub(activePeriodStart).Seconds()
				totalActiveSeconds += int64(activeDuration)
				logEventDate("INACTIVE (idle " + fmt.Sprintf("%ds", idle) + ", active was " + fmt.Sprintf("%.0fs", activeDuration) + ")")
				lastIdleState = "idle"
			}
			if idle < 300 && lastIdle >= 300 {
				activePeriodStart = now
				idleDuration := now.Sub(idlePeriodStart).Seconds()
				totalIdleSeconds += int64(idleDuration)
				logEventDate("ACTIVE (resumed after " + fmt.Sprintf("%.0fs", idleDuration) + ")")
				lastIdleState = "active"
			}
			lastIdle = idle
			currentIdleSeconds = idle
			lastLog++
			if lastLog >= 60 {
				lastLog = 0
				totalActive := totalActiveSeconds
				totalIdle := totalIdleSeconds
				if idle < 300 {
					totalActive += int64(now.Sub(activePeriodStart).Seconds())
				} else {
					totalIdle += int64(now.Sub(idlePeriodStart).Seconds())
				}
				logEventDate(fmt.Sprintf("RUNNING (uptime %dmin, active %ds, idle %ds)", osUptime(), totalActive, totalIdle))
			}
			statusTick++
			if statusTick >= 5 && wsRef != nil {
				statusTick = 0
				totalActive := totalActiveSeconds
				totalIdle := totalIdleSeconds
				if idle < 300 {
					totalActive += int64(now.Sub(activePeriodStart).Seconds())
				} else {
					totalIdle += int64(now.Sub(idlePeriodStart).Seconds())
				}
				wsRef.WriteJSON(Message{
					Type: "agent-status",
					Data: map[string]interface{}{
						"bootTime":     bootTime().Format(time.RFC3339),
						"programStart": programStartTime.Format(time.RFC3339),
						"totalIdle":    totalIdle,
						"totalActive":  totalActive,
						"currentState": lastIdleState,
						"currentIdle":  idle,
						"uptime":       osUptime(),
						"version":      Version,
					},
				})
			}
			time.Sleep(60 * time.Second)
		}
	}()
}
