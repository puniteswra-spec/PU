package main

// HTTP/3 (QUIC) transport for the PunMonitor agent↔server link.
//
// QUIC gives us a UDP-based, multiplexed, low-latency transport that works
// through most NATs and firewalls without port forwarding. It's the same
// transport Chrome and other browsers use for HTTP/3, so most ISP
// middleboxes already pass it through.
//
// On the server, startQUICServer() listens on a UDP port (default 4444)
// and accepts incoming agent connections. Each connection runs the same
// WireMessage protocol used by the WebSocket transport.
//
// On the agent, tryAgentQUIC() dials the server's QUIC port and runs the
// same hello handshake as tryAgentWebSocket. If the dial fails, the agent
// falls back to WebSocket or GitHub as before.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go"
)

// Default QUIC port for the agent transport. 4444 keeps it independent
// of SSH (22), HTTPS (443), and our HTTP config port (8080).
const defaultQUICPort = 4444

// quicAgents tracks the set of currently QUIC-connected agents.
var quicAgents sync.Map // map[string]*quicAgentConn

type quicAgentConn struct {
	conn   *quic.Conn
	stream *quic.Stream
	id     string
}

// startQUICServer launches a background goroutine that listens for
// incoming QUIC connections from agents.
func startQUICServer() {
	go func() {
		addr := fmt.Sprintf("0.0.0.0:%d", defaultQUICPort)
		tlsConfig := getQuicTLSConfig()
		if tlsConfig == nil {
			return // getQuicTLSConfig already logged the reason
		}
		listener, err := quic.ListenAddr(addr, tlsConfig, &quic.Config{
			MaxIdleTimeout:       120 * time.Second,
			HandshakeIdleTimeout: 10 * time.Second,
			KeepAlivePeriod:      20 * time.Second,
		})
		if err != nil {
			llog("warn", "QUIC server: failed to listen on %s: %v (agent QUIC transport will not be available)", addr, err)
			return
		}
		llog("info", "QUIC server listening on %s (UDP — agents can use HTTP/3 transport)", addr)
		for {
			conn, err := listener.Accept(context.Background())
			if err != nil {
				llog("warn", "QUIC accept error: %v", err)
				time.Sleep(1 * time.Second)
				continue
			}
			go handleQUICConnection(conn)
		}
	}()
}

// handleQUICConnection waits for a stream from a QUIC connection, reads
// the agent's hello, and holds the connection open. Admin→agent traffic
// continues to flow over WebSocket; QUIC is currently an additional
// keep-alive channel for transport-priority purposes.
func handleQUICConnection(conn *quic.Conn) {
	defer conn.CloseWithError(0, "server closing")
	stream, err := conn.AcceptStream(context.Background())
	if err != nil {
		llog("warn", "QUIC accept stream: %v", err)
		return
	}
	defer stream.Close()
	llog("info", "QUIC agent connected from %s", conn.RemoteAddr())

	// Read the hello (first frame must be a hello JSON, like WebSocket)
	data := make([]byte, 65536)
	n, err := stream.Read(data)
	if err != nil {
		llog("warn", "QUIC hello read: %v", err)
		return
	}
	var hello map[string]interface{}
	if err := json.Unmarshal(data[:n], &hello); err != nil {
		llog("warn", "QUIC hello parse: %v", err)
		return
	}
	agentID, _ := hello["agentId"].(string)
	isAgent, _ := hello["agent"].(bool)
	llog("info", "QUIC hello from %s (agent=%v)", agentID, isAgent)

	if !isAgent || agentID == "" {
		llog("info", "QUIC: non-agent or empty agentId, closing")
		return
	}

	// Register so /api/transport-status can show the QUIC path is live
	qt := &quicAgentConn{conn: conn, stream: stream, id: agentID}
	quicAgents.Store(agentID, qt)
	defer quicAgents.Delete(agentID)
	healthChecker.Register("quic")
	healthChecker.Heartbeat("quic")

	// Send hello ack so the agent knows the handshake completed
	ack, _ := json.Marshal(map[string]string{"type": "hello_ack", "agentId": cfg.AgentID})
	if _, err := stream.Write(ack); err != nil {
		llog("warn", "QUIC hello_ack write: %v", err)
		return
	}

	// Hold the connection open. Ping every 30s so the agent's
	// healthChecker sees a live QUIC transport.
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	pingMsg, _ := json.Marshal(map[string]string{"type": "ping"})
	for {
		select {
		case <-ping.C:
			if _, err := stream.Write(pingMsg); err != nil {
				llog("info", "QUIC agent %s ping write failed: %v (closing)", agentID, err)
				healthChecker.ReportFailure("quic", err)
				return
			}
			healthChecker.Heartbeat("quic")
		}
	}
}

