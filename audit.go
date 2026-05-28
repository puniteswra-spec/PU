package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type AuditEntry struct {
	Timestamp int64  `json:"ts"`
	Action    string `json:"action"`
	AgentID   string `json:"agentId"`
	User      string `json:"user"`
	Detail    string `json:"detail,omitempty"`
}

type AuditLog struct {
	mu      sync.Mutex
	entries []AuditEntry
	path    string
}

var globalAudit *AuditLog

func InitAuditLog() {
	dir := dataDir()
	_ = os.MkdirAll(dir, 0755)
	path := filepath.Join(dir, "audit.jsonl")
	al := &AuditLog{path: path}
	al.load()
	globalAudit = al
}

func (al *AuditLog) load() {
	data, err := os.ReadFile(al.path)
	if err != nil {
		return
	}
	lines := splitLines(string(data))
	for _, line := range lines {
		if line == "" {
			continue
		}
		var e AuditEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			al.entries = append(al.entries, e)
		}
	}
}

func splitLines(s string) []string {
	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		result = append(result, s[start:])
	}
	return result
}

func (al *AuditLog) Record(action, agentID, user, detail string) {
	al.mu.Lock()
	defer al.mu.Unlock()
	entry := AuditEntry{
		Timestamp: time.Now().UnixMilli(),
		Action:    action,
		AgentID:   agentID,
		User:      user,
		Detail:    detail,
	}
	al.entries = append(al.entries, entry)

	line, _ := json.Marshal(entry)
	f, err := os.OpenFile(al.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		f.Write(line)
		f.Write([]byte("\n"))
		f.Close()
	}
}

func (al *AuditLog) Recent(max int) []AuditEntry {
	al.mu.Lock()
	defer al.mu.Unlock()
	if max <= 0 || max > len(al.entries) {
		max = len(al.entries)
	}
	start := len(al.entries) - max
	result := make([]AuditEntry, max)
	copy(result, al.entries[start:])
	return result
}

func RecordAudit(action, agentID, user, detail string) {
	if globalAudit != nil {
		globalAudit.Record(action, agentID, user, detail)
	}
}
