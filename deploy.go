package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type DeployTarget struct {
	IP          string
	Hostname    string
	OS          string
	Arch        string
	HasAgent    bool
	HasPort8080 bool
}

type DeployCredentials struct {
	Username string
	Password string
	Domain   string
}

var deployCreds DeployCredentials
var deployCredsMu sync.RWMutex

func SetDeployCredentials(user, pass, domain string) {
	deployCredsMu.Lock()
	defer deployCredsMu.Unlock()
	deployCreds = DeployCredentials{Username: user, Password: pass, Domain: domain}
}

func GetDeployCredentials() DeployCredentials {
	deployCredsMu.RLock()
	defer deployCredsMu.RUnlock()
	return deployCreds
}

func HasDeployCredentials() bool {
	deployCredsMu.RLock()
	defer deployCredsMu.RUnlock()
	return deployCreds.Username != ""
}

func checkPort(host string, port int, timeout time.Duration) bool {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func isPunMonitorRunning(ip string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:8080/api/health", ip))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func getRemoteHostname(ip string) string {
	cmd := exec.Command("nbtstat", "-A", ip)
	output, err := cmd.Output()
	if err == nil {
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			if strings.Contains(line, "<00>") && strings.Contains(line, "UNIQUE") {
				parts := strings.Fields(line)
				if len(parts) > 0 {
					return strings.TrimSpace(parts[0])
				}
			}
		}
	}
	return ""
}

func deployToWindowsTarget(target DeployTarget, binaryPath string, serverURL string) error {
	creds := GetDeployCredentials()
	if creds.Username == "" {
		return fmt.Errorf("no deploy credentials configured")
	}

	domainUser := creds.Username
	if creds.Domain != "" {
		domainUser = creds.Domain + "\\" + creds.Username
	}

	remotePath := fmt.Sprintf("\\\\%s\\C$\\Users\\Public\\PunMonitor.exe", target.IP)

	llog("info", "Deploy: mapping network share to %s", target.IP)
	netUseCmd := fmt.Sprintf(`net use \\%s\IPC$ /user:%s "%s"`, target.IP, domainUser, creds.Password)
	if out, err := exec.Command("cmd", "/c", netUseCmd).CombinedOutput(); err != nil {
		llog("warn", "net use IPC failed for %s: %v — %s", target.IP, err, string(out))
	}

	llog("info", "Deploy: copying binary to %s", remotePath)
	copyCmd := fmt.Sprintf(`copy /Y "%s" "%s"`, binaryPath, remotePath)
	if out, err := exec.Command("cmd", "/c", copyCmd).CombinedOutput(); err != nil {
		llog("error", "Copy failed to %s: %v — %s", target.IP, err, string(out))

		netUseDel := fmt.Sprintf(`net use \\%s\IPC$ /delete`, target.IP)
		exec.Command("cmd", "/c", netUseDel).Run()
		return fmt.Errorf("copy failed: %v", err)
	}
	llog("info", "Deploy: binary copied to %s", target.IP)

	remoteExe := `C:\Users\Public\PunMonitor.exe`
	schtasksCmd := fmt.Sprintf(`schtasks /Create /S %s /RU "%s" /RP "%s" /TN "PunMonitor" /TR "%s --watchdog" /SC ONLOGON /F`, target.IP, domainUser, creds.Password, remoteExe)
	if out, err := exec.Command("cmd", "/c", schtasksCmd).CombinedOutput(); err != nil {
		llog("warn", "schtasks failed for %s: %v — %s, trying wmic", target.IP, err, string(out))

		wmicCmd := fmt.Sprintf(`wmic /node:"%s" /user:"%s" /password:"%s" process call create "%s --watchdog"`, target.IP, domainUser, creds.Password, remoteExe)
		if out2, err2 := exec.Command("cmd", "/c", wmicCmd).CombinedOutput(); err2 != nil {
			llog("error", "wmic failed for %s: %v — %s", target.IP, err2, string(out2))

			psexecCmd := fmt.Sprintf(`psexec \\\\%s -u "%s" -p "%s" -d -i "%s" --watchdog`, target.IP, domainUser, creds.Password, remoteExe)
			if out3, err3 := exec.Command("cmd", "/c", psexecCmd).CombinedOutput(); err3 != nil {
				llog("error", "All remote start methods failed for %s: %v — %s", target.IP, err3, string(out3))
				netUseDel := fmt.Sprintf(`net use \\%s\IPC$ /delete`, target.IP)
				exec.Command("cmd", "/c", netUseDel).Run()
				return fmt.Errorf("copy succeeded but remote start failed")
			}
		}
	}

	netUseDel := fmt.Sprintf(`net use \\%s\IPC$ /delete`, target.IP)
	exec.Command("cmd", "/c", netUseDel).Run()

	llog("info", "Deploy: PunMonitor started on %s (%s)", target.Hostname, target.IP)
	return nil
}

