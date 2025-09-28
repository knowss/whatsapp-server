package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	_ "github.com/mattn/go-sqlite3"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// Request/Response types for WebSocket communication
type PairingRequest struct {
	PhoneNumber string `json:"phoneNumber"`
}

type PairingResponse struct {
	PairingCode string `json:"pairingCode,omitempty"`
	Success     bool   `json:"success"`
	Error       string `json:"error,omitempty"`
}

type StatusRequest struct {
	Action string `json:"action"` // "checkStatus", "getMessages"
}

type StatusResponse struct {
	IsLoggedIn bool      `json:"isLoggedIn"`
	DeviceID   string    `json:"deviceId,omitempty"`
	Messages   []Message `json:"messages,omitempty"`
	Chats      []Chat    `json:"chats,omitempty"`
	Success    bool      `json:"success"`
	Error      string    `json:"error,omitempty"`
}

type Message struct {
	ID          string `json:"id"`
	ChatID      string `json:"chatId"`
	ContactName string `json:"contactName"`
	Body        string `json:"body"`
	Timestamp   int64  `json:"timestamp"`
	IsFromMe    bool   `json:"isFromMe"`
}

type Chat struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	IsGroup      bool   `json:"isGroup"`
	LastMessage  int64  `json:"lastMessage"`
	MessageCount int    `json:"messageCount"`
}

// WhatsApp client manager
type WhatsAppManager struct {
	container *sqlstore.Container
	client    *whatsmeow.Client
	device    *store.Device
	mutex     sync.RWMutex
	isReady   bool
	connected chan bool
}

var waManager *WhatsAppManager

func initWhatsApp() error {
	log.Println("🔄 Initializing WhatsApp client...")

	// Create database container
	dbLog := waLog.Stdout("Database", "INFO", true)
	ctx := context.Background()

	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "file:whatsapp.db?_foreign_keys=on"
	}

	container, err := sqlstore.New(ctx, "sqlite3", dbPath, dbLog)
	if err != nil {
		return fmt.Errorf("failed to create sqlstore: %w", err)
	}

	waManager = &WhatsAppManager{
		container: container,
		connected: make(chan bool, 1),
	}

	// Check if we have an existing paired device and try to connect
	device, err := container.GetFirstDevice(ctx)
	if err == nil && device.ID != nil {
		log.Printf("📱 Found existing paired device: %s", device.ID.String())
		log.Printf("🔄 Attempting to reconnect to existing device...")

		// Create client for existing device
		if err := waManager.createClient(); err != nil {
			log.Printf("⚠️ Failed to create client for existing device: %v", err)
		} else {
			// Try to connect in background
			go func() {
				if !waManager.client.IsConnected() {
					log.Printf("🔗 Connecting existing device to WhatsApp...")
					if err := waManager.client.Connect(); err != nil {
						log.Printf("⚠️ Failed to reconnect existing device: %v", err)
					} else {
						log.Printf("✅ Successfully reconnected existing device")
					}
				}
			}()
		}
	} else {
		log.Printf("📱 No existing paired device found")
	}

	log.Println("✅ WhatsApp manager initialized")
	return nil
}

func (wm *WhatsAppManager) createClient() error {
	wm.mutex.Lock()
	defer wm.mutex.Unlock()

	if wm.client != nil {
		log.Printf("📱 WhatsApp client already exists")
		return nil // Already created
	}

	ctx := context.Background()

	// Get first device or create new one
	device, err := wm.container.GetFirstDevice(ctx)
	if err != nil {
		log.Printf("Creating new device...")
		device = wm.container.NewDevice()
	}

	wm.device = device

	// Create client
	clientLog := waLog.Stdout("Client", "INFO", true)
	wm.client = whatsmeow.NewClient(device, clientLog)

	// Add event handler
	wm.client.AddEventHandler(wm.eventHandler)

	if device.ID != nil {
		log.Printf("📱 WhatsApp client created for device: %s", device.ID.String())
	} else {
		log.Printf("📱 WhatsApp client created for new device (not yet paired)")
	}
	return nil
}

