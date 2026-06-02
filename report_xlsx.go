package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xuri/excelize/v2"
)

func writeActivitySheet(f *excelize.File, sheetName string) error {
	sw, err := f.NewStreamWriter(sheetName)
	if err != nil {
		return err
	}
	setHeader := func(row string, vals []string) error {
		cells := make([]interface{}, len(vals))
		for i, v := range vals {
			cells[i] = excelize.Cell{StyleID: 1, Value: v}
		}
		return sw.SetRow(row, cells)
	}

	headerErr := setHeader("A1", []string{
		"Agent ID", "Hostname", "Local IP", "WAN IP",
		"OS", "Architecture", "Binary Version", "Mode",
		"Transport", "Health", "Latency (ms)", "Bytes/sec",
		"Frames Received", "Frames/sec", "Uptime", "Start Time",
		"Boot Time", "Wake Up Time", "Total Idle Time",
		"Report Date", "Report Time",
	})
	if headerErr != nil {
		return headerErr
	}

	hostname := getHostname()
	row := 2

	uptimeSecs := int64(time.Since(startTime).Seconds())
	uptimeStr := fmt.Sprintf("%dh %dm %ds", uptimeSecs/3600, (uptimeSecs%3600)/60, uptimeSecs%60)
	bootTime := "—"
	totalIdle := "—"
	if globalActivity != nil {
		s := globalActivity.Summary()
		bootTime = s["boot_time"]
		totalIdle = s["total_idle"]
	}
	wakeUpTime := "—"
	if !startTime.IsZero() {
		wakeUpTime = startTime.Format("2006-01-02 15:04:05")
	}
	mode := "agent"
	if cfg.IsServerMode {
		mode = "server+agent"
	}
	if err := sw.SetRow(fmt.Sprintf("A%d", row), []interface{}{
		cfg.AgentID, hostname, getLocalIP(), getWANIP(),
		runtime.GOOS, runtime.GOARCH, binaryVersion, mode,
		"local", "n/a", "0", "0", "0", "0",
		uptimeStr, startTime.Format("2006-01-02 15:04:05"),
		bootTime, wakeUpTime, totalIdle,
		time.Now().Format("2006-01-02"), time.Now().Format("15:04:05"),
	}); err != nil {
		return err
	}
	row++

	agentSystemInfo.Range(func(key, value interface{}) bool {
		aid, ok := key.(string)
		if !ok {
			return true
		}
		info, _ := value.(map[string]interface{})
		if info == nil {
			return true
		}
		getStr := func(field string) string {
			if v, ok := info[field].(string); ok {
				return v
			}
			return "—"
		}
		transport := "—"
		health := "—"
		latency := "0"
		bytesPerSec := "0"
		framesRec := "0"
		fps := "0"
		if v, ok := agentStats.Load(aid); ok {
			s := v.(*AgentStats)
			snap := s.Snapshot()
			if t, ok := snap["transport"].(string); ok {
				transport = t
			}
			if h, ok := snap["health"].(string); ok {
				health = h
			}
			if l, ok := snap["latency_ms"].(float64); ok {
				latency = fmt.Sprintf("%.1f", l)
			}
			if b, ok := snap["bytes_per_sec"].(float64); ok {
				bytesPerSec = fmt.Sprintf("%.1f", b)
			}
			if f, ok := snap["frames_per_sec"].(float64); ok {
				fps = fmt.Sprintf("%.1f", f)
			}
			framesRec = fmt.Sprintf("%d", snap["frames_received"])
		}
		cellRow := fmt.Sprintf("A%d", row)
		if err := sw.SetRow(cellRow, []interface{}{
			aid,
			getStr("hostname"),
			getStr("local_ip"),
			getStr("wan_ip"),
			getStr("os"),
			getStr("arch"),
			getStr("version"),
			"agent",
			transport, health, latency, bytesPerSec, framesRec, fps,
			getStr("uptime"),
			getStr("start_time"),
			getStr("boot_time"),
			getStr("wake_up_time"),
			getStr("idle_time"),
			time.Now().Format("2006-01-02"), time.Now().Format("15:04:05"),
		}); err != nil {
			llog("warn", "xlsx: SetRow activity agent: %v", err)
		}
		row++
		return true
	})

	knownAgents.Range(func(key, value interface{}) bool {
		aid, ok := key.(string)
		if !ok {
			return true
		}
		if _, exists := agentSystemInfo.Load(aid); exists {
			return true
		}
		info, _ := value.(map[string]interface{})
		if info == nil {
			return true
		}
		getStr := func(field string) string {
			if v, ok := info[field]; ok {
				if s, ok := v.(string); ok {
					return s
				}
			}
			return "—"
		}
		cellRow := fmt.Sprintf("A%d", row)
		if err := sw.SetRow(cellRow, []interface{}{
			aid, getStr("hostname"), getStr("local_ip"), getStr("wan_ip"),
			getStr("os"), getStr("arch"), getStr("version"), "disconnected",
			"—", "offline", "—", "0", "0", "0",
			getStr("uptime"), getStr("start_time"),
			getStr("boot_time"), getStr("wake_up_time"), getStr("idle_time"),
			time.Now().Format("2006-01-02"), time.Now().Format("15:04:05"),
		}); err != nil {
			llog("warn", "xlsx: SetRow activity known: %v", err)
		}
		row++
		return true
	})

	// Append a "Latest Election Event" row at the bottom of the Activity
	// sheet so users see the most recent GitHub election state without
	// having to switch to the Election tab. The row uses the same column
	// layout as the table (22 columns) for consistency.
	events := getElectionHistory()
	if len(events) > 0 {
		latest := events[len(events)-1]
		// Section header row (sentinel value in the first cell to mark "this is a meta-row")
		headerRow := row
		if err := sw.SetRow(fmt.Sprintf("A%d", headerRow), []interface{}{
			"── LATEST GITHUB ELECTION EVENT ──", "", "", "", "", "", "", "", "", "", "",
			"", "", "", "", "", "", "", "", "", "",
		}); err != nil {
			llog("warn", "xlsx: SetRow activity election header: %v", err)
		}
		row++

		// Data row: same 22 columns, with the latest event squashed into
		// the first few cells. Remaining cells are filled with the leader
		// age, action, method, etc.
		leaderAgeSec := latest.LeaderAgeMS / 1000
		if leaderAgeSec < 0 {
			leaderAgeSec = 0
		}
		dataRow := row
		if err := sw.SetRow(fmt.Sprintf("A%d", dataRow), []interface{}{
			fmt.Sprintf("[%s]", latest.Action),
			latest.TimestampISO,
			latest.Timestamp.Format("2006-01-02"),
			latest.Timestamp.Format("15:04:05.000"),
			latest.Action,
			latest.Method,
			latest.AgentID,
			latest.Hostname,
			latest.LeaderID,
			leaderAgeSec,
			latest.Result,
			latest.Error,
			"", "", "", "", "", "", "", "", "",
		}); err != nil {
			llog("warn", "xlsx: SetRow activity election event: %v", err)
		}
		row++
	}

	return sw.Flush()
}

