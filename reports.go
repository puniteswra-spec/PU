//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func writeActivityCSV(w http.ResponseWriter, reports map[string][]ActivityEvent, filename string) {
	events := mergeActivityEventsWithAgent(reports)
	var buf bytes.Buffer
	buf.WriteString("agent_id,timestamp,type,detail,hostname,username,local_ip,wan_ip,geo,uptime,idle_start,last_active\n")
	
	sysInfo := getSystemInfoForCSV()
	
	for _, e := range events {
		aid := "local"
		detail := e.Detail
		if idx := strings.Index(detail, "|agent="); idx >= 0 {
			parts := strings.SplitN(detail, "|agent=", 2)
			detail = parts[0]
			aid = parts[1]
		}
		ts := ""
		if e.Timestamp > 0 {
			ts = time.UnixMilli(e.Timestamp).Format(time.RFC3339)
		}
		detail = strings.ReplaceAll(detail, `"`, `""`)
		buf.WriteString(fmt.Sprintf(`%s,%s,%s,"%s",%s,%s,%s,%s,%s,%s,%s,%s`+"\n",
			csvEscape(aid), ts, csvEscape(e.Type), detail,
			csvEscape(sysInfo["hostname"]), csvEscape(sysInfo["username"]),
			csvEscape(sysInfo["local_ip"]), csvEscape(sysInfo["wan_ip"]),
			csvEscape(sysInfo["geo"]), csvEscape(sysInfo["uptime"]),
			csvEscape(sysInfo["last_idle_start"]), csvEscape(sysInfo["last_active"])))
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	w.Write(buf.Bytes())
}

func getSystemInfoForCSV() map[string]string {
	info := make(map[string]string)
	
	if hostname, err := os.Hostname(); err == nil {
		info["hostname"] = hostname
	}
	
	if user, err := user.Current(); err == nil {
		info["username"] = user.Username
	}
	
	// Local IP
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			ifaceAddrs, _ := iface.Addrs()
			for _, addr := range ifaceAddrs {
				if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
					ip := ipnet.IP.String()
					if !strings.HasPrefix(ip, "169.254.") {
						info["local_ip"] = ip
						break
					}
				}
			}
			if info["local_ip"] != "" {
				break
			}
		}
	}
	
	// WAN IP
	info["wan_ip"] = detectWANIPCSV()
	info["geo"] = detectGeoCSV()
	
	if globalActivity != nil {
		summary := globalActivity.Summary()
		info["uptime"] = summary["last_startup"]
		info["last_shutdown"] = summary["last_shutdown"]
		info["last_idle_start"] = summary["last_idle_start"]
		info["last_active"] = summary["last_active"]
		info["last_wake"] = summary["last_wake"]
		info["boot_time"] = summary["boot_time"]
	}
	
	return info
}

func detectWANIPCSV() string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://api.ipify.org")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		data, _ := io.ReadAll(resp.Body)
		ip := strings.TrimSpace(string(data))
		if net.ParseIP(ip) != nil {
			return ip
		}
	}
	return ""
}