func (wm *WhatsAppManager) eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Connected:
		log.Println("🔗 Connected to WhatsApp servers")
		wm.mutex.Lock()
		wm.isReady = true
		wm.mutex.Unlock()

		// Signal that we're ready
		select {
		case wm.connected <- true:
		default:
		}

	case *events.Disconnected:
		log.Println("🔌 Disconnected from WhatsApp servers")
		wm.mutex.Lock()
		wm.isReady = false
		wm.mutex.Unlock()

	case *events.LoggedOut:
		log.Println("📤 Logged out from WhatsApp")

	case *events.Message:
		log.Printf("📨 Received message: %s", v.Message.GetConversation())

	case *events.QR:
		log.Printf("📱 QR codes generated (not using for pairing)")
		// We don't use QR codes for pairing, but this event indicates connection readiness
		select {
		case wm.connected <- true:
		default:
		}
	}
}

func (wm *WhatsAppManager) requestPairingCode(phoneNumber string) (string, error) {
	log.Printf("🔄 Starting real WhatsApp pairing process for %s", phoneNumber)

	// Create client if needed
	log.Printf("📱 Creating WhatsApp client...")
	if err := wm.createClient(); err != nil {
		log.Printf("❌ Failed to create client: %v", err)
		return "", fmt.Errorf("failed to create client: %w", err)
	}

	// Check if already logged in and connected
	if wm.client.Store.ID != nil {
		log.Printf("📱 Device has stored ID: %s", wm.client.Store.ID.String())

		// Check if client is logged in according to whatsmeow
		if wm.client.IsLoggedIn() {
			log.Printf("✅ Already logged in and connected with device ID: %s", wm.client.Store.ID.String())
			return "ALREADY-LOGGED-IN", nil
		}

		// If we have a stored ID but not connected, try to connect first
		log.Printf("📱 Device paired but not connected, attempting to connect...")
		if !wm.client.IsConnected() {
			err := wm.client.Connect()
			if err != nil {
				log.Printf("❌ Failed to connect: %v", err)
				// Don't return error immediately, might just need new pairing
				log.Printf("⚠️ Connection failed, will continue with pairing process")
			} else {
				// Wait for connection and check if we're logged in
				log.Println("⏳ Waiting for connection to establish...")
				time.Sleep(3 * time.Second)

				if wm.client.IsLoggedIn() {
					log.Println("✅ Successfully reconnected with existing credentials")
					return "ALREADY-LOGGED-IN", nil
				} else {
					log.Println("⚠️ Connected but not logged in, need new pairing")
				}
			}
		} else {
			log.Printf("📡 Client is connected but not logged in, checking status...")
			if wm.client.IsLoggedIn() {
				log.Printf("✅ Actually logged in after status check")
				return "ALREADY-LOGGED-IN", nil
			}
		}
	}

	// Connect to WhatsApp (only if not already connected)
	if !wm.client.IsConnected() {
		log.Printf("🔗 Connecting to WhatsApp servers...")
		err := wm.client.Connect()
		if err != nil {
			log.Printf("❌ Failed to connect to WhatsApp: %v", err)
			return "", fmt.Errorf("failed to connect: %w", err)
		}
	} else {
		log.Printf("✅ WhatsApp client already connected")
	}

	// Wait for connection to be ready with shorter timeout
	log.Println("⏳ Waiting for WhatsApp connection to be ready...")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	select {
	case <-wm.connected:
		log.Println("✅ WhatsApp connection ready")
	case <-ctx.Done():
		log.Println("❌ Timeout waiting for WhatsApp connection")
		return "", fmt.Errorf("timeout waiting for connection")
	}

	// Request pairing code
	log.Printf("📞 Requesting real pairing code for %s", phoneNumber)
	pairingCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pairingCode, err := wm.client.PairPhone(pairingCtx, phoneNumber, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		log.Printf("❌ Failed to get pairing code: %v", err)
		return "", fmt.Errorf("failed to get pairing code: %w", err)
	}

	log.Printf("✅ Generated real WhatsApp pairing code: %s", pairingCode)
	return pairingCode, nil
}

