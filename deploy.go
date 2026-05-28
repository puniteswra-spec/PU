package main

import (
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// DeployTarget represents a discovered machine on the network
type DeployTarget struct {
	IP       string
	Hostname string
	OS       string // "windows", "darwin", "linux"
	Arch     string // "amd64", "arm64"
	OpenPort bool
	HasAgent bool
}

// discoverNetwork scans the local network for live machines
func discoverNetwork() []DeployTarget {
	llog("info", "Scanning local network for machines...")
	
	// Get local IP and subnet
	localIP := getLocalIP()
	if localIP == "unknown" {
		llog("error", "Cannot determine local IP for network scan")
		return nil
	}
	
	// Extract subnet (e.g., 192.168.1.x)
	parts := strings.Split(localIP, ".")
	if len(parts) != 4 {
		llog("error", "Invalid IP format: %s", localIP)
		return nil
	}
	subnet := strings.Join(parts[:3], ".")
	
	llog("info", "Scanning subnet %s.0/24 (local IP: %s)", subnet, localIP)
	
	var targets []DeployTarget
	var mu sync.Mutex
	var wg sync.WaitGroup
	
	// Scan all 254 IPs in parallel (limited concurrency)
	sem := make(chan struct{}, 50)
	
	for i := 1; i <= 254; i++ {
		ip := fmt.Sprintf("%s.%d", subnet, i)
		if ip == localIP {
			continue // Skip self
		}
		
		wg.Add(1)
		sem <- struct{}{}
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			
			// Ping with short timeout
			if pingHost(ip, 500*time.Millisecond) {
				mu.Lock()
				targets = append(targets, DeployTarget{IP: ip, OpenPort: true})
				mu.Unlock()
			}
		}(ip)
	}
	
	wg.Wait()
	
	llog("info", "Found %d live machines on network", len(targets))
	
	// Detect OS for each target
	for i := range targets {
		detectOS(&targets[i])
	}
	
	return targets
}

// pingHost sends a single ping to check if host is alive
func pingHost(ip string, timeout time.Duration) bool {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("ping", "-n", "1", "-w", fmt.Sprintf("%d", timeout.Milliseconds()), ip)
	} else {
		cmd = exec.Command("ping", "-c", "1", "-W", fmt.Sprintf("%.0f", timeout.Seconds()), ip)
	}
	return cmd.Run() == nil
}

// detectOS identifies the OS and architecture of a remote machine
func detectOS(target *DeployTarget) {
	// Try SSH first (works for Mac and Linux)
	sshConn := fmt.Sprintf("user@%s", target.IP)
	cmd := exec.Command("ssh", "-o", "ConnectTimeout=2", "-o", "StrictHostKeyChecking=no", sshConn, "uname -s -m 2>/dev/null || echo 'unknown'")
	if output, err := cmd.Output(); err == nil {
		out := strings.TrimSpace(string(output))
		if strings.Contains(out, "Darwin") {
			target.OS = "darwin"
			if strings.Contains(out, "arm64") {
				target.Arch = "arm64"
			} else {
				target.Arch = "amd64"
			}
			target.Hostname = getRemoteHostname(sshConn)
			return
		} else if strings.Contains(out, "Linux") {
			target.OS = "linux"
			if strings.Contains(out, "aarch64") || strings.Contains(out, "arm64") {
				target.Arch = "arm64"
			} else {
				target.Arch = "amd64"
			}
			target.Hostname = getRemoteHostname(sshConn)
			return
		}
	}
	
	// Try WinRM/PowerShell for Windows
	psConn := fmt.Sprintf("%s", target.IP)
	cmd = exec.Command("powershell", "-Command", 
		fmt.Sprintf("Test-NetConnection -ComputerName %s -Port 445 -InformationLevel Quiet", psConn))
	if output, err := cmd.Output(); err == nil && strings.TrimSpace(string(output)) == "True" {
		target.OS = "windows"
		target.Arch = "amd64" // Assume x64 for Windows
		target.Hostname = getRemoteWindowsHostname(target.IP)
		return
	}
	
	// If we can't determine, mark as unknown
	target.OS = "unknown"
	target.Hostname = "unknown"
}

// getRemoteHostname gets hostname via SSH
func getRemoteHostname(sshConn string) string {
	cmd := exec.Command("ssh", "-o", "ConnectTimeout=2", "-o", "StrictHostKeyChecking=no", sshConn, "hostname 2>/dev/null")
	if output, err := cmd.Output(); err == nil {
		return strings.TrimSpace(string(output))
	}
	return "unknown"
}

