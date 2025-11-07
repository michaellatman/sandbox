package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"

	"github.com/blaxel-ai/sandbox-api/src/handler/filesystem"
	"github.com/blaxel-ai/sandbox-api/src/handler/network"
	"github.com/blaxel-ai/sandbox-api/src/lib"
	"github.com/blaxel-ai/sandbox-api/src/lib/codegen"
)

// WebSocketHandler handles websocket operations
type WebSocketHandler struct {
	*BaseHandler
	fsHandler      *FileSystemHandler
	processHandler *ProcessHandler
	networkHandler *NetworkHandler
	codegenHandler *CodegenHandler
	upgrader       websocket.Upgrader
}

// NewWebSocketHandler creates a new websocket handler
func NewWebSocketHandler(fsHandler *FileSystemHandler, processHandler *ProcessHandler, networkHandler *NetworkHandler, codegenHandler *CodegenHandler) *WebSocketHandler {
	return &WebSocketHandler{
		BaseHandler:    NewBaseHandler(),
		fsHandler:      fsHandler,
		processHandler: processHandler,
		networkHandler: networkHandler,
		codegenHandler: codegenHandler,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins for now
			},
		},
	}
}

// WebSocketMessage represents a message sent over the websocket
type WebSocketMessage struct {
	ID        string                 `json:"id"`        // Request ID for matching responses
	Operation string                 `json:"operation"` // Operation to perform
	Data      map[string]interface{} `json:"data"`      // Operation-specific data
} // @name WebSocketMessage

// WebSocketResponse represents a response sent over the websocket
type WebSocketResponse struct {
	ID      string      `json:"id"`               // Request ID matching the request
	Success bool        `json:"success"`          // Whether the operation succeeded
	Data    interface{} `json:"data,omitempty"`   // Response data
	Error   string      `json:"error,omitempty"`  // Error message if failed
	Status  int         `json:"status,omitempty"` // HTTP status code
	Stream  bool        `json:"stream,omitempty"` // Whether this is a streaming response
	Done    bool        `json:"done,omitempty"`   // Whether the stream is complete
} // @name WebSocketResponse

// HandleWebSocket handles websocket connections at /ws
// @Summary WebSocket endpoint
// @Description WebSocket endpoint for all sandbox operations
// @Tags websocket
// @Accept json
// @Produce json
// @Router /ws [get]
func (h *WebSocketHandler) HandleWebSocket(c *gin.Context) {
	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		logrus.Errorf("Failed to upgrade to websocket: %v", err)
		return
	}
	defer conn.Close()

	logrus.Info("WebSocket connection established")

	// Set up ping/pong handler
	const (
		pongWait   = 60 * time.Second
		pingPeriod = (pongWait * 9) / 10 // Send pings at 90% of pong deadline
	)

	// Set pong handler - resets read deadline when pong is received
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// Start ping ticker in a goroutine
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(pingPeriod)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
					logrus.Debugf("Failed to send ping: %v", err)
					return
				}
			case <-done:
				return
			}
		}
	}()

	defer close(done)

	// Shared mutex for all websocket writes to prevent concurrent write panics
	var connWriteMu sync.Mutex

	// Track active log streams
	type logStream struct {
		identifier string
		cancel     chan struct{}
	}
	activeStreams := make(map[string]*logStream) // map[requestID]*logStream
	var streamsMu sync.Mutex

	for {
		var msg WebSocketMessage
		err := conn.ReadJSON(&msg)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				logrus.Errorf("WebSocket error: %v", err)
			}
			break
		}

		logrus.Debugf("Received websocket message: operation=%s, id=%s", msg.Operation, msg.ID)

		// Handle streaming operations specially
		if msg.Operation == "process:logs:stream:start" {
			identifier, ok := msg.Data["identifier"].(string)
			if !ok {
				connWriteMu.Lock()
				conn.WriteJSON(WebSocketResponse{
					ID:      msg.ID,
					Success: false,
					Error:   "identifier is required",
					Status:  http.StatusBadRequest,
				})
				connWriteMu.Unlock()
				continue
			}

			// Check if stream already exists for this request ID
			streamsMu.Lock()
			if _, exists := activeStreams[msg.ID]; exists {
				streamsMu.Unlock()
				connWriteMu.Lock()
				conn.WriteJSON(WebSocketResponse{
					ID:      msg.ID,
					Success: false,
					Error:   "stream already active for this request ID",
					Status:  http.StatusBadRequest,
				})
				connWriteMu.Unlock()
				continue
			}

			// Create stream
			cancel := make(chan struct{})
			activeStreams[msg.ID] = &logStream{
				identifier: identifier,
				cancel:     cancel,
			}
			streamsMu.Unlock()

			// Send initial response
			connWriteMu.Lock()
			conn.WriteJSON(WebSocketResponse{
				ID:      msg.ID,
				Success: true,
				Stream:  true,
				Data: map[string]string{
					"message": "Stream started - logs will be sent as they arrive",
				},
				Status: http.StatusOK,
			})
			connWriteMu.Unlock()

			// Start streaming logs in a goroutine
			go h.streamProcessLogs(conn, &connWriteMu, msg.ID, identifier, cancel, done)
			continue
		}

		if msg.Operation == "process:logs:stream:stop" {
			streamsMu.Lock()
			if stream, exists := activeStreams[msg.ID]; exists {
				close(stream.cancel)
				delete(activeStreams, msg.ID)
			}
			streamsMu.Unlock()

			connWriteMu.Lock()
			conn.WriteJSON(WebSocketResponse{
				ID:      msg.ID,
				Success: true,
				Done:    true,
				Data: map[string]string{
					"message": "Stream stopped",
				},
				Status: http.StatusOK,
			})
			connWriteMu.Unlock()
			continue
		}

		// Handle the operation
		response := h.handleOperation(msg)

		// Send response (with mutex protection)
		connWriteMu.Lock()
		err = conn.WriteJSON(response)
		connWriteMu.Unlock()

		if err != nil {
			logrus.Errorf("Failed to write response: %v", err)
			break
		}
	}

	// Clean up active streams
	streamsMu.Lock()
	for _, stream := range activeStreams {
		close(stream.cancel)
	}
	streamsMu.Unlock()

	logrus.Info("WebSocket connection closed")
}

