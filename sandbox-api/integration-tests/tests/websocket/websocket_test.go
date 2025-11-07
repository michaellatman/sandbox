package tests

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/blaxel-ai/sandbox-api/integration_tests/common"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WebSocketMessage represents a message sent over the websocket
type WebSocketMessage struct {
	ID        string                 `json:"id"`
	Operation string                 `json:"operation"`
	Data      map[string]interface{} `json:"data"`
}

// WebSocketResponse represents a response sent over the websocket
type WebSocketResponse struct {
	ID      string                 `json:"id"`
	Success bool                   `json:"success"`
	Data    map[string]interface{} `json:"data,omitempty"`
	Error   string                 `json:"error,omitempty"`
	Status  int                    `json:"status,omitempty"`
	Stream  bool                   `json:"stream,omitempty"`
	Done    bool                   `json:"done,omitempty"`
}

// connectWebSocket connects to the WebSocket endpoint
func connectWebSocket(t *testing.T) *websocket.Conn {
	// Parse the base URL to construct the WebSocket URL
	baseURL := common.GetEnv("API_BASE_URL", "http://localhost:8080")
	u, err := url.Parse(baseURL)
	require.NoError(t, err)

	// Convert HTTP to WS
	wsScheme := "ws"
	if u.Scheme == "https" {
		wsScheme = "wss"
	}

	wsURL := fmt.Sprintf("%s://%s/ws", wsScheme, u.Host)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)

	return conn
}

// sendMessage sends a message and returns the response
func sendMessage(t *testing.T, conn *websocket.Conn, msg WebSocketMessage) WebSocketResponse {
	err := conn.WriteJSON(msg)
	require.NoError(t, err)

	var response WebSocketResponse
	err = conn.ReadJSON(&response)
	require.NoError(t, err)

	return response
}

// TestWebSocketFilesystemGet tests getting a file via WebSocket
func TestWebSocketFilesystemGet(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	// Create a test file first
	testPath := fmt.Sprintf("/tmp/ws-test-file-%d", time.Now().Unix())
	testContent := "Hello WebSocket"

	createMsg := WebSocketMessage{
		ID:        "create-1",
		Operation: "filesystem:create",
		Data: map[string]interface{}{
			"path":    testPath,
			"content": testContent,
		},
	}

	createResp := sendMessage(t, conn, createMsg)
	assert.True(t, createResp.Success, "Create should succeed: %s", createResp.Error)
	assert.Equal(t, 200, createResp.Status)

	// Now get the file
	getMsg := WebSocketMessage{
		ID:        "get-1",
		Operation: "filesystem:get",
		Data: map[string]interface{}{
			"path": testPath,
		},
	}

	getResp := sendMessage(t, conn, getMsg)
	assert.True(t, getResp.Success, "Get should succeed: %s", getResp.Error)
	assert.Equal(t, 200, getResp.Status)
	assert.Contains(t, getResp.Data, "content")

	// Clean up
	deleteMsg := WebSocketMessage{
		ID:        "delete-1",
		Operation: "filesystem:delete",
		Data: map[string]interface{}{
			"path": testPath,
		},
	}

	deleteResp := sendMessage(t, conn, deleteMsg)
	assert.True(t, deleteResp.Success, "Delete should succeed: %s", deleteResp.Error)
}

// TestWebSocketFilesystemCreate tests creating a file via WebSocket
func TestWebSocketFilesystemCreate(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	testPath := fmt.Sprintf("/tmp/ws-test-create-%d", time.Now().Unix())
	testContent := "Test content"

	msg := WebSocketMessage{
		ID:        "create-1",
		Operation: "filesystem:create",
		Data: map[string]interface{}{
			"path":    testPath,
			"content": testContent,
		},
	}

	resp := sendMessage(t, conn, msg)
	assert.True(t, resp.Success, "Create should succeed: %s", resp.Error)
	assert.Equal(t, 200, resp.Status)

	// Verify the file exists
	_, err := os.Stat(testPath)
	assert.NoError(t, err)

	// Clean up
	deleteMsg := WebSocketMessage{
		ID:        "delete-1",
		Operation: "filesystem:delete",
		Data: map[string]interface{}{
			"path": testPath,
		},
	}
	sendMessage(t, conn, deleteMsg)
}

