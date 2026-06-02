package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	glssh "github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	xcssh "golang.org/x/crypto/ssh"
)

// ────────────────────────────────────────────────────────────────────
// SSH server — exposes a standard SSH endpoint on each PunMonitor
// instance so users can connect via `ssh user@host -p <port>` for
// terminal, `sftp` for file transfer, and use any standard SSH
// client's port-forwarding features. WebSocket stays for screen
// streaming (high bandwidth); SSH covers the rest of the SSH
// feature set.
//
// Capabilities:
//   • Interactive shell (cmd.exe on Windows, $SHELL on Unix)
//   • Non-interactive command execution (ssh host 'command')
//   • SFTP subsystem (file upload/download/manage)
//   • Local port forwarding (ssh -L)
//   • Remote port forwarding (ssh -R)
//   • Password auth + Public-key auth
// ────────────────────────────────────────────────────────────────────

var (
	sshServer           *glssh.Server
	sshServerMu         sync.Mutex
	sshHostKeyEd255     ed25519.PrivateKey
	sshFingerprint      string // SHA-256 of host public key, base64
	forwardedTCPHandler = &glssh.ForwardedTCPHandler{}
)

// setupSSHServer initializes the SSH server. Idempotent — safe to call
// from runServerComponents on every server start. Loads host key and
// password from settings; generates them on first run.
func setupSSHServer() error {
	sshServerMu.Lock()
	defer sshServerMu.Unlock()

	if !cfg.SSHEnabled {
		llog("info", "SSH server disabled in settings, skipping")
		return nil
	}
	port := cfg.SSHPort
	if port == 0 {
		port = 2222
	}
	// Auto-fill host key + password if missing
	if err := ensureSSHCredentials(); err != nil {
		return fmt.Errorf("ssh credentials: %w", err)
	}

	// Decode host key
	block, _ := pem.Decode([]byte(cfg.SSHHostKeyPEM))
	if block == nil {
		return fmt.Errorf("invalid host key PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse host key: %w", err)
	}
	var ok bool
	sshHostKeyEd255, ok = key.(ed25519.PrivateKey)
	if !ok {
		return fmt.Errorf("host key is not ed25519")
	}
	signer, err := xcssh.NewSignerFromKey(sshHostKeyEd255)
	if err != nil {
		return fmt.Errorf("ssh signer: %w", err)
	}
	// Compute SHA-256 fingerprint for display (over the wire-format key, per OpenSSH standard)
	signerPub := signer.PublicKey()
	wireBytes := signerPub.Marshal()
	sum := sha256.Sum256(wireBytes)
	sshFingerprint = "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	llog("info", "SSH host key configured: wire=%s fingerprint=%s", base64.StdEncoding.EncodeToString(wireBytes), sshFingerprint)

	// Write host key to data dir for backup / recovery
	hostKeyPath := filepath.Join(dataDir(), "ssh_host_key.pem")
	if _, err := os.Stat(hostKeyPath); os.IsNotExist(err) {
		_ = os.WriteFile(hostKeyPath, []byte(cfg.SSHHostKeyPEM), 0600)
	}

	server := &glssh.Server{
		Addr:        ":" + strconv.Itoa(port),
		Handler:     sshSessionHandler,
		HostSigners: []glssh.Signer{signer},
		// Subsystem dispatch — required for SFTP (gliderlabs/ssh rejects
		// subsystem requests with no registered handler)
		SubsystemHandlers: map[string]glssh.SubsystemHandler{
			"sftp": func(sess glssh.Session) {
				RecordAudit("sftp_session", cfg.AgentID, sess.User(), "from "+sess.RemoteAddr().String())
				sshSFTPHandler(sess)
			},
		},
		// Local port forwarding (`ssh -L host:port`): client opens a
		// "direct-tcpip" channel for each connection
		ChannelHandlers: map[string]glssh.ChannelHandler{
			"session":      glssh.DefaultSessionHandler, // required when ChannelHandlers is non-nil — gliderlabs doesn't auto-add
			"direct-tcpip": glssh.DirectTCPIPHandler,
		},
		// Reverse port forwarding (`ssh -R host:port`): client requests
		// a "tcpip-forward" global request
		RequestHandlers: map[string]glssh.RequestHandler{
			"tcpip-forward":        forwardedTCPHandler.HandleSSHRequest,
			"cancel-tcpip-forward": forwardedTCPHandler.HandleSSHRequest,
		},
		// Auth handlers
		PasswordHandler: func(ctx glssh.Context, password string) bool {
			if ctx.User() != cfg.SSHUsername {
				return false
			}
			ok := subtleEqual(password, cfg.SSHPassword)
			if ok {
				RecordAudit("ssh_login", cfg.AgentID, ctx.User(), "password from "+ctx.RemoteAddr().String())
			} else {
				llog("warn", "SSH password auth failed: user=%s addr=%s", ctx.User(), ctx.RemoteAddr())
			}
			return ok
		},
		PublicKeyHandler: func(ctx glssh.Context, key glssh.PublicKey) bool {
			if ctx.User() != cfg.SSHUsername {
				llog("warn", "SSH public-key auth: wrong user=%s expected=%s", ctx.User(), cfg.SSHUsername)
				return false
			}
			// Parse stored authorized keys into PublicKey objects (strips comment)
			stored := parseAuthorizedKeys(cfg.SSHAuthorizedKeys)
			for _, ak := range stored {
				if keyEqual(key, ak) {
					RecordAudit("ssh_login", cfg.AgentID, ctx.User(), "public-key from "+ctx.RemoteAddr().String())
					return true
				}
			}
			clientFP := sshKeyFingerprint(key)
			llog("warn", "SSH public-key auth failed: user=%s addr=%s client_key_fp=%s stored_count=%d", ctx.User(), ctx.RemoteAddr(), clientFP, len(stored))
			return false
		},
		// Port forwarding
		LocalPortForwardingCallback: func(ctx glssh.Context, bindHost string, bindPort uint32) bool {
			llog("info", "SSH -L forward: %s:%d from %s", bindHost, bindPort, ctx.RemoteAddr())
			RecordAudit("ssh_forward", cfg.AgentID, ctx.User(),
				fmt.Sprintf("local %s:%d from %s", bindHost, bindPort, ctx.RemoteAddr()))
			return true
		},
		ReversePortForwardingCallback: func(ctx glssh.Context, bindHost string, bindPort uint32) bool {
			llog("info", "SSH -R forward: %s:%d from %s", bindHost, bindPort, ctx.RemoteAddr())
			RecordAudit("ssh_reverse_forward", cfg.AgentID, ctx.User(),
				fmt.Sprintf("%s:%d from %s", bindHost, bindPort, ctx.RemoteAddr()))
			return true
		},
		IdleTimeout: 5 * time.Minute,
		MaxTimeout:  30 * time.Minute,
	}

	// Start listener
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("ssh listen on %s: %w", server.Addr, err)
	}
	go func() {
		llog("info", "SSH server listening on %s (user=%s fingerprint=%s)", server.Addr, cfg.SSHUsername, sshFingerprint)
		if err := server.Serve(listener); err != nil {
			llog("error", "SSH server stopped: %v", err)
		}
	}()
	sshServer = server
	return nil
}