// handleOperation routes the operation to the appropriate handler
func (h *WebSocketHandler) handleOperation(msg WebSocketMessage) WebSocketResponse {
	switch msg.Operation {
	// Filesystem operations
	case "filesystem:get":
		return h.handleFilesystemGet(msg)
	case "filesystem:create":
		return h.handleFilesystemCreate(msg)
	case "filesystem:delete":
		return h.handleFilesystemDelete(msg)
	case "filesystem:tree:get":
		return h.handleFilesystemTreeGet(msg)
	case "filesystem:tree:create":
		return h.handleFilesystemTreeCreate(msg)
	case "filesystem:tree:delete":
		return h.handleFilesystemTreeDelete(msg)

	// Multipart operations
	case "filesystem:multipart:list":
		return h.handleMultipartList(msg)
	case "filesystem:multipart:initiate":
		return h.handleMultipartInitiate(msg)
	case "filesystem:multipart:complete":
		return h.handleMultipartComplete(msg)
	case "filesystem:multipart:abort":
		return h.handleMultipartAbort(msg)
	case "filesystem:multipart:listParts":
		return h.handleMultipartListParts(msg)

	// Process operations
	case "process:execute":
		return h.handleProcessExecute(msg)
	case "process:list":
		return h.handleProcessList(msg)
	case "process:get":
		return h.handleProcessGet(msg)
	case "process:logs":
		return h.handleProcessLogs(msg)
	case "process:logs:stream":
		// Streaming operation - handled differently
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "use process:logs:stream:start to begin streaming",
			Status:  http.StatusBadRequest,
		}
	case "process:logs:stream:start":
		// This will be handled in HandleWebSocket to manage the connection
		return WebSocketResponse{
			ID:      msg.ID,
			Success: true,
			Stream:  true,
			Data: map[string]string{
				"message": "Stream started - logs will be sent as they arrive",
			},
			Status: http.StatusOK,
		}
	case "process:logs:stream:stop":
		// Signal to stop streaming
		return WebSocketResponse{
			ID:      msg.ID,
			Success: true,
			Data: map[string]string{
				"message": "Stream stop requested",
			},
			Status: http.StatusOK,
		}
	case "process:stop":
		return h.handleProcessStop(msg)
	case "process:kill":
		return h.handleProcessKill(msg)

	// Network operations
	case "network:ports:get":
		return h.handleNetworkPortsGet(msg)
	case "network:ports:monitor":
		return h.handleNetworkPortsMonitor(msg)
	case "network:ports:stopMonitor":
		return h.handleNetworkPortsStopMonitor(msg)

	// Codegen operations
	case "codegen:fastapply":
		return h.handleCodegenFastApply(msg)
	case "codegen:reranking":
		return h.handleCodegenReranking(msg)

	default:
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   fmt.Sprintf("Unknown operation: %s", msg.Operation),
			Status:  http.StatusBadRequest,
		}
	}
}

// Filesystem operations