// getQuicTLSConfig returns a TLS config suitable for QUIC. Reuses the
// existing self-signed cert from ensureTLSCert(). QUIC requires TLS 1.3.
func getQuicTLSConfig() *tls.Config {
	certFile, keyFile, err := ensureTLSCert()
	if err != nil || certFile == "" {
		llog("warn", "QUIC TLS: ensureTLSCert failed: %v — QUIC server will not start", err)
		return nil
	}
	tlsConfig, err := createTLSConfig(certFile, keyFile)
	if err != nil {
		llog("warn", "QUIC TLS: createTLSConfig failed: %v", err)
		return nil
	}
	tlsConfig.MinVersion = tls.VersionTLS13
	return tlsConfig
}

// tryAgentQUIC is the agent-side QUIC transport. Dials the server over
// UDP, sends a hello, and holds the connection open. Returns true if
// the handshake succeeded.
func tryAgentQUIC(hostname, serverURL string) bool {
	addr := quicAddrFromServerURL(serverURL)
	if addr == "" {
		return false
	}
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true, // self-signed cert; agents don't have a CA store
		MinVersion:         tls.VersionTLS13,
		ServerName:         strings.Split(addr, ":")[0],
	}
	conn, err := quic.DialAddr(context.Background(), addr, tlsConfig, &quic.Config{
		MaxIdleTimeout:       120 * time.Second,
		HandshakeIdleTimeout: 10 * time.Second,
		KeepAlivePeriod:      20 * time.Second,
	})
	if err != nil {
		llog("error", "Agent QUIC dial to %s failed: %v", addr, err)
		return false
	}
	defer conn.CloseWithError(0, "agent done")
	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		llog("error", "Agent QUIC open stream: %v", err)
		return false
	}
	defer stream.Close()
	llog("info", "Agent QUIC connected to %s", addr)

	// Send hello
	sysInfo := map[string]string{
		"hostname":  getHostname(),
		"local_ip":  getLocalIP(),
		"wan_ip":    getWANIP(),
		"os":        runtime.GOOS,
		"arch":      runtime.GOARCH,
		"uptime":    fmt.Sprintf("%.0f", time.Since(startTime).Seconds()),
		"version":   binaryVersion,
		"mode":      "agent",
		"transport": "quic",
	}
	hello, _ := json.Marshal(map[string]interface{}{
		"type":       "hello",
		"agentId":    hostname,
		"agent":      true,
		"systemInfo": sysInfo,
	})
	if _, err := stream.Write(hello); err != nil {
		llog("error", "Agent QUIC hello write: %v", err)
		return false
	}

	// Wait for hello_ack
	data := make([]byte, 65536)
	n, err := stream.Read(data)
	if err != nil {
		llog("error", "Agent QUIC hello_ack read: %v", err)
		return false
	}
	var ack map[string]interface{}
	if err := json.Unmarshal(data[:n], &ack); err != nil {
		llog("error", "Agent QUIC hello_ack parse: %v", err)
		return false
	}
	if ack["type"] != "hello_ack" {
		llog("warn", "Agent QUIC: unexpected ack type %v", ack["type"])
		return false
	}
	llog("info", "Agent QUIC: hello_ack received, transport live")

	// Set agent transport indicator so the dashboard can show "quic"
	setAgentTransport("quic")

	// Hold the connection open. Read pings from server.
	stream.SetReadDeadline(time.Time{})
	for {
		stream.SetReadDeadline(time.Now().Add(120 * time.Second))
		n, err := stream.Read(data)
		if err != nil {
			llog("info", "Agent QUIC read end: %v", err)
			return true
		}
		_ = n
	}
}

// quicAddrFromServerURL derives "host:4444" from the agent's serverURL
// (e.g., "https://relay.recruitedge.us" → "relay.recruitedge.us:4444").
func quicAddrFromServerURL(serverURL string) string {
	s := strings.TrimPrefix(serverURL, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "wss://")
	s = strings.TrimPrefix(s, "ws://")
	s = strings.TrimSuffix(s, "/")
	if s == "" {
		return ""
	}
	if !strings.Contains(s, ":") {
		s = s + ":4444"
	}
	return s
}

// setAgentTransport records the active transport name so the dashboard
// can show it. Falls back to a no-op if the helper doesn't exist in
// main.go (added as part of this change).
func setAgentTransport(name string) {
	if agentActiveTransport != nil {
		agentActiveTransport(name)
	}
	_ = websocket.TextMessage // keep gorilla/websocket import used
}