// stopSSHServer stops the running SSH server. Safe to call when not running.
func stopSSHServer() {
	sshServerMu.Lock()
	defer sshServerMu.Unlock()
	if sshServer != nil {
		_ = sshServer.Close()
		sshServer = nil
		llog("info", "SSH server stopped")
	}
}

// ensureSSHCredentials generates SSH host key + admin password if not
// already set. Persists them to settings.json so they survive restarts.
func ensureSSHCredentials() error {
	dirty := false
	if cfg.SSHUsername == "" {
		cfg.SSHUsername = "admin"
		dirty = true
	}
	if cfg.SSHPassword == "" {
		// 12 bytes = 96 bits entropy, base64 = 16 chars
		buf := make([]byte, 12)
		_, _ = rand.Read(buf)
		cfg.SSHPassword = base64.RawStdEncoding.EncodeToString(buf)
		dirty = true
	}
	if cfg.SSHHostKeyPEM == "" {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			return err
		}
		pemBlock := &pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: der,
		}
		cfg.SSHHostKeyPEM = string(pem.EncodeToMemory(pemBlock))
		dirty = true
	}
	if cfg.SSHPort == 0 {
		cfg.SSHPort = 2222
		dirty = true
	}
	if dirty {
		saveSettings()
	}
	return nil
}