func detectGeoCSV() string {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://ip-api.com/json/")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	var result struct {
		Status  string `json:"status"`
		City    string `json:"city"`
		Region  string `json:"regionName"`
		Country string `json:"country"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return ""
	}
	if result.Status == "success" {
		return fmt.Sprintf("%s, %s, %s", result.City, result.Region, result.Country)
	}
	return ""
}

func csvEscape(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}

func mergeActivityEventsWithAgent(reports map[string][]ActivityEvent) []ActivityEvent {
	var mem []ActivityEvent
	for id, evs := range reports {
		for _, e := range evs {
			ev := e
			if ev.Detail != "" {
				ev.Detail = ev.Detail + "|agent=" + id
			} else {
				ev.Detail = "|agent=" + id
			}
			mem = append(mem, ev)
		}
	}
	if globalActivity != nil {
		for _, e := range globalActivity.RecentEvents(1000) {
			mem = append(mem, e)
		}
	}
	sort.Slice(mem, func(i, j int) bool { return mem[i].Timestamp > mem[j].Timestamp })
	if len(mem) > 2000 {
		mem = mem[:2000]
	}
	return mem
}

func (sm *ServerMode) apiCompileMonthlyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path, count, err := compileMonthlyCSV(dataDir())
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"agentCount": count,
		"path":       path,
		"month":      time.Now().Format("2006-01"),
	})
}

func compileMonthlyCSV(dir string) (string, int, error) {
	month := time.Now().Format("2006-01")
	outPath := filepath.Join(dir, "reports", "compiled-"+month+".csv")
	_ = os.MkdirAll(filepath.Join(dir, "reports"), 0755)

	agentIDs := map[string]bool{}
	var all []ActivityEvent
	sysInfo := getSystemInfoForCSV()

	// activity_log.jsonl
	logPath := filepath.Join(dir, "activity_log.jsonl")
	if data, err := os.ReadFile(logPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var ev ActivityEvent
			if json.Unmarshal([]byte(line), &ev) == nil {
				all = append(all, ev)
			}
		}
	}

	// daily CSV snapshots in reports/
	reportsDir := filepath.Join(dir, "reports")
	entries, _ := os.ReadDir(reportsDir)
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".csv") {
			continue
		}
		if !strings.Contains(ent.Name(), month) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(reportsDir, ent.Name()))
		if err != nil {
			continue
		}
		parseCSVEvents(string(b), &all, agentIDs)
	}

	// sort
	sort.Slice(all, func(i, j int) bool { return all[i].Timestamp > all[j].Timestamp })

	var buf bytes.Buffer
	buf.WriteString("agent_id,timestamp,type,detail,hostname,username,local_ip,uptime,idle_start,last_active\n")
	for _, e := range all {
		aid := "local"
		detail := e.Detail
		if idx := strings.Index(detail, "|agent="); idx >= 0 {
			parts := strings.SplitN(detail, "|agent=", 2)
			detail = parts[0]
			aid = parts[1]
		}
		agentIDs[aid] = true
		ts := ""
		if e.Timestamp > 0 {
			ts = time.UnixMilli(e.Timestamp).Format(time.RFC3339)
		}
		detail = strings.ReplaceAll(detail, `"`, `""`)
		buf.WriteString(fmt.Sprintf(`%s,%s,%s,"%s",%s,%s,%s,%s,%s,%s`+"\n",
			csvEscape(aid), ts, csvEscape(e.Type), detail,
			csvEscape(sysInfo["hostname"]), csvEscape(sysInfo["username"]),
			csvEscape(sysInfo["local_ip"]), csvEscape(sysInfo["uptime"]),
			csvEscape(sysInfo["last_idle_start"]), csvEscape(sysInfo["last_active"])))
	}

	if err := os.WriteFile(outPath, buf.Bytes(), 0644); err != nil {
		return "", 0, err
	}
	return outPath, len(agentIDs), nil
}

func parseCSVEvents(csv string, out *[]ActivityEvent, agents map[string]bool) {
	lines := strings.Split(csv, "\n")
	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		// simple: agent_id,timestamp,type,detail
		parts := strings.SplitN(line, ",", 4)
		if len(parts) < 3 {
			continue
		}
		aid := strings.Trim(parts[0], `"`)
		agents[aid] = true
		ts, _ := time.Parse(time.RFC3339, strings.Trim(parts[1], `"`))
		typ := strings.Trim(parts[2], `"`)
		detail := ""
		if len(parts) > 3 {
			detail = strings.Trim(parts[3], `"`)
		}
		*out = append(*out, ActivityEvent{
			Timestamp: ts.UnixMilli(),
			Type:      typ,
			Detail:    detail,
		})
	}
}

func saveDailyReportSnapshot(reports map[string][]ActivityEvent) {
	dir := filepath.Join(dataDir(), "reports")
	_ = os.MkdirAll(dir, 0755)
	path := filepath.Join(dir, "activity-"+time.Now().Format("2006-01-02")+".csv")
	sysInfo := getSystemInfoForCSV()
	
	var buf bytes.Buffer
	buf.WriteString("agent_id,timestamp,type,detail,hostname,username,local_ip,uptime,idle_start,last_active\n")
	for id, evs := range reports {
		for _, e := range evs {
			ts := time.UnixMilli(e.Timestamp).Format(time.RFC3339)
			detail := strings.ReplaceAll(e.Detail, `"`, `""`)
			buf.WriteString(fmt.Sprintf(`%s,%s,%s,"%s",%s,%s,%s,%s,%s,%s`+"\n",
				csvEscape(id), ts, csvEscape(e.Type), detail,
				csvEscape(sysInfo["hostname"]), csvEscape(sysInfo["username"]),
				csvEscape(sysInfo["local_ip"]), csvEscape(sysInfo["uptime"]),
				csvEscape(sysInfo["last_idle_start"]), csvEscape(sysInfo["last_active"])))
		}
	}
	if globalActivity != nil {
		for _, e := range globalActivity.RecentEvents(500) {
			ts := ""
			if e.Timestamp > 0 {
				ts = time.UnixMilli(e.Timestamp).Format(time.RFC3339)
			}
			detail := strings.ReplaceAll(e.Detail, `"`, `""`)
			buf.WriteString(fmt.Sprintf(`local,%s,%s,"%s",%s,%s,%s,%s,%s,%s`+"\n",
				ts, csvEscape(e.Type), detail,
				csvEscape(sysInfo["hostname"]), csvEscape(sysInfo["username"]),
				csvEscape(sysInfo["local_ip"]), csvEscape(sysInfo["uptime"]),
				csvEscape(sysInfo["last_idle_start"]), csvEscape(sysInfo["last_active"])))
		}
	}
	_ = os.WriteFile(path, buf.Bytes(), 0644)
}
