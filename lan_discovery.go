package main

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	landiscoveryPort     = 8182
	landiscoveryInterval = 3 * time.Second
	landiscoveryTimeout  = 5 * time.Second
	landiscoveryMagic    = "PUNMONITOR_DISCOVER"
)

type LANDiscovery struct {
	mu            sync.Mutex
	servers       map[string]bool
	listener      *net.UDPConn
	broadcast     *net.UDPConn
	done          chan struct{}
	agentID       string
	port          int
	onURLsFound   func([]string)
	knownURLs     map[string]bool
	knownURLsMu   sync.Mutex
	lastBroadcast time.Time
}

type discoverMsg struct {
	Type      string   `json:"type"`
	AgentID   string   `json:"agent_id"`
	Port      int      `json:"port"`
	Host      string   `json:"host"`
	ServerURLs []string `json:"server_urls,omitempty"`
	Version   int      `json:"version,omitempty"`
}

func NewLANDiscovery() *LANDiscovery {
	return &LANDiscovery{
		servers:   make(map[string]bool),
		done:      make(chan struct{}),
		knownURLs: make(map[string]bool),
	}
}

func (ld *LANDiscovery) SetAgentID(id string) {
	ld.mu.Lock()
	defer ld.mu.Unlock()
	ld.agentID = id
}

func (ld *LANDiscovery) SetPort(port int) {
	ld.mu.Lock()
	defer ld.mu.Unlock()
	ld.port = port
}

func (ld *LANDiscovery) SetOnURLsFound(cb func([]string)) {
	ld.mu.Lock()
	defer ld.mu.Unlock()
	ld.onURLsFound = cb
}

func (ld *LANDiscovery) UpdateServerURLs(urls []string) {
	ld.knownURLsMu.Lock()
	defer ld.knownURLsMu.Unlock()
	ld.knownURLs = make(map[string]bool)
	for _, u := range urls {
		ld.knownURLs[u] = true
	}
}

func (ld *LANDiscovery) Start() {
	go ld.listenLoop()
	go ld.broadcastLoop()
	llog("info", "LAN discovery started on port %d", landiscoveryPort)
}

func (ld *LANDiscovery) Stop() {
	close(ld.done)
	if ld.listener != nil {
		ld.listener.Close()
	}
	if ld.broadcast != nil {
		ld.broadcast.Close()
	}
	llog("info", "LAN discovery stopped")
}

func (ld *LANDiscovery) GetServers() []string {
	ld.mu.Lock()
	defer ld.mu.Unlock()
	var list []string
	for addr := range ld.servers {
		list = append(list, addr)
	}
	return list
}

func (ld *LANDiscovery) listenLoop() {
	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf(":%d", landiscoveryPort))
	if err != nil {
		llog("error", "LAN discovery resolve: %v", err)
		return
	}

	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		llog("error", "LAN discovery listen: %v", err)
		return
	}
	ld.listener = conn
	defer conn.Close()

	buf := make([]byte, 4096)
	for {
		select {
		case <-ld.done:
			return
		default:
		}
		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}

		var msg discoverMsg
		if err := json.Unmarshal(buf[:n], &msg); err != nil {
			continue
		}

		if msg.Type == "discover" && msg.AgentID != ld.agentID {
			// Someone is looking for servers, respond with our info
			ld.mu.Lock()
			myAgentID := ld.agentID
			myPort := ld.port
			ld.mu.Unlock()

			// Get current server URLs to share
			ld.knownURLsMu.Lock()
			var serverURLs []string
			for u := range ld.knownURLs {
				serverURLs = append(serverURLs, u)
			}
			ld.knownURLsMu.Unlock()

			resp := discoverMsg{
				Type:       "discover_response",
				AgentID:    myAgentID,
				Port:       myPort,
				Host:       getLocalIP(),
				ServerURLs: serverURLs,
			}
			data, _ := json.Marshal(resp)
			conn.WriteToUDP(data, remoteAddr)
		} else if msg.Type == "discover_response" {
			// Found a server or another agent
			serverURL := fmt.Sprintf("ws://%s:%d/agent/ws", msg.Host, msg.Port)
			ld.mu.Lock()
			if !ld.servers[serverURL] {
				ld.servers[serverURL] = true
				llog("info", "LAN discovery found server: %s", serverURL)
			}
			ld.mu.Unlock()

			// Process shared server URLs from the responding agent
			if len(msg.ServerURLs) > 0 {
				ld.knownURLsMu.Lock()
				var newURLs []string
				for _, u := range msg.ServerURLs {
					u = strings.TrimSpace(u)
					if u != "" && !ld.knownURLs[u] {
						ld.knownURLs[u] = true
						newURLs = append(newURLs, u)
					}
				}
				ld.knownURLsMu.Unlock()

				if len(newURLs) > 0 {
					llog("info", "LAN discovery: received %d new server URLs from %s", len(newURLs), msg.AgentID)
					ld.mu.Lock()
					cb := ld.onURLsFound
					ld.mu.Unlock()
					if cb != nil {
						cb(newURLs)
					}
				}
			}
		}
	}
}

func (ld *LANDiscovery) broadcastLoop() {
	// Set up broadcast socket
	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf(":%d", landiscoveryPort))
	if err != nil {
		return
	}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return
	}
	ld.broadcast = conn
	defer conn.Close()

	ticker := time.NewTicker(landiscoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ld.done:
			return
		case <-ticker.C:
			ld.mu.Lock()
			myAgentID := ld.agentID
			ld.mu.Unlock()
			
			// Get current URLs to share
			ld.knownURLsMu.Lock()
			var urlsToShare []string
			for u := range ld.knownURLs {
				urlsToShare = append(urlsToShare, u)
			}
			ld.knownURLsMu.Unlock()
			
			msg := discoverMsg{
				Type:       "discover",
				AgentID:    myAgentID,
				Port:       ld.port,
				Host:       getLocalIP(),
				ServerURLs: urlsToShare,
			}
			data, _ := json.Marshal(msg)

			// Broadcast to all local subnets
			broadcastAddrs := getBroadcastAddresses()
			for _, broadcastAddr := range broadcastAddrs {
				bcastUDP, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", broadcastAddr, landiscoveryPort))
				if err != nil {
					continue
				}
				conn.WriteToUDP(data, bcastUDP)
			}
		}
	}
}

func getBroadcastAddresses() []string {
	var addrs []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return addrs
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagBroadcast == 0 {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrsList, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrsList {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if ipnet.IP.To4() != nil {
					ip := ipnet.IP.To4()
					mask := ipnet.Mask
					broadcast := net.IPv4(ip[0]|^mask[0], ip[1]|^mask[1], ip[2]|^mask[2], ip[3]|^mask[3])
					addrs = append(addrs, broadcast.String())
				}
			}
		}
	}
	return addrs
}

// Helper to check if URL is a LAN URL
func isLANURL(url string) bool {
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
	return strings.HasPrefix(host, "192.168.") || strings.HasPrefix(host, "10.") || strings.HasPrefix(host, "172.16.") || strings.HasPrefix(host, "127.")
}
