//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var inputAccessOK = true

func hideCmd(cmd *exec.Cmd) {}

func hideConsole() {}

func screenSize() (int, int) {
	defer func() { recover() }()
	out, err := exec.Command("system_profiler", "SPDisplaysDataType").CombinedOutput()
	if err != nil { return 1920, 1080 }
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Resolution:") {
			var w, h int
			if n, _ := fmt.Sscanf(line, "Resolution: %d x %d", &w, &h); n == 2 {
				if w > 0 && h > 0 { return w, h }
			}
		}
	}
	return 1920, 1080
}

func macRunScript(script string) error {
	cmd := exec.Command("osascript", "-e", script)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

func moveMouse(x, y int) {
	if !inputAccessOK { return }
	if err := macRunScript(fmt.Sprintf(`tell application "System Events" to set position of mouse to {%d, %d}`, x, y)); err != nil {
		inputAccessOK = false
		log("macOS: Remote control disabled (enable Accessibility in System Settings)")
	}
}

func clickMouse(x, y int, right bool) {
	if !inputAccessOK { return }
	moveMouse(x, y)
	if err := macRunScript(fmt.Sprintf(`tell application "System Events" to click at {%d, %d}`, x, y)); err != nil {
		inputAccessOK = false
	}
}

func pressKey(key string) {
	if !inputAccessOK || key == "" { return }
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(key)
	specialKeys := map[string]string{
		"Enter": "return", "Tab": "tab", "Escape": "escape",
		"Backspace": "delete", "Delete": "forward delete",
		"ArrowUp": "up", "ArrowDown": "down",
		"ArrowLeft": "left", "ArrowRight": "right",
		"Home": "home", "End": "end",
		"PageUp": "page up", "PageDown": "page down",
		"Shift": "shift down", "Control": "control down",
		"Alt": "option down", "Meta": "command down",
		" ": "space",
	}
	script := ""
	if mapped, ok := specialKeys[escaped]; ok {
		script = fmt.Sprintf(`tell application "System Events" to keystroke %s`, mapped)
	} else if len(escaped) == 1 {
		script = fmt.Sprintf(`tell application "System Events" to keystroke "%s"`, escaped)
	}
	if script != "" {
		if err := macRunScript(script); err != nil {
			inputAccessOK = false
		}
	}
}

func bootTime() time.Time {
	out, err := exec.Command("sysctl", "-n", "kern.boottime").CombinedOutput()
	if err != nil { return time.Now() }
	parts := strings.Fields(string(out))
	for _, p := range parts {
		p = strings.TrimRight(p, ",}")
		if sec, err := strconv.ParseInt(p, 10, 64); err == nil && sec > 1000000000 {
			return time.Unix(sec, 0)
		}
	}
	return time.Now()
}

func getIdleSeconds() int {
	out, err := exec.Command("ioreg", "-c", "IOHIDSystem").CombinedOutput()
	if err != nil { return 0 }
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "HIDIdleTime") {
			parts := strings.Fields(line)
			for _, p := range parts {
				if strings.HasPrefix(p, "\"HIDIdleTime\"") || strings.Contains(p, "=") {
					continue
				}
				if val, err := strconv.ParseInt(p, 10, 64); err == nil && val > 0 {
					return int(val / 1000000000)
				}
			}
		}
	}
	return 0
}

func osUptime() int {
	out, err := exec.Command("sysctl", "-n", "kern.boottime").CombinedOutput()
	if err != nil { return 0 }
	parts := strings.Fields(string(out))
	for _, p := range parts {
		p = strings.TrimRight(p, ",}")
		if sec, err := strconv.ParseInt(p, 10, 64); err == nil && sec > 1000000000 {
			return int(time.Since(time.Unix(sec, 0)).Minutes())
		}
	}
	return 0
}

func preventDuplicate() {
	lockFile := filepath.Join(dataDir(), "agent.lock")
	if data, err := os.ReadFile(lockFile); err == nil {
		oldPID, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if oldPID > 0 && oldPID != os.Getpid() {
			proc, err := os.FindProcess(oldPID)
			if err == nil {
				proc.Kill()
				time.Sleep(200 * time.Millisecond)
			}
		}
	}
	os.WriteFile(lockFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644)
}

func setupAutostart() {
	exe, _ := os.Executable()
	home, _ := os.UserHomeDir()
	launchDir := filepath.Join(home, "Library", "LaunchAgents")
	os.MkdirAll(launchDir, 0755)
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>com.systemhelper.agent</string>
<key>ProgramArguments</key><array><string>%s</string></array>
<key>KeepAlive</key><true/>
<key>RunAtLoad</key><true/>
<key>ProcessType</key><string>Interactive</string>
</dict></plist>`, exe)
	plistPath := filepath.Join(launchDir, "com.systemhelper.agent.plist")
	os.WriteFile(plistPath, []byte(plist), 0644)
	exec.Command("launchctl", "load", plistPath).Run()
	log("LaunchAgent installed (auto-restarts on crash)")
}

func receivedDir() string {
	return filepath.Join(dataDir(), "received")
}

func startPopupKiller() {}

func startActivityLogger() {
	go func() {
		bt := bootTime()
		logEventDate("STARTED (boot: " + bt.Format("15:04") + ")")
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
			if statusTick >= 5 {
				activeConnsMu.RLock()
				conns := make([]*serverConnection, len(activeConnections))
				copy(conns, activeConnections)
				activeConnsMu.RUnlock()
				hasConn := false
				for _, sc := range conns {
					if sc != nil && !sc.dead { hasConn = true; break }
				}
				if hasConn {
					statusTick = 0
					totalActive := totalActiveSeconds
					totalIdle := totalIdleSeconds
					if idle < 300 {
						totalActive += int64(now.Sub(activePeriodStart).Seconds())
					} else {
						totalIdle += int64(now.Sub(idlePeriodStart).Seconds())
					}
					for _, sc := range conns {
						if sc == nil || sc.dead { continue }
						sc.mu.Lock()
						if sc.conn != nil {
							safeWriteJSON(sc.conn, Message{
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
						sc.mu.Unlock()
					}
				}
			}
			time.Sleep(60 * time.Second)
		}
	}()
}

func getSystemInfo() map[string]interface{} { return nil }
func getProcessList() []map[string]interface{} { return nil }
func killProcess(string) bool { return false }
func getServiceList() []map[string]interface{} { return nil }
func controlService(string, string) bool { return false }
func getDriveList() []map[string]interface{} { return nil }
func listFiles(string) []map[string]interface{} { return nil }
func getNetworkInfo() map[string]interface{} { return nil }
func getEventLogs(int) []map[string]interface{} { return nil }
func executeShellCommand(string) string { return "" }
func lockWorkstation() {}
func logoffUser() {}
func shutdownPC() {}
func restartPC() {}
func sleepPC() {}