// TestWebSocketFilesystemDelete tests deleting a file via WebSocket
func TestWebSocketFilesystemDelete(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	// Create a test file
	testPath := fmt.Sprintf("/tmp/ws-test-delete-%d", time.Now().Unix())
	err := os.WriteFile(testPath, []byte("test"), 0644)
	require.NoError(t, err)

	// Delete via WebSocket
	msg := WebSocketMessage{
		ID:        "delete-1",
		Operation: "filesystem:delete",
		Data: map[string]interface{}{
			"path": testPath,
		},
	}

	resp := sendMessage(t, conn, msg)
	assert.True(t, resp.Success, "Delete should succeed: %s", resp.Error)
	assert.Equal(t, 200, resp.Status)

	// Verify the file is deleted
	_, err = os.Stat(testPath)
	assert.True(t, os.IsNotExist(err), "File should be deleted")
}

// TestWebSocketFilesystemTreeGet tests getting a directory tree via WebSocket
func TestWebSocketFilesystemTreeGet(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	msg := WebSocketMessage{
		ID:        "tree-get-1",
		Operation: "filesystem:tree:get",
		Data: map[string]interface{}{
			"path": "/tmp",
		},
	}

	resp := sendMessage(t, conn, msg)
	assert.True(t, resp.Success, "Tree get should succeed: %s", resp.Error)
	assert.Equal(t, 200, resp.Status)
	assert.Contains(t, resp.Data, "files")
}

// TestWebSocketFilesystemTreeCreate tests creating a directory tree via WebSocket
func TestWebSocketFilesystemTreeCreate(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	testDir := fmt.Sprintf("/tmp/ws-test-tree-%d", time.Now().Unix())

	msg := WebSocketMessage{
		ID:        "tree-create-1",
		Operation: "filesystem:tree:create",
		Data: map[string]interface{}{
			"path": testDir,
			"files": map[string]interface{}{
				"file1.txt": "content1",
				"file2.txt": "content2",
			},
		},
	}

	resp := sendMessage(t, conn, msg)
	assert.True(t, resp.Success, "Tree create should succeed: %s", resp.Error)
	assert.Equal(t, 200, resp.Status)

	// Verify files exist
	_, err := os.Stat(fmt.Sprintf("%s/file1.txt", testDir))
	assert.NoError(t, err)

	// Clean up
	deleteMsg := WebSocketMessage{
		ID:        "tree-delete-1",
		Operation: "filesystem:tree:delete",
		Data: map[string]interface{}{
			"path":      testDir,
			"recursive": true,
		},
	}
	sendMessage(t, conn, deleteMsg)
}

// TestWebSocketFilesystemTreeDelete tests deleting a directory tree via WebSocket
func TestWebSocketFilesystemTreeDelete(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	// Create a test directory
	testDir := fmt.Sprintf("/tmp/ws-test-tree-delete-%d", time.Now().Unix())
	err := os.MkdirAll(testDir, 0755)
	require.NoError(t, err)

	msg := WebSocketMessage{
		ID:        "tree-delete-1",
		Operation: "filesystem:tree:delete",
		Data: map[string]interface{}{
			"path":      testDir,
			"recursive": true,
		},
	}

	resp := sendMessage(t, conn, msg)
	assert.True(t, resp.Success, "Tree delete should succeed: %s", resp.Error)
	assert.Equal(t, 200, resp.Status)

	// Verify directory is deleted
	_, err = os.Stat(testDir)
	assert.True(t, os.IsNotExist(err), "Directory should be deleted")
}

// TestWebSocketMultipartList tests listing multipart uploads via WebSocket
func TestWebSocketMultipartList(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	msg := WebSocketMessage{
		ID:        "multipart-list-1",
		Operation: "filesystem:multipart:list",
		Data:      map[string]interface{}{},
	}

	resp := sendMessage(t, conn, msg)
	assert.True(t, resp.Success, "Multipart list should succeed: %s", resp.Error)
	assert.Equal(t, 200, resp.Status)
	assert.Contains(t, resp.Data, "uploads")
}

// TestWebSocketMultipartInitiate tests initiating a multipart upload via WebSocket
func TestWebSocketMultipartInitiate(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	testPath := fmt.Sprintf("/tmp/ws-multipart-%d.dat", time.Now().Unix())

	msg := WebSocketMessage{
		ID:        "multipart-init-1",
		Operation: "filesystem:multipart:initiate",
		Data: map[string]interface{}{
			"path": testPath,
		},
	}

	resp := sendMessage(t, conn, msg)
	assert.True(t, resp.Success, "Multipart initiate should succeed: %s", resp.Error)
	assert.Equal(t, 200, resp.Status)
	assert.Contains(t, resp.Data, "uploadId")

	// Clean up by aborting the upload
	if uploadID, ok := resp.Data["uploadId"].(string); ok {
		abortMsg := WebSocketMessage{
			ID:        "multipart-abort-1",
			Operation: "filesystem:multipart:abort",
			Data: map[string]interface{}{
				"uploadId": uploadID,
			},
		}
		sendMessage(t, conn, abortMsg)
	}
}