func writeAuditSheet(f *excelize.File, sheetName string) error {
	sw, err := f.NewStreamWriter(sheetName)
	if err != nil {
		return err
	}
	if err := sw.SetRow("A1", []interface{}{
		excelize.Cell{StyleID: 1, Value: "Timestamp"},
		excelize.Cell{StyleID: 1, Value: "Time (HH:MM:SS)"},
		excelize.Cell{StyleID: 1, Value: "Date"},
		excelize.Cell{StyleID: 1, Value: "Action"},
		excelize.Cell{StyleID: 1, Value: "Agent ID"},
		excelize.Cell{StyleID: 1, Value: "User"},
		excelize.Cell{StyleID: 1, Value: "Detail"},
	}); err != nil {
		return err
	}

	if globalAudit == nil {
		return sw.Flush()
	}
	entries := globalAudit.Recent(10000)
	row := 2
	for _, e := range entries {
		t := time.UnixMilli(e.Timestamp)
		cellRow := fmt.Sprintf("A%d", row)
		if err := sw.SetRow(cellRow, []interface{}{
			e.Timestamp,
			t.Format("15:04:05"),
			t.Format("2006-01-02"),
			e.Action,
			e.AgentID,
			e.User,
			e.Detail,
		}); err != nil {
			llog("warn", "xlsx: SetRow audit: %v", err)
		}
		row++
	}
	return sw.Flush()
}

