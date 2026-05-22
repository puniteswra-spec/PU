package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/gorilla/websocket"
)

func NewDashboardServer(cfg *Config, frameCh chan *WireMessage, agent *Agent, pool *TransportPool, bandwidth *BandwidthMonitor) *DashboardServer {
	ds := &DashboardServer{
		cfg:       cfg,
		startTime: time.Now(),
		reports:   make(map[string][]ActivityEvent),
		frameCh:   frameCh,
		agent:     agent,
		pool:      pool,
		bandwidth: bandwidth,
		dashConns: make(map[*websocket.Conn]bool),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	ds.addReport = ds.addReportEvents
	return ds
}

func (ds *DashboardServer) addReportEvents(agentID string, events []ActivityEvent) {
	ds.reportsMu.Lock()
	defer ds.reportsMu.Unlock()
	ds.reports[agentID] = append(ds.reports[agentID], events...)
	cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()
	var kept []ActivityEvent
	for _, e := range ds.reports[agentID] {
		if e.Timestamp >= cutoff {
			kept = append(kept, e)
		}
	}
	ds.reports[agentID] = kept
}

func (ds *DashboardServer) Start(ctx context.Context) error {
	if ds.frameCh != nil {
		go ds.localFrameRelay(ctx)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", ds.serveDashboard)
	mux.HandleFunc("/ws", ds.dashboardWS)
	mux.HandleFunc("/dashboard/ws", ds.dashboardWS)
	mux.HandleFunc("/api/agents", ds.localAgentsHandler)
	mux.HandleFunc("/api/report", ds.apiReportHandler)
	mux.HandleFunc("/api/report.csv", ds.apiReportCSVHandler)
	mux.HandleFunc("/api/settings", ds.apiSettingsHandler)
	mux.HandleFunc("/api/push-report", ds.apiPushReportHandler)
	mux.HandleFunc("/api/system-info", ds.apiSystemInfoHandler)
	mux.HandleFunc("/api/metrics", ds.localMetricsHandler)
	mux.HandleFunc("/api/config", ds.localConfigHandler)
	mux.HandleFunc("/api/bandwidth", ds.localBandwidthHandler)
	mux.HandleFunc("/api/tier", ds.localTierHandler)
	mux.HandleFunc("/api/compile-monthly-report", ds.localCompileMonthlyHandler)
	mux.HandleFunc("/api/update", ds.apiUpdateHandler)
	mux.HandleFunc("/api/restart", ds.apiRestartHandler)

	server := &http.Server{
		Addr:    fmt.Sprintf("0.0.0.0:%d", ds.cfg.ConfigPort),
		Handler: mux,
	}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(sc)
	}()
	llog("info", "local dashboard listening on :%d", ds.cfg.ConfigPort)
	return server.ListenAndServe()
}

func (ds *DashboardServer) serveDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data, err := embeddedFS.ReadFile("dashboard.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Write(data)
}

func (ds *DashboardServer) localFrameRelay(ctx context.Context) {
	agentLabel := agentID
	if agentLabel == "" {
		agentLabel = "local"
	}
	for {
		select {
		case <-ctx.Done():
			return
		case wm, ok := <-ds.frameCh:
			if !ok || wm == nil {
				return
			}
			if wm.Type == MSG_FRAME_KEY || wm.Type == MSG_FRAME_DELTA {
				relay := map[string]interface{}{
					"type":    "frame",
					"agentId": agentLabel,
					"data":    base64.StdEncoding.EncodeToString(wm.Data),
				}
				data, _ := json.Marshal(relay)
				ds.dashMu.Lock()
				for dc := range ds.dashConns {
					if err := dc.WriteMessage(websocket.TextMessage, data); err != nil {
						dc.Close()
						delete(ds.dashConns, dc)
					}
				}
				ds.dashMu.Unlock()
			} else if wm.Type == MSG_REPORT {
				var status map[string]interface{}
				if json.Unmarshal(wm.Data, &status) == nil {
					if statusType, _ := status["type"].(string); statusType == "status" {
						data, _ := json.Marshal(status)
						ds.dashMu.Lock()
						for dc := range ds.dashConns {
							if err := dc.WriteMessage(websocket.TextMessage, data); err != nil {
								dc.Close()
								delete(ds.dashConns, dc)
							}
						}
						ds.dashMu.Unlock()
					}
				}
			}
		}
	}
}

func (ds *DashboardServer) dashboardWS(w http.ResponseWriter, r *http.Request) {
	conn, err := ds.upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, "upgrade failed", http.StatusBadRequest)
		return
	}
	ds.dashMu.Lock()
	ds.dashConns[conn] = true
	ds.dashMu.Unlock()
	defer func() {
		ds.dashMu.Lock()
		delete(ds.dashConns, conn)
		ds.dashMu.Unlock()
		conn.Close()
	}()

	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		if mt != websocket.TextMessage {
			continue
		}
		var cmd map[string]interface{}
		if json.Unmarshal(msg, &cmd) != nil {
			continue
		}
		if cmd["type"] == "generate_share_link" {
			localIP := getLocalIP()
			port := ds.cfg.ConfigPort
			agentId := agentID
			if agentId == "" {
				agentId = "local"
			}
			shareURL := fmt.Sprintf("http://%s:%d/?agent=%s", localIP, port, agentId)
			sendJSON(conn, map[string]interface{}{
				"type":    "share_link",
				"url":     shareURL,
				"agentId": agentId,
			})
			continue
		}
		if ds.agent == nil {
			continue
		}
		ds.agent.handleRemoteCommand(cmd)
	}
}

