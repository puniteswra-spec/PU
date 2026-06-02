package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/csv"
	"encoding/hex"
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"crypto/sha1"

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

type AgentStats struct {
	mu             sync.Mutex
	Transport      string  `json:"transport"`
	LatencyMS      float64 `json:"latency_ms"`
	BytesReceived  int64   `json:"bytes_received"`
	BytesPerSec    float64 `json:"bytes_per_sec"`
	FramesReceived int64   `json:"frames_received"`
	FramesPerSec   float64 `json:"frames_per_sec"`
	LastFrameTime  int64   `json:"last_frame_time"`
	Health         string  `json:"health"`
	ConnectedAt    int64   `json:"connected_at"`
	LastPingSent   int64   `json:"-"`
	LastPongRecv   int64   `json:"-"`
	recentBytes    []int64
	recentFrames   []int64
}

func NewAgentStats() *AgentStats {
	return &AgentStats{
		Transport:    "unknown",
		Health:       "unknown",
		ConnectedAt:  time.Now().UnixMilli(),
		recentBytes:  make([]int64, 0, 10),
		recentFrames: make([]int64, 0, 10),
	}
}

func (s *AgentStats) RecordBytes(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.BytesReceived += int64(n)
	s.recentBytes = append(s.recentBytes, int64(n))
	if len(s.recentBytes) > 10 {
		s.recentBytes = s.recentBytes[1:]
	}
	var sum int64
	for _, v := range s.recentBytes {
		sum += v
	}
	s.BytesPerSec = float64(sum) / float64(len(s.recentBytes)) / 2.0
}

func (s *AgentStats) RecordFrame() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.FramesReceived++
	s.LastFrameTime = time.Now().UnixMilli()
	s.recentFrames = append(s.recentFrames, 1)
	if len(s.recentFrames) > 10 {
		s.recentFrames = s.recentFrames[1:]
	}
	var sum int64
	for _, v := range s.recentFrames {
		sum += v
	}
	s.FramesPerSec = float64(sum) / float64(len(s.recentFrames)) / 2.0
}

func (s *AgentStats) UpdateHealth() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.LastFrameTime == 0 {
		s.Health = "waiting"
		return
	}
	age := time.Since(time.UnixMilli(s.LastFrameTime))
	if age < 5*time.Second {
		s.Health = "good"
	} else if age < 15*time.Second {
		s.Health = "slow"
	} else {
		s.Health = "stale"
	}
}

func (s *AgentStats) Snapshot() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]interface{}{
		"transport":       s.Transport,
		"latency_ms":      s.LatencyMS,
		"bytes_received":  s.BytesReceived,
		"bytes_per_sec":   s.BytesPerSec,
		"frames_received": s.FramesReceived,
		"frames_per_sec":  s.FramesPerSec,
		"last_frame_time": s.LastFrameTime,
		"health":          s.Health,
		"connected_at":    s.ConnectedAt,
	}
}

var agentStats sync.Map

type WireMessage struct {
    Type    string `json:"type"`
    Data    []byte `json:"data,omitempty"`
    AgentID string `json:"agentId,omitempty"`
    Server  bool   `json:"server,omitempty"`
}


const MSG_FRAME = "frame"

var wsClients sync.Map

// connWriteMu prevents concurrent writes to the same WebSocket connection.
// gorilla/websocket panics on concurrent WriteMessage to the same conn.
var connWriteMu sync.Map // map[*websocket.Conn]*sync.Mutex