func (h *WebSocketHandler) handleFilesystemGet(msg WebSocketMessage) WebSocketResponse {
	path, ok := msg.Data["path"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "path is required",
			Status:  http.StatusBadRequest,
		}
	}

	path, err := lib.FormatPath(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusBadRequest,
		}
	}

	// Check if it's a directory
	isDir, err := h.fsHandler.DirectoryExists(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	if isDir {
		dir, err := h.fsHandler.ListDirectory(path)
		if err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   fmt.Sprintf("error listing directory: %v", err),
				Status:  http.StatusUnprocessableEntity,
			}
		}
		return WebSocketResponse{
			ID:      msg.ID,
			Success: true,
			Data:    dir,
			Status:  http.StatusOK,
		}
	}

	// Check if it's a file
	isFile, err := h.fsHandler.FileExists(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	if isFile {
		file, err := h.fsHandler.ReadFile(path)
		if err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   fmt.Sprintf("error reading file: %v", err),
				Status:  http.StatusUnprocessableEntity,
			}
		}
		return WebSocketResponse{
			ID:      msg.ID,
			Success: true,
			Data:    file,
			Status:  http.StatusOK,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: false,
		Error:   "file or directory not found",
		Status:  http.StatusNotFound,
	}
}

func (h *WebSocketHandler) handleFilesystemCreate(msg WebSocketMessage) WebSocketResponse {
	path, ok := msg.Data["path"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "path is required",
			Status:  http.StatusBadRequest,
		}
	}

	path, err := lib.FormatPath(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusBadRequest,
		}
	}

	isDirectory := false
	if isDirVal, ok := msg.Data["isDirectory"].(bool); ok {
		isDirectory = isDirVal
	}

	permissions := os.FileMode(0644)
	if permStr, ok := msg.Data["permissions"].(string); ok && permStr != "" {
		permInt, err := strconv.ParseUint(permStr, 8, 32)
		if err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   fmt.Sprintf("invalid permissions format: %v", err),
				Status:  http.StatusBadRequest,
			}
		}
		permissions = os.FileMode(permInt)
	} else if isDirectory {
		permissions = 0755
	}

	if isDirectory {
		err := h.fsHandler.CreateDirectory(path, permissions)
		if err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   fmt.Sprintf("error creating directory: %v", err),
				Status:  http.StatusUnprocessableEntity,
			}
		}
		return WebSocketResponse{
			ID:      msg.ID,
			Success: true,
			Data: map[string]string{
				"path":    path,
				"message": "Directory created successfully",
			},
			Status: http.StatusOK,
		}
	}

	content := ""
	if contentVal, ok := msg.Data["content"].(string); ok {
		content = contentVal
	}

	err = h.fsHandler.WriteFile(path, []byte(content), permissions)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   fmt.Sprintf("error writing file: %v", err),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data: map[string]string{
			"path":    path,
			"message": "File created/updated successfully",
		},
		Status: http.StatusOK,
	}
}

func (h *WebSocketHandler) handleFilesystemDelete(msg WebSocketMessage) WebSocketResponse {
	path, ok := msg.Data["path"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "path is required",
			Status:  http.StatusBadRequest,
		}
	}

	path, err := lib.FormatPath(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusBadRequest,
		}
	}

	recursive := false
	if recursiveVal, ok := msg.Data["recursive"].(bool); ok {
		recursive = recursiveVal
	}

	// Check if it's a directory
	isDir, err := h.fsHandler.DirectoryExists(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	if isDir {
		err := h.fsHandler.DeleteDirectory(path, recursive)
		if err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   fmt.Sprintf("error deleting directory: %v", err),
				Status:  http.StatusUnprocessableEntity,
			}
		}
		return WebSocketResponse{
			ID:      msg.ID,
			Success: true,
			Data: map[string]string{
				"path":    path,
				"message": "Directory deleted successfully",
			},
			Status: http.StatusOK,
		}
	}

	// Check if it's a file
	isFile, err := h.fsHandler.FileExists(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	if isFile {
		err := h.fsHandler.DeleteFile(path)
		if err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   fmt.Sprintf("error deleting file: %v", err),
				Status:  http.StatusUnprocessableEntity,
			}
		}
		return WebSocketResponse{
			ID:      msg.ID,
			Success: true,
			Data: map[string]string{
				"path":    path,
				"message": "File deleted successfully",
			},
			Status: http.StatusOK,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: false,
		Error:   "file or directory not found",
		Status:  http.StatusNotFound,
	}
}