func autoDeployToPeer(peer *PeerInfo) {
	if peer.AgentID == cfg.AgentID {
		return
	}
	if peer.Mode == "server" || peer.Mode == "agent" {
		return
	}
	if isPunMonitorRunning(peer.IP) {
		llog("info", "Auto-deploy: %s already running PunMonitor — skipping", peer.Hostname)
		return
	}

	if !HasDeployCredentials() {
		llog("info", "Auto-deploy: %s needs PunMonitor but no credentials configured — skipping", peer.Hostname)
		return
	}

	llog("info", "Auto-deploy: installing PunMonitor on %s (%s)", peer.Hostname, peer.IP)

	exePath, err := os.Executable()
	if err != nil {
		llog("error", "Auto-deploy: cannot get own binary path: %v", err)
		return
	}
	permPath := filepath.Join(binDir(), filepath.Base(exePath))
	if _, err := os.Stat(permPath); err == nil {
		exePath = permPath
	}

	if err := deployToWindowsTarget(DeployTarget{IP: peer.IP, Hostname: peer.Hostname}, exePath, cfg.ServerURL); err != nil {
		llog("error", "Auto-deploy to %s failed: %v", peer.Hostname, err)
	} else {
		llog("info", "Auto-deploy: %s installed successfully", peer.Hostname)
	}
}

func runDeployment() {
	llog("info", "=== Starting Network Deployment ===")
	llog("info", "This machine: %s (%s/%s)", getHostname(), runtime.GOOS, runtime.GOARCH)

	exePath, err := os.Executable()
	if err != nil {
		llog("error", "Cannot get binary path: %v", err)
		return
	}
	permPath := filepath.Join(binDir(), filepath.Base(exePath))
	if _, err := os.Stat(permPath); err == nil {
		exePath = permPath
	}

	if globalDiscovery == nil {
		llog("error", "Discovery not initialized")
		return
	}

	peers := globalDiscovery.GetPeers()
	llog("info", "Found %d peers via UDP discovery", len(peers))

	if len(peers) == 0 {
		llog("info", "No peers found. Waiting 15 seconds for discovery...")
		time.Sleep(15 * time.Second)
		peers = globalDiscovery.GetPeers()
		llog("info", "Found %d peers after wait", len(peers))
	}

	success := 0
	failed := 0
	for _, peer := range peers {
		if isPunMonitorRunning(peer.IP) {
			llog("info", "  %s (%s) — already running", peer.Hostname, peer.IP)
			continue
		}

		if !HasDeployCredentials() {
			llog("warn", "  %s (%s) — needs install but no credentials. Use dashboard Settings to configure.", peer.Hostname, peer.IP)
			failed++
			continue
		}

		if err := deployToWindowsTarget(DeployTarget{IP: peer.IP, Hostname: peer.Hostname}, exePath, cfg.ServerURL); err != nil {
			llog("error", "  %s (%s) — FAILED: %v", peer.Hostname, peer.IP, err)
			failed++
		} else {
			llog("info", "  %s (%s) — deployed OK", peer.Hostname, peer.IP)
			success++
		}
	}

	llog("info", "=== Deployment Complete ===")
	llog("info", "Success: %d, Failed: %d, Total: %d", success, failed, len(peers))
}