// getRemoteWindowsHostname gets hostname via SMB
func getRemoteWindowsHostname(ip string) string {
	// Try to get hostname via nbtstat or similar
	cmd := exec.Command("nbtstat", "-A", ip)
	if output, err := cmd.Output(); err == nil {
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
	return "unknown"
}

// deployToTarget copies the binary and starts it on a remote machine
func deployToTarget(target DeployTarget, binaryPath string, serverURL string) error {
	llog("info", "Deploying to %s (%s/%s) at %s", target.Hostname, target.OS, target.Arch, target.IP)
	
	switch target.OS {
	case "darwin", "linux":
		return deployViaSSH(target, binaryPath, serverURL)
	case "windows":
		return deployViaWinRM(target, binaryPath, serverURL)
	default:
		return fmt.Errorf("unsupported OS: %s", target.OS)
	}
}

// deployViaSSH copies and starts the binary on Mac/Linux via SSH
func deployViaSSH(target DeployTarget, binaryPath string, serverURL string) error {
	sshConn := fmt.Sprintf("user@%s", target.IP)
	
	// Determine remote path
	remotePath := "/tmp/PunMonitor"
	if target.OS == "darwin" {
		remotePath = "/tmp/PunMonitor"
	} else {
		remotePath = "/tmp/punmonitor"
	}
	
	// Copy binary
	llog("info", "Copying binary to %s:%s", target.IP, remotePath)
	cmd := exec.Command("scp", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=no",
		binaryPath, fmt.Sprintf("%s:%s", sshConn, remotePath))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("SCP failed: %v - %s", err, string(output))
	}
	
	// Make executable
	cmd = exec.Command("ssh", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=no",
		sshConn, fmt.Sprintf("chmod +x %s", remotePath))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("chmod failed: %v", err)
	}
	
	// Start the binary with server_url
	llog("info", "Starting PunMonitor on %s", target.IP)
	startCmd := fmt.Sprintf("nohup %s > /dev/null 2>&1 &", remotePath)
	cmd = exec.Command("ssh", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=no",
		sshConn, startCmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start failed: %v", err)
	}
	
	llog("info", "Successfully deployed to %s (%s)", target.Hostname, target.IP)
	return nil
}

// deployViaWinRM copies and starts the binary on Windows via WinRM/PSRemoting
func deployViaWinRM(target DeployTarget, binaryPath string, serverURL string) error {
	// For Windows, we use WinRM or PSRemoting
	// This requires the user to have admin access
	
	remotePath := fmt.Sprintf("C:\\Users\\Public\\PunMonitor.exe")
	
	// Copy binary using WinRM
	llog("info", "Copying binary to %s:%s", target.IP, remotePath)
	copyCmd := fmt.Sprintf("Copy-Item -Path '\\\\%s\\share\\PunMonitor.exe' -Destination '%s' -Force", 
		target.IP, remotePath)
	cmd := exec.Command("powershell", "-Command", copyCmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		// Fallback: try using SMB directly
		llog("warn", "PSRemoting failed, trying direct copy: %v", err)
		return fmt.Errorf("deployment to Windows requires manual setup: %v - %s", err, string(output))
	}
	
	// Start the binary
	startCmd := fmt.Sprintf("Start-Process -FilePath '%s' -WindowStyle Hidden", remotePath)
	cmd = exec.Command("powershell", "-Command", 
		fmt.Sprintf("Invoke-Command -ComputerName %s -ScriptBlock { %s }", target.IP, startCmd))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start failed: %v", err)
	}
	
	llog("info", "Successfully deployed to %s (%s)", target.Hostname, target.IP)
	return nil
}

// runDeployment runs the full deployment process
func runDeployment() {
	llog("info", "=== Starting Network Deployment ===")
	llog("info", "This machine: %s (%s/%s)", getHostname(), runtime.GOOS, runtime.GOARCH)
	
	// Get the path to the current binary
	exePath := getDeployBinaryPath()
	
	// Discover network
	targets := discoverNetwork()
	if len(targets) == 0 {
		llog("info", "No machines found on network")
		return
	}
	
	// Print discovered machines
	llog("info", "Discovered machines:")
	for _, t := range targets {
		status := "unknown"
		if t.OS != "unknown" {
			status = fmt.Sprintf("%s/%s", t.OS, t.Arch)
		}
		llog("info", "  %s - %s (%s)", t.IP, t.Hostname, status)
	}
	
	// Deploy to each target
	success := 0
	failed := 0
	for _, target := range targets {
		if target.OS == "unknown" {
			llog("warn", "Skipping %s - OS unknown", target.IP)
			failed++
			continue
		}
		
		if err := deployToTarget(target, exePath, cfg.ServerURL); err != nil {
			llog("error", "Failed to deploy to %s: %v", target.IP, err)
			failed++
		} else {
			success++
		}
	}
	
	llog("info", "=== Deployment Complete ===")
	llog("info", "Success: %d, Failed: %d, Total: %d", success, failed, len(targets))
}

// --- Helper functions ---

// getLocalSubnet returns the local subnet in CIDR notation
func getLocalSubnet() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}

// checkPort checks if a specific port is open on a host
func checkPort(host string, port int, timeout time.Duration) bool {
	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// getBinaryName returns the appropriate binary name for the target OS
func getBinaryName(os string) string {
	if os == "windows" {
		return "PunMonitor.exe"
	}
	return "PunMonitor"
}

// getDeployBinaryPath returns the path to the binary for deployment
func getDeployBinaryPath() string {
	exePath, err := exec.LookPath("punmonitor")
	if err == nil {
		return exePath
	}
	return "./PunMonitor"
}