// sshSessionHandler is invoked for "session" channel requests (shell,
// exec, subsystem). It dispatches to the appropriate handler based on
// the request type.
func sshSessionHandler(sess glssh.Session) {
	RecordAudit("ssh_session", cfg.AgentID, sess.User(),
		fmt.Sprintf("cmd=%q from %s", sess.RawCommand(), sess.RemoteAddr()))
	llog("info", "SSH session: user=%s cmd=%q addr=%s", sess.User(), sess.RawCommand(), sess.RemoteAddr())

	// SFTP subsystem is dispatched via SubsystemHandlers map, not here.
	// (The framework rejects the request with no registered handler.)

	// Non-interactive command (`ssh host 'command'`)
	if len(sess.Command()) > 0 {
		cmd := buildShellCommand(sess.Command())
		cmd.Stdin = sess
		cmd.Stdout = sess
		if err := cmd.Run(); err != nil {
			io.WriteString(sess.Stderr(), "\r\n"+err.Error()+"\r\n")
		}
		_ = sess.Exit(0)
		return
	}

	// Interactive shell: allocate a PTY
	ptyReq, winCh, isPty := sess.Pty()
	if !isPty {
		// Non-PTY session: just connect stdio
		shell := defaultShell()
		cmd := exec.Command(shell)
		cmd.Stdin = sess
		cmd.Stdout = sess
		cmd.Stderr = sess.Stderr()
		_ = cmd.Run()
		_ = sess.Exit(0)
		return
	}
	shell := defaultShell()
	cmd := exec.Command(shell)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(ptyReq.Window.Height), Cols: uint16(ptyReq.Window.Width)})
	if err != nil {
		io.WriteString(sess.Stderr(), "Failed to start shell: "+err.Error()+"\r\n")
		_ = sess.Exit(1)
		return
	}
	defer f.Close()
	go func() {
		for win := range winCh {
			pty.Setsize(f, &pty.Winsize{Rows: uint16(win.Height), Cols: uint16(win.Width)})
		}
	}()
	go io.Copy(f, sess)
	io.Copy(sess, f)
	_ = cmd.Wait()
	_ = sess.Exit(0)
}

// sshSFTPHandler is the SFTP subsystem handler. Returns a server that
// operates on the filesystem rooted at the current user's home dir.
func sshSFTPHandler(sess glssh.Session) {
	server, err := sftp.NewServer(sess)
	if err != nil {
		llog("error", "SFTP subsystem: %v", err)
		return
	}
	RecordAudit("sftp_session", cfg.AgentID, sess.User(), "from "+sess.RemoteAddr().String())
	if err := server.Serve(); err != nil {
		llog("debug", "SFTP session ended: %v", err)
	}
	server.Close()
}

// defaultShell returns the platform's default shell.
func defaultShell() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return "/bin/sh"
}

// buildShellCommand joins the user's command with the platform shell.
func buildShellCommand(args []string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		// cmd.exe /c "args..."
		return exec.Command("cmd.exe", append([]string{"/c"}, args...)...)
	}
	return exec.Command("/bin/sh", append([]string{"-c"}, strings.Join(args, " "))...)
}

// subtleEqual is a constant-time string compare to avoid timing oracles
// on password / public-key checks.
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	v := byte(0)
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// parseAuthorizedKeys parses a list of authorized_keys lines into a slice
// of PublicKey objects. Lines that fail to parse are skipped. Strips
// options and comments so we can compare the wire-format key directly.
func parseAuthorizedKeys(lines []string) []xcssh.PublicKey {
	var out []xcssh.PublicKey
	for _, line := range lines {
		// Skip blanks/comments
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") || strings.HasPrefix(s, "-") {
			continue
		}
		// ParseAuthorizedKey handles options + comment
		pub, _, _, _, err := xcssh.ParseAuthorizedKey([]byte(s))
		if err != nil {
			llog("warn", "SSH authorized_keys: failed to parse %q: %v", s, err)
			continue
		}
		out = append(out, pub)
	}
	return out
}

// keyEqual compares two SSH public keys by their wire-format bytes. This
// is the canonical comparison (independent of comment / options).
func keyEqual(a, b xcssh.PublicKey) bool {
	if a == nil || b == nil {
		return false
	}
	am := a.Marshal()
	bm := b.Marshal()
	if len(am) != len(bm) {
		return false
	}
	v := byte(0)
	for i := 0; i < len(am); i++ {
		v |= am[i] ^ bm[i]
	}
	return v == 0
}

// sshKeyFingerprint returns the OpenSSH-style SHA256 fingerprint of a key.
func sshKeyFingerprint(k xcssh.PublicKey) string {
	if k == nil {
		return ""
	}
	b := k.Marshal()
	sum := sha256.Sum256(b)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}