// TestWebSocketMultipartAbort tests aborting a multipart upload via WebSocket
func TestWebSocketMultipartAbort(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	// First initiate an upload
	testPath := fmt.Sprintf("/tmp/ws-multipart-abort-%d.dat", time.Now().Unix())

	initiateMsg := WebSocketMessage{
		ID:        "multipart-init-1",
		Operation: "filesystem:multipart:initiate",
		Data: map[string]interface{}{
			"path": testPath,
		},
	}

	initiateResp := sendMessage(t, conn, initiateMsg)
	require.True(t, initiateResp.Success, "Initiate should succeed")
	uploadID := initiateResp.Data["uploadId"].(string)

	// Now abort it
	abortMsg := WebSocketMessage{
		ID:        "multipart-abort-1",
		Operation: "filesystem:multipart:abort",
		Data: map[string]interface{}{
			"uploadId": uploadID,
		},
	}

	abortResp := sendMessage(t, conn, abortMsg)
	assert.True(t, abortResp.Success, "Abort should succeed: %s", abortResp.Error)
	assert.Equal(t, 200, abortResp.Status)
}

// TestWebSocketMultipartListParts tests listing parts of a multipart upload via WebSocket
func TestWebSocketMultipartListParts(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	// First initiate an upload
	testPath := fmt.Sprintf("/tmp/ws-multipart-parts-%d.dat", time.Now().Unix())

	initiateMsg := WebSocketMessage{
		ID:        "multipart-init-1",
		Operation: "filesystem:multipart:initiate",
		Data: map[string]interface{}{
			"path": testPath,
		},
	}

	initiateResp := sendMessage(t, conn, initiateMsg)
	require.True(t, initiateResp.Success, "Initiate should succeed")
	uploadID := initiateResp.Data["uploadId"].(string)

	// List parts
	listMsg := WebSocketMessage{
		ID:        "multipart-list-parts-1",
		Operation: "filesystem:multipart:listParts",
		Data: map[string]interface{}{
			"uploadId": uploadID,
		},
	}

	listResp := sendMessage(t, conn, listMsg)
	assert.True(t, listResp.Success, "List parts should succeed: %s", listResp.Error)
	assert.Equal(t, 200, listResp.Status)
	assert.Contains(t, listResp.Data, "parts")

	// Clean up
	abortMsg := WebSocketMessage{
		ID:        "multipart-abort-1",
		Operation: "filesystem:multipart:abort",
		Data: map[string]interface{}{
			"uploadId": uploadID,
		},
	}
	sendMessage(t, conn, abortMsg)
}

// TestWebSocketProcessExecute tests executing a process via WebSocket
func TestWebSocketProcessExecute(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	processName := fmt.Sprintf("ws-test-process-%d", time.Now().Unix())

	msg := WebSocketMessage{
		ID:        "process-execute-1",
		Operation: "process:execute",
		Data: map[string]interface{}{
			"command": "echo 'hello websocket'",
			"name":    processName,
		},
	}

	resp := sendMessage(t, conn, msg)
	assert.True(t, resp.Success, "Process execute should succeed: %s", resp.Error)
	assert.Equal(t, 200, resp.Status)

	// Parse the response data into a map to access nested fields
	respData, err := json.Marshal(resp.Data)
	require.NoError(t, err)

	var processInfo map[string]interface{}
	err = json.Unmarshal(respData, &processInfo)
	require.NoError(t, err)

	assert.Contains(t, processInfo, "pid")

	// Clean up
	if pid, ok := processInfo["pid"].(string); ok && pid != "" {
		stopMsg := WebSocketMessage{
			ID:        "process-stop-1",
			Operation: "process:stop",
			Data: map[string]interface{}{
				"identifier": processName,
			},
		}
		sendMessage(t, conn, stopMsg)
	}
}

