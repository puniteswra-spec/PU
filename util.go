//go:build windows

package main

import "strings"

const Version = "9.0.0"

func normalizeAgentWSURL(url string) string {
	wsURL := strings.TrimSpace(url)
	if strings.HasPrefix(wsURL, "quic://") {
		wsURL = "ws://" + wsURL[7:]
	}
	if !strings.HasPrefix(wsURL, "ws://") && !strings.HasPrefix(wsURL, "wss://") {
		wsURL = "ws://" + wsURL
	}
	if !strings.Contains(wsURL, "/agent/ws") {
		wsURL = strings.TrimRight(wsURL, "/") + "/agent/ws"
	}
	return wsURL
}

func normalizeConfigURL(url string) string {
	url = strings.TrimSpace(url)
	if url == "" {
		return url
	}
	if strings.HasPrefix(url, "quic://") {
		return url
	}
	return normalizeAgentWSURL(url)
}