func handleReportXLSX(w http.ResponseWriter, r *http.Request) {
	data, err := buildReportXLSX()
	if err != nil {
		http.Error(w, "build report: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", "attachment; filename=\"punmonitor-report-"+time.Now().Format("2006-01-02")+".xlsx\"")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.Write(data)
}

// buildReportXLSX builds the full 3-tab report (Activity + Audit Log +
// Election row-wise history) and returns it as bytes. Used both by the
// HTTP handler /api/report.xlsx and the daily GitHub auto-push.
func buildReportXLSX() ([]byte, error) {
	f := excelize.NewFile()
	defer f.Close()

	defaultSheet := f.GetSheetName(0)
	if err := f.SetSheetName(defaultSheet, "Activity"); err != nil {
		llog("warn", "xlsx: SetSheetName: %v", err)
	}
	if err := writeActivitySheet(f, "Activity"); err != nil {
		return nil, fmt.Errorf("activity: %w", err)
	}
	if err := f.SetSheetView("Activity", 0, &excelize.ViewOptions{ShowGridLines: boolPtr(false)}); err != nil {
		llog("warn", "xlsx: SetSheetView activity: %v", err)
	}
	if err := f.SetColWidth("Activity", "A", "A", 30); err != nil {
		llog("warn", "xlsx: SetColWidth A: %v", err)
	}
	if err := f.SetColWidth("Activity", "B", "B", 20); err != nil {
		llog("warn", "xlsx: SetColWidth B: %v", err)
	}
	if err := f.SetColWidth("Activity", "C", "D", 15); err != nil {
		llog("warn", "xlsx: SetColWidth CD: %v", err)
	}
	if err := f.SetColWidth("Activity", "E", "I", 12); err != nil {
		llog("warn", "xlsx: SetColWidth E-I: %v", err)
	}

	if _, err := f.NewSheet("Audit Log"); err != nil {
		return nil, fmt.Errorf("new audit sheet: %w", err)
	}
	if err := writeAuditSheet(f, "Audit Log"); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	if err := f.SetSheetView("Audit Log", 0, &excelize.ViewOptions{ShowGridLines: boolPtr(false)}); err != nil {
		llog("warn", "xlsx: SetSheetView audit: %v", err)
	}
	if err := f.SetColWidth("Audit Log", "A", "A", 16); err != nil {
		llog("warn", "xlsx: SetColWidth A: %v", err)
	}
	if err := f.SetColWidth("Audit Log", "B", "B", 12); err != nil {
		llog("warn", "xlsx: SetColWidth B: %v", err)
	}
	if err := f.SetColWidth("Audit Log", "C", "C", 13); err != nil {
		llog("warn", "xlsx: SetColWidth C: %v", err)
	}
	if err := f.SetColWidth("Audit Log", "D", "D", 22); err != nil {
		llog("warn", "xlsx: SetColWidth D: %v", err)
	}
	if err := f.SetColWidth("Audit Log", "E", "F", 18); err != nil {
		llog("warn", "xlsx: SetColWidth EF: %v", err)
	}
	if err := f.SetColWidth("Audit Log", "G", "G", 50); err != nil {
		llog("warn", "xlsx: SetColWidth G: %v", err)
	}
	if err := f.SetPanes("Audit Log", &excelize.Panes{
		Freeze:      true,
		Split:       false,
		XSplit:      0,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
	}); err != nil {
		llog("warn", "xlsx: SetPanes audit: %v", err)
	}

	if _, err := f.NewSheet("Election"); err != nil {
		return nil, fmt.Errorf("new election sheet: %w", err)
	}
	if err := writeElectionSheet(f, "Election"); err != nil {
		return nil, fmt.Errorf("election: %w", err)
	}
	if err := f.SetSheetView("Election", 0, &excelize.ViewOptions{ShowGridLines: boolPtr(false)}); err != nil {
		llog("warn", "xlsx: SetSheetView election: %v", err)
	}
	if err := f.SetColWidth("Election", "A", "A", 5); err != nil {
		llog("warn", "xlsx: SetColWidth election A: %v", err)
	}
	if err := f.SetColWidth("Election", "B", "B", 28); err != nil {
		llog("warn", "xlsx: SetColWidth election B: %v", err)
	}
	if err := f.SetColWidth("Election", "C", "D", 12); err != nil {
		llog("warn", "xlsx: SetColWidth election CD: %v", err)
	}
	if err := f.SetColWidth("Election", "E", "F", 12); err != nil {
		llog("warn", "xlsx: SetColWidth election EF: %v", err)
	}
	if err := f.SetColWidth("Election", "G", "H", 22); err != nil {
		llog("warn", "xlsx: SetColWidth election GH: %v", err)
	}
	if err := f.SetColWidth("Election", "I", "I", 32); err != nil {
		llog("warn", "xlsx: SetColWidth election I: %v", err)
	}
	if err := f.SetColWidth("Election", "J", "J", 14); err != nil {
		llog("warn", "xlsx: SetColWidth election J: %v", err)
	}
	if err := f.SetColWidth("Election", "K", "K", 16); err != nil {
		llog("warn", "xlsx: SetColWidth election K: %v", err)
	}
	if err := f.SetColWidth("Election", "L", "L", 50); err != nil {
		llog("warn", "xlsx: SetColWidth election L: %v", err)
	}
	if err := f.SetPanes("Election", &excelize.Panes{
		Freeze: true, Split: false, XSplit: 0, YSplit: 1,
		TopLeftCell: "A2", ActivePane: "bottomLeft",
		Selection: []excelize.Selection{{SQRef: "A2", ActiveCell: "A2", Pane: "bottomLeft"}},
	}); err != nil {
		llog("warn", "xlsx: SetPanes election: %v", err)
	}

	if err := f.SetSheetView(defaultSheet, 0, &excelize.ViewOptions{ShowGridLines: boolPtr(false)}); err != nil {
		llog("warn", "xlsx: SetSheetView activity2: %v", err)
	}

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		return nil, fmt.Errorf("write xlsx: %w", err)
	}
	return buf.Bytes(), nil
}