func (h *WebSocketHandler) handleFilesystemTreeGet(msg WebSocketMessage) WebSocketResponse {
	path, ok := msg.Data["path"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "path is required",
			Status:  http.StatusBadRequest,
		}
	}

	path, err := lib.FormatPath(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusBadRequest,
		}
	}

	isDir, err := h.fsHandler.DirectoryExists(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	if !isDir {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "path is not a directory",
			Status:  http.StatusBadRequest,
		}
	}

	dir, err := h.fsHandler.ListDirectory(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   fmt.Sprintf("error getting file system tree: %v", err),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data:    dir,
		Status:  http.StatusOK,
	}
}

func (h *WebSocketHandler) handleFilesystemTreeCreate(msg WebSocketMessage) WebSocketResponse {
	path, ok := msg.Data["path"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "path is required",
			Status:  http.StatusBadRequest,
		}
	}

	path, err := lib.FormatPath(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusBadRequest,
		}
	}

	filesData, ok := msg.Data["files"].(map[string]interface{})
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "files is required",
			Status:  http.StatusBadRequest,
		}
	}

	// Convert to map[string]string
	files := make(map[string]string)
	for k, v := range filesData {
		if strVal, ok := v.(string); ok {
			files[k] = strVal
		}
	}

	// Create the root directory if it doesn't exist
	isDir, err := h.fsHandler.DirectoryExists(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	if !isDir {
		if err := h.fsHandler.CreateDirectory(path, 0755); err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   fmt.Sprintf("error creating root directory: %v", err),
				Status:  http.StatusUnprocessableEntity,
			}
		}
	}

	// Create files
	for filePath, content := range files {
		// Get the absolute path of the file
		absPath := fmt.Sprintf("%s/%s", path, filePath)

		// Create parent directories if they don't exist
		parentDir := absPath[:len(absPath)-len(filePath)-1]
		isDir, err := h.fsHandler.DirectoryExists(parentDir)
		if err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   err.Error(),
				Status:  http.StatusUnprocessableEntity,
			}
		}

		if !isDir {
			if err := h.fsHandler.CreateDirectory(parentDir, 0755); err != nil {
				return WebSocketResponse{
					ID:      msg.ID,
					Success: false,
					Error:   fmt.Sprintf("error creating parent directory: %v", err),
					Status:  http.StatusUnprocessableEntity,
				}
			}
		}

		// Write the file
		if err := h.fsHandler.WriteFile(absPath, []byte(content), 0644); err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   fmt.Sprintf("error writing file: %v", err),
				Status:  http.StatusUnprocessableEntity,
			}
		}
	}

	// Get updated tree
	dir, err := h.fsHandler.ListDirectory(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   fmt.Sprintf("error getting updated file system tree: %v", err),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data:    dir,
		Status:  http.StatusOK,
	}
}

func (h *WebSocketHandler) handleFilesystemTreeDelete(msg WebSocketMessage) WebSocketResponse {
	path, ok := msg.Data["path"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "path is required",
			Status:  http.StatusBadRequest,
		}
	}

	path, err := lib.FormatPath(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusBadRequest,
		}
	}

	recursive := false
	if recursiveVal, ok := msg.Data["recursive"].(bool); ok {
		recursive = recursiveVal
	}

	err = h.fsHandler.DeleteDirectory(path, recursive)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   fmt.Sprintf("error deleting directory: %v", err),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data: map[string]string{
			"path":    path,
			"message": "Directory deleted successfully",
		},
		Status: http.StatusOK,
	}
}

// Multipart operations

func (h *WebSocketHandler) handleMultipartList(msg WebSocketMessage) WebSocketResponse {
	if h.fsHandler.multipartManager == nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "multipart upload not available",
			Status:  http.StatusInternalServerError,
		}
	}

	uploads := h.fsHandler.multipartManager.ListUploads()

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data: map[string]interface{}{
			"uploads": uploads,
		},
		Status: http.StatusOK,
	}
}

func (h *WebSocketHandler) handleMultipartInitiate(msg WebSocketMessage) WebSocketResponse {
	if h.fsHandler.multipartManager == nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "multipart upload not available",
			Status:  http.StatusInternalServerError,
		}
	}

	path, ok := msg.Data["path"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "path is required",
			Status:  http.StatusBadRequest,
		}
	}

	path, err := lib.FormatPath(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusBadRequest,
		}
	}

	// Get absolute path for final destination
	absPath, err := h.fsHandler.fs.GetAbsolutePath(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusBadRequest,
		}
	}

	permissions := os.FileMode(0644)
	if permStr, ok := msg.Data["permissions"].(string); ok && permStr != "" {
		permInt, err := strconv.ParseUint(permStr, 8, 32)
		if err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   fmt.Sprintf("invalid permissions format: %v", err),
				Status:  http.StatusBadRequest,
			}
		}
		permissions = os.FileMode(permInt)
	}

	upload, err := h.fsHandler.multipartManager.InitiateUpload(absPath, permissions)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   fmt.Sprintf("failed to initiate upload: %v", err),
			Status:  http.StatusInternalServerError,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data: map[string]string{
			"uploadId": upload.UploadID,
			"path":     absPath,
		},
		Status: http.StatusOK,
	}
}