func (wm *WhatsAppManager) isLoggedIn() bool {
	wm.mutex.RLock()
	defer wm.mutex.RUnlock()

	if wm.client == nil {
		log.Printf("🔍 isLoggedIn: client is nil")
		return false
	}

	hasDeviceID := wm.client.Store.ID != nil
	isLoggedIn := wm.client.IsLoggedIn()
	isConnected := wm.client.IsConnected()

	log.Printf("🔍 Login status check: hasDeviceID=%v, isLoggedIn=%v, isConnected=%v", hasDeviceID, isLoggedIn, isConnected)

	// First check if we have basic requirements
	if !hasDeviceID {
		log.Printf("🔍 No device ID stored")
		return false
	}

	// If client says we're logged in, we're good
	if isLoggedIn {
		log.Printf("🔍 Client reports logged in")
		return true
	}

	// If we have a device ID but client says not logged in, try to connect and check again
	if !isConnected {
		log.Printf("🔍 Not connected, attempting to connect...")
		go func() {
			if err := wm.client.Connect(); err != nil {
				log.Printf("🔍 Background connection failed: %v", err)
			} else {
				log.Printf("🔍 Background connection successful")
			}
		}()
		// Return false for now, connection will happen in background
		return false
	}

	// Connected but not logged in - this shouldn't happen if device is properly paired
	log.Printf("🔍 Connected but not logged in - device may need re-pairing")
	return false
}

func (wm *WhatsAppManager) getDeviceID() string {
	wm.mutex.RLock()
	defer wm.mutex.RUnlock()

	if wm.client == nil || wm.client.Store.ID == nil {
		return ""
	}

	return wm.client.Store.ID.String()
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("❌ WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	log.Println("📱 iOS app connected")

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Printf("❌ Read error: %v", err)
			break
		}

		log.Printf("📥 Received: %s", string(message))

		// Parse message type
		var msgType map[string]interface{}
		if err := json.Unmarshal(message, &msgType); err != nil {
			log.Printf("❌ JSON parse error: %v", err)
			continue
		}

		if _, exists := msgType["action"]; exists {
			handleStatusRequest(conn, message)
		} else if _, exists := msgType["phoneNumber"]; exists {
			handlePairingRequest(conn, message)
		} else {
			log.Printf("❌ Unknown message type")
		}
	}
}

func handlePairingRequest(conn *websocket.Conn, message []byte) {
	log.Printf("🔍 Processing pairing request...")

	var req PairingRequest
	if err := json.Unmarshal(message, &req); err != nil {
		log.Printf("❌ JSON unmarshal error: %v", err)
		sendError(conn, "Invalid pairing request")
		return
	}

	log.Printf("📱 Pairing request for: %s", req.PhoneNumber)

	// Validate phone number
	if len(req.PhoneNumber) < 10 || !strings.HasPrefix(req.PhoneNumber, "+") {
		log.Printf("❌ Invalid phone number: %s", req.PhoneNumber)
		sendPairingResponse(conn, "", false, "Invalid phone number format. Use international format like +33685606511")
		return
	}

	// Request pairing code
	log.Printf("⏳ Requesting pairing code...")
	pairingCode, err := waManager.requestPairingCode(req.PhoneNumber)
	if err != nil {
		log.Printf("❌ Pairing error: %v", err)
		sendPairingResponse(conn, "", false, err.Error())
		return
	}

	// Handle special case for already logged in
	if pairingCode == "ALREADY-LOGGED-IN" {
		log.Printf("✅ Device already logged in, sending success response")
		sendPairingResponse(conn, "", true, "Device is already logged in and connected to WhatsApp")
	} else {
		log.Printf("✅ Sending pairing response: %s", pairingCode)
		sendPairingResponse(conn, pairingCode, true, "")
	}
}

