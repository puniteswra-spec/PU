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
)

// ────────────────────────────────────────────────────────────────────
// Election history — row-wise event log of every leader-election state
// change. Stored in an in-memory ring buffer and merged into the main
// daily report (report-YYYY-MM-DD.xlsx) that is auto-pushed to GitHub.
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
	isStateChange := ev.Action == "claimed" || ev.Action == "stale-takeover" || ev.Action == "error"
	if !isStateChange && key == lastElectionKey && time.Since(lastElectionLogTime) < dedupInterval {
		return
	}
	lastElectionKey = key
	lastElectionLogTime = ev.Timestamp
	electionHistory = append(electionHistory, ev)
	if len(electionHistory) > electionHistoryMax {
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

// nextMidnight returns the next 00:05 local time (5 minutes after
// midnight, so the date stamp on the file matches the day that just
// ended). Used by the daily report pusher and the /api/report/status
// endpoint.
func nextMidnight() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 5, 0, 0, now.Location())
}

// ────────────────────────────────────────────────────────────────────
// Daily report pusher — builds the full 3-tab report (Activity + Audit
// Log + Election row-wise history) once a day and pushes it to GitHub
// as report-YYYY-MM-DD.xlsx. The user can then pull any day's compiled
// report directly from the repo.
// ────────────────────────────────────────────────────────────────────

var (
	dailyReportPushMu      sync.Mutex
	lastDailyReportPushed  string // YYYY-MM-DD of last pushed file
	dailyReportPusherState string // last status message
)

func lastDailyReportStatus() string {
	dailyReportPushMu.Lock()
	defer dailyReportPushMu.Unlock()
	return dailyReportPusherState
}

// pushDailyReportToGitHub builds the full report and pushes it as
// report-YYYY-MM-DD.xlsx. Returns (filename, message, error).
func pushDailyReportToGitHub() (string, string, error) {
	if cfg.GitHubRepo == "" || cfg.GitHubToken == "" {
		return "", "github not configured", nil
	}
	// Back off if we already know the token is bad
	if ok, errStr := isGitHubAuthOK(); !ok && errStr != "" {
		return "", "auth failed: " + errStr, fmt.Errorf("github auth failed: %s", errStr)
	}
	data, err := buildReportXLSX()
	if err != nil {
		return "", "", fmt.Errorf("xlsx build: %w", err)
	}
	filename := "report-" + time.Now().Format("2006-01-02") + ".xlsx"
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s",
		normalizeGitHubRepo(cfg.GitHubRepo), filename)

	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	resp, err := httpFastClient.Do(req)
	if err != nil {
		return filename, "", fmt.Errorf("github GET: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		setGitHubAuth(false, fmt.Sprintf("GitHub API %d — token revoked or missing scopes", resp.StatusCode))
		return filename, "auth failed", fmt.Errorf("github auth failed (%d) — update token in settings", resp.StatusCode)
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

	encoded := base64.StdEncoding.EncodeToString(data)
	auditCount := 0
	if globalAudit != nil {
		auditCount = len(globalAudit.Recent(10000))
	}
	payload := map[string]interface{}{
		"message": fmt.Sprintf("daily report %s: %d election events, %d audit entries",
			time.Now().Format("2006-01-02"),
			len(getElectionHistory()),
			auditCount),
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
		return filename, "", fmt.Errorf("github PUT: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		setGitHubAuth(false, fmt.Sprintf("GitHub API %d — token revoked or missing scopes", resp.StatusCode))
		return filename, "auth failed", fmt.Errorf("github auth failed (%d) — update token in settings", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		bodyStr := strings.TrimSpace(string(body))
		if len(bodyStr) > 300 {
			bodyStr = bodyStr[:300] + "..."
		}
		return filename, fmt.Sprintf("github returned %d", resp.StatusCode), fmt.Errorf("body: %s", bodyStr)
	}
	setGitHubAuth(true, "")
	return filename, "pushed to " + normalizeGitHubRepo(cfg.GitHubRepo) + "/" + filename, nil
}

// startDailyReportPusher launches a background goroutine that pushes the
// full report to GitHub at midnight local time (and a few minutes after
// startup as a smoke test).
func startDailyReportPusher() {
	go func() {
		// Smoke test: push ~2 minutes after startup so the file appears
		// in the repo even on the first day.
		time.Sleep(2 * time.Minute)
		pushOnce()

		for {
			// Sleep until the next midnight local time
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 5, 0, 0, now.Location())
			dur := time.Until(next)
			if dur < time.Minute {
				dur = time.Minute
			}
			timer := time.NewTimer(dur)
			<-timer.C
			pushOnce()
		}
	}()
}

// pushOnce performs one daily report push. Safe to call concurrently.
func pushOnce() {
	dailyReportPushMu.Lock()
	defer dailyReportPushMu.Unlock()
	filename, msg, err := pushDailyReportToGitHub()
	if err != nil {
		dailyReportPusherState = "ERROR " + time.Now().Format(time.RFC3339) + ": " + err.Error()
		llog("warn", "daily report push failed: %v", err)
		return
	}
	dailyReportPusherState = "OK " + time.Now().Format(time.RFC3339) + ": " + msg
	lastDailyReportPushed = filename
	llog("info", "daily report auto-push: %s", msg)
}
