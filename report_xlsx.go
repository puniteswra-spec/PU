package main

import (
	"bytes"
	"fmt"
	"net/http"
	"runtime"
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

var _ = sync.Mutex{}