func (h *WebSocketHandler) handleMultipartComplete(msg WebSocketMessage) WebSocketResponse {
	if h.fsHandler.multipartManager == nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "multipart upload not available",
			Status:  http.StatusInternalServerError,
		}
	}

	uploadID, ok := msg.Data["uploadId"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "uploadId is required",
			Status:  http.StatusBadRequest,
		}
	}

	partsData, ok := msg.Data["parts"].([]interface{})
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "parts is required",
			Status:  http.StatusBadRequest,
		}
	}

	if len(partsData) == 0 {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "at least one part is required",
			Status:  http.StatusBadRequest,
		}
	}

	// Get upload metadata to get the path
	upload, err := h.fsHandler.multipartManager.GetUpload(uploadID)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusNotFound,
		}
	}

	// Convert parts to UploadedPart
	parts := make([]filesystem.UploadedPart, len(partsData))
	for i, p := range partsData {
		partMap, ok := p.(map[string]interface{})
		if !ok {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   "invalid part format",
				Status:  http.StatusBadRequest,
			}
		}

		partNumber, ok := partMap["partNumber"].(float64)
		if !ok {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   "partNumber is required for each part",
				Status:  http.StatusBadRequest,
			}
		}

		etag, ok := partMap["etag"].(string)
		if !ok {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   "etag is required for each part",
				Status:  http.StatusBadRequest,
			}
		}

		parts[i] = filesystem.UploadedPart{
			PartNumber: int(partNumber),
			ETag:       etag,
		}
	}

	if err := h.fsHandler.multipartManager.CompleteUpload(uploadID, parts); err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   fmt.Sprintf("failed to complete upload: %v", err),
			Status:  http.StatusInternalServerError,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data: map[string]string{
			"path":    upload.Path,
			"message": "Multipart upload completed successfully",
		},
		Status: http.StatusOK,
	}
}

func (h *WebSocketHandler) handleMultipartAbort(msg WebSocketMessage) WebSocketResponse {
	if h.fsHandler.multipartManager == nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "multipart upload not available",
			Status:  http.StatusInternalServerError,
		}
	}

	uploadID, ok := msg.Data["uploadId"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "uploadId is required",
			Status:  http.StatusBadRequest,
		}
	}

	if err := h.fsHandler.multipartManager.AbortUpload(uploadID); err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusNotFound,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data: map[string]string{
			"message": "Multipart upload aborted successfully",
		},
		Status: http.StatusOK,
	}
}

func (h *WebSocketHandler) handleMultipartListParts(msg WebSocketMessage) WebSocketResponse {
	if h.fsHandler.multipartManager == nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "multipart upload not available",
			Status:  http.StatusInternalServerError,
		}
	}

	uploadID, ok := msg.Data["uploadId"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "uploadId is required",
			Status:  http.StatusBadRequest,
		}
	}

	parts, err := h.fsHandler.multipartManager.ListParts(uploadID)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusNotFound,
		}
	}

	// Convert pointers to values for the response
	partsList := make([]filesystem.UploadedPart, len(parts))
	for i, p := range parts {
		partsList[i] = *p
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data: map[string]interface{}{
			"uploadId": uploadID,
			"parts":    partsList,
		},
		Status: http.StatusOK,
	}
}

// Process operations

