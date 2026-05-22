//go:build windows

package main

import (
	"math"
	"sync"
	"time"
)

type poolEntry struct {
	transport Transport
	priority  int
}

type TransportPool struct {
	mu      sync.RWMutex
	entries map[string]poolEntry
}

func NewTransportPool() *TransportPool {
	return &TransportPool{entries: make(map[string]poolEntry)}
}

func (tp *TransportPool) Add(id string, t Transport) {
	if tp == nil || t == nil {
		return
	}
	pri := math.MaxInt32
	if wt, ok := t.(*wsTransport); ok {
		pri = wt.Priority()
	} else if qt, ok := t.(*quicTransport); ok {
		pri = qt.Priority()
	} else if wrt, ok := t.(*webrtcTransport); ok {
		pri = wrt.Priority()
	}
	tp.mu.Lock()
	tp.entries[id] = poolEntry{transport: t, priority: pri}
	tp.mu.Unlock()
}

func (tp *TransportPool) Remove(id string) {
	tp.mu.Lock()
	delete(tp.entries, id)
	tp.mu.Unlock()
}

func (tp *TransportPool) GetBest() Transport {
	tp.mu.RLock()
	defer tp.mu.RUnlock()
	var best Transport
	bestPri := math.MaxInt32
	for _, e := range tp.entries {
		if e.priority < bestPri {
			bestPri = e.priority
			best = e.transport
		}
	}
	return best
}

type HealthChecker struct {
	mu     sync.Mutex
	dead   map[string]bool
	onDead func(string)
}

func NewHealthChecker() *HealthChecker {
	return &HealthChecker{dead: make(map[string]bool)}
}

func (hc *HealthChecker) SetOnDead(cb func(string)) { hc.onDead = cb }
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

type ReconnectManager struct {
	mu       sync.Mutex
	backoff  map[string]time.Duration
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

func (rm *ReconnectManager) RecordFailure(url string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	d := rm.backoff[url]
	if d == 0 {
		d = time.Second
	} else if d < 30*time.Second {
		d *= 2
	}
	rm.backoff[url] = d
}

func (rm *ReconnectManager) RecordSuccess(url string) {
	rm.mu.Lock()
	delete(rm.backoff, url)
	rm.mu.Unlock()
}
