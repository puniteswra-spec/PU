package main

import (
	"bytes"
	"fmt"
	"net/http"
	"runtime"
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
	f := excelize.NewFile()

	defaultSheet := f.GetSheetName(0)
	if err := f.SetSheetName(defaultSheet, "Activity"); err != nil {
		llog("warn", "xlsx: SetSheetName: %v", err)
	}
	if err := writeActivitySheet(f, "Activity"); err != nil {
		http.Error(w, "activity sheet error: "+err.Error(), 500)
		return
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

	auditIdx, err := f.NewSheet("Audit Log")
	if err != nil {
		http.Error(w, "new audit sheet: "+err.Error(), 500)
		return
	}
	_ = auditIdx
	if err := writeAuditSheet(f, "Audit Log"); err != nil {
		http.Error(w, "audit sheet error: "+err.Error(), 500)
		return
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
		llog("warn", "xlsx: NewSheet election: %v", err)
	}
	if err := writeElectionSheet(f, "Election"); err != nil {
		llog("warn", "xlsx: writeElectionSheet: %v", err)
	}
	if err := f.SetSheetView("Election", 0, &excelize.ViewOptions{ShowGridLines: boolPtr(false)}); err != nil {
		llog("warn", "xlsx: SetSheetView election: %v", err)
	}
	if err := f.SetColWidth("Election", "A", "A", 30); err != nil {
		llog("warn", "xlsx: SetColWidth election A: %v", err)
	}
	if err := f.SetColWidth("Election", "B", "B", 60); err != nil {
		llog("warn", "xlsx: SetColWidth election B: %v", err)
	}

	if err := f.SetSheetView(defaultSheet, 0, &excelize.ViewOptions{ShowGridLines: boolPtr(false)}); err != nil {
		llog("warn", "xlsx: SetSheetView activity2: %v", err)
	}

	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		http.Error(w, "write xlsx: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", "attachment; filename=\"punmonitor-report-"+time.Now().Format("2006-01-02")+".xlsx\"")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", buf.Len()))
	buf.WriteTo(w)

	if f != nil {
		_ = f.Close()
	}
}

func boolPtr(b bool) *bool { return &b }

func writeElectionSheet(f *excelize.File, sheetName string) error {
	globalElectionStatusMu.RLock()
	s := globalElectionStatus
	globalElectionStatusMu.RUnlock()
	s.Configured = cfg.GitHubRepo != "" && cfg.GitHubToken != ""
	s.Repo = cfg.GitHubRepo
	s.SelfIsLeader = s.LeaderID != "" && s.LeaderID == cfg.AgentID
	if !s.LeaderUpdated.IsZero() {
		s.LeaderStale = time.Since(s.LeaderUpdated) > electionInterval
	}

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
	labelStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Size: 10, Color: "333333"},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"F2F2F2"}, Pattern: 1},
		Alignment: &excelize.Alignment{Vertical: "center", Horizontal: "left"},
	})
	valueStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Size: 10},
		Alignment: &excelize.Alignment{Vertical: "center", Horizontal: "left", WrapText: true},
	})
	sectionStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Color: "FFFFFF", Size: 10},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"4F81BD"}, Pattern: 1},
	})

	method := s.Method
	if method == "" {
		method = "none"
	}
	if !s.Configured {
		method = "no-github"
	}
	repoDisplay := s.Repo
	if repoDisplay == "" {
		repoDisplay = "(not configured)"
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
	section := func(row int, title string) error {
		return f.SetCellValue(sheetName, fmt.Sprintf("A%d", row), title)
	}
	_ = section
	f.SetCellValue(sheetName, "A1", "PunMonitor Leader Election Status")
	f.SetCellValue(sheetName, "B1", "Generated "+time.Now().Format(time.RFC3339))
	f.SetCellStyle(sheetName, "A1", "B1", sectionStyle)

	rows := [][2]string{
		{"ELECTION METHOD", method},
		{"GitHub configured", boolStr(s.Configured)},
		{"GitHub repo", repoDisplay},
		{"Fallback relay", firstNonEmpty(s.FallbackServer, "https://relay.recruitedge.us")},
		{"", ""},
		{"THIS INSTANCE", ""},
		{"AgentID", cfg.AgentID},
		{"Hostname", getHostname()},
		{"Local IP", getLocalIP()},
		{"Mode badge", modeBadge()},
		{"Role on network", role},
		{"Self is leader", selfIsLeader},
		{"", ""},
		{"CURRENT LEADER", ""},
		{"Leader AgentID", leaderDisplay},
		{"Leader last update", updatedDisplay},
		{"Leader is stale", leaderStaleDisplay},
		{"Election interval", electionInterval.String()},
		{"", ""},
		{"LAST CHECK", ""},
		{"Checked at", lastCheckDisplay},
		{"Result", firstNonEmpty(s.LastResult, "(no check yet)")},
		{"Check count", fmt.Sprintf("%d", s.CheckCount)},
		{"Last error", firstNonEmpty(s.LastError, "(none)")},
	}
	for i, r := range rows {
		row := 3 + i
		f.SetCellValue(sheetName, fmt.Sprintf("A%d", row), r[0])
		f.SetCellValue(sheetName, fmt.Sprintf("B%d", row), r[1])
		if r[0] != "" && (r[0] == strings.ToUpper(r[0]) || strings.Contains(r[0], "STATUS") || strings.HasSuffix(r[0], "LEADER") || strings.Contains(r[0], "INSTANCE") || strings.Contains(r[0], "CHECK")) {
			f.SetCellStyle(sheetName, fmt.Sprintf("A%d", row), fmt.Sprintf("B%d", row), sectionStyle)
		} else if r[0] == "" {
			_ = 0
		} else {
			f.SetCellStyle(sheetName, fmt.Sprintf("A%d", row), fmt.Sprintf("A%d", row), labelStyle)
			f.SetCellStyle(sheetName, fmt.Sprintf("B%d", row), fmt.Sprintf("B%d", row), valueStyle)
		}
	}
	_ = headerStyle
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

var _ = sync.Mutex{}