func boolPtr(b bool) *bool { return &b }

func writeElectionSheet(f *excelize.File, sheetName string) error {
	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF", Size: 11},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"1F4E78"}, Pattern: 1},
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
	sectionStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF", Size: 10},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"4F81BD"}, Pattern: 1},
		Alignment: &excelize.Alignment{Vertical: "center", Horizontal: "left"},
	})
	labelStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Size: 10, Color: "333333"},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"F2F2F2"}, Pattern: 1},
		Alignment: &excelize.Alignment{Vertical: "center", Horizontal: "left"},
	})

	// ── CURRENT STATE HEADER (rows 1-12) ──
	// Show the current election state at the top of the sheet so users
	// see it immediately when they open the Election tab, without having
	// to scroll past the history table.
	globalElectionStatusMu.RLock()
	s := globalElectionStatus
	globalElectionStatusMu.RUnlock()
	s.Configured = cfg.GitHubRepo != "" && cfg.GitHubToken != ""
	s.Repo = cfg.GitHubRepo
	s.SelfIsLeader = s.LeaderID != "" && s.LeaderID == cfg.AgentID
	if !s.LeaderUpdated.IsZero() {
		s.LeaderStale = time.Since(s.LeaderUpdated) > electionInterval
	}

	method := s.Method
	if method == "" {
		method = "none"
	}
	if !s.Configured {
		method = "no-github"
	}
	selfIsLeader := "No"
	if s.SelfIsLeader {
		selfIsLeader = "YES — this instance is the primary server"
	}
	leaderDisplay := s.LeaderID
	if leaderDisplay == "" {
		leaderDisplay = "(no leader recorded)"
	}
	updatedDisplay := "(never)"
	if !s.LeaderUpdated.IsZero() {
		updatedDisplay = s.LeaderUpdated.Format(time.RFC3339) + " (" + time.Since(s.LeaderUpdated).Truncate(time.Second).String() + " ago)"
	}
	lastCheckDisplay := "(never)"
	if !s.LastCheck.IsZero() {
		lastCheckDisplay = s.LastCheck.Format(time.RFC3339) + " (" + time.Since(s.LastCheck).Truncate(time.Second).String() + " ago)"
	}
	leaderStaleDisplay := "No"
	if s.LeaderStale {
		leaderStaleDisplay = "YES — leader hasn't renewed in " + electionInterval.String()
	}
	role := "AGENT (connects to leader)"
	if cfg.IsServerMode {
		role = "SERVER (primary)"
	}
	if s.SelfIsLeader {
		role = "SERVER+LEADER (this instance is the GitHub primary)"
	}

	// Row 1: title banner
	f.SetCellValue(sheetName, "A1", "PunMonitor Leader Election — Current State")
	f.SetCellValue(sheetName, "B1", "Generated "+time.Now().Format(time.RFC3339))
	f.SetCellStyle(sheetName, "A1", "B1", sectionStyle)

	// Rows 2-10: current state key-value pairs
	currentRows := [][2]string{
		{"Election method", method},
		{"GitHub configured", boolStr(s.Configured)},
		{"GitHub repo", firstNonEmpty(s.Repo, "(not configured)")},
		{"Self AgentID", cfg.AgentID},
		{"Self hostname", getHostname()},
		{"Mode / Role", role},
		{"Self is leader", selfIsLeader},
		{"Current leader", leaderDisplay},
		{"Leader last update", updatedDisplay},
		{"Leader stale?", leaderStaleDisplay},
		{"Last check", lastCheckDisplay},
		{"Check count", fmt.Sprintf("%d", s.CheckCount)},
		{"Last error", firstNonEmpty(s.LastError, "(none)")},
	}
	for i, r := range currentRows {
		row := 2 + i
		f.SetCellValue(sheetName, fmt.Sprintf("A%d", row), r[0])
		f.SetCellValue(sheetName, fmt.Sprintf("B%d", row), r[1])
		f.SetCellStyle(sheetName, fmt.Sprintf("A%d", row), fmt.Sprintf("A%d", row), labelStyle)
		f.SetCellStyle(sheetName, fmt.Sprintf("B%d", row), fmt.Sprintf("B%d", row), cellStyle)
	}

	// Row 16: section banner for the history table
	historyHeaderRow := 16
	f.SetCellValue(sheetName, fmt.Sprintf("A%d", historyHeaderRow), "Election History (row-wise — latest first)")
	f.SetCellStyle(sheetName, fmt.Sprintf("A%d", historyHeaderRow), fmt.Sprintf("L%d", historyHeaderRow), sectionStyle)

	// Row 17: column headers
	tableHeaderRow := historyHeaderRow + 1
	headers := []string{
		"#", "Timestamp", "Date", "Time", "Action", "Method",
		"Agent ID (self)", "Hostname", "Leader ID", "Leader Age (sec)",
		"Result", "Error",
	}
	for i, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, tableHeaderRow)
		f.SetCellValue(sheetName, cell, h)
	}
	f.SetCellStyle(sheetName, fmt.Sprintf("A%d", tableHeaderRow), fmt.Sprintf("%s%d", colLetter(len(headers)), tableHeaderRow), headerStyle)

	// Column widths
	f.SetColWidth(sheetName, "A", "A", 5)
	f.SetColWidth(sheetName, "B", "B", 28)
	f.SetColWidth(sheetName, "C", "D", 12)
	f.SetColWidth(sheetName, "E", "F", 12)
	f.SetColWidth(sheetName, "G", "H", 22)
	f.SetColWidth(sheetName, "I", "I", 32)
	f.SetColWidth(sheetName, "J", "J", 14)
	f.SetColWidth(sheetName, "K", "K", 16)
	f.SetColWidth(sheetName, "L", "L", 50)

	events := getElectionHistory()
	for i, ev := range events {
		row := tableHeaderRow + 1 + i
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
			f.SetCellValue(sheetName, cell, v)
		}
		rowEnd := fmt.Sprintf("L%d", row)
		switch ev.Action {
		case "claimed", "renewed":
			f.SetCellStyle(sheetName, fmt.Sprintf("A%d", row), rowEnd, claimedStyle)
		case "error":
			f.SetCellStyle(sheetName, fmt.Sprintf("A%d", row), rowEnd, errorStyle)
		default:
			f.SetCellStyle(sheetName, fmt.Sprintf("A%d", row), rowEnd, cellStyle)
		}
	}

	footerRow := tableHeaderRow + 1 + len(events) + 1
	f.SetCellValue(sheetName, fmt.Sprintf("A%d", footerRow), "Generated")
	f.SetCellValue(sheetName, fmt.Sprintf("B%d", footerRow), time.Now().Format(time.RFC3339))
	f.SetCellValue(sheetName, fmt.Sprintf("D%d", footerRow), "Total events")
	f.SetCellValue(sheetName, fmt.Sprintf("E%d", footerRow), len(events))

	f.SetPanes(sheetName, &excelize.Panes{
		Freeze:      true,
		Split:       false,
		XSplit:      0,
		YSplit:      tableHeaderRow,
		TopLeftCell: fmt.Sprintf("A%d", tableHeaderRow+1),
		ActivePane:  "bottomLeft",
		Selection: []excelize.Selection{
			{SQRef: fmt.Sprintf("A%d", tableHeaderRow+1), ActiveCell: fmt.Sprintf("A%d", tableHeaderRow+1), Pane: "bottomLeft"},
		},
	})
	return nil
}

