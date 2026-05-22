package main

import (
	"net"
	"strings"
	"sync"
	"time"
)

// DNSURLChecker periodically checks DNS TXT records for updated server URLs
type DNSURLChecker struct {
	domain      string
	onURLsFound func([]string)
	mu          sync.Mutex
	lastCheck   time.Time
	lastURLs    []string
	done        chan struct{}
}

func NewDNSURLChecker(domain string, onURLsFound func([]string)) *DNSURLChecker {
	return &DNSURLChecker{
		domain:      domain,
		onURLsFound: onURLsFound,
		done:        make(chan struct{}),
	}
}

func (d *DNSURLChecker) Start() {
	go d.checkLoop()
	llog("info", "DNS URL checker started for domain: %s (1h interval)", d.domain)
}

func (d *DNSURLChecker) Stop() {
	close(d.done)
}

func (d *DNSURLChecker) checkLoop() {
	// Check immediately on start
	d.checkOnce()

	// Check every hour
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-d.done:
			return
		case <-ticker.C:
			d.checkOnce()
		}
	}
}

func (d *DNSURLChecker) checkOnce() {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.domain == "" {
		return
	}

	llog("info", "DNS URL checker: looking up TXT record for _punmonitor.%s", d.domain)

	// Try multiple possible record names
	recordNames := []string{
		"_punmonitor." + d.domain,
		"_punmonitor-servers." + d.domain,
		"_remote-monitor." + d.domain,
	}

	var urls []string
	for _, name := range recordNames {
		txts, err := net.LookupTXT(name)
		if err != nil {
			continue
		}
		for _, txt := range txts {
			// Parse TXT record: "ws://server1:8181/agent/ws,wss://server2:443/agent/ws"
			parts := strings.Split(txt, ",")
			for _, part := range parts {
				part = strings.TrimSpace(part)
				if part != "" && (strings.HasPrefix(part, "ws://") || strings.HasPrefix(part, "wss://") || strings.HasPrefix(part, "quic://") || strings.HasPrefix(part, "webrtc://")) {
					urls = append(urls, normalizeConfigURL(part))
				}
			}
		}
		if len(urls) > 0 {
			break
		}
	}

	if len(urls) == 0 {
		llog("info", "DNS URL checker: no URLs found in TXT records")
		return
	}

	// Check if URLs changed
	if !urlsEqual(d.lastURLs, urls) {
		llog("info", "DNS URL checker: new URLs found: %v", urls)
		d.lastURLs = urls
		d.lastCheck = time.Now()

		if d.onURLsFound != nil {
			d.onURLsFound(urls)
		}
	} else {
		llog("info", "DNS URL checker: URLs unchanged")
	}
}

func urlsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