func getWriteMu(conn *websocket.Conn) *sync.Mutex {
	v, _ := connWriteMu.LoadOrStore(conn, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func safeWriteMessage(conn *websocket.Conn, msgType int, data []byte) error {
	mu := getWriteMu(conn)
	mu.Lock()
	defer mu.Unlock()
	return conn.WriteMessage(msgType, data)
}

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

// ElectionStatus is the live snapshot of leader election state — written by
// startGitHubLeaderElection / tryClaimLeadership, read by /api/election-status
// and the XLSX report so admins can see who is leader and how the election
// resolved (GitHub vs LAN vs relay).
type ElectionStatus struct {
    Method         string    `json:"method"`          // "github" | "lan" | "relay" | "none"
    Configured     bool      `json:"configured"`      // GitHubRepo + GitHubToken both set
    Repo           string    `json:"repo"`            // GitHub repo (masked)
    LeaderID       string    `json:"leader_id"`       // AgentID of current primary server
    LeaderUpdated  time.Time `json:"leader_updated"`  // Last update time of primary_server.json
    LeaderStale    bool      `json:"leader_stale"`    // True if leader hasn't renewed in electionInterval
    SelfIsLeader   bool      `json:"self_is_leader"`  // True if this instance is the primary
    LastCheck      time.Time `json:"last_check"`
    LastResult     string    `json:"last_result"`     // "claimed" | "renewed" | "active" | "stale-takeover" | "error" | "no-github"
    LastError      string    `json:"last_error"`
    CheckCount     int       `json:"check_count"`
    FallbackServer string    `json:"fallback_server"` // Relay URL used when no GitHub
}

var globalElectionStatus ElectionStatus
var globalElectionStatusMu sync.RWMutex

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
    CaptureSchedule     string // "HH:MM-HH:MM" or "24/7"
    CaptureDays         string // "Mon-Fri" or "daily"
    DisplayIndex        int    // which monitor to capture (-1 = primary)
    AutoQuality         bool   // auto-adjust JPEG quality based on bandwidth
    JPEGQuality         int    // manual quality override (30-95)
    SSHEnabled          bool   // expose SSH server on this machine
    SSHPort             int    // default 2222
    SSHUsername         string // default "admin"
    SSHPassword         string // auto-generated 16-char password
    SSHHostKeyPEM       string // ed25519 host key (PEM)
    SSHAuthorizedKeys   []string
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
    DeployUser             string           `json:"deploy_user,omitempty"`
    DeployPass             string           `json:"deploy_pass,omitempty"`
    DeployDomain           string           `json:"deploy_domain,omitempty"`
    CaptureSchedule        string           `json:"capture_schedule,omitempty"`
    CaptureDays            string           `json:"capture_days,omitempty"`
    DisplayIndex           int              `json:"display_index,omitempty"`
    AutoQuality            bool             `json:"auto_quality,omitempty"`
    JPEGQuality            int              `json:"jpeg_quality,omitempty"`
    SSHEnabled             bool             `json:"ssh_enabled,omitempty"`
    SSHPort                int              `json:"ssh_port,omitempty"`
    SSHUsername            string           `json:"ssh_username,omitempty"`
    SSHPassword            string           `json:"ssh_password,omitempty"`
    SSHHostKeyPEM          string           `json:"ssh_host_key_pem,omitempty"`
    SSHAuthorizedKeys      []string         `json:"ssh_authorized_keys,omitempty"`
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
var agentModeMu sync.RWMutex
var connAgentIDMu sync.RWMutex

// LAN discovery and election
var globalLAN *LANLeaderElection
var lanElectionDone = make(chan struct{})

var (
	consecutiveRestarts int
	monitorStartTime    time.Time
)

// Assist sessions (browser-based remote screen sharing)
type AssistSession struct {
	ID        string
	UserConn  *websocket.Conn
	AdminConn *websocket.Conn
	CreatedAt time.Time
	Active    bool
}

var assistSessions = make(map[string]*AssistSession)
var assistSessionsMu sync.RWMutex

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

// getStableMachineID returns a hardware/OS-level identifier that is unique
// per machine and stable across reboots, reinstalls, and config wipes. Used
// for AgentID so admins can identify "which system was this" in past logs.
//
// Windows: MachineGuid from HKLM\SOFTWARE\Microsoft\Cryptography (unique
//
//	per Windows install; persists across reboots and config wipes;
//	survives the user clearing their PunMonitor settings).
//
// macOS / Linux: SHA-1 of the first non-loopback MAC address (stable per
//
//	hardware; identical across reboots and re-installs).
//
// Returns "" if no identifier can be obtained (caller falls back to
// hostname).
func getStableMachineID() string {
	if id := platformStableMachineID(); id != "" {
		return id
	}
	// Generic fallback: SHA-1 of hostname. Not perfect (hostnames can
	// change) but at least deterministic per machine.
	h := getHostname()
	if h == "" || h == "unknown" {
		return ""
	}
	sum := sha1.Sum([]byte(h))
	return hex.EncodeToString(sum[:])[:8]
}

// isLegacyRandomAgentID returns true if id looks like the old
// "hostname-XXXX" pattern where XXXX is 4 random alphanumeric characters.
// Used to detect settings that were generated with the previous
// (unstable) random-suffix AgentID format so we can migrate them to
// the new stable format on next run.
func isLegacyRandomAgentID(id string) bool {
	i := strings.LastIndex(id, "-")
	if i < 0 || i == len(id)-1 {
		return false
	}
	suffix := id[i+1:]
	if len(suffix) != 4 {
		return false
	}
	for _, r := range suffix {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
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
        DeployUser:             deployCreds.Username,
        DeployPass:             deployCreds.Password,
        DeployDomain:           deployCreds.Domain,
        CaptureSchedule:        cfg.CaptureSchedule,
        CaptureDays:            cfg.CaptureDays,
        SSHEnabled:             cfg.SSHEnabled,
        SSHPort:                cfg.SSHPort,
        SSHUsername:            cfg.SSHUsername,
        SSHPassword:            cfg.SSHPassword,
        SSHHostKeyPEM:          cfg.SSHHostKeyPEM,
        SSHAuthorizedKeys:      cfg.SSHAuthorizedKeys,
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
	if s.DeployUser != "" {
		SetDeployCredentials(s.DeployUser, s.DeployPass, s.DeployDomain)
		llog("info", "Loaded deploy credentials for user %s", s.DeployUser)
	}
	cfg.CaptureSchedule = s.CaptureSchedule
	cfg.CaptureDays = s.CaptureDays
	cfg.SSHEnabled = s.SSHEnabled
	if s.SSHPort > 0 { cfg.SSHPort = s.SSHPort }
	// After loadSettings, if the file didn't include SSHEnabled (defaults to
	// false in JSON), restore the ON default so the SSH server starts up
	// out of the box. Users who explicitly disable it in /api/settings will
	// set it to false, which will then be persisted via saveSettings().
	if !s.SSHEnabled && s.SSHHostKeyPEM == "" {
		cfg.SSHEnabled = true
	}
	if s.SSHUsername != "" { cfg.SSHUsername = s.SSHUsername }
	if s.SSHPassword != "" { cfg.SSHPassword = s.SSHPassword }
	if s.SSHHostKeyPEM != "" { cfg.SSHHostKeyPEM = s.SSHHostKeyPEM }
	if s.SSHAuthorizedKeys != nil { cfg.SSHAuthorizedKeys = s.SSHAuthorizedKeys }
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
	llog("info", "startScreenCapture: starting (fps=%v display=%d quality=%d)", cfg.MaxFPS, cfg.DisplayIndex, cfg.JPEGQuality)
	fps := cfg.MaxFPS
    if fps <= 0 {
        fps = 1
    }
    interval := time.Duration(float64(time.Second) / fps)
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    quality := 85
    if cfg.JPEGQuality > 0 && cfg.JPEGQuality <= 95 {
        quality = cfg.JPEGQuality
    }
    for {
        select {
        case <-ticker.C:
            // Check schedule
            if !isCaptureAllowed() {
                continue
            }
			img, err := captureScreenByIndex(cfg.DisplayIndex)
			if err != nil {
				llog("warn", "screen capture failed: %v", err)
				continue
			}
			llog("info", "frame captured: %dx%d", img.Bounds().Dx(), img.Bounds().Dy())
            // Auto-adjust quality based on bandwidth
            if cfg.AutoQuality {
                quality = autoAdjustQuality(quality)
            }
            var buf bytes.Buffer
            if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
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

func isCaptureAllowed() bool {
	schedule := cfg.CaptureSchedule
	if schedule == "" || schedule == "24/7" {
		return true
	}
	days := cfg.CaptureDays
	if days == "" || days == "daily" {
		// Check time only
	} else {
		// Check day of week
		now := time.Now().Weekday()
		dayName := now.String()[:3]
		if !strings.Contains(days, dayName) {
			return false
		}
	}
	// Parse time range "HH:MM-HH:MM"
	parts := strings.Split(schedule, "-")
	if len(parts) != 2 {
		return true
	}
	startParts := strings.Split(parts[0], ":")
	endParts := strings.Split(parts[1], ":")
	if len(startParts) != 2 || len(endParts) != 2 {
		return true
	}
	now := time.Now()
	startH, _ := strconv.Atoi(startParts[0])
	startM, _ := strconv.Atoi(startParts[1])
	endH, _ := strconv.Atoi(endParts[0])
	endM, _ := strconv.Atoi(endParts[1])
	startMin := startH*60 + startM
	endMin := endH*60 + endM
	nowMin := now.Hour()*60 + now.Minute()
	if startMin < endMin {
		return nowMin >= startMin && nowMin <= endMin
	}
	// Overnight schedule (e.g., 22:00-06:00)
	return nowMin >= startMin || nowMin <= endMin
}

func autoAdjustQuality(current int) int {
	// Get current bandwidth usage
	if v, ok := agentStats.Load(cfg.AgentID); ok {
		stats := v.(*AgentStats)
		stats.mu.Lock()
		kbps := stats.BytesPerSec / 1024
		stats.mu.Unlock()
		if kbps > 500 {
			// High bandwidth — reduce quality
			current -= 5
			if current < 30 {
				current = 30
			}
		} else if kbps < 100 {
			// Low bandwidth — increase quality
			current += 3
			if current > 95 {
				current = 95
			}
		}
	}
	return current
}

func handleWSMessage(conn *websocket.Conn, msg []byte) {
	var msgMap map[string]interface{}
	if err := json.Unmarshal(msg, &msgMap); err != nil {
		return
	}
	msgType, _ := msgMap["type"].(string)
	switch msgType {
	case "hello":
		// Hello is already handled by the WS upgrade handler
		// This case is here to prevent falling through to default
		llog("debug", "Hello message passed to handleWSMessage (already processed)")
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
		safeWriteMessage(conn, websocket.TextMessage, reply)
	case "promote_to_server":
		if target, ok := msgMap["target"].(string); ok && target != "" {
			agentConnsMu.RLock()
			agentConn, exists := agentConns[target]
			agentConnsMu.RUnlock()
			if exists {
				forward, _ := json.Marshal(msgMap)
				safeWriteMessage(agentConn, websocket.TextMessage, forward)
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
			safeWriteMessage(conn, websocket.TextMessage, reply)
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
		webrtcManager.mu.Lock()
		connID := webrtcManager.wsToID[conn]
		webrtcManager.mu.Unlock()
		webrtcManager.HandleICE(connID, candidate)
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
	case "command_output", "dir_listing", "file_download", "upload_result":
		routeAgentResponse(msgMap)
	case "exec_command", "list_dir", "download_file", "upload_file":
		agentHandleTerminalMessage(msgMap)
	case "ping":
		ts, _ := msgMap["ts"].(float64)
		connID := ""
		agentConnsMu.RLock()
		agentIDForConn := connAgentID[conn]
		agentConnsMu.RUnlock()
		if agentIDForConn != "" {
			connID = agentIDForConn
		}
		_ = connID
		pongMsg, _ := json.Marshal(map[string]interface{}{
			"type": "pong",
			"ts":   int64(ts),
		})
		safeWriteMessage(conn, websocket.TextMessage, pongMsg)
	case "pong":
		ts, _ := msgMap["ts"].(float64)
		agentConnsMu.RLock()
		aid := connAgentID[conn]
		agentConnsMu.RUnlock()
		if aid != "" {
			handleAgentPong(aid, int64(ts))
		}
	default:
		llog("debug", "Received WebSocket message type=%s", msgType)
	}

}




func startGitHubLeaderElection() {
	// === PHASE 1: LAN discovery + election (works without internet) ===
	self := &PeerInfo{
		AgentID:  cfg.AgentID,
		Hostname: myHostname,
		IP:       getLocalIP(),
		Port:     cfg.ConfigPort,
		Mode:     "standalone",
		Version:  binaryVersion,
		Uptime:   time.Since(startTime).Seconds(),
	}
	globalDiscovery = NewPeerDiscovery(self)
	globalLAN = NewLANLeaderElection(globalDiscovery)

	lanLeaderCh := make(chan struct{})
	lanAgentCh := make(chan string, 1)

	globalLAN.SetCallbacks(
		func() {
			// LAN elected us as leader
			select {
			case <-lanLeaderCh:
			default:
				close(lanLeaderCh)
			}
		},
		func(serverURL string) {
			// LAN found a server or lost server
			select {
			case lanAgentCh <- serverURL:
			default:
			}
		},
	)

	lanCtx, lanCancel := context.WithCancel(context.Background())
	defer lanCancel()
	globalLAN.Start(lanCtx)

	// Wait up to 8 seconds for LAN election result
	select {
	case <-lanLeaderCh:
		llog("info", "LAN election: elected as server — starting server components")
		cfg.IsServerMode = true
		agentModeMu.Lock()
		agentMode = false
		agentModeMu.Unlock()
		globalDiscovery.UpdateSelf("server", true)
		runServerComponents()
		// After server stops, continue to re-elect
		llog("error", "Server stopped — re-electing")
		time.Sleep(jitterDuration(5, 10))
		// Fall through to full election loop
	case serverURL := <-lanAgentCh:
		llog("info", "LAN election: found server at %s — connecting as agent", serverURL)
		cfg.ServerURL = serverURL
		cfg.IsServerMode = false
		agentModeMu.Lock()
		agentMode = true
		agentModeMu.Unlock()
		globalDiscovery.UpdateSelf("agent", false)
		runAgentClient()
		llog("error", "Agent disconnected from LAN server — re-electing")
		time.Sleep(jitterDuration(5, 10))
		// Fall through to full election loop
	case <-time.After(8 * time.Second):
		llog("info", "LAN election: no result after 8s — proceeding to full election")
	}

	// === PHASE 2: Full election loop (LAN + relay + GitHub fallback) ===
	if cfg.GitHubRepo == "" || cfg.GitHubToken == "" {
		// No GitHub — keep trying: server_url → relay → LAN retry → eventually self-promote
		attempts := 0
		for {
			// 1. If we have a configured server URL, try connecting
			if cfg.ServerURL != "" && !cfg.IsServerMode {
				llog("info", "Connecting to configured server: %s", cfg.ServerURL)
				cfg.IsServerMode = false
				agentModeMu.Lock()
				agentMode = true
				agentModeMu.Unlock()
				globalDiscovery.UpdateSelf("agent", false)
				runAgentClient()
				llog("error", "Disconnected from %s — retrying", cfg.ServerURL)
				time.Sleep(5 * time.Second)
				continue
			}

			// 2. Try relay.recruitedge.us
			relayURL := "https://relay.recruitedge.us"
			llog("info", "Trying relay at %s (attempt %d)", relayURL, attempts+1)
			setElectionStatus("relay", "checking", "", "", time.Time{})
			if checkServerHealth(relayURL) {
				llog("info", "Relay reachable — connecting as agent")
				setElectionStatus("relay", "connected", "", relayURL, time.Now())
				cfg.ServerURL = relayURL
				cfg.IsServerMode = false
				agentModeMu.Lock()
				agentMode = true
				agentModeMu.Unlock()
				globalDiscovery.UpdateSelf("agent", false)
				runAgentClient()
				llog("error", "Disconnected from relay — retrying")
				time.Sleep(5 * time.Second)
				continue
			}

			attempts++

			// 3. After many failed attempts, become server (only ONE machine does this)
			if attempts >= 2 {
				// Check if ANY other machine on LAN is already a server
				serverPeer := globalDiscovery.GetServer()
				if serverPeer != nil && checkServerHealth(fmt.Sprintf("http://%s:%d", serverPeer.IP, serverPeer.Port)) {
					llog("info", "Found LAN server %s — connecting as agent", serverPeer.Hostname)
					cfg.ServerURL = fmt.Sprintf("http://%s:%d", serverPeer.IP, serverPeer.Port)
					cfg.IsServerMode = false
					agentModeMu.Lock()
					agentMode = true
					agentModeMu.Unlock()
					globalDiscovery.UpdateSelf("agent", false)
					runAgentClient()
					attempts = 0
					continue
				}

				// No server found anywhere — become server (highest uptime wins via discovery)
				llog("info", "No server found after %d attempts — becoming server", attempts)
				cfg.IsServerMode = true
				globalDiscovery.UpdateSelf("server", true)
				runServerComponents()
				// After server stops, restart election
				llog("error", "Server stopped — re-electing")
				attempts = 0
				time.Sleep(jitterDuration(5, 10))
				continue
			}

			// 4. Not yet time to self-promote — wait and retry
			llog("info", "No server found — retrying in 30s (attempt %d/2)", attempts)
			time.Sleep(30 * time.Second)
		}
	}

	// Election loop with GitHub — ANY machine can become server
	llog("info", "Starting GitHub leader election (repo=%s, token=%s)", cfg.GitHubRepo, maskToken(cfg.GitHubToken))
	for {
		cfg.IsServerMode = false
		agentModeMu.Lock()
		agentMode = false
		agentModeMu.Unlock()
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
						verifyReq, _ := http.NewRequest("GET", serverURL+"/api/system-info", nil)
						verifyReq.Header.Set("User-Agent", "PunMonitor-Election")
						verifyResp, verifyErr := httpClient.Do(verifyReq)
						if verifyErr == nil && verifyResp.StatusCode == 200 {
							var sysInfo map[string]string
							if json.NewDecoder(verifyResp.Body).Decode(&sysInfo) == nil {
								verifyResp.Body.Close()
								if sysInfo["hostname"] == myHostname || sysInfo["hostname"] == getHostname() {
									llog("info", "Detected server is ourselves (%s) – not connecting as agent", sysInfo["hostname"])
									continue
								}
							} else {
								verifyResp.Body.Close()
							}
						} else if verifyResp != nil {
							verifyResp.Body.Close()
						}
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
					agentModeMu.Lock()
					agentMode = true
					agentModeMu.Unlock()
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
			llog("info", "Elected as leader on %s — starting server + tunnel", myHostname)
			cfg.IsServerMode = true
			globalDiscovery.UpdateSelf("server", true)
			runServerComponents()
			llog("error", "Server stopped, re-electing after jitter")
			time.Sleep(jitterDuration(10, 20))
		} else {
			llog("info", "Not the leader – connecting as agent")
			agentModeMu.Lock()
			agentMode = true
			agentModeMu.Unlock()
			globalDiscovery.UpdateSelf("agent", false)
			runAgentClient()
			llog("error", "Agent disconnected, re-electing after jitter")
			time.Sleep(jitterDuration(5, 10))
		}
	}
}

func jitterDuration(minSec, maxSec int) time.Duration {
    return time.Duration(minSec+int(rand.Intn(maxSec-minSec+1))) * time.Second
}

func maskToken(t string) string {
	if len(t) <= 8 {
		return "***"
	}
	return t[:4] + "..." + t[len(t)-4:]
}

func compareVersions(a, b string) int {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	for len(pa) < len(pb) {
		pa = append(pa, "0")
	}
	for len(pb) < len(pa) {
		pb = append(pb, "0")
	}
	for i := range pa {
		ai, _ := strconv.Atoi(strings.TrimLeft(pa[i], "0"))
		bi, _ := strconv.Atoi(strings.TrimLeft(pb[i], "0"))
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return 0
}

func normalizeGitHubRepo(repo string) string {
    // Strip URL prefix if present — API expects "owner/repo" format
    repo = strings.TrimPrefix(repo, "https://github.com/")
    repo = strings.TrimPrefix(repo, "http://github.com/")
    repo = strings.TrimPrefix(repo, "github.com/")
    repo = strings.TrimSuffix(repo, ".git")
    return repo
}

func setElectionStatus(method, result, errStr, leaderID string, leaderUpdated time.Time) {
    globalElectionStatusMu.Lock()
    globalElectionStatus.Method = method
    globalElectionStatus.Configured = cfg.GitHubRepo != "" && cfg.GitHubToken != ""
    globalElectionStatus.Repo = cfg.GitHubRepo
    globalElectionStatus.LeaderID = leaderID
    globalElectionStatus.LeaderUpdated = leaderUpdated
    globalElectionStatus.SelfIsLeader = leaderID != "" && leaderID == cfg.AgentID
    if !leaderUpdated.IsZero() {
        globalElectionStatus.LeaderStale = time.Since(leaderUpdated) > electionInterval
    } else {
        globalElectionStatus.LeaderStale = false
    }
    globalElectionStatus.LastCheck = time.Now()
    globalElectionStatus.LastResult = result
    globalElectionStatus.LastError = errStr
    globalElectionStatus.CheckCount++
    if method == "relay" {
        globalElectionStatus.FallbackServer = "https://relay.recruitedge.us"
    }
    // Snapshot for the history row (still under the lock so the values are consistent)
    ev := ElectionEvent{
        Timestamp:    globalElectionStatus.LastCheck,
        Action:       result,
        Method:       method,
        AgentID:      cfg.AgentID,
        Hostname:     getHostname(),
        LeaderID:     leaderID,
        LeaderAgeMS:  time.Since(leaderUpdated).Milliseconds(),
        Result:       result,
        Error:        errStr,
    }
    globalElectionStatusMu.Unlock()
    appendElectionEvent(ev)
}

func tryClaimLeadership() (bool, error) {
    // Use GitHub API (not raw) to get the SHA for potential takeover
    apiURL := fmt.Sprintf("https://api.github.com/repos/%s/contents/primary_server.json", normalizeGitHubRepo(cfg.GitHubRepo))
    req, _ := http.NewRequest("GET", apiURL, nil)
    req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
    req.Header.Set("Accept", "application/vnd.github.v3+json")
    resp, err := httpFastClient.Do(req)
    if err != nil {
        setElectionStatus("github", "error", err.Error(), "", time.Time{})
        return false, fmt.Errorf("failed to read primary_server.json: %w", err)
    }
    defer resp.Body.Close()

    llog("debug", "Election: GET %s returned %d", apiURL, resp.StatusCode)

    if resp.StatusCode == http.StatusNotFound {
        llog("info", "No primary_server.json found, attempting to claim leadership")
        ok, werr := writePrimaryServerFile(cfg.AgentID, "")
        if werr != nil {
            setElectionStatus("github", "error", werr.Error(), "", time.Time{})
        } else {
            setElectionStatus("github", "claimed", "", cfg.AgentID, time.Now())
        }
        return ok, werr
    }

    if resp.StatusCode != http.StatusOK {
        setElectionStatus("github", "error", fmt.Sprintf("GitHub API %d", resp.StatusCode), "", time.Time{})
        return false, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
    }

    var ghResp struct {
        Content string `json:"content"`
        SHA     string `json:"sha"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&ghResp); err != nil {
        setElectionStatus("github", "error", err.Error(), "", time.Time{})
        return false, fmt.Errorf("failed to parse response: %w", err)
    }

    body, err := base64.StdEncoding.DecodeString(ghResp.Content)
    if err != nil {
        setElectionStatus("github", "error", err.Error(), "", time.Time{})
        return false, fmt.Errorf("failed to decode content: %w", err)
    }

    var primary struct {
        Host    string `json:"host"`
        Updated int64  `json:"updated"`
    }
    if err := json.Unmarshal(body, &primary); err != nil {
        setElectionStatus("github", "error", err.Error(), "", time.Time{})
        return false, fmt.Errorf("failed to parse primary file: %w", err)
    }

    leaderTime := time.UnixMilli(primary.Updated)
    interval := getElectionInterval()
    if primary.Host == cfg.AgentID {
        llog("info", "Already the leader, renewing leadership")
        ok, werr := writePrimaryServerFile(cfg.AgentID, ghResp.SHA)
        if werr != nil {
            setElectionStatus("github", "error", werr.Error(), primary.Host, leaderTime)
        } else {
            setElectionStatus("github", "renewed", "", primary.Host, time.Now())
        }
        return ok, werr
    }

    if time.Since(leaderTime) > interval {
        llog("info", "Leader %s is stale, attempting to take over", primary.Host)
        ok, werr := writePrimaryServerFile(cfg.AgentID, ghResp.SHA)
        if werr != nil {
            setElectionStatus("github", "error", werr.Error(), primary.Host, leaderTime)
        } else {
            setElectionStatus("github", "stale-takeover", "", cfg.AgentID, time.Now())
        }
        return ok, werr
    }

    llog("info", "Active leader: %s (updated %s ago)", primary.Host, time.Since(leaderTime))
    setElectionStatus("github", "active", "", primary.Host, leaderTime)
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
    apiURL := fmt.Sprintf("https://api.github.com/repos/%s/contents/primary_server.json", normalizeGitHubRepo(cfg.GitHubRepo))
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
    // Read response body for diagnostic detail
    bodyBytes, _ := io.ReadAll(resp.Body)
    bodyStr := strings.TrimSpace(string(bodyBytes))
    if len(bodyStr) > 500 {
        bodyStr = bodyStr[:500] + "..."
    }
    return false, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, bodyStr)
}

func renewLeadership() (bool, error) {
    apiURL := fmt.Sprintf("https://api.github.com/repos/%s/contents/primary_server.json", normalizeGitHubRepo(cfg.GitHubRepo))
    req, _ := http.NewRequest("GET", apiURL, nil)
    req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
    req.Header.Set("Accept", "application/vnd.github.v3+json")
    resp, err := httpFastClient.Do(req)
    if err != nil {
        setElectionStatus("github", "error", err.Error(), "", time.Time{})
        return false, err
    }
    defer resp.Body.Close()
    if resp.StatusCode == http.StatusNotFound {
        llog("info", "No primary_server.json found, claiming leadership")
        ok, werr := writePrimaryServerFile(cfg.AgentID, "")
        if werr != nil {
            setElectionStatus("github", "error", werr.Error(), "", time.Time{})
        } else {
            setElectionStatus("github", "claimed", "", cfg.AgentID, time.Now())
        }
        return ok, werr
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
        llog("warn", "Leader renewed by another host %s – checking if it's actually different", primary.Host)
        // Don't step down if the "other" leader is actually us behind a tunnel
        // Check by comparing the leader's reported hostname against our own
        if primary.Host == myHostname || primary.Host == getHostname() {
            llog("info", "Leader %s is actually us (hostname match) – renewing leadership", primary.Host)
            return writePrimaryServerFile(cfg.AgentID, ghResp.SHA)
        }
        // Different host claimed leadership — check if it's actually serving
        // Only check a DIRECT connection to that host, not via tunnel (which might loop back to us)
        checkURL := "http://" + primary.Host + ":8080/api/health"
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
            llog("info", "New leader %s is actually serving – stepping down gracefully", primary.Host)
            cfg.IsServerMode = false
            agentModeMu.Lock()
            agentMode = true
            agentModeMu.Unlock()
            if serverCancel != nil { serverCancel() }
        } else {
            llog("info", "New leader %s not reachable – reclaiming leadership", primary.Host)
            return writePrimaryServerFile(cfg.AgentID, ghResp.SHA)
        }
        return true, nil
    }
    ok, werr := writePrimaryServerFile(cfg.AgentID, ghResp.SHA)
    if werr != nil {
        setElectionStatus("github", "error", werr.Error(), primary.Host, time.UnixMilli(primary.Updated))
    } else {
        setElectionStatus("github", "renewed", "", primary.Host, time.Now())
    }
    return ok, werr
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
	go startServerLoadMonitor()
	if err := setupSSHServer(); err != nil {
		llog("error", "SSH server failed to start: %v", err)
	} else {
		defer stopSSHServer()
	}
	startDailyReportPusher()
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
		ticker := time.NewTicker(5 * time.Minute)
		for {
			select {
			case <-ticker.C:
				llog("info", "Syncing settings from GitHub...")
				syncFromGitHub()
			}
		}
	}()

	// Auto-deploy: when we become server, scan for peers and deploy PunMonitor
	go safeRun("auto-deploy", func() {
		time.Sleep(10 * time.Second)
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if globalDiscovery != nil && HasDeployCredentials() {
					peers := globalDiscovery.GetPeers()
					for _, peer := range peers {
						go autoDeployToPeer(peer)
					}
				}
			case <-serverCtx.Done():
				return
			}
		}
	})

	// Periodic: broadcast pending update URL to all agents (every 30s).
	// setupAutostart is NOT called here — it's only needed at startup and on
	// binary relocate. Calling schtasks every 2 min caused cmd flashes on
	// non-admin systems.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		for range ticker.C {
			if cfg.UpdateURL == "" {
				continue
			}
			updateMsg, _ := json.Marshal(map[string]string{
				"type": "update",
				"url":  cfg.UpdateURL,
			})
			connAgentIDMu.RLock()
			wsClients.Range(func(key, value interface{}) bool {
				conn := key.(*websocket.Conn)
				if _, isAgent := connAgentID[conn]; isAgent {
					safeWriteMessage(conn, websocket.TextMessage, updateMsg)
				}
				return true
			})
			connAgentIDMu.RUnlock()
		}
	}()

	llog("info", "Server components started – blocking until cancelled")
	<-serverCtx.Done()
	llog("info", "Server components stopped")
}

func startHTTPServer() {
	// Always use embedded dashboard as the source of truth
	// Disk version is only used as a development override if explicitly present
	dashboardContent = dashboardHTML
	dashDiskPath := filepath.Join(dataDir(), "dashboard.html")
	if data, err := os.ReadFile(dashDiskPath); err == nil {
		// Only use disk if it contains a flag indicating custom code
		if strings.Contains(string(data), "<!-- CUSTOM DASHBOARD -->") {
			dashboardContent = string(data)
			llog("info", "Dashboard loaded from disk (custom) (%d bytes)", len(dashboardContent))
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
							if data, err := os.ReadFile(dashDiskPath); err == nil && strings.Contains(string(data), "<!-- CUSTOM DASHBOARD -->") {
								dashboardContent = string(data)
								llog("info", "Dashboard hot-reloaded (%d bytes)", len(dashboardContent))
							}
						}
					}
				}
			}()
		} else {
			llog("info", "Dashboard on disk ignored (missing custom flag). Using embedded version.")
		}
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
				"turn_server_url":           cfg.TurnServerURL,
				"turn_server_credential":    cfg.TurnServerCredential,
				"capture_schedule":          cfg.CaptureSchedule,
				"capture_days":              cfg.CaptureDays,
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
			cfg.CaptureSchedule = s.CaptureSchedule
			cfg.CaptureDays = s.CaptureDays
			saveSettings()
			pushCredsToGitHub()
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}
	})

	http.HandleFunc("/api/system-info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mode := "standalone"
		agentModeMu.RLock()
		isAgent := agentMode
		agentModeMu.RUnlock()
		if cfg.IsServerMode && isAgent {
			mode = "server+agent"
		} else if cfg.IsServerMode {
			mode = "server"
		} else if isAgent {
			mode = "agent"
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
			"agent_id": cfg.AgentID,
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
			"Agent ID", "Hostname", "Local IP", "WAN IP",
			"OS", "Architecture", "Binary Version", "Mode",
			"Transport", "Health", "Latency (ms)", "Jitter (ms)", "Packet Loss %",
			"Reconnections", "Bytes Received", "Frames Received", "Frames/sec",
			"Uptime", "Start Time", "Boot Time", "Wake Up Time",
			"Total Idle Time",
			"Report Date", "Report Time",
		})
		// Server's own row
		{
			uptimeSecs := int64(time.Since(startTime).Seconds())
			uptimeStr := fmt.Sprintf("%dh %dm %ds", uptimeSecs/3600, (uptimeSecs%3600)/60, uptimeSecs%60)
			bootTime := "—"
			totalIdle := "—"
			if globalActivity != nil {
				s := globalActivity.Summary()
				bootTime = s["boot_time"]
				totalIdle = s["total_idle"]
			}
			wakeUpTime := "—"
			if !startTime.IsZero() {
				wakeUpTime = startTime.Format("2006-01-02 15:04:05")
			}
			writer.Write([]string{
				cfg.AgentID, hostname, getLocalIP(), getWANIP(),
				runtime.GOOS, runtime.GOARCH, binaryVersion, "server",
				"local", "n/a", "0", "0", "0",
				"0", "0", "0", "0",
				uptimeStr, startTime.Format("2006-01-02 15:04:05"),
				bootTime, wakeUpTime, totalIdle,
				time.Now().Format("2006-01-02"), time.Now().Format("15:04:05"),
			})
		}
		// Per-agent rows from agentSystemInfo + agentStats
		agentSystemInfo.Range(func(key, value interface{}) bool {
			aid, ok := key.(string)
			if !ok { return true }
			info, _ := value.(map[string]interface{})
			if info == nil { return true }
			getStr := func(field string) string {
				if v, ok := info[field].(string); ok { return v }
				return "—"
			}
			transport := "—"
			health := "—"
			latency := "0"
			jitter := "0"
			pktLoss := "0"
			reconn := "0"
			bytesRec := "0"
			framesRec := "0"
			fps := "0"
			if v, ok := agentStats.Load(aid); ok {
				s := v.(*AgentStats)
				snap := s.Snapshot()
				if t, ok := snap["transport"].(string); ok { transport = t }
				if h, ok := snap["health"].(string); ok { health = h }
				if l, ok := snap["latency_ms"].(float64); ok { latency = fmt.Sprintf("%.1f", l) }
				if j, ok := snap["bytes_per_sec"].(float64); ok { jitter = fmt.Sprintf("%.1f", j) }
				if f, ok := snap["frames_per_sec"].(float64); ok { fps = fmt.Sprintf("%.1f", f) }
				bytesRec = fmt.Sprintf("%d", snap["bytes_received"])
				framesRec = fmt.Sprintf("%d", snap["frames_received"])
			}
			writer.Write([]string{
				aid,
				getStr("hostname"),
				getStr("local_ip"),
				getStr("wan_ip"),
				getStr("os"),
				getStr("arch"),
				getStr("version"),
				"agent",
				transport, health, latency, jitter, pktLoss,
				reconn, bytesRec, framesRec, fps,
				getStr("uptime"),
				getStr("start_time"),
				getStr("boot_time"),
				getStr("wake_up_time"),
				getStr("idle_time"),
				time.Now().Format("2006-01-02"), time.Now().Format("15:04:05"),
			})
			return true
		})
		// Also include known (disconnected) agents
		knownAgents.Range(func(key, value interface{}) bool {
			aid, ok := key.(string)
			if !ok { return true }
			if _, exists := agentSystemInfo.Load(aid); exists { return true }
			info, _ := value.(map[string]interface{})
			if info == nil { return true }
			getStr := func(field string) string {
				if v, ok := info[field]; ok {
					if s, ok := v.(string); ok { return s }
				}
				return "—"
			}
			writer.Write([]string{
				aid, getStr("hostname"), getStr("local_ip"), getStr("wan_ip"),
				getStr("os"), "—", getStr("version"), "offline",
				"—", "offline", "—", "—", "—",
				"—", "—", "—", "—",
				"—", "—", "—", "—", "—",
				time.Now().Format("2006-01-02"), time.Now().Format("15:04:05"),
			})
			return true
		})
		// Audit log entries as separate section
		if globalAudit != nil {
			writer.Write([]string{})
			writer.Write([]string{"=== AUDIT LOG ==="})
			writer.Write([]string{"Timestamp", "Action", "Agent ID", "User", "Detail"})
			for _, e := range globalAudit.Recent(500) {
				ts := time.UnixMilli(e.Timestamp).Format("2006-01-02 15:04:05")
				writer.Write([]string{ts, e.Action, e.AgentID, e.User, e.Detail})
			}
		}
		writer.Flush()
	})

	http.HandleFunc("/api/report.xlsx", handleReportXLSX)
	http.HandleFunc("/api/report.xls", handleReportXLSX)

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
		var result []agentInfo
		knownAgents.Range(func(key, value interface{}) bool {
			id, ok := key.(string)
			if !ok { return true }
			info, ok := value.(map[string]interface{})
			if !ok { return true }
			a := agentInfo{
				ID:         id,
				Connected:  connected[id],
				Registered: true,
			}
			if ls, ok := info["last_seen"].(float64); ok { a.LastSeen = int64(ls) }
			if h, ok := info["hostname"].(string); ok { a.Hostname = h }
			if ip, ok := info["local_ip"].(string); ok { a.LocalIP = ip }
			if ip, ok := info["wan_ip"].(string); ok { a.WANIP = ip }
			if o, ok := info["os"].(string); ok { a.OS = o }
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
		RecordAudit("promote_to_server", cfg.AgentID, "admin", "")
		if serverCancel != nil { serverCancel() }
		serverCtx, serverCancel = context.WithCancel(context.Background())
		go startScreenCapture(serverCtx)
		globalDiscovery.UpdateSelf("server", true)

		// Notify all connected agents
		notifyMsg, _ := json.Marshal(map[string]interface{}{
			"type": "server_changed",
			"server_url": fmt.Sprintf("http://%s:%d", getLocalIP(), cfg.ConfigPort),
			"message": "This machine has been promoted to server. All agents should reconnect.",
		})
		connAgentIDMu.RLock()
		wsClients.Range(func(key, value interface{}) bool {
			conn := key.(*websocket.Conn)
			if _, isAgent := connAgentID[conn]; isAgent {
				safeWriteMessage(conn, websocket.TextMessage, notifyMsg)
			}
			return true
		})
		connAgentIDMu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "mode": "server", "msg": "Promoted to server. All agents notified."})
	})

	http.HandleFunc("/api/transport-status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(getTransportStatus())
	})

	http.HandleFunc("/api/server-load", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(globalServerLoad.Snapshot())
	})

	http.HandleFunc("/api/agent-stats", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		stats := make(map[string]interface{})
		agentStats.Range(func(key, value interface{}) bool {
			aid, ok := key.(string)
			if !ok { return true }
			s := value.(*AgentStats)
			s.UpdateHealth()
			stats[aid] = s.Snapshot()
			return true
		})
		json.NewEncoder(w).Encode(stats)
	})

	http.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"version": binaryVersion})
	})

	http.HandleFunc("/api/ssh-info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		port := cfg.SSHPort
		if port == 0 {
			port = 2222
		}
		// Determine the public-facing host (tunnel or local IP)
		host := "localhost"
		if h := cfg.TunnelHostname; h != "" && (cfg.TunnelProvider == "cloudflare" || cfg.TunnelProvider == "direct") {
			host = h
		} else {
			if ip := getLocalIP(); ip != "" {
				host = ip
			}
		}
		cmd := fmt.Sprintf("ssh -p %d %s@%s", port, cfg.SSHUsername, host)
		sftpCmd := fmt.Sprintf("sftp -P %d %s@%s", port, cfg.SSHUsername, host)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled":     cfg.SSHEnabled && sshServer != nil,
			"port":        port,
			"host":        host,
			"user":        cfg.SSHUsername,
			"password":    cfg.SSHPassword,
			"fingerprint": sshFingerprint,
			"ssh_cmd":     cmd,
			"sftp_cmd":    sftpCmd,
			"features":    []string{"shell", "exec", "sftp", "port-forwarding", "reverse-tunnel", "public-key", "password"},
		})
	})

	http.HandleFunc("/api/election-status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		globalElectionStatusMu.RLock()
		status := globalElectionStatus
		globalElectionStatusMu.RUnlock()
		status.Configured = cfg.GitHubRepo != "" && cfg.GitHubToken != ""
		status.Repo = cfg.GitHubRepo
		status.SelfIsLeader = status.LeaderID != "" && status.LeaderID == cfg.AgentID
		if !status.LeaderUpdated.IsZero() {
			status.LeaderStale = time.Since(status.LeaderUpdated) > electionInterval
		}
		json.NewEncoder(w).Encode(status)
	})

	// /api/election-history — returns the full event log as JSON (newest last)
	http.HandleFunc("/api/election-history", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"events":    getElectionHistory(),
			"count":     len(getElectionHistory()),
			"last_push": lastDailyReportStatus(),
		})
	})

	// /api/report/status — returns the last daily report push status
	http.HandleFunc("/api/report/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		dailyReportPushMu.Lock()
		pushed := lastDailyReportPushed
		state := dailyReportPusherState
		dailyReportPushMu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"last_pushed_file": pushed,
			"status":           state,
			"next_push_at":     time.Now().Add(time.Until(nextMidnight())),
		})
	})

	// /api/report/push — POST to manually trigger a daily report push
	http.HandleFunc("/api/report/push", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		filename, msg, err := pushDailyReportToGitHub()
		dailyReportPushMu.Lock()
		if err != nil {
			dailyReportPusherState = "ERROR " + time.Now().Format(time.RFC3339) + ": " + err.Error()
		} else {
			dailyReportPusherState = "OK " + time.Now().Format(time.RFC3339) + ": " + msg
			lastDailyReportPushed = filename
		}
		dailyReportPushMu.Unlock()
		resp := map[string]interface{}{
			"filename":    filename,
			"message":     msg,
			"event_count": len(getElectionHistory()),
		}
		if err != nil {
			resp["error"] = err.Error()
			resp["ok"] = false
		} else {
			resp["ok"] = true
		}
		json.NewEncoder(w).Encode(resp)
	})

	http.HandleFunc("/api/setup-status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		needsSetup := cfg.ServerURL == "" && cfg.GitHubRepo == ""
		json.NewEncoder(w).Encode(map[string]interface{}{
			"needs_setup":  needsSetup,
			"server_url":   cfg.ServerURL,
			"auth_user":    cfg.AuthUser,
			"has_auth":     cfg.AuthUser != "",
			"has_tunnel":   cfg.CloudflareTunnelID != "",
			"is_server":    cfg.IsServerMode,
			"agent_id":     cfg.AgentID,
			"hostname":     getHostname(),
		})
	})

	http.HandleFunc("/api/setup", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			ServerURL       string `json:"server_url"`
			AuthUser        string `json:"auth_user"`
			AuthPass        string `json:"auth_pass"`
			MaxFPS          float64 `json:"max_fps"`
			TunnelHostname  string `json:"tunnel_hostname"`
			CloudflareID    string `json:"cloudflare_tunnel_id"`
			CloudflareSecret string `json:"cloudflare_tunnel_secret"`
			CloudflareTag   string `json:"cloudflare_account_tag"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ServerURL != "" {
			cfg.ServerURL = req.ServerURL
		}
		if req.AuthUser != "" {
			cfg.AuthUser = req.AuthUser
			cfg.AuthPass = req.AuthPass
		}
		if req.MaxFPS > 0 {
			cfg.MaxFPS = req.MaxFPS
		} else {
			cfg.MaxFPS = 1.0
		}
		if req.TunnelHostname != "" {
			cfg.TunnelHostname = req.TunnelHostname
		}
		if req.CloudflareID != "" {
			cfg.CloudflareTunnelID = req.CloudflareID
			cfg.CloudflareTunnelSecret = req.CloudflareSecret
			cfg.CloudflareAccountTag = req.CloudflareTag
		}
		cfg.ConfigPort = 8080
		saveSettings()
		RecordAudit("setup_complete", cfg.AgentID, "admin", req.ServerURL)
		llog("info", "Setup completed — server_url=%s auth=%s", cfg.ServerURL, cfg.AuthUser)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "msg": "Setup complete. Restart the application to apply changes."})
	})

	http.HandleFunc("/download/", func(w http.ResponseWriter, r *http.Request) {
		exePath, err := os.Executable()
		if err != nil {
			http.Error(w, "cannot locate binary", http.StatusInternalServerError)
			return
		}
		permPath := filepath.Join(binDir(), filepath.Base(exePath))
		if _, err := os.Stat(permPath); err == nil {
			exePath = permPath
		}
		w.Header().Set("Content-Disposition", "attachment; filename=\"PunMonitor.exe\"")
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, exePath)
	})

	http.HandleFunc("/api/audit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if globalAudit == nil {
			json.NewEncoder(w).Encode([]AuditEntry{})
			return
		}
		json.NewEncoder(w).Encode(globalAudit.Recent(200))
	})

	http.HandleFunc("/api/terminal", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			ID      string `json:"id"`
			Command string `json:"command"`
			AgentID string `json:"agentId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		RecordAudit("terminal_exec", req.AgentID, "dashboard", truncateForAudit(req.Command, 200))
		handleTerminalCommand(req.ID, req.Command, req.AgentID, "dashboard")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	http.HandleFunc("/api/files/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			ID      string `json:"id"`
			Path    string `json:"path"`
			AgentID string `json:"agentId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		RecordAudit("file_browse", req.AgentID, "dashboard", truncateForAudit(req.Path, 200))
		handleDirListRequest(req.ID, req.Path, req.AgentID, "dashboard")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	http.HandleFunc("/api/files/download", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			ID      string `json:"id"`
			Path    string `json:"path"`
			AgentID string `json:"agentId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		RecordAudit("file_download", req.AgentID, "dashboard", truncateForAudit(req.Path, 200))
		handleFileDownloadRequest(req.ID, req.Path, req.AgentID, "dashboard")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	http.HandleFunc("/api/sync", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		llog("info", "Manual sync triggered from dashboard")
		syncFromGitHub()
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	http.HandleFunc("/api/deploy-credentials", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			creds := GetDeployCredentials()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"configured": creds.Username != "",
				"username":   creds.Username,
				"domain":     creds.Domain,
			})
			return
		}
		if r.Method == "POST" {
			var req struct {
				Username string `json:"username"`
				Password string `json:"password"`
				Domain   string `json:"domain"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			SetDeployCredentials(req.Username, req.Password, req.Domain)
			llog("info", "Deploy credentials configured for user %s", req.Username)
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	})

	http.HandleFunc("/api/deploy", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if !HasDeployCredentials() {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":  "error",
				"message": "No deploy credentials configured. Set them via /api/deploy-credentials first.",
			})
			return
		}
		go runDeployment()
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "msg": "Deployment started — check logs for progress"})
	})

	http.HandleFunc("/api/migrate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			ServerURL string `json:"server_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ServerURL == "" {
			http.Error(w, "server_url required", http.StatusBadRequest)
			return
		}
		cfg.ServerURL = req.ServerURL
		saveSettings()
		RecordAudit("server_migrate", "", "dashboard", req.ServerURL)
		migrateMsg, _ := json.Marshal(map[string]interface{}{
			"type":      "migrate",
			"server_url": req.ServerURL,
		})
		connAgentIDMu.RLock()
		wsClients.Range(func(key, value interface{}) bool {
			conn := key.(*websocket.Conn)
			if _, isAgent := connAgentID[conn]; isAgent {
				safeWriteMessage(conn, websocket.TextMessage, migrateMsg)
			}
			return true
		})
		connAgentIDMu.RUnlock()
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "msg": "Migration command sent to all agents"})
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

	http.HandleFunc("/api/check-update", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if cfg.GitHubRepo == "" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"available": false,
				"reason":    "GitHub repo not configured",
			})
			return
		}
		apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", normalizeGitHubRepo(cfg.GitHubRepo))
		req, _ := http.NewRequest("GET", apiURL, nil)
		if cfg.GitHubToken != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
		}
		req.Header.Set("Accept", "application/vnd.github.v3+json")
		req.Header.Set("User-Agent", "PunMonitor")
		client := &http.Client{Timeout: 15 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"available": false,
				"reason":    "GitHub API unreachable: " + err.Error(),
			})
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode == 404 {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"available": false,
				"reason":    "No releases published yet",
			})
			return
		}
		if resp.StatusCode != 200 {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"available": false,
				"reason":    fmt.Sprintf("GitHub API %d", resp.StatusCode),
			})
			return
		}
		var release struct {
			TagName     string `json:"tag_name"`
			Name        string `json:"name"`
			HTMLURL     string `json:"html_url"`
			PublishedAt string `json:"published_at"`
			Body        string `json:"body"`
			Assets      []struct {
				Name               string `json:"name"`
				BrowserDownloadURL string `json:"browser_download_url"`
				Size               int64  `json:"size"`
			} `json:"assets"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"available": false,
				"reason":    "Bad response: " + err.Error(),
			})
			return
		}
		// Pick the right asset for this platform
		assetName := "PunMonitor.exe"
		if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
			assetName = "punmonitor"
		}
		var downloadURL string
		var sizeBytes int64
		var allAssets []map[string]interface{}
		for _, a := range release.Assets {
			allAssets = append(allAssets, map[string]interface{}{
				"name": a.Name, "size": a.Size, "url": a.BrowserDownloadURL,
			})
			if a.Name == assetName {
				downloadURL = a.BrowserDownloadURL
				sizeBytes = a.Size
			}
		}
		if downloadURL == "" {
			// Fall back to first binary-looking asset
			for _, a := range release.Assets {
				if strings.HasSuffix(a.Name, ".exe") || strings.HasSuffix(a.Name, "punmonitor") {
					downloadURL = a.BrowserDownloadURL
					sizeBytes = a.Size
					break
				}
			}
		}
		currentVersion := binaryVersion
		latestVersion := strings.TrimPrefix(release.TagName, "v")
		isNewer := compareVersions(latestVersion, currentVersion) > 0
		json.NewEncoder(w).Encode(map[string]interface{}{
			"available":       downloadURL != "" && isNewer,
			"current_version": currentVersion,
			"latest_version":  latestVersion,
			"tag":             release.TagName,
			"name":            release.Name,
			"html_url":        release.HTMLURL,
			"published_at":    release.PublishedAt,
			"size_bytes":      sizeBytes,
			"size_mb":         float64(sizeBytes) / (1024 * 1024),
			"download_url":    downloadURL,
			"assets":          allAssets,
			"notes":           truncateForAudit(release.Body, 500),
		})
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
				safeWriteMessage(conn, websocket.TextMessage, agentUpdateMsg)
			}
			return true
		})
		connAgentIDMu.RUnlock()
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "msg": "Update sent to server + agents"})
	})

	// --- Assist: browser-based remote screen sharing ---

	http.HandleFunc("/assist/new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		id := randomString(8)
		assistSessionsMu.Lock()
		assistSessions[id] = &AssistSession{
			ID:        id,
			CreatedAt: time.Now(),
			Active:    true,
		}
		assistSessionsMu.Unlock()
		RecordAudit("assist_created", "", "admin", id)
		json.NewEncoder(w).Encode(map[string]string{
			"id":  id,
			"url": fmt.Sprintf("/assist/%s", id),
		})
	})

	http.HandleFunc("/assist/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assistSessionsMu.RLock()
		var list []map[string]interface{}
		for _, s := range assistSessions {
			list = append(list, map[string]interface{}{
				"id":        s.ID,
				"active":    s.Active,
				"createdAt": s.CreatedAt.UnixMilli(),
				"hasUser":   s.UserConn != nil,
				"hasAdmin":  s.AdminConn != nil,
			})
		}
		assistSessionsMu.RUnlock()
		if list == nil {
			list = []map[string]interface{}{}
		}
		json.NewEncoder(w).Encode(list)
	})

	http.HandleFunc("/api/assist-close", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			SessionID string `json:"session_id"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		if req.SessionID == "" {
			http.Error(w, "session_id required", http.StatusBadRequest)
			return
		}
		assistSessionsMu.Lock()
		s, ok := assistSessions[req.SessionID]
		if ok {
			if s.UserConn != nil { _ = s.UserConn.Close() }
			if s.AdminConn != nil { _ = s.AdminConn.Close() }
			delete(assistSessions, req.SessionID)
		}
		assistSessionsMu.Unlock()
		RecordAudit("assist_closed", "", "admin", req.SessionID)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	http.HandleFunc("/assist/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/assist/")
		if id == "" || id == "new" || id == "list" || id == "ws" {
			return
		}
		assistPath := filepath.Join(filepath.Dir(os.Args[0]), "assist.html")
		if data, err := os.ReadFile(assistPath); err == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(data)
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, "<html><body><h2>Assist Session %s</h2><p>assist.html not found.</p></body></html>", id)
		}
	})

	http.HandleFunc("/view/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/view/")
		if id == "" {
			return
		}
		viewerPath := filepath.Join(filepath.Dir(os.Args[0]), "viewer.html")
		if data, err := os.ReadFile(viewerPath); err == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(data)
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, "<html><body><h2>Viewer for %s</h2><p>viewer.html not found.</p></body></html>", id)
		}
	})

	http.HandleFunc("/assist-ws/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/assist-ws/")
		if id == "" {
			return
		}
		upgrader := websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		assistSessionsMu.Lock()
		session, exists := assistSessions[id]
		if !exists {
			session = &AssistSession{ID: id, CreatedAt: time.Now(), Active: true}
			assistSessions[id] = session
		}
		assistSessionsMu.Unlock()

		_, firstMsg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var hello map[string]interface{}
		json.Unmarshal(firstMsg, &hello)

		isAdmin := hello["type"] == "assist_admin_view"
		if isAdmin {
			assistSessionsMu.Lock()
			session.AdminConn = conn
			assistSessionsMu.Unlock()
			llog("info", "Assist: admin viewing session %s", id)
			if session.UserConn != nil {
				joinMsg, _ := json.Marshal(map[string]string{"type": "assist_viewer_joined"})
				safeWriteMessage(session.UserConn, websocket.TextMessage, joinMsg)
			}
			RecordAudit("assist_view", id, "admin", "")
		} else {
			assistSessionsMu.Lock()
			session.UserConn = conn
			assistSessionsMu.Unlock()
			llog("info", "Assist: user joined session %s", id)
		}

		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				if isAdmin && session.UserConn != nil {
					leftMsg, _ := json.Marshal(map[string]string{"type": "assist_viewer_left"})
					safeWriteMessage(session.UserConn, websocket.TextMessage, leftMsg)
				}
				if !isAdmin {
					assistSessionsMu.Lock()
					session.Active = false
					assistSessionsMu.Unlock()
				}
				return
			}
			var msgMap map[string]interface{}
			json.Unmarshal(msg, &msgMap)

			msgType, _ := msgMap["type"].(string)
			switch msgType {
			case "assist_frame":
				// Forward frame from user to admin
				if session.AdminConn != nil {
					safeWriteMessage(session.AdminConn, websocket.TextMessage, msg)
				}
			case "assist_chat":
				// Forward chat both ways
				if isAdmin && session.UserConn != nil {
					safeWriteMessage(session.UserConn, websocket.TextMessage, msg)
				} else if !isAdmin && session.AdminConn != nil {
					safeWriteMessage(session.AdminConn, websocket.TextMessage, msg)
				}
			case "assist_mouse", "assist_key":
				// Forward control commands from admin to user
				if session.UserConn != nil {
					safeWriteMessage(session.UserConn, websocket.TextMessage, msg)
				}
			case "assist_file":
				// Forward file data both ways
				if isAdmin && session.UserConn != nil {
					safeWriteMessage(session.UserConn, websocket.TextMessage, msg)
				} else if !isAdmin && session.AdminConn != nil {
					safeWriteMessage(session.AdminConn, websocket.TextMessage, msg)
				}
			}
		}
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
					stats := NewAgentStats()
					stats.Transport = "websocket"
					agentStats.Store(agentID, stats)
					go startAgentPingLoop(agentID)
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
							safeWriteMessage(conn, websocket.TextMessage, updateMsg)
						}
					}
				}
			} else {
				// Dashboard client connected – send agent list + status immediately
				sendAgentListToWS(conn)
				sendStatusToWS(conn)
			}
			// Do NOT call handleWSMessage for hello — already handled above
		} else {
			// Non-hello messages: delegate to handleWSMessage
			handleWSMessage(conn, msg)
		}
	}
		}
	})

	addr := fmt.Sprintf(":%d", cfg.ConfigPort)
	httpsAddr := fmt.Sprintf(":%d", cfg.ConfigPort+443)

	go func() {
		certFile, keyFile, err := ensureTLSCert()
		if err != nil {
			llog("info", "TLS cert generation failed: %v — HTTPS disabled", err)
			return
		}
		tlsCfg, err := createTLSConfig(certFile, keyFile)
		if err != nil {
			llog("info", "TLS config failed: %v — HTTPS disabled", err)
			return
		}
		srv := &http.Server{Addr: httpsAddr, Handler: nil, TLSConfig: tlsCfg}
		llog("info", "Starting HTTPS server on %s", httpsAddr)
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			llog("warn", "HTTPS server failed: %v", err)
		}
	}()

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
		url := fmt.Sprintf("https://api.github.com/repos/%s/contents/punmonitor-credentials.json", normalizeGitHubRepo(cfg.GitHubRepo))
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
		url := fmt.Sprintf("https://api.github.com/repos/%s/contents/settings.json", normalizeGitHubRepo(cfg.GitHubRepo))
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
	rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/main/punmonitor-credentials.json", normalizeGitHubRepo(cfg.GitHubRepo))
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
	settingsURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/main/settings.json", normalizeGitHubRepo(cfg.GitHubRepo))
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
						safeWriteMessage(conn, websocket.TextMessage, updateMsg)
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
	dashURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/main/dashboard.html", normalizeGitHubRepo(cfg.GitHubRepo))
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

