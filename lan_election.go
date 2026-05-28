package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

type LANLeaderElection struct {
	discovery      *PeerDiscovery
	mu             sync.Mutex
	isLeader       bool
	leaderID       string
	leaderIP       string
	leaderPort     int
	onBecomeLeader func()
	onLoseLeader   func(serverURL string)
	serverChecked  map[string]bool
}

func NewLANLeaderElection(discovery *PeerDiscovery) *LANLeaderElection {
	return &LANLeaderElection{
		discovery:     discovery,
		serverChecked: make(map[string]bool),
	}
}

func (le *LANLeaderElection) SetCallbacks(onLeader func(), onLose func(string)) {
	le.mu.Lock()
	defer le.mu.Unlock()
	le.onBecomeLeader = onLeader
	le.onLoseLeader = onLose
}

func (le *LANLeaderElection) Start(ctx context.Context) {
	le.discovery.SetCallbacks(
		func(peer *PeerInfo) { le.onPeerUpdate(peer) },
		func(id string) { le.onPeerLost(id) },
	)

	le.discovery.Start()

	go le.electionLoop(ctx)
}

func (le *LANLeaderElection) electionLoop(ctx context.Context) {
	time.Sleep(3 * time.Second)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			le.runElection()
		}
	}
}

func (le *LANLeaderElection) runElection() {
	le.mu.Lock()
	le.discovery.UpdateSelf(le.currentMode(), le.isLeader)
	le.mu.Unlock()

	peers := le.discovery.GetPeers()

	all := make([]*PeerInfo, 0, len(peers)+1)
	all = append(all, le.discovery.self)
	all = append(all, peers...)

	sort.Slice(all, func(i, j int) bool {
		if all[i].Mode == "server" && all[j].Mode != "server" {
			return true
		}
		if all[j].Mode == "server" && all[i].Mode != "server" {
			return false
		}
		if all[i].Uptime != all[j].Uptime {
			return all[i].Uptime > all[j].Uptime
		}
		return all[i].AgentID < all[j].AgentID
	})

	serverPeer := le.discovery.GetServer()

	le.mu.Lock()
	wasLeader := le.isLeader
	le.mu.Unlock()

	if serverPeer != nil && serverPeer.AgentID != le.discovery.self.AgentID {
		alive := le.checkServerAlive(serverPeer.IP, serverPeer.Port)

		le.mu.Lock()
		if alive {
			if wasLeader {
				llog("info", "LAN: Another server reachable (%s at %s) — stepping down", serverPeer.Hostname, serverPeer.IP)
				le.isLeader = false
				le.leaderID = serverPeer.AgentID
				le.leaderIP = serverPeer.IP
				le.leaderPort = serverPeer.Port
				cb := le.onLoseLeader
				url := fmt.Sprintf("http://%s:%d", serverPeer.IP, serverPeer.Port)
				le.mu.Unlock()
				if cb != nil {
					go cb(url)
				}
				return
			}

			le.leaderID = serverPeer.AgentID
			le.leaderIP = serverPeer.IP
			le.leaderPort = serverPeer.Port
			cb := le.onLoseLeader
			url := fmt.Sprintf("http://%s:%d", serverPeer.IP, serverPeer.Port)
			le.mu.Unlock()
			llog("info", "LAN: Server %s at %s:%d — connecting", serverPeer.Hostname, serverPeer.IP, serverPeer.Port)
			if cb != nil {
				go cb(url)
			}
			return
		}
		le.mu.Unlock()
	}

	if !wasLeader {
		le.mu.Lock()
		winner := all[0]
		if winner.AgentID == le.discovery.self.AgentID {
			llog("info", "LAN: No reachable server — elected self as leader (id=%s, uptime=%.0fs)", winner.AgentID, winner.Uptime)
			le.isLeader = true
			le.leaderID = le.discovery.self.AgentID
			le.discovery.UpdateSelf("server", true)
			cb := le.onBecomeLeader
			le.mu.Unlock()
			if cb != nil {
				go cb()
			}
		} else {
			le.leaderID = winner.AgentID
			le.leaderIP = winner.IP
			le.leaderPort = winner.Port
			cb := le.onLoseLeader
			url := fmt.Sprintf("http://%s:%d", winner.IP, winner.Port)
			le.mu.Unlock()
			llog("info", "LAN: Elected %s as leader (uptime=%.0fs)", winner.Hostname, winner.Uptime)
			if cb != nil {
				go cb(url)
			}
		}
	}
}

func (le *LANLeaderElection) checkServerAlive(ip string, port int) bool {
	if ip == "" || port == 0 {
		return false
	}
	url := fmt.Sprintf("http://%s:%d/api/health", ip, port)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func (le *LANLeaderElection) onPeerUpdate(peer *PeerInfo) {
	if peer.Mode == "server" && peer.AgentID != le.discovery.self.AgentID {
		if le.checkServerAlive(peer.IP, peer.Port) {
			le.mu.Lock()
			previousLeader := le.leaderID
			le.leaderID = peer.AgentID
			le.leaderIP = peer.IP
			le.leaderPort = peer.Port
			wasLeader := le.isLeader
			le.isLeader = false
			cb := le.onLoseLeader
			url := fmt.Sprintf("http://%s:%d", peer.IP, peer.Port)
			le.mu.Unlock()

			if wasLeader {
				llog("info", "LAN: Server %s appeared — stepping down", peer.Hostname)
				if cb != nil {
					go cb(url)
				}
			} else if previousLeader != peer.AgentID {
				llog("info", "LAN: Discovered server %s at %s:%d — connecting", peer.Hostname, peer.IP, peer.Port)
				if cb != nil {
					go cb(url)
				}
			}
		}
	}
}

func (le *LANLeaderElection) onPeerLost(id string) {
	le.mu.Lock()
	defer le.mu.Unlock()
	if id == le.leaderID && le.isLeader {
		llog("info", "LAN: Former leader %s disappeared — re-electing soon", id)
		le.leaderID = ""
		le.leaderIP = ""
		le.leaderPort = 0
	}
}

func (le *LANLeaderElection) currentMode() string {
	if le.isLeader {
		return "server"
	}
	if le.leaderID != "" {
		return "agent"
	}
	return "standalone"
}

func (le *LANLeaderElection) IsLeader() bool {
	le.mu.Lock()
	defer le.mu.Unlock()
	return le.isLeader
}

func (le *LANLeaderElection) GetServerURL() string {
	le.mu.Lock()
	defer le.mu.Unlock()
	if le.leaderIP == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", le.leaderIP, le.leaderPort)
}

func (le *LANLeaderElection) GetLeaderID() string {
	le.mu.Lock()
	defer le.mu.Unlock()
	return le.leaderID
}