func (h *WebSocketHandler) handleProcessExecute(msg WebSocketMessage) WebSocketResponse {
	command, ok := msg.Data["command"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "command is required",
			Status:  http.StatusBadRequest,
		}
	}

	name := ""
	if nameVal, ok := msg.Data["name"].(string); ok {
		name = nameVal
	}

	workingDir := ""
	if wdVal, ok := msg.Data["workingDir"].(string); ok {
		workingDir = wdVal
		formattedWorkingDir, err := lib.FormatPath(workingDir)
		if err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   err.Error(),
				Status:  http.StatusBadRequest,
			}
		}
		workingDir = formattedWorkingDir
	}

	env := make(map[string]string)
	if envVal, ok := msg.Data["env"].(map[string]interface{}); ok {
		for k, v := range envVal {
			if strVal, ok := v.(string); ok {
				env[k] = strVal
			}
		}
	}

	waitForCompletion := false
	if wfcVal, ok := msg.Data["waitForCompletion"].(bool); ok {
		waitForCompletion = wfcVal
	}

	timeout := 0
	if timeoutVal, ok := msg.Data["timeout"].(float64); ok {
		timeout = int(timeoutVal)
	}

	var waitForPorts []int
	if wfpVal, ok := msg.Data["waitForPorts"].([]interface{}); ok {
		for _, port := range wfpVal {
			if portFloat, ok := port.(float64); ok {
				waitForPorts = append(waitForPorts, int(portFloat))
			}
		}
	}

	restartOnFailure := false
	if rofVal, ok := msg.Data["restartOnFailure"].(bool); ok {
		restartOnFailure = rofVal
	}

	maxRestarts := 0
	if mrVal, ok := msg.Data["maxRestarts"].(float64); ok {
		maxRestarts = int(mrVal)
	}

	// Check if a process with the same name already exists and is running
	if name != "" {
		if existingProc, err := h.processHandler.GetProcess(name); err == nil && existingProc.Status == "running" {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   fmt.Sprintf("process with name '%s' already exists and is running", name),
				Status:  http.StatusBadRequest,
			}
		}
	}

	processInfo, err := h.processHandler.ExecuteProcess(command, workingDir, name, env, waitForCompletion, timeout, waitForPorts, restartOnFailure, maxRestarts)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data:    processInfo,
		Status:  http.StatusOK,
	}
}

func (h *WebSocketHandler) handleProcessList(msg WebSocketMessage) WebSocketResponse {
	processes := h.processHandler.ListProcesses()
	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data: map[string]interface{}{
			"processes": processes,
		},
		Status: http.StatusOK,
	}
}

func (h *WebSocketHandler) handleProcessGet(msg WebSocketMessage) WebSocketResponse {
	identifier, ok := msg.Data["identifier"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "identifier is required",
			Status:  http.StatusBadRequest,
		}
	}

	processInfo, err := h.processHandler.GetProcess(identifier)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusNotFound,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data:    processInfo,
		Status:  http.StatusOK,
	}
}

func (h *WebSocketHandler) handleProcessLogs(msg WebSocketMessage) WebSocketResponse {
	identifier, ok := msg.Data["identifier"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "identifier is required",
			Status:  http.StatusBadRequest,
		}
	}

	logs, err := h.processHandler.GetProcessOutput(identifier)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusNotFound,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data:    logs,
		Status:  http.StatusOK,
	}
}

func (h *WebSocketHandler) handleProcessStop(msg WebSocketMessage) WebSocketResponse {
	identifier, ok := msg.Data["identifier"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "identifier is required",
			Status:  http.StatusBadRequest,
		}
	}

	err := h.processHandler.StopProcess(identifier)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusNotFound,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data: map[string]string{
			"message": "Process stopped successfully",
		},
		Status: http.StatusOK,
	}
}

func (h *WebSocketHandler) handleProcessKill(msg WebSocketMessage) WebSocketResponse {
	identifier, ok := msg.Data["identifier"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "identifier is required",
			Status:  http.StatusBadRequest,
		}
	}

	err := h.processHandler.KillProcess(identifier)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusNotFound,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data: map[string]string{
			"message": "Process killed successfully",
		},
		Status: http.StatusOK,
	}
}

// Network operations

func (h *WebSocketHandler) handleNetworkPortsGet(msg WebSocketMessage) WebSocketResponse {
	pidVal, ok := msg.Data["pid"]
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "pid is required",
			Status:  http.StatusBadRequest,
		}
	}

	var pid int
	switch v := pidVal.(type) {
	case float64:
		pid = int(v)
	case string:
		var err error
		pid, err = strconv.Atoi(v)
		if err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   "invalid PID",
				Status:  http.StatusBadRequest,
			}
		}
	default:
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "invalid PID type",
			Status:  http.StatusBadRequest,
		}
	}

	ports, err := h.networkHandler.GetPortsForPID(pid)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data: map[string]interface{}{
			"pid":   pid,
			"ports": ports,
		},
		Status: http.StatusOK,
	}
}

