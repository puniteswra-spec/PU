package main

import (
	"encoding/json"
	"net"
	"sync"
	"time"
)

const (
	discoveryPort = 9999
	broadcastAddr = "255.255.255.255:9999"
	peerTimeout   = 15 * time.Second
)

type PeerInfo struct {
	AgentID  string    `json:"agentId"`
	Hostname string    `json:"hostname"`
	IP       string    `json:"ip"`
	Port     int       `json:"port"`
	Mode     string    `json:"mode"` // "server", "agent", "standalone"
	Version  string    `json:"version"`
	Uptime   float64   `json:"uptime"`
	LastSeen time.Time `json:"-"`
	IsLeader bool      `json:"isLeader"`
}

type PeerDiscovery struct {
	mu          sync.RWMutex
	peers       map[string]*PeerInfo
	self        *PeerInfo
	conn        *net.UDPConn
	onPeerFound func(*PeerInfo)
	onPeerLost  func(string)
	stopCh      chan struct{}
}

var globalDiscovery *PeerDiscovery

func NewPeerDiscovery(self *PeerInfo) *PeerDiscovery {
	return &PeerDiscovery{
		peers:  make(map[string]*PeerInfo),
		self:   self,
		stopCh: make(chan struct{}),
	}
}

func (pd *PeerDiscovery) SetCallbacks(onFound func(*PeerInfo), onLost func(string)) {
	pd.mu.Lock()
	defer pd.mu.Unlock()
	pd.onPeerFound = onFound
	pd.onPeerLost = onLost
}

func (pd *PeerDiscovery) Start() error {
	addr := &net.UDPAddr{Port: discoveryPort}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		llog("error", "UDP discovery listen failed: %v", err)
		return err
	}
	pd.conn = conn

	go pd.listenLoop()
	go pd.broadcastLoop()
	go pd.cleanupLoop()

	llog("info", "LAN discovery started on UDP port %d", discoveryPort)
	return nil
}

func (pd *PeerDiscovery) Stop() {
	close(pd.stopCh)
	if pd.conn != nil {
		pd.conn.Close()
	}
}

func (pd *PeerDiscovery) listenLoop() {
	buf := make([]byte, 4096)
	for {
		select {
		case <-pd.stopCh:
			return
		default:
		}
		pd.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, raddr, err := pd.conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		var info PeerInfo
		if err := json.Unmarshal(buf[:n], &info); err != nil {
			continue
		}
		if info.AgentID == pd.self.AgentID {
			continue
		}
		info.IP = raddr.IP.String()
		info.LastSeen = time.Now()

		pd.mu.Lock()
		existing, exists := pd.peers[info.AgentID]
		pd.peers[info.AgentID] = &info
		pd.mu.Unlock()

		if !exists {
			llog("info", "LAN peer discovered: %s at %s:%d (mode=%s)", info.Hostname, info.IP, info.Port, info.Mode)
			pd.mu.RLock()
			cb := pd.onPeerFound
			pd.mu.RUnlock()
			if cb != nil {
				cb(&info)
			}
		} else {
			existing.LastSeen = time.Now()
			existing.Mode = info.Mode
			existing.Version = info.Version
			existing.Uptime = info.Uptime
			existing.IsLeader = info.IsLeader
		}
	}
}

func (pd *PeerDiscovery) broadcastLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	// Use subnet broadcast for better WiFi compatibility
	bcastAddr := GetBroadcastIP()
	addr, _ := net.ResolveUDPAddr("udp4", bcastAddr)

	for {
		select {
		case <-pd.stopCh:
			return
		case <-ticker.C:
			pd.mu.RLock()
			data, _ := json.Marshal(pd.self)
			pd.mu.RUnlock()

			if _, err := pd.conn.WriteToUDP(data, addr); err != nil {
				llog("debug", "UDP broadcast failed: %v", err)
			}
		}
	}
}

func (pd *PeerDiscovery) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-pd.stopCh:
			return
		case <-ticker.C:
			pd.mu.Lock()
			now := time.Now()
			for id, peer := range pd.peers {
				if now.Sub(peer.LastSeen) > peerTimeout {
					llog("info", "LAN peer timed out: %s (%s)", peer.Hostname, id)
					delete(pd.peers, id)
					pd.mu.Unlock()
					pd.mu.RLock()
					cb := pd.onPeerLost
					pd.mu.RUnlock()
					if cb != nil {
						cb(id)
					}
					pd.mu.Lock()
				}
			}
			pd.mu.Unlock()
		}
	}
}

func (pd *PeerDiscovery) GetPeers() []*PeerInfo {
	pd.mu.RLock()
	defer pd.mu.RUnlock()
	list := make([]*PeerInfo, 0, len(pd.peers))
	for _, p := range pd.peers {
		list = append(list, p)
	}
	return list
}

func (pd *PeerDiscovery) GetServer() *PeerInfo {
	pd.mu.RLock()
	defer pd.mu.RUnlock()
	for _, p := range pd.peers {
		if p.Mode == "server" {
			return p
		}
	}
	return nil
}

func (pd *PeerDiscovery) UpdateSelf(mode string, isLeader bool) {
	pd.mu.Lock()
	defer pd.mu.Unlock()
	pd.self.Mode = mode
	pd.self.IsLeader = isLeader
	pd.self.Uptime = time.Since(startTime).Seconds()
}

func GetBroadcastIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return broadcastAddr
	}
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		ip := ipnet.IP.To4()
		if ip[0] == 169 && ip[1] == 254 {
			continue
		}
		bcast := make(net.IP, 4)
		for i := 0; i < 4; i++ {
			bcast[i] = ip[i] | ^ipnet.Mask[i]
		}
		return bcast.String() + ":9999"
	}
	return broadcastAddr
}
