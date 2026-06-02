package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xuri/excelize/v2"
)

// ────────────────────────────────────────────────────────────────────
// Election history — row-wise event log of every leader-election state
// change. Used to power the GitHub-pushed election report
// (election_history.xlsx) and the local /api/election-history endpoint.
//
// Events are appended in setElectionStatus whenever the high-level
// (method, result, leaderID) tuple changes from the previous record, so
// the log is dense with state changes but not with no-op renewals that
// happen at every interval tick.
// ────────────────────────────────────────────────────────────────────

// ElectionEvent is one row in the election history log.
type ElectionEvent struct {
	Timestamp    time.Time `json:"timestamp"`
	TimestampISO string    `json:"timestamp_iso"`
	Action       string    `json:"action"`     // claimed | renewed | stale-takeover | active | error | no-github | check
	Method       string    `json:"method"`     // github | lan | relay | none
	AgentID      string    `json:"agent_id"`   // self (the instance writing this row)
	Hostname     string    `json:"hostname"`
	LeaderID     string    `json:"leader_id"`  // current leader after the event
	LeaderAgeMS  int64     `json:"leader_age_ms"`
	Result       string    `json:"result"`
	Error        string    `json:"error"`
}

const (
	electionHistoryMax = 5000 // ring buffer cap
)

var (
	electionHistory   []ElectionEvent
	electionHistoryMu sync.RWMutex
	// For deduplication: last appended event's (method, result, leaderID) tuple + timestamp
	lastElectionKey     string
	lastElectionLogTime time.Time
	// Track last push time so we can throttle auto-pushes
	lastElectionPushAttempt time.Time
	electionPushMu          sync.Mutex
	// dedupInterval is the minimum time between two consecutive "same-state" log
	// entries. Anything inside this window is skipped, anything outside is logged
	// (so periodic renewals are captured, but no more than ~1/minute).
	dedupInterval = 60 * time.Second
)

// appendElectionEvent adds an event to the history, deduplicating entries
// that have the same (method, result, leaderID, error) within a short window.
// Periodic "renewed" events are still captured once per dedupInterval so the
// log shows ongoing activity. State changes (claimed, takeover, error,
// different leader) are always logged immediately.
func appendElectionEvent(ev ElectionEvent) {
	ev.TimestampISO = ev.Timestamp.Format(time.RFC3339Nano)
	key := ev.Method + "|" + ev.Result + "|" + ev.LeaderID + "|" + ev.Error
	electionHistoryMu.Lock()
	defer electionHistoryMu.Unlock()
	// State-changing actions: always log
	isStateChange := ev.Action == "claimed" || ev.Action == "stale-takeover" || ev.Action == "error"
	// For periodic "renewed" / "active" events: dedup within dedupInterval
	if !isStateChange && key == lastElectionKey && time.Since(lastElectionLogTime) < dedupInterval {
		return
	}
	lastElectionKey = key
	lastElectionLogTime = ev.Timestamp
	electionHistory = append(electionHistory, ev)
	if len(electionHistory) > electionHistoryMax {
		// Drop oldest to maintain ring buffer
		electionHistory = electionHistory[len(electionHistory)-electionHistoryMax:]
	}
}

// getElectionHistory returns a snapshot copy of the election history.
func getElectionHistory() []ElectionEvent {
	electionHistoryMu.RLock()
	defer electionHistoryMu.RUnlock()
	out := make([]ElectionEvent, len(electionHistory))
	copy(out, electionHistory)
	return out
}

// clearElectionHistory wipes the in-memory history (useful for tests).
func clearElectionHistory() {
	electionHistoryMu.Lock()
	defer electionHistoryMu.Unlock()
	electionHistory = nil
	lastElectionKey = ""
	lastElectionLogTime = time.Time{}
}

