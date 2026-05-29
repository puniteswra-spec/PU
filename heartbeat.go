package main

import (
	"encoding/json"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// --- Connection Quality Tracking ---

type ConnectionQuality struct {
	mu sync.Mutex

	LatencyMS     float64 `json:"latency_ms"`
	JitterMS      float64 `json:"jitter_ms"`
	PacketLossPct float64 `json:"packet_loss_pct"`

	PingsSent     int64 `json:"pings_sent"`
	PongsReceived int64 `json:"pongs_received"`
	PongsMissed   int64 `json:"pongs_missed"`

	LastPingTime  int64 `json:"last_ping_time"`
	LastPongTime  int64 `json:"last_pong_time"`
	ConnectedAt   int64 `json:"connected_at"`
	Reconnections int   `json:"reconnections"`

	recentLatencies []float64
	recentSent      []bool
}

func NewConnectionQuality() *ConnectionQuality {
	return &ConnectionQuality{
		ConnectedAt:    time.Now().UnixMilli(),
		recentLatencies: make([]float64, 0, 20),
		recentSent:      make([]bool, 0, 30),
	}
}

func (cq *ConnectionQuality) RecordPing() {
	cq.mu.Lock()
	defer cq.mu.Unlock()
	cq.PingsSent++
	cq.LastPingTime = time.Now().UnixMilli()
	cq.recentSent = append(cq.recentSent, true)
	if len(cq.recentSent) > 30 {
		cq.recentSent = cq.recentSent[1:]
	}
}

func (cq *ConnectionQuality) RecordPong(pingSentAt int64) {
	cq.mu.Lock()
	defer cq.mu.Unlock()
	now := time.Now().UnixMilli()
	cq.PongsReceived++
	cq.LastPongTime = now

	latency := float64(now - pingSentAt)
	cq.recentLatencies = append(cq.recentLatencies, latency)
	if len(cq.recentLatencies) > 20 {
		cq.recentLatencies = cq.recentLatencies[1:]
	}

	// Calculate average latency
	var sum float64
	for _, l := range cq.recentLatencies {
		sum += l
	}
	cq.LatencyMS = sum / float64(len(cq.recentLatencies))

	// Calculate jitter (std dev of recent latencies)
	if len(cq.recentLatencies) > 1 {
		var variance float64
		for _, l := range cq.recentLatencies {
			diff := l - cq.LatencyMS
			variance += diff * diff
		}
		variance /= float64(len(cq.recentLatencies) - 1)
		cq.JitterMS = math.Sqrt(variance)
	}

	// Calculate packet loss
	if len(cq.recentSent) > 5 {
		missed := 0
		for _, s := range cq.recentSent {
			if !s {
				missed++
			}
		}
		cq.PacketLossPct = float64(missed) / float64(len(cq.recentSent)) * 100
	}
}

func (cq *ConnectionQuality) RecordMissedPong() {
	cq.mu.Lock()
	defer cq.mu.Unlock()
	cq.PongsMissed++
	cq.recentSent = append(cq.recentSent, false)
	if len(cq.recentSent) > 30 {
		cq.recentSent = cq.recentSent[1:]
	}
	if len(cq.recentSent) > 5 {
		missed := 0
		for _, s := range cq.recentSent {
			if !s {
				missed++
			}
		}
		cq.PacketLossPct = float64(missed) / float64(len(cq.recentSent)) * 100
	}
}

func (cq *ConnectionQuality) RecordReconnect() {
	cq.mu.Lock()
	defer cq.mu.Unlock()
	cq.Reconnections++
}

func (cq *ConnectionQuality) Snapshot() map[string]interface{} {
	cq.mu.Lock()
	defer cq.mu.Unlock()
	health := "excellent"
	if cq.LatencyMS > 100 || cq.JitterMS > 30 {
		health = "good"
	} else if cq.LatencyMS > 200 || cq.PacketLossPct > 5 {
		health = "fair"
	} else if cq.LatencyMS > 500 || cq.PacketLossPct > 15 {
		health = "poor"
	}
	return map[string]interface{}{
		"latency_ms":      math.Round(cq.LatencyMS*10) / 10,
		"jitter_ms":       math.Round(cq.JitterMS*10) / 10,
		"packet_loss_pct": math.Round(cq.PacketLossPct*10) / 10,
		"pings_sent":      cq.PingsSent,
		"pongs_received":  cq.PongsReceived,
		"pongs_missed":    cq.PongsMissed,
		"reconnections":   cq.Reconnections,
		"connected_at":    cq.ConnectedAt,
		"health":          health,
	}
}

// --- Server-side: handle ping/pong from agents ---

func handleAgentPing(connID string) {
	msg, _ := json.Marshal(map[string]interface{}{
		"type": "pong",
		"ts":   time.Now().UnixMilli(),
	})
	agentConnsMu.RLock()
	if conn, ok := agentConns[connID]; ok {
		conn.WriteMessage(websocket.TextMessage, msg)
	}
	agentConnsMu.RUnlock()
}

func handleAgentPong(connID string, pingSentAt int64) {
	if v, ok := agentStats.Load(connID); ok {
		stats := v.(*AgentStats)
		stats.mu.Lock()
		latency := float64(time.Now().UnixMilli() - pingSentAt)
		stats.LatencyMS = latency
		stats.mu.Unlock()
	}
}

// --- Agent-side: handle pong from server ---

var agentConnQuality = NewConnectionQuality()

func agentHandlePong(ts int64) {
	agentConnQuality.RecordPong(ts)
}

// --- Periodic ping sender (runs on server per agent) ---

func startAgentPingLoop(connID string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			agentConnsMu.RLock()
			conn, ok := agentConns[connID]
			agentConnsMu.RUnlock()
			if !ok {
				return
			}
			pingMsg, _ := json.Marshal(map[string]interface{}{
				"type": "ping",
				"ts":   time.Now().UnixMilli(),
			})
			if err := conn.WriteMessage(websocket.TextMessage, pingMsg); err != nil {
				return
			}
			// Track missed pongs
			go func() {
				time.Sleep(10 * time.Second)
				if v, ok := agentStats.Load(connID); ok {
					stats := v.(*AgentStats)
					stats.mu.Lock()
					if stats.LastPongRecv < stats.LastPingSent {
						// Pong missed
						stats.Health = "degraded"
					}
					stats.LastPingSent = time.Now().UnixMilli()
					stats.mu.Unlock()
				}
			}()
		}
	}
}

// --- Agent-side: periodic ping to server ---

func agentStartPingLoop(conn *websocket.Conn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	missed := 0
	for {
		select {
		case <-ticker.C:
			agentConnQuality.RecordPing()
			pingMsg, _ := json.Marshal(map[string]interface{}{
				"type": "ping",
				"ts":   time.Now().UnixMilli(),
			})
			if err := conn.WriteMessage(websocket.TextMessage, pingMsg); err != nil {
				return
			}
			// Wait for pong with timeout
			go func() {
				time.Sleep(10 * time.Second)
				agentConnQuality.mu.Lock()
				if agentConnQuality.LastPongTime < agentConnQuality.LastPingTime {
					agentConnQuality.RecordMissedPong()
					missed++
				}
				agentConnQuality.mu.Unlock()
			}()
		}
	}
}

// --- Server health check (for agent auto-failover) ---

func checkServerHealth(serverURL string) bool {
	if serverURL == "" {
		return false
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(serverURL + "/api/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}
