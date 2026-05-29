package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var agentWSConn *websocket.Conn
var agentWSConnMu sync.RWMutex

func setAgentWSConn(conn *websocket.Conn) {
	agentWSConnMu.Lock()
	defer agentWSConnMu.Unlock()
	agentWSConn = conn
}

// --- Server-side: forward requests to agents ---

func handleTerminalCommand(id, command, agentID, user string) {
	RecordAudit("terminal_exec", agentID, user, command)
	msg, _ := json.Marshal(map[string]interface{}{
		"type":    "exec_command",
		"id":      id,
		"command": command,
	})
	forwardToAgent(agentID, msg)
}

func handleDirListRequest(id, path, agentID, user string) {
	RecordAudit("file_browse", agentID, user, path)
	msg, _ := json.Marshal(map[string]interface{}{
		"type": "list_dir",
		"id":   id,
		"path": path,
	})
	forwardToAgent(agentID, msg)
}

func handleFileDownloadRequest(id, path, agentID, user string) {
	RecordAudit("file_download", agentID, user, path)
	msg, _ := json.Marshal(map[string]interface{}{
		"type": "download_file",
		"id":   id,
		"path": path,
	})
	forwardToAgent(agentID, msg)
}

// --- Server-side: route agent responses to dashboard ---

func routeAgentResponse(msg map[string]interface{}) {
	data, _ := json.Marshal(msg)
	wsClients.Range(func(key, value interface{}) bool {
		conn := key.(*websocket.Conn)
		connAgentIDMu.RLock()
		_, isAgent := connAgentID[conn]
		connAgentIDMu.RUnlock()
		if !isAgent {
			conn.WriteMessage(websocket.TextMessage, data)
		}
		return true
	})
}

// --- Agent-side: execute commands ---

func agentExecuteCommand(id, command string) map[string]interface{} {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	done := make(chan error, 1)
	go func() {
		done <- cmd.Run()
	}()

	select {
	case err := <-done:
		output := stdout.String()
		if stderr.Len() > 0 {
			if output != "" {
				output += "\n"
			}
			output += stderr.String()
		}
		if err != nil {
			if output != "" {
				output += "\n"
			}
			output += fmt.Sprintf("Error: %v", err)
		}
		if len(output) > 50000 {
			output = output[len(output)-50000:]
		}
		return map[string]interface{}{"type": "command_output", "id": id, "output": output, "exitCode": exitCode(err)}
	case <-time.After(30 * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return map[string]interface{}{"type": "command_output", "id": id, "output": "Command timed out after 30 seconds", "exitCode": -1}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return -1
}

// --- Agent-side: file operations ---

func agentListDir(path string) map[string]interface{} {
	if path == "" {
		if runtime.GOOS == "windows" {
			path = "C:\\"
		} else {
			path = "/"
		}
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return map[string]interface{}{
			"type":    "dir_listing",
			"path":    path,
			"entries": []interface{}{},
			"error":   err.Error(),
		}
	}

	var result []map[string]interface{}

	parent := filepath.Dir(path)
	if parent != path {
		result = append(result, map[string]interface{}{
			"name":  "..",
			"isDir": true,
			"size":  0,
		})
	}

	for _, e := range entries {
		info, err := e.Info()
		size := int64(0)
		modTime := ""
		if err == nil {
			size = info.Size()
			modTime = info.ModTime().Format("2006-01-02 15:04:05")
		}
		result = append(result, map[string]interface{}{
			"name":    e.Name(),
			"isDir":   e.IsDir(),
			"size":    size,
			"modTime": modTime,
		})
	}

	return map[string]interface{}{
		"type":    "dir_listing",
		"path":    path,
		"entries": result,
	}
}

func agentDownloadFile(path string) map[string]interface{} {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]interface{}{
			"type":  "file_download",
			"id":    "",
			"error": err.Error(),
		}
	}
	name := filepath.Base(path)
	if len(data) > 10485760 {
		return map[string]interface{}{
			"type":  "file_download",
			"id":    "",
			"error": fmt.Sprintf("file too large (%d bytes, max 10MB)", len(data)),
		}
	}
	return map[string]interface{}{
		"type": "file_download",
		"name": name,
		"data": base64.StdEncoding.EncodeToString(data),
	}
}

func agentUploadFile(path string, dataB64 string) map[string]interface{} {
	data, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return map[string]interface{}{"type": "upload_result", "error": err.Error()}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return map[string]interface{}{"type": "upload_result", "error": err.Error()}
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return map[string]interface{}{"type": "upload_result", "error": err.Error()}
	}
	return map[string]interface{}{"type": "upload_result", "path": path, "size": len(data)}
}

func agentHandleTerminalMessage(msg map[string]interface{}) {
	t, _ := msg["type"].(string)
	id, _ := msg["id"].(string)

	switch t {
	case "exec_command":
		command, _ := msg["command"].(string)
		result := agentExecuteCommand(id, command)
		result["id"] = id
		resp, _ := json.Marshal(result)
		agentSendWS(resp)

	case "list_dir":
		path, _ := msg["path"].(string)
		result := agentListDir(path)
		result["id"] = id
		resp, _ := json.Marshal(result)
		agentSendWS(resp)

	case "download_file":
		path, _ := msg["path"].(string)
		result := agentDownloadFile(path)
		result["id"] = id
		resp, _ := json.Marshal(result)
		agentSendWS(resp)

	case "upload_file":
		path, _ := msg["path"].(string)
		dataB64, _ := msg["data"].(string)
		result := agentUploadFile(path, dataB64)
		result["id"] = id
		resp, _ := json.Marshal(result)
		agentSendWS(resp)
	}
}

func agentSendWS(data []byte) {
	if agentWSConn != nil {
		agentWSConn.WriteMessage(websocket.TextMessage, data)
	}
}
