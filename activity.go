package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type SessionState struct {
	BootTimeMS       int64  `json:"boot_time_ms"`
	LastStartupMS    int64  `json:"last_startup_ms"`
	LastShutdownMS   int64  `json:"last_shutdown_ms"`
	LastIdleStartMS  int64  `json:"last_idle_start_ms"`
	LastActiveMS     int64  `json:"last_active_ms"`
	LastWakeMS       int64  `json:"last_wake_ms"`
	LastShutdownNote string `json:"last_shutdown_note,omitempty"`
}

type ActivityStore struct {
	mu    sync.Mutex
	state SessionState
	path  string
	log   string
}

var (
	globalActivity   *ActivityStore
	activityInitOnce sync.Once
)

func initActivityStore() *ActivityStore {
	activityInitOnce.Do(func() {
		dir := dataDir()
		s := &ActivityStore{
			path: filepath.Join(dir, "session_state.json"),
			log:  filepath.Join(dir, "activity_log.jsonl"),
		}
		s.load()
		now := time.Now().UnixMilli()
		boot := systemBootTimeMS()
		s.mu.Lock()
		if boot > 0 && boot != s.state.BootTimeMS {
			s.state.BootTimeMS = boot
			s.recordLocked("system_startup", formatTime(boot)+" — system boot")
			s.state.LastWakeMS = now
		}
		s.state.LastStartupMS = now
		s.recordLocked("agent_start", "PunMonitor started")
		s.mu.Unlock()
		s.save()
		globalActivity = s
	})
	return globalActivity
}

func (s *ActivityStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		llog("warn", "failed to read activity store: %v", err)
		return
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		llog("warn", "failed to parse activity store: %v", err)
	}
}

func (s *ActivityStore) save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		llog("error", "failed to marshal activity state: %v", err)
		return
	}
	if err := os.WriteFile(s.path, data, 0644); err != nil {
		llog("error", "failed to write activity store: %v", err)
	}
}

func (s *ActivityStore) Record(typ, detail string) {
	s.mu.Lock()
	s.recordLocked(typ, detail)
	s.mu.Unlock()
	s.save()
}

func (s *ActivityStore) recordLocked(typ, detail string) {
	now := time.Now().UnixMilli()
	switch typ {
	case "user_idle_start":
		s.state.LastIdleStartMS = now
	case "user_active":
		s.state.LastActiveMS = now
		if s.state.LastIdleStartMS > 0 {
			d := time.Duration(now-s.state.LastIdleStartMS) * time.Millisecond
			if detail == "" {
				detail = "idle for " + d.Round(time.Second).String()
			}
		}
	case "system_shutdown", "agent_stop":
		s.state.LastShutdownMS = now
		s.state.LastShutdownNote = detail
	case "system_startup", "agent_start":
		if typ == "system_startup" {
			s.state.LastWakeMS = now
		}
	}
	ev := ActivityEvent{Timestamp: now, Type: typ, Detail: detail}
	appendActivityLog(s.log, ev)
}

func appendActivityLog(path string, ev ActivityEvent) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		llog("error", "failed to open activity log: %v", err)
		return
	}
	defer f.Close()
	line, err := json.Marshal(ev)
	if err != nil {
		llog("error", "failed to marshal activity event: %v", err)
		return
	}
	if _, err := f.Write(line); err != nil {
		llog("error", "failed to write activity log: %v", err)
	}
	if _, err := f.Write([]byte("\n")); err != nil {
		llog("error", "failed to write newline to activity log: %v", err)
	}
}

func (s *ActivityStore) Summary() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]string{
		"boot_time":         formatTime(s.state.BootTimeMS),
		"last_startup":      formatTime(s.state.LastStartupMS),
		"last_shutdown":     formatTime(s.state.LastShutdownMS),
		"last_idle_start":   formatTime(s.state.LastIdleStartMS),
		"last_active":       formatTime(s.state.LastActiveMS),
		"last_wake":         formatTime(s.state.LastWakeMS),
		"last_shutdown_note": s.state.LastShutdownNote,
	}
}

func (s *ActivityStore) RecentEvents(max int) []ActivityEvent {
	f, err := os.Open(s.log)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	b, _ := os.ReadFile(s.log)
	for _, line := range splitLines(string(b)) {
		if line != "" {
			lines = append(lines, line)
		}
	}
	var out []ActivityEvent
	start := 0
	if len(lines) > max {
		start = len(lines) - max
	}
	for _, line := range lines[start:] {
		var ev ActivityEvent
		if json.Unmarshal([]byte(line), &ev) == nil {
			out = append(out, ev)
		}
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func formatTime(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}

func recordShutdown(reason string) {
	if globalActivity == nil {
		initActivityStore()
	}
	globalActivity.Record("system_shutdown", reason)
	globalActivity.Record("agent_stop", reason)
	globalActivity.save()
}

func updateSystemInfoFromActivity(info map[string]string) {
	if globalActivity == nil {
		initActivityStore()
	}
	for k, v := range globalActivity.Summary() {
		info[k] = v
	}
	info["last_idle"] = globalActivity.Summary()["last_idle_start"]
	info["last_wake"] = globalActivity.Summary()["last_wake"]
}
