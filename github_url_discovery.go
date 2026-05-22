//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ServerList is the format expected from GitHub servers.json
type ServerList struct {
	Servers []string `json:"servers"`
	Version int      `json:"version"`
}

// GitHubURLChecker periodically checks GitHub for updated server URLs
type GitHubURLChecker struct {
	cfg         *Config
	onURLsFound func([]string)
	mu          sync.Mutex
	lastVersion int
	lastCheck   time.Time
	done        chan struct{}
}

func NewGitHubURLChecker(cfg *Config, onURLsFound func([]string)) *GitHubURLChecker {
	return &GitHubURLChecker{
		cfg:         cfg,
		onURLsFound: onURLsFound,
		done:        make(chan struct{}),
		lastVersion: loadLastGitHubVersion(),
	}
}

func (g *GitHubURLChecker) Start() {
	go g.checkLoop()
	llog("info", "GitHub URL checker started (24h interval)")
}

func (g *GitHubURLChecker) Stop() {
	close(g.done)
}

func (g *GitHubURLChecker) checkLoop() {
	// Check immediately on start
	g.checkOnce()

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-g.done:
			return
		case <-ticker.C:
			g.checkOnce()
		}
	}
}

func (g *GitHubURLChecker) checkOnce() {
	g.mu.Lock()
	defer g.mu.Unlock()

	url := g.getServerListURL()
	if url == "" {
		return
	}

	llog("info", "GitHub URL checker: fetching %s", url)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		llog("warn", "GitHub URL checker: fetch failed: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		llog("warn", "GitHub URL checker: status %d", resp.StatusCode)
		return
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		llog("warn", "GitHub URL checker: read failed: %v", err)
		return
	}

	var list ServerList
	if err := json.Unmarshal(data, &list); err != nil {
		llog("warn", "GitHub URL checker: parse failed: %v", err)
		return
	}

	if len(list.Servers) == 0 {
		llog("info", "GitHub URL checker: no servers in list")
		return
	}

	// Normalize URLs
	var normalized []string
	for _, u := range list.Servers {
		u = strings.TrimSpace(u)
		if u != "" {
			normalized = append(normalized, normalizeConfigURL(u))
		}
	}

	// Check if version changed or URLs differ
	if list.Version > g.lastVersion {
		llog("info", "GitHub URL checker: new version %d, updating URLs", list.Version)
		g.lastVersion = list.Version
		g.lastCheck = time.Now()
		saveLastGitHubVersion(list.Version)

		// Update config
		g.cfg.SetServerURLs(normalized)

		// Notify agent to reconnect
		if g.onURLsFound != nil {
			g.onURLsFound(normalized)
		}
	} else {
		llog("info", "GitHub URL checker: no change (version %d)", list.Version)
	}
}

func (g *GitHubURLChecker) getServerListURL() string {
	// Option 1: Direct URL from env/config
	if v := os.Getenv("PUN_SERVER_LIST_URL"); v != "" {
		return v
	}

	// Option 2: Derive from GitHub repo
	repo := g.cfg.GitHubRepo
	if repo == "" {
		return ""
	}

	// Format: https://raw.githubusercontent.com/{user}/{repo}/main/servers.json
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/main/servers.json", repo)
}

func loadLastGitHubVersion() int {
	path := filepath.Join(dataDir(), "github_version.txt")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	var v int
	fmt.Sscanf(string(data), "%d", &v)
	return v
}

func saveLastGitHubVersion(version int) {
	path := filepath.Join(dataDir(), "github_version.txt")
	os.WriteFile(path, []byte(fmt.Sprintf("%d", version)), 0644)
}