func handleStatusRequest(conn *websocket.Conn, message []byte) {
	var req StatusRequest
	if err := json.Unmarshal(message, &req); err != nil {
		sendError(conn, "Invalid status request")
		return
	}

	switch req.Action {
	case "checkStatus":
		isLoggedIn := waManager.isLoggedIn()
		deviceID := waManager.getDeviceID()
		log.Printf("🔍 Status check result: isLoggedIn=%v, deviceID=%s", isLoggedIn, deviceID)
		sendStatusResponse(conn, isLoggedIn, deviceID, nil, nil, true, "")
	case "forceReconnect":
		// Force a reconnection attempt for existing devices
		if waManager.client != nil && waManager.client.Store.ID != nil {
			log.Printf("🔄 Force reconnection requested for device: %s", waManager.client.Store.ID.String())
			go func() {
				if !waManager.client.IsConnected() {
					if err := waManager.client.Connect(); err != nil {
						log.Printf("❌ Force reconnection failed: %v", err)
					} else {
						log.Printf("✅ Force reconnection successful")
					}
				} else {
					log.Printf("📡 Already connected, checking login status")
				}
			}()
			sendStatusResponse(conn, false, waManager.getDeviceID(), nil, nil, true, "Reconnection attempt started")
		} else {
			sendStatusResponse(conn, false, "", nil, nil, false, "No paired device found")
		}
	case "getMessages":
		if !waManager.isLoggedIn() {
			sendStatusResponse(conn, false, "", nil, nil, false, "Not logged in")
			return
		}
		// Return mock data for now
		messages := []Message{
			{
				ID:          "msg1",
				ChatID:      "chat1",
				ContactName: "Mom",
				Body:        "How are you doing?",
				Timestamp:   time.Now().Unix(),
				IsFromMe:    false,
			},
		}
		chats := []Chat{
			{
				ID:           "chat1",
				Name:         "Mom",
				IsGroup:      false,
				LastMessage:  time.Now().Unix(),
				MessageCount: 1,
			},
		}
		sendStatusResponse(conn, true, waManager.getDeviceID(), messages, chats, true, "")
	default:
		sendStatusResponse(conn, false, "", nil, nil, false, "Unknown action")
	}
}

func sendPairingResponse(conn *websocket.Conn, code string, success bool, errMsg string) {
	resp := PairingResponse{
		PairingCode: code,
		Success:     success,
		Error:       errMsg,
	}
	data, _ := json.Marshal(resp)
	conn.WriteMessage(websocket.TextMessage, data)
}

func sendStatusResponse(conn *websocket.Conn, loggedIn bool, deviceID string, messages []Message, chats []Chat, success bool, errMsg string) {
	resp := StatusResponse{
		IsLoggedIn: loggedIn,
		DeviceID:   deviceID,
		Messages:   messages,
		Chats:      chats,
		Success:    success,
		Error:      errMsg,
	}
	data, _ := json.Marshal(resp)
	conn.WriteMessage(websocket.TextMessage, data)
}

func sendError(conn *websocket.Conn, errMsg string) {
	resp := map[string]interface{}{
		"success": false,
		"error":   errMsg,
	}
	data, _ := json.Marshal(resp)
	conn.WriteMessage(websocket.TextMessage, data)
}

func main() {
	log.Println("🚀 Starting WhatsApp Pairing Server...")

	// Initialize WhatsApp
	if err := initWhatsApp(); err != nil {
		log.Fatalf("❌ Failed to initialize WhatsApp: %v", err)
	}

	// Setup routes
	http.HandleFunc("/ws", handleWebSocket)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "WhatsApp Pairing Server - Ready")
	})
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "healthy",
			"service": "whatsapp-pairing-server",
		})
	})

	// Get port
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("🌐 Server starting on port %s", port)
	log.Printf("📱 WebSocket endpoint: /ws")
	log.Printf("💚 Health check: /health")

	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("❌ Server failed: %v", err)
	}
}