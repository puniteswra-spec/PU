package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ServerLoad struct {
	mu            sync.Mutex
	CPUPercent    float64 `json:"cpu_percent"`
	MemUsedMB     float64 `json:"mem_used_mb"`
	MemTotalMB    float64 `json:"mem_total_mb"`
	MemPercent    float64 `json:"mem_percent"`
	NetSentBytes  int64   `json:"net_sent_bytes"`
	NetRecvBytes  int64   `json:"net_recv_bytes"`
	NetSentRate   float64 `json:"net_sent_rate_kbps"`
	NetRecvRate   float64 `json:"net_recv_rate_kbps"`
	WSConnections int     `json:"ws_connections"`
	AgentCount    int     `json:"agent_count"`
	AssistCount   int     `json:"assist_count"`
	UptimeSeconds float64 `json:"uptime_seconds"`
	LastCheck     time.Time `json:"last_check"`
	prevSent      int64
	prevRecv      int64
	prevTime      time.Time
}

var globalServerLoad = &ServerLoad{}

func (sl *ServerLoad) Update() {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	// CPU usage
	sl.CPUPercent = getCPUPercent()

	// Memory usage
	used, total := getMemoryUsage()
	sl.MemUsedMB = used
	sl.MemTotalMB = total
	if total > 0 {
		sl.MemPercent = (used / total) * 100
	}

	// Network rates
	now := time.Now()
	if !sl.prevTime.IsZero() {
		elapsed := now.Sub(sl.prevTime).Seconds()
		if elapsed > 0 {
			sl.NetSentRate = float64(sl.NetSentBytes-sl.prevSent) / elapsed / 1024
			sl.NetRecvRate = float64(sl.NetRecvBytes-sl.prevRecv) / elapsed / 1024
		}
	}
	sl.prevSent = sl.NetSentBytes
	sl.prevRecv = sl.NetRecvBytes
	sl.prevTime = now

	// Connections
	sl.WSConnections = 0
	wsClients.Range(func(key, value interface{}) bool {
		sl.WSConnections++
		return true
	})

	sl.AgentCount = 0
	agentConnsMu.RLock()
	sl.AgentCount = len(agentConns)
	agentConnsMu.RUnlock()

	sl.AssistCount = 0
	assistSessionsMu.RLock()
	for _, s := range assistSessions {
		if s.Active {
			sl.AssistCount++
		}
	}
	assistSessionsMu.RUnlock()

	sl.UptimeSeconds = time.Since(startTime).Seconds()
	sl.LastCheck = now
}

func (sl *ServerLoad) Snapshot() map[string]interface{} {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	return map[string]interface{}{
		"cpu_percent":     fmt.Sprintf("%.1f", sl.CPUPercent),
		"mem_used_mb":     fmt.Sprintf("%.0f", sl.MemUsedMB),
		"mem_total_mb":    fmt.Sprintf("%.0f", sl.MemTotalMB),
		"mem_percent":     fmt.Sprintf("%.1f", sl.MemPercent),
		"net_sent_rate":   fmt.Sprintf("%.0f", sl.NetSentRate),
		"net_recv_rate":   fmt.Sprintf("%.0f", sl.NetRecvRate),
		"ws_connections":  sl.WSConnections,
		"agent_count":     sl.AgentCount,
		"assist_count":    sl.AssistCount,
		"uptime_seconds":  fmt.Sprintf("%.0f", sl.UptimeSeconds),
		"tunnel_type":     cfg.TunnelProvider,
		"tunnel_active":   tunnelCmd != nil || cfg.CloudflareTunnelID != "",
		"version":         binaryVersion,
		"os":              runtime.GOOS,
		"arch":            runtime.GOARCH,
	}
}

func getCPUPercent() float64 {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("wmic", "cpu", "get", "LoadPercentage", "/value").Output()
		if err != nil {
			return 0
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "LoadPercentage=") {
				val := strings.TrimPrefix(strings.TrimSpace(line), "LoadPercentage=")
				if f, err := strconv.ParseFloat(val, 64); err == nil {
					return f
				}
			}
		}
	} else if runtime.GOOS == "darwin" {
		out, err := exec.Command("sh", "-c", "top -l 1 | grep 'CPU usage' | awk '{print $3}' | tr -d '%'").Output()
		if err == nil {
			val := strings.TrimSpace(string(out))
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				return f
			}
		}
	} else {
		out, err := exec.Command("sh", "-c", "top -bn1 | grep 'Cpu(s)' | awk '{print $2}'").Output()
		if err == nil {
			val := strings.TrimSpace(string(out))
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				return f
			}
		}
	}
	return 0
}

func getMemoryUsage() (usedMB float64, totalMB float64) {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("wmic", "OS", "get", "TotalVisibleMemorySize,FreePhysicalMemory", "/value").Output()
		if err != nil {
			return 0, 0
		}
		var totalKB, freeKB int64
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "TotalVisibleMemorySize=") {
				val := strings.TrimPrefix(line, "TotalVisibleMemorySize=")
				totalKB, _ = strconv.ParseInt(strings.TrimSpace(val), 10, 64)
			}
			if strings.HasPrefix(line, "FreePhysicalMemory=") {
				val := strings.TrimPrefix(line, "FreePhysicalMemory=")
				freeKB, _ = strconv.ParseInt(strings.TrimSpace(val), 10, 64)
			}
		}
		totalMB = float64(totalKB) / 1024
		usedMB = float64(totalKB-freeKB) / 1024
		return usedMB, totalMB
	}
	// Linux/macOS fallback
	out, err := exec.Command("sh", "-c", "free -m 2>/dev/null | awk '/Mem:/{print $3, $2}' || sysctl -n hw.memsize 2>/dev/null | awk '{print $1/1048576, $1/1048576}'").Output()
	if err != nil {
		return 0, 0
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) >= 2 {
		usedMB, _ = strconv.ParseFloat(parts[0], 64)
		totalMB, _ = strconv.ParseFloat(parts[1], 64)
	}
	return usedMB, totalMB
}

func startServerLoadMonitor() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		globalServerLoad.Update()
	}
}