// TestWebSocketProcessList tests listing processes via WebSocket
func TestWebSocketProcessList(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	msg := WebSocketMessage{
		ID:        "process-list-1",
		Operation: "process:list",
		Data:      map[string]interface{}{},
	}

	resp := sendMessage(t, conn, msg)
	assert.True(t, resp.Success, "Process list should succeed: %s", resp.Error)
	assert.Equal(t, 200, resp.Status)
	assert.Contains(t, resp.Data, "processes")
}

// TestWebSocketProcessGet tests getting a process via WebSocket
func TestWebSocketProcessGet(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	// First create a process
	processName := fmt.Sprintf("ws-test-get-%d", time.Now().Unix())

	createMsg := WebSocketMessage{
		ID:        "process-execute-1",
		Operation: "process:execute",
		Data: map[string]interface{}{
			"command": "sleep 5",
			"name":    processName,
		},
	}

	createResp := sendMessage(t, conn, createMsg)
	require.True(t, createResp.Success, "Process execute should succeed")

	// Now get the process
	getMsg := WebSocketMessage{
		ID:        "process-get-1",
		Operation: "process:get",
		Data: map[string]interface{}{
			"identifier": processName,
		},
	}

	getResp := sendMessage(t, conn, getMsg)
	assert.True(t, getResp.Success, "Process get should succeed: %s", getResp.Error)
	assert.Equal(t, 200, getResp.Status)

	// Clean up
	stopMsg := WebSocketMessage{
		ID:        "process-stop-1",
		Operation: "process:stop",
		Data: map[string]interface{}{
			"identifier": processName,
		},
	}
	sendMessage(t, conn, stopMsg)
}

// TestWebSocketProcessLogs tests getting process logs via WebSocket
func TestWebSocketProcessLogs(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	// Create a process
	processName := fmt.Sprintf("ws-test-logs-%d", time.Now().Unix())

	createMsg := WebSocketMessage{
		ID:        "process-execute-1",
		Operation: "process:execute",
		Data: map[string]interface{}{
			"command": "echo 'test log output'",
			"name":    processName,
		},
	}

	createResp := sendMessage(t, conn, createMsg)
	require.True(t, createResp.Success, "Process execute should succeed")

	// Wait a bit for the process to complete
	time.Sleep(500 * time.Millisecond)

	// Get logs
	logsMsg := WebSocketMessage{
		ID:        "process-logs-1",
		Operation: "process:logs",
		Data: map[string]interface{}{
			"identifier": processName,
		},
	}

	logsResp := sendMessage(t, conn, logsMsg)
	assert.True(t, logsResp.Success, "Process logs should succeed: %s", logsResp.Error)
	assert.Equal(t, 200, logsResp.Status)
}

// TestWebSocketProcessStop tests stopping a process via WebSocket
func TestWebSocketProcessStop(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	// Create a long-running process
	processName := fmt.Sprintf("ws-test-stop-%d", time.Now().Unix())

	createMsg := WebSocketMessage{
		ID:        "process-execute-1",
		Operation: "process:execute",
		Data: map[string]interface{}{
			"command": "sleep 30",
			"name":    processName,
		},
	}

	createResp := sendMessage(t, conn, createMsg)
	require.True(t, createResp.Success, "Process execute should succeed")

	// Stop the process
	stopMsg := WebSocketMessage{
		ID:        "process-stop-1",
		Operation: "process:stop",
		Data: map[string]interface{}{
			"identifier": processName,
		},
	}

	stopResp := sendMessage(t, conn, stopMsg)
	assert.True(t, stopResp.Success, "Process stop should succeed: %s", stopResp.Error)
	assert.Equal(t, 200, stopResp.Status)
}

// TestWebSocketProcessKill tests killing a process via WebSocket
func TestWebSocketProcessKill(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	// Create a long-running process
	processName := fmt.Sprintf("ws-test-kill-%d", time.Now().Unix())

	createMsg := WebSocketMessage{
		ID:        "process-execute-1",
		Operation: "process:execute",
		Data: map[string]interface{}{
			"command": "sleep 30",
			"name":    processName,
		},
	}

	createResp := sendMessage(t, conn, createMsg)
	require.True(t, createResp.Success, "Process execute should succeed")

	// Kill the process
	killMsg := WebSocketMessage{
		ID:        "process-kill-1",
		Operation: "process:kill",
		Data: map[string]interface{}{
			"identifier": processName,
		},
	}

	killResp := sendMessage(t, conn, killMsg)
	assert.True(t, killResp.Success, "Process kill should succeed: %s", killResp.Error)
	assert.Equal(t, 200, killResp.Status)
}