// writeElectionHistoryXLSX returns the XLSX file as bytes.
func writeElectionHistoryXLSX() ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()

	sheet := "Election History"
	f.SetSheetName("Sheet1", sheet)

	// Header
	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Color: "FFFFFF", Size: 11},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"1F4E78"}, Pattern: 1},
		Alignment: &excelize.Alignment{Vertical: "center", Horizontal: "left"},
		Border: []excelize.Border{
			{Type: "left", Color: "BFBFBF", Style: 1},
			{Type: "right", Color: "BFBFBF", Style: 1},
			{Type: "top", Color: "BFBFBF", Style: 1},
			{Type: "bottom", Color: "BFBFBF", Style: 1},
		},
	})
	cellStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Size: 10},
		Alignment: &excelize.Alignment{Vertical: "center", Horizontal: "left", WrapText: true},
		Border: []excelize.Border{
			{Type: "left", Color: "DDDDDD", Style: 1},
			{Type: "right", Color: "DDDDDD", Style: 1},
			{Type: "top", Color: "DDDDDD", Style: 1},
			{Type: "bottom", Color: "DDDDDD", Style: 1},
		},
	})
	errorStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Size: 10, Color: "C00000"},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"FFEEEE"}, Pattern: 1},
		Alignment: &excelize.Alignment{Vertical: "center", Horizontal: "left", WrapText: true},
	})
	claimedStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Size: 10, Color: "006100", Bold: true},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"E2F0D9"}, Pattern: 1},
		Alignment: &excelize.Alignment{Vertical: "center", Horizontal: "left"},
	})

	headers := []string{
		"#", "Timestamp", "Date", "Time", "Action", "Method",
		"Agent ID (self)", "Hostname", "Leader ID", "Leader Age (sec)",
		"Result", "Error",
	}
	for i, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(sheet, cell, h)
	}
	f.SetCellStyle(sheet, "A1", fmt.Sprintf("%s1", colLetter(len(headers))), headerStyle)

	// Column widths
	f.SetColWidth(sheet, "A", "A", 5)
	f.SetColWidth(sheet, "B", "B", 28)
	f.SetColWidth(sheet, "C", "D", 12)
	f.SetColWidth(sheet, "E", "F", 12)
	f.SetColWidth(sheet, "G", "H", 22)
	f.SetColWidth(sheet, "I", "I", 32)
	f.SetColWidth(sheet, "J", "J", 14)
	f.SetColWidth(sheet, "K", "K", 16)
	f.SetColWidth(sheet, "L", "L", 50)

	events := getElectionHistory()
	for i, ev := range events {
		row := i + 2
		dateStr := ev.Timestamp.Format("2006-01-02")
		timeStr := ev.Timestamp.Format("15:04:05.000")
		leaderAgeSec := ev.LeaderAgeMS / 1000
		if leaderAgeSec < 0 {
			leaderAgeSec = 0
		}
		rowVals := []interface{}{
			i + 1,
			ev.TimestampISO,
			dateStr,
			timeStr,
			ev.Action,
			ev.Method,
			ev.AgentID,
			ev.Hostname,
			ev.LeaderID,
			leaderAgeSec,
			ev.Result,
			ev.Error,
		}
		for ci, v := range rowVals {
			cell, _ := excelize.CoordinatesToCellName(ci+1, row)
			f.SetCellValue(sheet, cell, v)
		}
		// Apply per-row style: highlight claimed/renewed rows green, error rows red
		rowEnd := fmt.Sprintf("L%d", row)
		switch ev.Action {
		case "claimed", "renewed":
			f.SetCellStyle(sheet, fmt.Sprintf("A%d", row), rowEnd, claimedStyle)
		case "error":
			f.SetCellStyle(sheet, fmt.Sprintf("A%d", row), rowEnd, errorStyle)
		default:
			f.SetCellStyle(sheet, fmt.Sprintf("A%d", row), rowEnd, cellStyle)
		}
	}

	// Footer: total events, generated at
	footerRow := len(events) + 3
	f.SetCellValue(sheet, fmt.Sprintf("A%d", footerRow), "Generated")
	f.SetCellValue(sheet, fmt.Sprintf("B%d", footerRow), time.Now().Format(time.RFC3339))
	f.SetCellValue(sheet, fmt.Sprintf("D%d", footerRow), "Total events")
	f.SetCellValue(sheet, fmt.Sprintf("E%d", footerRow), len(events))

	// Freeze header row
	f.SetPanes(sheet, &excelize.Panes{
		Freeze:      true,
		Split:       false,
		XSplit:      0,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
		Selection: []excelize.Selection{
			{SQRef: "A2", ActiveCell: "A2", Pane: "bottomLeft"},
		},
	})
	f.SetSheetView(sheet, 0, &excelize.ViewOptions{ShowGridLines: boolPtr(false)})

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// colLetter returns the spreadsheet column letter for a 1-based index (1=A, 26=Z, 27=AA).
func colLetter(n int) string {
	s := ""
	for n > 0 {
		n--
		s = string(rune('A'+(n%26))) + s
		n /= 26
	}
	return s
}

// pushElectionHistoryToGitHub builds the XLSX and uploads it to GitHub via
// the contents API. Path: election_history.xlsx at the repo root.
func pushElectionHistoryToGitHub() (bool, string, error) {
	electionPushMu.Lock()
	lastElectionPushAttempt = time.Now()
	electionPushMu.Unlock()

	if cfg.GitHubRepo == "" || cfg.GitHubToken == "" {
		return false, "github not configured", nil
	}

	data, err := writeElectionHistoryXLSX()
	if err != nil {
		return false, "", fmt.Errorf("xlsx build: %w", err)
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/contents/election_history.xlsx",
		normalizeGitHubRepo(cfg.GitHubRepo))

	// First, GET the current file to get the SHA (required for update)
	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	resp, err := httpFastClient.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("github GET: %w", err)
	}
	var existingSHA string
	if resp.StatusCode == http.StatusOK {
		var gh struct {
			SHA string `json:"sha"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&gh)
		existingSHA = gh.SHA
	}
	resp.Body.Close()

	// Now PUT the new content
	encoded := base64.StdEncoding.EncodeToString(data)
	payload := map[string]interface{}{
		"message": fmt.Sprintf("election history: %d events, %s",
			len(getElectionHistory()), time.Now().Format("2006-01-02 15:04:05")),
		"content": encoded,
		"branch":  "main",
	}
	if existingSHA != "" {
		payload["sha"] = existingSHA
	}
	payloadData, _ := json.Marshal(payload)
	req, _ = http.NewRequest("PUT", apiURL, bytes.NewReader(payloadData))
	req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err = httpFastClient.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("github PUT: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		bodyStr := strings.TrimSpace(string(body))
		if len(bodyStr) > 300 {
			bodyStr = bodyStr[:300] + "..."
		}
		return false, fmt.Sprintf("github returned %d", resp.StatusCode), fmt.Errorf("body: %s", bodyStr)
	}
	return true, "pushed to " + normalizeGitHubRepo(cfg.GitHubRepo) + "/election_history.xlsx", nil
}

// startElectionHistoryPusher runs a background goroutine that periodically
// pushes the election history to GitHub.
func startElectionHistoryPusher() {
	go func() {
		// First push after 30 seconds (let some events accumulate)
		time.Sleep(30 * time.Second)
		for {
			ok, msg, err := pushElectionHistoryToGitHub()
			if err != nil {
				llog("warn", "election history push failed: %v", err)
			} else if ok {
				llog("info", "election history auto-push: %s", msg)
			}
			// Push every 10 minutes
			time.Sleep(10 * time.Minute)
		}
	}()
}