func (ds *DashboardServer) localAgentsHandler(w http.ResponseWriter, r *http.Request) {
	id := agentID
	if id == "" {
		id = "local"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode([]string{id})
}

func (ds *DashboardServer) apiSystemInfoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ds.getSystemInfo())
}

func (ds *DashboardServer) localMetricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"frames_captured": atomicLoadFrames(),
		"server_mode":     false,
	})
}

func (ds *DashboardServer) localConfigHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"server_port":      ds.cfg.ConfigPort,
		"monthly_limit_mb": ds.cfg.MonthlyLimitMB,
	})
}

func (ds *DashboardServer) localBandwidthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var body struct {
			MB int64 `json:"mb"`
		}
		if json.NewDecoder(r.Body).Decode(&body) == nil && body.MB > 0 {
			ds.cfg.MonthlyLimitMB = body.MB
			if ds.bandwidth != nil {
				ds.bandwidth.SetLimitMB(body.MB)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"monthly_limit_mb": ds.cfg.MonthlyLimitMB,
	})
}

func (ds *DashboardServer) localTierHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Tier int `json:"tier"`
	}
	if json.NewDecoder(r.Body).Decode(&body) == nil && ds.agent != nil && ds.agent.captureLoop != nil {
		ds.agent.captureLoop.ForceTier(CaptureTier(body.Tier))
	}
	w.WriteHeader(http.StatusOK)
}

func atomicLoadFrames() uint64 {
	return framesCaptured
}

func (ds *DashboardServer) localCompileMonthlyHandler(w http.ResponseWriter, r *http.Request) {
	sm := &ServerMode{cfg: ds.cfg}
	sm.apiCompileMonthlyHandler(w, r)
}

func (ds *DashboardServer) apiUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct{ URL string `json:"url"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if ds.agent == nil {
		// Agent mode: apply update to self
		applyUpdate(body.URL)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("update initiated"))
		return
	}
	// In agent mode, update self directly
	applyUpdate(body.URL)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("update initiated"))
}

func (ds *DashboardServer) apiRestartHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	exe, err := os.Executable()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	cmd := exec.Command(exe)
	hideCmdWindow(cmd)
	if err := cmd.Start(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	os.Exit(0)
}
