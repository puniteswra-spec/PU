//go:build !windows && !darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func hideCmd(cmd *exec.Cmd) {}

func hideConsole() {}

func screenSize() (int, int) { return 1920, 1080 }

func moveMouse(x, y int) {}

func clickMouse(x, y int, right bool) {}

func pressKey(key string) {}

func bootTime() time.Time { return time.Now() }

func getIdleSeconds() int { return 0 }

func osUptime() int { return 0 }

func preventDuplicate() {
	lockFile := filepath.Join(dataDir(), "agent.lock")
	os.WriteFile(lockFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644)
}

func setupAutostart() {}

func receivedDir() string {
	return filepath.Join(dataDir(), "received")
}

func startPopupKiller() {}

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