func runPull(serverURL string) {
	fmt.Println("PunMonitor Installer")
	fmt.Println("====================")
	fmt.Printf("Server: %s\n", serverURL)

	// Step 1: Check server is reachable
	fmt.Print("Checking server... ")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(serverURL + "/api/health")
	if err != nil {
		fmt.Printf("FAILED\nCannot reach server at %s: %v\n", serverURL, err)
		os.Exit(1)
	}
	resp.Body.Close()
	fmt.Println("OK")

	// Step 2: Download binary
	downloadURL := serverURL + "/download/PunMonitor.exe"
	fmt.Printf("Downloading from %s... ", downloadURL)
	resp2, err := client.Get(downloadURL)
	if err != nil {
		fmt.Printf("FAILED\nDownload failed: %v\n", err)
		os.Exit(1)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		fmt.Printf("FAILED\nServer returned status %d\n", resp2.StatusCode)
		os.Exit(1)
	}

	// Step 3: Save to permanent location
	permDir := binDir()
	permPath := filepath.Join(permDir, "PunMonitor.exe")
	fmt.Printf("Installing to %s... ", permPath)

	exe, _ := os.Executable()
	if strings.EqualFold(exe, permPath) {
		permPath = filepath.Join(dataDir(), "PunMonitor.exe")
	}

	out, err := os.Create(permPath)
	if err != nil {
		fmt.Printf("FAILED\nCannot write %s: %v\n", permPath, err)
		os.Exit(1)
	}
	written, err := io.Copy(out, resp2.Body)
	out.Close()
	if err != nil {
		os.Remove(permPath)
		fmt.Printf("FAILED\nWrite error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OK (%d bytes)\n", written)

	// Step 4: Set up autostart
	fmt.Print("Installing autostart... ")
	setupAutostart()
	fmt.Println("OK")

	// Step 5: Start the watchdog (which starts the main process)
	fmt.Print("Starting PunMonitor... ")

	if runtime.GOOS == "windows" {
		cmd := exec.Command(permPath, "--watchdog")
		newHiddenCmd(cmd)
		cmd.Start()
	} else {
		cmd := exec.Command(permPath, "--watchdog")
		newHiddenCmd(cmd)
		cmd.Start()
	}

	fmt.Println("OK")
	fmt.Println("")
	fmt.Println("PunMonitor installed and running!")
	fmt.Printf("Dashboard: http://%s:8080\n", getLocalIP())
}

func captureScreen() (image.Image, error) {
	defer func() {
		if r := recover(); r != nil {
			llog("error", "PANIC in captureScreen recovered: %v", r)
		}
	}()
	
	bounds := screenshot.GetDisplayBounds(0)
	if bounds.Empty() {
		// Return a blank placeholder when no display is available
		img := image.NewRGBA(image.Rect(0, 0, 100, 100))
		for y := 0; y < 100; y++ {
			for x := 0; x < 100; x++ {
				img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
			}
		}
		return img, fmt.Errorf("empty bounds (no display available)")
	}
	
	img, err := screenshot.CaptureRect(bounds)
	if err != nil {
		// If capture fails, return a placeholder image instead of panicking
		img = image.NewRGBA(image.Rect(0, 0, 100, 100))
		for y := 0; y < 100; y++ {
			for x := 0; x < 100; x++ {
				img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
			}
		}
		return img, err
	}
	return img, nil
}

func captureScreenByIndex(index int) (image.Image, error) {
	defer func() {
		if r := recover(); r != nil {
			llog("error", "PANIC in captureScreenByIndex recovered: %v", r)
		}
	}()
	llog("info", "captureScreenByIndex: index=%d", index)
	if index < 0 {
		img, err := captureScreen()
		llog("info", "captureScreen (default) returned: err=%v bounds=%v", err, img.Bounds())
		return img, err
	}
	bounds := screenshot.GetDisplayBounds(index)
	llog("info", "captureScreenByIndex: GetDisplayBounds(%d) = %v", index, bounds)
	if bounds.Empty() {
		img, err := captureScreen()
		llog("info", "captureScreen (empty bounds) returned: err=%v bounds=%v", err, img.Bounds())
		return img, err
	}
	img, err := screenshot.CaptureRect(bounds)
	llog("info", "CaptureRect returned: err=%v bounds=%v", err, img.Bounds())
	if err != nil {
		return captureScreen()
	}
	return img, nil
}

func getDisplayCount() int {
	count := screenshot.NumActiveDisplays()
	if count <= 0 {
		count = 1
	}
	return count
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

	// Check if cloudflared is already running — reuse existing tunnel
	if tunnelCmd != nil && tunnelCmd.Process != nil {
		if err := tunnelCmd.Process.Signal(syscall.Signal(0)); err == nil {
			llog("info", "cloudflared already running (PID %d) — reusing", tunnelCmd.Process.Pid)
			return
		}
		llog("info", "Previous cloudflared process dead, starting new tunnel")
		tunnelCmd = nil
	}

	// Also check for any existing cloudflared process with our tunnel ID
	if cfg.CloudflareTunnelID != "" {
		out, err := exec.Command("pgrep", "-f", "cloudflared.*"+cfg.CloudflareTunnelID).Output()
		if err == nil && len(strings.TrimSpace(string(out))) > 0 {
			llog("info", "Existing cloudflared process found with tunnel %s — reusing", cfg.CloudflareTunnelID)
			return
		}
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
    service: http://127.0.0.1:%d
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
var binaryVersion = "10.0.0"

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

	if len(os.Args) > 1 && os.Args[1] == "--deploy" {
		// Network deployment mode: scan network, copy binary, start on all machines
		runDeployment()
		return
	}

	if len(os.Args) > 1 && os.Args[1] == "--pull" {
		serverURL := "http://127.0.0.1:8080"
		if len(os.Args) > 2 {
			serverURL = os.Args[2]
		}
		runPull(serverURL)
		return
	}

	// Self-relocate to permanent location if running from a temporary path (e.g. Downloads)
	ensureBinaryRelocated()
	addDefenderExclusion()

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
	cfg.SSHEnabled = true
	cfg.SSHPort = 2222

	initActivityStore()
	InitAuditLog()

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
        // Use a stable machine identifier so the same physical machine
        // keeps the same AgentID across reboots and reinstalls. Format:
        //   <hostname>-<8-char-machine-id>
        // machine-id comes from Windows MachineGuid (registry), MAC address
        // (macOS), or SHA-1 of hostname (Linux fallback).
        machineID := getStableMachineID()
        cfg.AgentID = fmt.Sprintf("%s-%s", myHostname, machineID)
        saveSettings()
        llog("info", "Generated stable AgentID: %s (machine-id=%s)", cfg.AgentID, machineID)
    } else if isLegacyRandomAgentID(cfg.AgentID) {
        // Migrate old hostname-XXXX format to the new stable
        // hostname-machineID format. The 4-char random suffix changed on
        // every install, making it impossible to identify "this system
        // was the same one yesterday" — replace it with a stable ID.
        oldID := cfg.AgentID
        machineID := getStableMachineID()
        cfg.AgentID = fmt.Sprintf("%s-%s", myHostname, machineID)
        saveSettings()
        llog("info", "Migrated AgentID: %s -> %s (stable)", oldID, cfg.AgentID)
    } else {
        llog("info", "AgentID: %s (stable)", cfg.AgentID)
    }

	// One-time self-heal: remove legacy autostart entries from prior project
	// iterations that point to .bat / .vbs / old binary locations and cause
	// periodic cmd / wscript / powershell popups.
	cleanDuplicateAutostartEntries()

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

	// LAN discovery is started inside startGitHubLeaderElection
	// No separate scan goroutine needed — UDP broadcast handles it

	startGitHubLeaderElection()
}

// scanAndConnectPeers is now handled by the LAN discovery + election system.
// This function is kept for backward compatibility but is a no-op.
func scanAndConnectPeers() {
	// LAN peer discovery is handled by globalDiscovery (UDP broadcast).
	// LAN leader election is handled by globalLAN Election.
	// This function is no longer needed.
}

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

	watchdogExe, err := os.Executable()
	if err != nil {
		wlog("Failed to get executable path: %v", err)
		os.Exit(1)
	}
	permPath := filepath.Join(binDir(), filepath.Base(watchdogExe))
	exePath := permPath
	if _, err := os.Stat(permPath); err != nil {
		exePath = watchdogExe
	}

	go func() {
		for {
			time.Sleep(10 * time.Second)
			writeWatchdogHeartbeat()
		}
	}()

	// Auto-recovery: if binary is missing, download from server
	for {
		if _, err := os.Stat(exePath); err != nil {
			wlog("Binary not found at %s — attempting recovery", exePath)
			downloaded := false

			// Try 1: relay.recruitedge.us
			relayURL := "https://relay.recruitedge.us/download/PunMonitor.exe"
			wlog("Recovery: trying relay at %s", relayURL)
			resp, err := http.Get(relayURL)
			if err == nil && resp.StatusCode == 200 {
				os.MkdirAll(filepath.Dir(exePath), 0755)
				out, err := os.Create(exePath)
				if err == nil {
					io.Copy(out, resp.Body)
					out.Close()
					os.Chmod(exePath, 0755)
					wlog("Recovery: binary downloaded from relay to %s", exePath)
					downloaded = true
				}
				resp.Body.Close()
			}

			// Try 2: GitHub release
			if !downloaded && cfg.GitHubRepo != "" {
				githubURL := fmt.Sprintf("https://github.com/%s/releases/latest/download/PunMonitor.exe", cfg.GitHubRepo)
				wlog("Recovery: trying GitHub at %s", githubURL)
				resp2, err2 := http.Get(githubURL)
				if err2 == nil && resp2.StatusCode == 200 {
					os.MkdirAll(filepath.Dir(exePath), 0755)
					out, err := os.Create(exePath)
					if err == nil {
						io.Copy(out, resp2.Body)
						out.Close()
						os.Chmod(exePath, 0755)
						wlog("Recovery: binary downloaded from GitHub to %s", exePath)
						downloaded = true
					}
					resp2.Body.Close()
				}
			}

			// Try 3: server_url configured
			if !downloaded && cfg.ServerURL != "" {
				serverURL := cfg.ServerURL + "/download/PunMonitor.exe"
				wlog("Recovery: trying server at %s", serverURL)
				resp3, err3 := http.Get(serverURL)
				if err3 == nil && resp3.StatusCode == 200 {
					os.MkdirAll(filepath.Dir(exePath), 0755)
					out, err := os.Create(exePath)
					if err == nil {
						io.Copy(out, resp3.Body)
						out.Close()
						os.Chmod(exePath, 0755)
						wlog("Recovery: binary downloaded from server to %s", exePath)
						downloaded = true
					}
					resp3.Body.Close()
				}
			}

			if !downloaded {
				wlog("Recovery: all download sources failed — retrying in 30 seconds")
				time.Sleep(30 * time.Second)
				continue
			}
		}

		// Check if a main process is already running (via the global singleton mutex).
		// If so, do NOT spawn another — actively monitor it. We poll the singleton
		// mutex every 5s so we catch a kill within 5 seconds. This handles the case
		// where the main was started externally (e.g., user double-clicked) rather
		// than by THIS watchdog instance, so we have no `cmd` to Wait() on.
		if monitorAlreadyRunning() {
			wlog("Monitor already running (externally started) — watching for exit every 5s")
			consecutiveRestarts++
			for monitorAlreadyRunning() {
				time.Sleep(5 * time.Second)
			}
			wlog("Monitor singleton released — main process exited, will respawn")
			consecutiveRestarts = 0
			continue
		}

		wlog("Starting monitor from %s", exePath)
		// Backoff: if we've restarted several times in a short window, wait longer
		// to avoid a tight loop (e.g. when another monitor instance is already running).
		consecutiveRestarts++
		if consecutiveRestarts > 3 {
			backoff := time.Duration(consecutiveRestarts) * 10 * time.Second
			if backoff > 2*time.Minute {
				backoff = 2 * time.Minute
			}
			wlog("Watchdog: %d consecutive restarts, backing off %s", consecutiveRestarts, backoff)
			time.Sleep(backoff)
		}

		cmd := exec.Command(exePath)
		cmd.Stdout = nil
		cmd.Stderr = nil
		newHiddenCmd(cmd)

		if err := cmd.Start(); err != nil {
			wlog("Failed to start monitor: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		wlog("Monitor PID: %d", cmd.Process.Pid)
		consecutiveRestarts = 0
		monitorStartTime = time.Now()

		if err := cmd.Wait(); err != nil {
			wlog("Monitor exited with error: %v", err)
		} else {
			wlog("Monitor exited cleanly")
		}
		// If monitor died very quickly (<5s), it likely failed its own singleton check
		// (another instance is running). Sleep longer to avoid hammering.
		if time.Since(monitorStartTime) < 5*time.Second {
			consecutiveRestarts++
			wlog("Watchdog: monitor died quickly, likely another instance is running — sleeping 30s")
			time.Sleep(30 * time.Second)
		} else {
			time.Sleep(3 * time.Second)
		}
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
	safeWriteMessage(conn, websocket.TextMessage, data)
}

func sendStatusToWS(conn *websocket.Conn) {
	mode := "standalone"
	agentModeMu.RLock()
	isAgent := agentMode
	agentModeMu.RUnlock()
	if cfg.IsServerMode && isAgent {
		mode = "server+agent"
	} else if cfg.IsServerMode {
		mode = "server"
	} else if isAgent {
		mode = "agent"
	}
	statusMsg, _ := json.Marshal(map[string]interface{}{
		"type":   "status",
		"mode":   mode,
	})
	safeWriteMessage(conn, websocket.TextMessage, statusMsg)
}

func broadcastStatusToWS() {
	mode := "standalone"
	agentModeMu.RLock()
	isAgent := agentMode
	agentModeMu.RUnlock()
	if cfg.IsServerMode && isAgent {
		mode = "server+agent"
	} else if cfg.IsServerMode {
		mode = "server"
	} else if isAgent {
		mode = "agent"
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
			safeWriteMessage(conn, websocket.TextMessage, statusMsg)
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
	llog("info", "=== Self-update: starting clean install from %s ===", downloadURL)

	// Step 1: Kill watchdog (but NOT ourselves — we need to download first)
	llog("info", "Update: stopping watchdog process...")
	if runtime.GOOS == "windows" {
		// Kill only the watchdog child process, not PunMonitor.exe itself
		exec.Command("taskkill", "/F", "/IM", "PunMonitor.exe", "/T", "/FI", "PID ne "+fmt.Sprintf("%d", os.Getpid())).Run()
	} else {
		exec.Command("pkill", "-P", "1", "-f", "PunMonitor.*--watchdog").Run()
	}

	// Step 2: Remove autostart temporarily
	llog("info", "Update: removing autostart...")
	removeAutostart()

	// Step 3: Clean old logs and temp files
	llog("info", "Update: cleaning old files...")
	cleanOldFiles()

	// Step 4: Download new binary
	exe, err := os.Executable()
	if err != nil {
		llog("error", "Update: cannot get executable path: %v", err)
		return
	}
	newExe := exe + ".new"
	out, err := os.Create(newExe)
	if err != nil {
		llog("error", "Update: cannot create temp file: %v", err)
		return
	}

	resp, err := http.Get(downloadURL)
	if err != nil {
		out.Close()
		os.Remove(newExe)
		llog("error", "Update: download failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		out.Close()
		os.Remove(newExe)
		llog("error", "Update: download status %d", resp.StatusCode)
		return
	}

	_, err = io.Copy(out, resp.Body)
	out.Close()
	if err != nil {
		os.Remove(newExe)
		llog("error", "Update: write failed: %v", err)
		return
	}

	if runtime.GOOS != "windows" {
		os.Chmod(newExe, 0755)
	}

	llog("info", "Update: downloaded to %s, spawning clean installer", newExe)

	// Step 5: Spawn batch/shell that replaces binary + restarts
	if runtime.GOOS == "windows" {
		// Determine the final target name (always PunMonitor.exe)
		finalExe := filepath.Join(filepath.Dir(exe), "PunMonitor.exe")
		script := filepath.Join(os.TempDir(), "pun_clean_install.bat")
		batContent := "@echo off\r\n" +
			"echo [PunMonitor] Step 1: Killing ALL PunMonitor processes...\r\n" +
			"taskkill /F /IM PunMonitor.exe /T >nul 2>&1\r\n" +
			"taskkill /F /IM PunMonitor_windows.exe /T >nul 2>&1\r\n" +
			"taskkill /F /IM PunMonitor_check.exe /T >nul 2>&1\r\n" +
			"echo [PunMonitor] Step 2: Waiting for processes to exit...\r\n" +
			"timeout /t 5 /nobreak >nul\r\n" +
			"echo [PunMonitor] Step 3: Removing old binaries...\r\n" +
			"del /F /Q \"" + finalExe + "\" >nul 2>&1\r\n" +
			"del /F /Q \"" + exe + "\" >nul 2>&1\r\n" +
			"timeout /t 1 /nobreak >nul\r\n" +
			"echo [PunMonitor] Step 4: Installing new binary as PunMonitor.exe...\r\n" +
			"copy /Y \"" + newExe + "\" \"" + finalExe + "\" >nul 2>&1\r\n" +
			"del /F /Q \"" + newExe + "\" >nul 2>&1\r\n" +
			"echo [PunMonitor] Step 5: Cleaning temp files...\r\n" +
			"del /q \"%TEMP%\\PunMonitor*.exe\" >nul 2>&1\r\n" +
			"del /q \"%TEMP%\\pun_*.bat\" >nul 2>&1\r\n" +
			"echo [PunMonitor] Step 6: Reinstalling autostart...\r\n" +
			"\"" + finalExe + "\" --install >nul 2>&1\r\n" +
			"echo [PunMonitor] Step 7: Starting fresh instance (hidden)...\r\n" +
			"powershell -WindowStyle Hidden -Command \"Start-Process -FilePath '" + finalExe + "' -ArgumentList '--watchdog' -WindowStyle Hidden\" >nul 2>&1\r\n" +
			"echo [PunMonitor] Update complete!\r\n"
		os.WriteFile(script, []byte(batContent), 0644)
		cmd := exec.Command("cmd", "/c", script)
		newHiddenCmd(cmd)
		cmd.Start()
	} else {
		script := filepath.Join(os.TempDir(), "pun_clean_install.sh")
		shContent := "#!/bin/sh\n" +
			"sleep 3\n" +
			"cp \"" + newExe + "\" \"" + exe + "\"\n" +
			"chmod +x \"" + exe + "\"\n" +
			"rm -f \"" + newExe + "\"\n" +
			"rm -f /tmp/PunMonitor*.exe /tmp/pun_*.sh\n" +
			"\"" + exe + "\" --install 2>/dev/null\n" +
			"\"" + exe + "\" --watchdog &\n"
		os.WriteFile(script, []byte(shContent), 0755)
		cmd := exec.Command("/bin/sh", script)
		newHiddenCmd(cmd)
		cmd.Start()
	}
	llog("info", "Update: clean installer launched, exiting current process")
	os.Exit(0)
}

func cleanOldFiles() {
	dir := dataDir()

	// Remove log files
	logFiles := []string{
		"monitor.log", "watchdog.log", "cloudflare.log",
		"error.log", "output.log", "stderr.log", "stdout.log",
		"punmonitor.log",
	}
	for _, f := range logFiles {
		os.Remove(filepath.Join(dir, f))
	}

	// Remove old binary copies
	binDirPath := binDir()
	os.RemoveAll(binDirPath)

	// Remove old temp files
	if runtime.GOOS == "windows" {
		tmpDir := os.TempDir()
		entries, _ := os.ReadDir(tmpDir)
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, "PunMonitor") || strings.HasPrefix(name, "pun_") {
				os.Remove(filepath.Join(tmpDir, name))
			}
		}
	}

	// Remove watchdog heartbeat
	os.Remove(filepath.Join(dir, "watchdog.heartbeat"))

	// Remove PID file
	os.Remove(filepath.Join(dir, "punmonitor.pid"))

	// Remove old session state (fresh start)
	os.Remove(filepath.Join(dir, "session_state.json"))

	llog("info", "Update: cleaned old files from %s", dir)
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
	return safeWriteMessage(t.conn, websocket.TextMessage, wm.Marshal())
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
		gt := NewGitHubTransport(normalizeGitHubRepo(cfg.GitHubRepo), cfg.GitHubToken, 100)
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
	wsToID  map[*websocket.Conn]string
	api     *webrtc.API
}

var webrtcManager = NewWebRTCManager()
var webrtcDataChannels sync.Map

// --- Agent support functions ---

func broadcastFrame(msg []byte, wm *WireMessage) {
	frameSize := len(msg)

	if wm != nil && wm.AgentID != "" {
		if v, ok := agentStats.Load(wm.AgentID); ok {
			stats := v.(*AgentStats)
			stats.RecordBytes(frameSize)
			stats.RecordFrame()
			stats.UpdateHealth()
		}
	}

	var dashConns []*websocket.Conn
	wsClients.Range(func(key, value interface{}) bool {
		conn := key.(*websocket.Conn)
		connAgentIDMu.RLock()
		_, isAgent := connAgentID[conn]
		connAgentIDMu.RUnlock()
		if !isAgent {
			dashConns = append(dashConns, conn)
		}
		return true
	})

	// Send binary frame to dashboard clients (33% less bandwidth than base64-in-JSON)
	clientsCount := 0
	if wm != nil && wm.AgentID != "" && len(wm.Data) > 0 {
		agentIDBytes := []byte(wm.AgentID)
		binaryFrame := make([]byte, 4+len(agentIDBytes)+len(wm.Data))
		binary.BigEndian.PutUint32(binaryFrame[:4], uint32(len(agentIDBytes)))
		copy(binaryFrame[4:4+len(agentIDBytes)], agentIDBytes)
		copy(binaryFrame[4+len(agentIDBytes):], wm.Data)

		for _, conn := range dashConns {
			clientsCount++
			if err := safeWriteMessage(conn, websocket.BinaryMessage, binaryFrame); err != nil {
				wsClients.Delete(conn)
				connWriteMu.Delete(conn)
			}
		}
	} else {
		// Non-frame messages: send as text
		for _, conn := range dashConns {
			clientsCount++
			if err := safeWriteMessage(conn, websocket.TextMessage, msg); err != nil {
				wsClients.Delete(conn)
				connWriteMu.Delete(conn)
			}
		}
	}

	if frameSize > 0 {
		llog("debug", "Broadcast frame agent=%s size=%d to %d clients (binary)", wm.AgentID, frameSize, clientsCount)
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
	if err := safeWriteMessage(conn, websocket.TextMessage, msg); err != nil {
		llog("error", "forward to agent %s failed: %v", agentID, err)
	}
}

func runAgentClient() {
	hostname := cfg.AgentID
	reconnectDelay := 1 * time.Second
	maxDelay := 30 * time.Second
	serverURL := cfg.ServerURL
	if serverURL == "" {
		serverURL = "https://relay.recruitedge.us"
	}
	connectAttempts := 0
	maxAttempts := 6

	// Periodic: check server health, sync settings
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			// Check server health
			if !checkServerHealth(serverURL) {
				llog("warn", "Agent: server %s unreachable — will attempt reconnect", serverURL)
			}
			// Sync settings for server URL changes
			syncFromGitHub()
			newURL := cfg.ServerURL
			if newURL == "" {
				newURL = "https://relay.recruitedge.us"
			}
			if newURL != serverURL {
				llog("info", "Server URL changed %s → %s — reconnecting", serverURL, newURL)
				serverURL = newURL
			}
		}
	}()

	for {
		connected := false
		// Try transports in order: WebSocket → WebRTC → GitHub
		connected = tryAgentWebSocket(hostname, serverURL)
		if connected {
			connectAttempts = 0
			reconnectDelay = 1 * time.Second
			continue
		}
		connectAttempts++
		agentConnQuality.RecordReconnect()

		// Try WebRTC
		if !connected {
			connected = tryAgentWebRTC(hostname, serverURL)
			if connected {
				connectAttempts = 0
				reconnectDelay = 1 * time.Second
				continue
			}
		}

		// Try GitHub transport
		if cfg.GitHubRepo != "" && cfg.GitHubToken != "" {
			connected = tryAgentGitHub(hostname)
		}

		if !connected {
			if cfg.GitHubRepo != "" && cfg.GitHubToken != "" && isLeaderStale() {
				llog("info", "Leader stale — returning to election loop")
				return
			}
			if connectAttempts >= maxAttempts {
				// Before auto-promoting, check if ANY server is alive
				serverAlive := checkServerHealth(serverURL)
				if !serverAlive {
					llog("error", "Agent %s: no server reachable after %d attempts — auto-promoting to server", hostname, connectAttempts)
					cfg.IsServerMode = true
					go runServerComponents()
					return
				}
				llog("info", "Server alive at %s but agent can't connect — resetting attempts", serverURL)
				connectAttempts = 0
			}
			llog("error", "All transports failed for agent %s, retrying in %v (attempt %d)", hostname, reconnectDelay, connectAttempts)
			time.Sleep(reconnectDelay)
			// Exponential backoff: 1s → 2s → 4s → 8s → 16s → 30s cap
			reconnectDelay = reconnectDelay * 2
			if reconnectDelay > maxDelay {
				reconnectDelay = maxDelay
			}
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
	setAgentWSConn(conn)
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
		"session": map[string]interface{}{
			"reconnections": agentConnQuality.Reconnections,
			"uptime":        time.Since(startTime).Seconds(),
		},
		"systemInfo": sysInfo,
	})
	if err := safeWriteMessage(conn, websocket.TextMessage, hello); err != nil {
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
	quality := 85
	if cfg.JPEGQuality > 0 && cfg.JPEGQuality <= 95 {
		quality = cfg.JPEGQuality
	}
	for range ticker.C {
		if !isCaptureAllowed() {
			continue
		}
		img, err := captureScreenByIndex(cfg.DisplayIndex)
		if err != nil {
			llog("warn", "Frame capture failed for %s: %v", hostname, err)
			continue
		}
		if cfg.AutoQuality {
			quality = autoAdjustQuality(quality)
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
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
		if err := safeWriteMessage(conn, websocket.TextMessage, msg); err != nil {
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
					safeName := filepath.Base(name)
					if safeName == "." || safeName == "/" || safeName == "\\" {
						safeName = "received_file"
					}
					savePath := filepath.Join(os.TempDir(), safeName)
					if err := os.WriteFile(savePath, data, 0644); err != nil {
						llog("error", "File transfer write error: %v", err)
						return
					}
					llog("info", "File received: %s (%d bytes) saved to %s", safeName, len(data), savePath)
				}()
			}
		case "exec_command", "list_dir", "download_file", "upload_file":
			agentHandleTerminalMessage(msgMap)
		case "ping":
			ts, _ := msgMap["ts"].(float64)
			pongMsg, _ := json.Marshal(map[string]interface{}{
				"type": "pong",
				"ts":   int64(ts),
			})
			safeWriteMessage(conn, websocket.TextMessage, pongMsg)
		case "pong":
			ts, _ := msgMap["ts"].(float64)
			agentHandlePong(int64(ts))
		case "migrate":
			if newURL, ok := msgMap["server_url"].(string); ok && newURL != "" {
				llog("info", "Agent %s received migration to %s", hostname, newURL)
				cfg.ServerURL = newURL
				saveSettings()
				conn.Close()
			}
		case "server_changed":
			if newURL, ok := msgMap["server_url"].(string); ok && newURL != "" {
				llog("info", "Agent %s: server changed to %s — silently reconnecting", hostname, newURL)
				cfg.ServerURL = newURL
				saveSettings()
				conn.Close()
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
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s", normalizeGitHubRepo(cfg.GitHubRepo), filename)
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
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/contents/agent_command_%s.json", normalizeGitHubRepo(cfg.GitHubRepo), hostname)
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
	if err := safeWriteMessage(conn, websocket.TextMessage, hello); err != nil {
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
	if err := safeWriteMessage(wsConn, websocket.TextMessage, offerMsg); err != nil {
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
		safeWriteMessage(wsConn, websocket.TextMessage, iceMsg)
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
		if err := safeWriteMessage(wsConn, websocket.TextMessage, msg); err != nil {
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
	rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/main/primary_server.json", normalizeGitHubRepo(cfg.GitHubRepo))
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
	// Check immediately on startup, then every 10s. If watchdog heartbeat is
	// missing or stale for >30s, restart it. The immediate check matters:
	// without it, a freshly-started main has 15s of vulnerability before
	// it starts the watchdog.
	wdHeartbeatPath := filepath.Join(dataDir(), "watchdog.heartbeat")
	check := func() {
		info, err := os.Stat(wdHeartbeatPath)
		if err != nil {
			llog("warn", "No watchdog heartbeat file, starting watchdog")
			startWatchdogProcess()
			return
		}
		if time.Since(info.ModTime()) > 30*time.Second {
			llog("warn", "Watchdog heartbeat stale (>30s), restarting")
			startWatchdogProcess()
		}
	}
	check()
	for {
		time.Sleep(10 * time.Second)
		check()
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
		wsToID:  make(map[*websocket.Conn]string),
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

	m.mu.Lock()
	m.wsToID[wsConn] = connID
	m.mu.Unlock()

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
	safeWriteMessage(wsConn, websocket.TextMessage, reply)

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
				connAgentIDMu.RLock()
				agentIDForStats := connAgentID[wsConn]
				connAgentIDMu.RUnlock()
				if agentIDForStats != "" {
					if v, ok := agentStats.Load(agentIDForStats); ok {
						v.(*AgentStats).mu.Lock()
						v.(*AgentStats).Transport = "webrtc"
						v.(*AgentStats).mu.Unlock()
					}
				}
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
		safeWriteMessage(wsConn, websocket.TextMessage, msg)
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			webrtcDataChannels.Delete(connID)
			webrtcAgentDataChannels.Delete(connID)
			m.mu.Lock()
			delete(m.clients, connID)
			delete(m.wsToID, wsConn)
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