func boolStr(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}

func firstNonEmpty(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

func modeBadge() string {
	if cfg.IsServerMode {
		return "SERVER"
	}
	return "AGENT"
}

// buildMergedReportXLSX downloads every report-*.xlsx file from the GitHub
// repo, parses the row data from each sheet, and writes a single merged
// XLSX with all days' data row-wise. This is the "compile sheet" view
// the admin asked for: one XLSX with every Activity, Audit Log, and
// Election event from every day concatenated.
//
// The merged file is named punmonitor-merged-YYYY-MM-DD.xlsx and the
// endpoint is /api/reports/merged. The user can pull this file from
// the dashboard and see the full history in row-wise form.
func buildMergedReportXLSX(w io.Writer) error {
	if cfg.GitHubRepo == "" || cfg.GitHubToken == "" {
		return fmt.Errorf("GitHub not configured")
	}
	authOK, _ := isGitHubAuthOK()
	if !authOK {
		return fmt.Errorf("GitHub auth failed — update token in settings")
	}
	// 1. List all report-*.xlsx files in the repo
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/contents/", normalizeGitHubRepo(cfg.GitHubRepo))
	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.GitHubToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	httpClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("list repo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("list repo returned %d: %s", resp.StatusCode, string(body)[:minInt(200, len(body))])
	}
	var files []struct {
		Name        string `json:"name"`
		DownloadURL string `json:"download_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&files); err != nil {
		return fmt.Errorf("decode list: %w", err)
	}

	// 2. Build a fresh XLSX in memory
	f := excelize.NewFile()
	defer f.Close()
	// Create the 3 sheets (same as daily report)
	for _, name := range []string{"Activity", "Audit Log", "Election"} {
		if _, err := f.NewSheet(name); err != nil {
			return err
		}
	}
	f.DeleteSheet("Sheet1")

	// 3. Write headers
	activityHeader := []string{"Source Date", "Agent ID", "Hostname", "Local IP", "WAN IP", "OS", "Architecture", "Binary Version", "Mode", "Transport", "Health", "Latency (ms)", "Bytes/sec", "CPU%", "Mem%", "Uptime (s)", "Last update"}
	auditHeader := []string{"Source Date", "Timestamp (ISO)", "Timestamp (Unix)", "Action", "Agent ID", "User", "Detail", "Duration (ms)"}
	electionHeader := []string{"Source Date", "Timestamp (ISO)", "Action", "Method", "Agent ID", "Hostname", "Leader ID", "Leader Age (ms)", "Result", "Error"}

	for _, sh := range []struct {
		name   string
		header []string
	}{
		{"Activity", activityHeader},
		{"Audit Log", auditHeader},
		{"Election", electionHeader},
	} {
		for i, h := range sh.header {
			cell, _ := excelize.CoordinatesToCellName(i+1, 1)
			f.SetCellValue(sh.name, cell, h)
		}
	}

	// 4. For each report file, download + parse + append
	activityRow := 2
	auditRow := 2
	electionRow := 2

	// Sort files by name (YYYY-MM-DD) so they merge chronologically
	var reportFiles []struct{ Name, URL string }
	for _, fi := range files {
		if strings.HasPrefix(fi.Name, "report-") && strings.HasSuffix(fi.Name, ".xlsx") {
			reportFiles = append(reportFiles, struct{ Name, URL string }{fi.Name, fi.DownloadURL})
		}
	}
	sort.Slice(reportFiles, func(i, j int) bool { return reportFiles[i].Name < reportFiles[j].Name })

	httpClient2 := &http.Client{Timeout: 60 * time.Second}
	for _, rf := range reportFiles {
		// Extract YYYY-MM-DD from filename
		dateStr := strings.TrimSuffix(strings.TrimPrefix(rf.Name, "report-"), ".xlsx")
		llog("info", "merged report: downloading %s (%s)", rf.Name, rf.URL)
		dresp, derr := httpClient2.Get(rf.URL)
		if derr != nil {
			llog("warn", "merged report: download %s failed: %v", rf.Name, derr)
			continue
		}
		data, _ := io.ReadAll(dresp.Body)
		dresp.Body.Close()
		if dresp.StatusCode != 200 {
			llog("warn", "merged report: download %s returned %d", rf.Name, dresp.StatusCode)
			continue
		}
		// Open the daily report XLSX in memory
		rf2, rerr := excelize.OpenReader(bytes.NewReader(data))
		if rerr != nil {
			llog("warn", "merged report: parse %s failed: %v", rf.Name, rerr)
			continue
		}
		// For each sheet in the daily report, copy rows into the merged file
		for _, shName := range []string{"Activity", "Audit Log", "Election"} {
			rows, rerr := rf2.GetRows(shName)
			if rerr != nil {
				continue
			}
			if len(rows) <= 1 {
				continue // empty (only header)
			}
			// Find which row in the merged file to write to
			var targetRow int
			switch shName {
			case "Activity":
				targetRow = activityRow
			case "Audit Log":
				targetRow = auditRow
			case "Election":
				targetRow = electionRow
			}
			// Skip header row (rows[0]), copy data rows
			for _, row := range rows[1:] {
				// Prepend the source date to each row
				out := append([]string{dateStr}, row...)
				// Pad to header width
				for len(out) < len(activityHeader) && shName == "Activity" {
					out = append(out, "")
				}
				for len(out) < len(auditHeader) && shName == "Audit Log" {
					out = append(out, "")
				}
				for len(out) < len(electionHeader) && shName == "Election" {
					out = append(out, "")
				}
				for ci, val := range out {
					cell, _ := excelize.CoordinatesToCellName(ci+1, targetRow)
					f.SetCellValue(shName, cell, val)
				}
				targetRow++
			}
			switch shName {
			case "Activity":
				activityRow = targetRow
			case "Audit Log":
				auditRow = targetRow
			case "Election":
				electionRow = targetRow
			}
		}
		rf2.Close()
	}

	// 5. Set the first sheet active so Excel opens to it
	idx, _ := f.GetSheetIndex("Activity")
	f.SetActiveSheet(idx)

	// 6. Write to the response
	return f.Write(w)
}

var _ = sync.Mutex{}
