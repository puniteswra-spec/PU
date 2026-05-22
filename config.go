//go:build windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultConfigPort = 8181

func LoadConfig() *Config {
	cfg := &Config{
		ConfigPort:          defaultConfigPort,
		QuicPort:            8182, // default QUIC port
		MonthlyLimitMB:      5000,
		MaxAgentBandwidthMB: 10, // default 10 MB/s per agent
		MaxFPS:             15,
		TunnelMode:         "cloudflare",
		AuthUser:           "puneet",
		AuthPass:           "puneet12",
		GitHubRepo:         "puniteswra-spec/PU",
		Autostart:          false,
	}

	path := filepath.Join(dataDir(), "config.json")
	if data, err := os.ReadFile(path); err == nil {
		var file cfgFile
		if err := json.Unmarshal(data, &file); err == nil {
			applyCfgFile(cfg, &file)
		} else {
			llog("warn", "failed to parse config.json: %v", err)
		}
	} else {
		llog("debug", "no config file found at %s: %v", path, err)
	}

	if v := os.Getenv("PUN_SERVER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			cfg.ConfigPort = p
		}
	}
	if v := os.Getenv("PUN_QUIC_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			cfg.QuicPort = p
		}
	}
	if v := os.Getenv("PUN_MAX_AGENT_BANDWIDTH_MB"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			cfg.MaxAgentBandwidthMB = p
		}
	}
	if v := os.Getenv("PUN_SERVER_URL"); v != "" {
		for _, u := range strings.Split(v, ",") {
			u = strings.TrimSpace(u)
			if u != "" {
				cfg.ServerURLs = append(cfg.ServerURLs, u)
			}
		}
	}
	if v := os.Getenv("PUN_AUTH_USER"); v != "" {
		cfg.AuthUser = v
	}
	if v := os.Getenv("PUN_AUTH_PASS"); v != "" {
		cfg.AuthPass = v
	}
	if v := os.Getenv("PUN_MONTHLY_LIMIT_MB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.MonthlyLimitMB = n
		}
	}
	if v := os.Getenv("PUN_TUNNEL"); v != "" {
		cfg.TunnelMode = v
	}
	if v := os.Getenv("PUN_GITHUB_REPO"); v != "" {
		cfg.GitHubRepo = v
	}
	if v := os.Getenv("PUN_GITHUB_TOKEN"); v != "" {
		cfg.GitHubToken = v
	}
	if v := os.Getenv("PUN_DNS_DOMAIN"); v != "" {
		cfg.DNSDomain = v
	}

	return cfg
}

type cfgFile struct {
	ConfigPort          int      `json:"config_port"`
	QuicPort            int      `json:"quic_port"`
	MonthlyLimitMB      int64    `json:"monthly_limit_mb"`
	MaxAgentBandwidthMB int      `json:"max_agent_bandwidth_mb"`
	TunnelMode          string   `json:"tunnel_mode"`
	ServerURLs          []string `json:"server_urls"`
	MaxFPS              float64  `json:"max_fps"`
	GitHubRepo          string   `json:"github_repo"`
	GitHubToken         string   `json:"github_token"`
	AuthUser            string   `json:"auth_user"`
	AuthPass            string   `json:"auth_pass"`
	Autostart           bool     `json:"autostart"`
	StealthMode         bool     `json:"stealth_mode"`
	DNSDomain           string   `json:"dns_domain"`
}

func applyCfgFile(cfg *Config, f *cfgFile) {
	if f.ConfigPort > 0 {
		cfg.ConfigPort = f.ConfigPort
	}
	if f.QuicPort > 0 {
		cfg.QuicPort = f.QuicPort
	}
	if f.MonthlyLimitMB > 0 {
		cfg.MonthlyLimitMB = f.MonthlyLimitMB
	}
	if f.MaxAgentBandwidthMB > 0 {
		cfg.MaxAgentBandwidthMB = f.MaxAgentBandwidthMB
	}
	if f.TunnelMode != "" {
		cfg.TunnelMode = f.TunnelMode
	}
	if len(f.ServerURLs) > 0 {
		cfg.ServerURLs = f.ServerURLs
	}
	if f.MaxFPS > 0 {
		cfg.MaxFPS = f.MaxFPS
	}
	if f.GitHubRepo != "" {
		cfg.GitHubRepo = f.GitHubRepo
	}
	if f.GitHubToken != "" {
		cfg.GitHubToken = f.GitHubToken
	}
	if f.AuthUser != "" {
		cfg.AuthUser = f.AuthUser
	}
	if f.AuthPass != "" {
		cfg.AuthPass = f.AuthPass
	}
	cfg.Autostart = f.Autostart
	cfg.StealthMode = f.StealthMode
	cfg.DNSDomain = f.DNSDomain
}

func saveConfig(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	f := cfgFile{
		ConfigPort:          cfg.ConfigPort,
		QuicPort:            cfg.QuicPort,
		MonthlyLimitMB:      cfg.MonthlyLimitMB,
		MaxAgentBandwidthMB: cfg.MaxAgentBandwidthMB,
		TunnelMode:          cfg.TunnelMode,
		ServerURLs:          cfg.ServerURLs,
		MaxFPS:              cfg.MaxFPS,
		GitHubRepo:          cfg.GitHubRepo,
		GitHubToken:         cfg.GitHubToken,
		AuthUser:            cfg.AuthUser,
		AuthPass:            cfg.AuthPass,
		Autostart:           cfg.Autostart,
		StealthMode:         cfg.StealthMode,
		DNSDomain:           cfg.DNSDomain,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir(), "config.json"), data, 0644)
}