// TestWebSocketNetworkPortsGet tests getting network ports via WebSocket
func TestWebSocketNetworkPortsGet(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	// Create a process that opens a port
	processName := fmt.Sprintf("ws-test-network-%d", time.Now().Unix())

	createMsg := WebSocketMessage{
		ID:        "process-execute-1",
		Operation: "process:execute",
		Data: map[string]interface{}{
			"command": "python3 -m http.server 8888",
			"name":    processName,
		},
	}

	createResp := sendMessage(t, conn, createMsg)
	require.True(t, createResp.Success, "Process execute should succeed")

	// Parse the response to get the PID
	respData, err := json.Marshal(createResp.Data)
	require.NoError(t, err)

	var processInfo map[string]interface{}
	err = json.Unmarshal(respData, &processInfo)
	require.NoError(t, err)

	pidStr, ok := processInfo["pid"].(string)
	require.True(t, ok, "PID should be a string")

	// Wait for the process to open the port
	time.Sleep(2 * time.Second)

	// Get ports
	portsMsg := WebSocketMessage{
		ID:        "network-ports-1",
		Operation: "network:ports:get",
		Data: map[string]interface{}{
			"pid": pidStr,
		},
	}

	portsResp := sendMessage(t, conn, portsMsg)
	assert.True(t, portsResp.Success, "Network ports get should succeed: %s", portsResp.Error)
	assert.Equal(t, 200, portsResp.Status)
	assert.Contains(t, portsResp.Data, "ports")

	// Clean up
	stopMsg := WebSocketMessage{
		ID:        "process-kill-1",
		Operation: "process:kill",
		Data: map[string]interface{}{
			"identifier": processName,
		},
	}
	sendMessage(t, conn, stopMsg)
}

// TestWebSocketNetworkPortsMonitor tests monitoring network ports via WebSocket
func TestWebSocketNetworkPortsMonitor(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	// Create a process
	processName := fmt.Sprintf("ws-test-monitor-%d", time.Now().Unix())

	createMsg := WebSocketMessage{
		ID:        "process-execute-1",
		Operation: "process:execute",
		Data: map[string]interface{}{
			"command": "sleep 5",
			"name":    processName,
		},
	}

	createResp := sendMessage(t, conn, createMsg)
	require.True(t, createResp.Success, "Process execute should succeed")

	// Parse the response to get the PID
	respData, err := json.Marshal(createResp.Data)
	require.NoError(t, err)

	var processInfo map[string]interface{}
	err = json.Unmarshal(respData, &processInfo)
	require.NoError(t, err)

	pidStr, ok := processInfo["pid"].(string)
	require.True(t, ok, "PID should be a string")

	// Monitor ports
	monitorMsg := WebSocketMessage{
		ID:        "network-monitor-1",
		Operation: "network:ports:monitor",
		Data: map[string]interface{}{
			"pid":      pidStr,
			"callback": "http://example.com/callback",
		},
	}

	monitorResp := sendMessage(t, conn, monitorMsg)
	assert.True(t, monitorResp.Success, "Network ports monitor should succeed: %s", monitorResp.Error)
	assert.Equal(t, 200, monitorResp.Status)

	// Stop monitoring
	stopMonitorMsg := WebSocketMessage{
		ID:        "network-stop-monitor-1",
		Operation: "network:ports:stopMonitor",
		Data: map[string]interface{}{
			"pid": pidStr,
		},
	}

	stopMonitorResp := sendMessage(t, conn, stopMonitorMsg)
	assert.True(t, stopMonitorResp.Success, "Stop monitoring should succeed: %s", stopMonitorResp.Error)

	// Clean up
	stopMsg := WebSocketMessage{
		ID:        "process-stop-1",
		Operation: "process:stop",
		Data: map[string]interface{}{
			"identifier": processName,
		},
	}
	sendMessage(t, conn, stopMsg)
}