func (h *WebSocketHandler) handleNetworkPortsMonitor(msg WebSocketMessage) WebSocketResponse {
	pidVal, ok := msg.Data["pid"]
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "pid is required",
			Status:  http.StatusBadRequest,
		}
	}

	var pid int
	switch v := pidVal.(type) {
	case float64:
		pid = int(v)
	case string:
		var err error
		pid, err = strconv.Atoi(v)
		if err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   "invalid PID",
				Status:  http.StatusBadRequest,
			}
		}
	default:
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "invalid PID type",
			Status:  http.StatusBadRequest,
		}
	}

	callback, ok := msg.Data["callback"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "callback is required",
			Status:  http.StatusBadRequest,
		}
	}

	// Register a callback
	h.networkHandler.RegisterPortOpenCallback(pid, func(pid int, port *network.PortInfo) {
		type PortCallbackRequest struct {
			PID  int `json:"pid"`
			Port int `json:"port"`
		}
		jsonData, err := json.Marshal(PortCallbackRequest{PID: pid, Port: port.LocalPort})
		if err != nil {
			logrus.Debugf("Error marshalling port callback request: %v", err)
			return
		}
		// Make HTTP POST request to callback URL
		client := &http.Client{Timeout: 10 * time.Second}
		req, err := http.NewRequest("POST", callback, nil)
		if err != nil {
			logrus.Debugf("Error creating port callback request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Body = io.NopCloser(strings.NewReader(string(jsonData)))
		resp, err := client.Do(req)
		if err != nil {
			logrus.Debugf("Error sending port callback request: %v", err)
			return
		}
		defer resp.Body.Close()
		logrus.Debugf("Port callback request sent to %s", callback)
	})

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data: map[string]string{
			"message": "Port monitoring started",
		},
		Status: http.StatusOK,
	}
}

func (h *WebSocketHandler) handleNetworkPortsStopMonitor(msg WebSocketMessage) WebSocketResponse {
	pidVal, ok := msg.Data["pid"]
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "pid is required",
			Status:  http.StatusBadRequest,
		}
	}

	var pid int
	switch v := pidVal.(type) {
	case float64:
		pid = int(v)
	case string:
		var err error
		pid, err = strconv.Atoi(v)
		if err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   "invalid PID",
				Status:  http.StatusBadRequest,
			}
		}
	default:
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "invalid PID type",
			Status:  http.StatusBadRequest,
		}
	}

	h.networkHandler.UnregisterPortOpenCallback(pid)

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data: map[string]string{
			"message": "Port monitoring stopped",
		},
		Status: http.StatusOK,
	}
}

// Codegen operations

func (h *WebSocketHandler) handleCodegenFastApply(msg WebSocketMessage) WebSocketResponse {
	path, ok := msg.Data["path"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "path is required",
			Status:  http.StatusBadRequest,
		}
	}

	path, err := lib.FormatPath(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusBadRequest,
		}
	}

	codeEdit, ok := msg.Data["codeEdit"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "codeEdit is required",
			Status:  http.StatusBadRequest,
		}
	}

	model := "auto"
	if modelVal, ok := msg.Data["model"].(string); ok && modelVal != "" {
		model = modelVal
	}

	// Check if path is a directory
	isDir, err := h.fsHandler.DirectoryExists(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   fmt.Sprintf("failed to check path: %v", err),
			Status:  http.StatusUnprocessableEntity,
		}
	}
	if isDir {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "path is a directory, not a file",
			Status:  http.StatusBadRequest,
		}
	}

	// Check if file exists and read its content
	fileExists, err := h.fsHandler.FileExists(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   fmt.Sprintf("failed to check if file exists: %v", err),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	var originalContent string
	if fileExists {
		file, err := h.fsHandler.ReadFile(path)
		if err != nil {
			return WebSocketResponse{
				ID:      msg.ID,
				Success: false,
				Error:   fmt.Sprintf("failed to read file: %v", err),
				Status:  http.StatusUnprocessableEntity,
			}
		}
		originalContent = string(file.Content)
	}

	// Create codegen client
	client, err := codegen.NewClient()
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   fmt.Sprintf("failed to create codegen client: %v", err),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	// Apply the code edit
	updatedContent, err := client.ApplyCodeEdit(originalContent, codeEdit, model)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   fmt.Sprintf("failed to apply code edit: %v", err),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	// Write the updated content back to the file
	err = h.fsHandler.WriteFile(path, []byte(updatedContent), 0644)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   fmt.Sprintf("failed to write file: %v", err),
			Status:  http.StatusUnprocessableEntity,
		}
	}

	return WebSocketResponse{
		ID:      msg.ID,
		Success: true,
		Data: map[string]interface{}{
			"success":         true,
			"path":            path,
			"originalContent": originalContent,
			"updatedContent":  updatedContent,
			"provider":        client.ProviderName(),
			"message":         fmt.Sprintf("Code edit applied successfully to %s", path),
		},
		Status: http.StatusOK,
	}
}

func (h *WebSocketHandler) handleCodegenReranking(msg WebSocketMessage) WebSocketResponse {
	path, ok := msg.Data["path"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "path is required",
			Status:  http.StatusBadRequest,
		}
	}

	path, err := lib.FormatPath(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   err.Error(),
			Status:  http.StatusBadRequest,
		}
	}

	_, ok = msg.Data["query"].(string)
	if !ok {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "query is required",
			Status:  http.StatusBadRequest,
		}
	}

	// Check if directory exists
	isDir, err := h.fsHandler.DirectoryExists(path)
	if err != nil {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   fmt.Sprintf("failed to check directory: %v", err),
			Status:  http.StatusUnprocessableEntity,
		}
	}
	if !isDir {
		return WebSocketResponse{
			ID:      msg.ID,
			Success: false,
			Error:   "path is not a directory",
			Status:  http.StatusBadRequest,
		}
	}

	// This is a simplified version - full implementation would need the codegen client
	// For now, return a not implemented error
	return WebSocketResponse{
		ID:      msg.ID,
		Success: false,
		Error:   "codegen reranking is not yet implemented via websocket",
		Status:  http.StatusNotImplemented,
	}
}

// streamProcessLogs streams process logs to the websocket connection
func (h *WebSocketHandler) streamProcessLogs(conn *websocket.Conn, connWriteMu *sync.Mutex, requestID string, identifier string, cancel chan struct{}, connectionDone chan struct{}) {
	// Create a custom writer that sends log lines via websocket
	logWriter := &websocketLogWriter{
		conn:      conn,
		requestID: requestID,
		mu:        connWriteMu,
	}

	// Start streaming logs from the process
	if err := h.processHandler.StreamProcessOutput(identifier, logWriter); err != nil {
		connWriteMu.Lock()
		conn.WriteJSON(WebSocketResponse{
			ID:      requestID,
			Success: false,
			Stream:  true,
			Done:    true,
			Error:   fmt.Sprintf("failed to stream logs: %v", err),
			Status:  http.StatusNotFound,
		})
		connWriteMu.Unlock()
		return
	}

	// Monitor process status to detect completion
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	// Wait for process completion, cancellation, or connection close
	for {
		select {
		case <-cancel:
			logrus.Debugf("Log stream cancelled for request %s", requestID)
			h.processHandler.RemoveLogWriter(identifier, logWriter)

			connWriteMu.Lock()
			conn.WriteJSON(WebSocketResponse{
				ID:      requestID,
				Success: true,
				Stream:  true,
				Done:    true,
				Data: map[string]string{
					"message": "Stream cancelled",
				},
				Status: http.StatusOK,
			})
			connWriteMu.Unlock()
			return

		case <-connectionDone:
			logrus.Debugf("Connection closed, stopping log stream for request %s", requestID)
			h.processHandler.RemoveLogWriter(identifier, logWriter)
			return

		case <-ticker.C:
			// Check if process has completed
			processInfo, err := h.processHandler.GetProcess(identifier)
			if err != nil {
				// Process not found, end stream
				logrus.Debugf("Process not found, ending stream for request %s", requestID)
				h.processHandler.RemoveLogWriter(identifier, logWriter)

				connWriteMu.Lock()
				conn.WriteJSON(WebSocketResponse{
					ID:      requestID,
					Success: true,
					Stream:  true,
					Done:    true,
					Data: map[string]string{
						"message": "Process not found",
					},
					Status: http.StatusOK,
				})
				connWriteMu.Unlock()
				return
			}

			// Check if process is no longer running
			if processInfo.Status != "running" {
				logrus.Debugf("Process completed, ending stream for request %s", requestID)

				// Give it a moment for final logs to be written
				time.Sleep(100 * time.Millisecond)

				h.processHandler.RemoveLogWriter(identifier, logWriter)

				connWriteMu.Lock()
				conn.WriteJSON(WebSocketResponse{
					ID:      requestID,
					Success: true,
					Stream:  true,
					Done:    true,
					Data: map[string]string{
						"message": "Stream ended - process completed",
					},
					Status: http.StatusOK,
				})
				connWriteMu.Unlock()
				return
			}
		}
	}
}

// websocketLogWriter implements io.Writer to send logs via websocket
type websocketLogWriter struct {
	conn      *websocket.Conn
	requestID string
	mu        *sync.Mutex
}

func (w *websocketLogWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Send log data as a streaming message
	err := w.conn.WriteJSON(WebSocketResponse{
		ID:      w.requestID,
		Success: true,
		Stream:  true,
		Data: map[string]interface{}{
			"log": string(data),
		},
		Status: http.StatusOK,
	})
	if err != nil {
		return 0, err
	}
	return len(data), nil
}