// TestWebSocketNetworkPortsStopMonitor tests stopping port monitoring via WebSocket
func TestWebSocketNetworkPortsStopMonitor(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	// Create a process
	processName := fmt.Sprintf("ws-test-stop-monitor-%d", time.Now().Unix())

	createMsg := WebSocketMessage{
		ID:        "process-execute-1",
		Operation: "process:execute",
		Data: map[string]interface{}{
			"command": "sleep 5",
			"name":    processName,
		},
	}

	createResp := sendMessage(t, conn, createMsg)
	require.True(t, createResp.Success, "Process execute should succeed")

	// Parse the response to get the PID
	respData, err := json.Marshal(createResp.Data)
	require.NoError(t, err)

	var processInfo map[string]interface{}
	err = json.Unmarshal(respData, &processInfo)
	require.NoError(t, err)

	pidStr, ok := processInfo["pid"].(string)
	require.True(t, ok, "PID should be a string")

	// Monitor ports first
	monitorMsg := WebSocketMessage{
		ID:        "network-monitor-1",
		Operation: "network:ports:monitor",
		Data: map[string]interface{}{
			"pid":      pidStr,
			"callback": "http://example.com/callback",
		},
	}
	sendMessage(t, conn, monitorMsg)

	// Stop monitoring
	stopMonitorMsg := WebSocketMessage{
		ID:        "network-stop-monitor-1",
		Operation: "network:ports:stopMonitor",
		Data: map[string]interface{}{
			"pid": pidStr,
		},
	}

	stopMonitorResp := sendMessage(t, conn, stopMonitorMsg)
	assert.True(t, stopMonitorResp.Success, "Stop monitoring should succeed: %s", stopMonitorResp.Error)
	assert.Equal(t, 200, stopMonitorResp.Status)

	// Clean up
	stopMsg := WebSocketMessage{
		ID:        "process-stop-1",
		Operation: "process:stop",
		Data: map[string]interface{}{
			"identifier": processName,
		},
	}
	sendMessage(t, conn, stopMsg)
}

// TestWebSocketProcessLogsStream tests streaming process logs via WebSocket
func TestWebSocketProcessLogsStream(t *testing.T) {
	conn := connectWebSocket(t)
	defer conn.Close()

	// Create a process that generates output
	processName := fmt.Sprintf("ws-test-stream-%d", time.Now().Unix())

	createMsg := WebSocketMessage{
		ID:        "process-execute-1",
		Operation: "process:execute",
		Data: map[string]interface{}{
			"command": "for i in 1 2 3 4 5; do echo \"Line $i\"; sleep 0.1; done",
			"name":    processName,
		},
	}

	createResp := sendMessage(t, conn, createMsg)
	require.True(t, createResp.Success, "Process execute should succeed")

	// Start streaming logs
	streamMsg := WebSocketMessage{
		ID:        "stream-1",
		Operation: "process:logs:stream:start",
		Data: map[string]interface{}{
			"identifier": processName,
		},
	}

	err := conn.WriteJSON(streamMsg)
	require.NoError(t, err)

	// Read streaming responses
	logMessages := 0
	streamStarted := false
	streamEnded := false

	// Read messages for up to 5 seconds
	timeout := time.After(5 * time.Second)

readLoop:
	for !streamEnded {
		select {
		case <-timeout:
			t.Logf("Timeout reached, ending stream loop")
			break readLoop
		default:
			// No select case ready, try to read
		}

		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		var response WebSocketResponse
		err := conn.ReadJSON(&response)

		if err != nil {
			// Check if it's a timeout or actual error
			if netErr, ok := err.(interface{ Timeout() bool }); ok && netErr.Timeout() {
				// Just a read timeout, continue waiting
				time.Sleep(10 * time.Millisecond)
				continue
			}
			// Connection closed or other error - stream must be done
			t.Logf("Connection closed or error: %v", err)
			break readLoop
		}

		if response.ID != "stream-1" {
			continue // Not our stream
		}

		assert.True(t, response.Success, "Stream response should succeed: %s", response.Error)

		if response.Stream && response.Done {
			streamEnded = true
			t.Logf("Stream ended with done message")
			break readLoop
		}

		if !streamStarted && response.Stream {
			streamStarted = true
			t.Logf("Stream started")
		}

		// Check if this is a log message
		if log, hasLog := response.Data["log"]; hasLog {
			logMessages++
			t.Logf("Received log: %v", log)
		}
	}

	assert.True(t, streamStarted, "Stream should have started")
	// streamEnded might not be true if connection closed, but we should have logs
	assert.Greater(t, logMessages, 0, "Should have received at least one log message")

	// Clean up
	stopMsg := WebSocketMessage{
		ID:        "process-stop-1",
		Operation: "process:stop",
		Data: map[string]interface{}{
			"identifier": processName,
		},
	}
	sendMessage(t, conn, stopMsg)
}
