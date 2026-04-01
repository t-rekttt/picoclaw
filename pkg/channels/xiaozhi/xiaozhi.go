package xiaozhi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/sipeed/picoclaw/pkg/audio/asr"
	"github.com/sipeed/picoclaw/pkg/audio/tts"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/identity"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// xiaozhiConn represents a single device WebSocket connection.
type xiaozhiConn struct {
	id        string
	conn      *websocket.Conn
	sessionID string
	deviceID  string
	writeMu   sync.Mutex
	closed    atomic.Bool
	cancel    context.CancelFunc
	state     atomic.Value // connStateIdle | connStateListening | connStateSpeaking

	// Voice accumulation for the current listening session.
	voice     *voiceAccumulator
	voiceMu   sync.Mutex
	ttsCancel context.CancelFunc // cancel active TTS streaming
}

// writeJSON sends a JSON message with write locking.
func (c *xiaozhiConn) writeJSON(v any) error {
	if c.closed.Load() {
		return fmt.Errorf("connection closed")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteJSON(v)
}

// writeBinary sends a binary message with write locking.
func (c *xiaozhiConn) writeBinary(data []byte) error {
	if c.closed.Load() {
		return fmt.Errorf("connection closed")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteMessage(websocket.BinaryMessage, data)
}

// close closes the connection and cancels associated goroutines.
func (c *xiaozhiConn) close() {
	if c.closed.CompareAndSwap(false, true) {
		if c.cancel != nil {
			c.cancel()
		}
		c.cancelTTS()
		c.closeVoice()
		c.conn.Close()
	}
}

// cancelTTS cancels any active TTS streaming.
func (c *xiaozhiConn) cancelTTS() {
	c.voiceMu.Lock()
	defer c.voiceMu.Unlock()
	if c.ttsCancel != nil {
		c.ttsCancel()
		c.ttsCancel = nil
	}
}

// closeVoice closes and cleans up any active voice accumulator.
func (c *xiaozhiConn) closeVoice() {
	c.voiceMu.Lock()
	defer c.voiceMu.Unlock()
	if c.voice != nil {
		c.voice.Close()
		c.voice = nil
	}
}

// XiaozhiChannel implements a xiaozhi-compatible WebSocket server channel.
// Xiaozhi devices (like DS-01) connect and stream Opus audio for voice interaction.
type XiaozhiChannel struct {
	*channels.BaseChannel
	cfg         config.XiaozhiConfig
	fullCfg     *config.Config
	upgrader    websocket.Upgrader
	connections map[string]*xiaozhiConn
	connsMu     sync.RWMutex
	transcriber asr.Transcriber
	ttsProvider tts.TTSProvider
	msgBus      *bus.MessageBus
	ctx         context.Context
	cancel      context.CancelFunc
}

// NewXiaozhiChannel creates a new xiaozhi protocol channel.
func NewXiaozhiChannel(cfg *config.Config, messageBus *bus.MessageBus) (*XiaozhiChannel, error) {
	xCfg := cfg.Channels.Xiaozhi
	base := channels.NewBaseChannel("xiaozhi", xCfg, messageBus, xCfg.AllowFrom)

	transcriber := asr.DetectTranscriber(cfg)
	ttsProvider := tts.DetectTTS(cfg)

	if transcriber == nil {
		logger.WarnCF("xiaozhi", "No ASR transcriber configured — voice input will not work", nil)
	}
	if ttsProvider == nil {
		logger.WarnCF("xiaozhi", "No TTS provider configured — voice output will not work", nil)
	}

	return &XiaozhiChannel{
		BaseChannel: base,
		cfg:         xCfg,
		fullCfg:     cfg,
		upgrader: websocket.Upgrader{
			CheckOrigin:     func(r *http.Request) bool { return true },
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
		},
		connections: make(map[string]*xiaozhiConn),
		transcriber: transcriber,
		ttsProvider: ttsProvider,
		msgBus:      messageBus,
	}, nil
}

// Start implements channels.Channel.
func (ch *XiaozhiChannel) Start(ctx context.Context) error {
	logger.InfoC("xiaozhi", "Starting Xiaozhi channel")
	ch.ctx, ch.cancel = context.WithCancel(ctx)
	ch.SetRunning(true)
	logger.InfoC("xiaozhi", "Xiaozhi channel started")
	return nil
}

// Stop implements channels.Channel.
func (ch *XiaozhiChannel) Stop(ctx context.Context) error {
	logger.InfoC("xiaozhi", "Stopping Xiaozhi channel")
	ch.SetRunning(false)

	// Close all device connections.
	ch.connsMu.Lock()
	all := make([]*xiaozhiConn, 0, len(ch.connections))
	for _, c := range ch.connections {
		all = append(all, c)
	}
	clear(ch.connections)
	ch.connsMu.Unlock()

	for _, c := range all {
		c.close()
	}

	if ch.cancel != nil {
		ch.cancel()
	}

	logger.InfoC("xiaozhi", "Xiaozhi channel stopped")
	return nil
}

// WebhookPath implements channels.WebhookHandler.
func (ch *XiaozhiChannel) WebhookPath() string { return "/xiaozhi/" }

// ServeHTTP implements http.Handler for the gateway HTTP mux.
func (ch *XiaozhiChannel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/xiaozhi")
	switch path {
	case "/v1/chat", "/v1/chat/":
		ch.handleUpgrade(w, r)
	default:
		http.NotFound(w, r)
	}
}

// VoiceCapabilities implements channels.VoiceCapabilityProvider.
func (ch *XiaozhiChannel) VoiceCapabilities() channels.VoiceCapabilities {
	return channels.VoiceCapabilities{
		ASR: ch.transcriber != nil,
		TTS: ch.ttsProvider != nil,
	}
}

// Send implements channels.Channel — converts text response to TTS audio and streams to device.
func (ch *XiaozhiChannel) Send(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	if !ch.IsRunning() {
		return nil, channels.ErrNotRunning
	}

	// chatID format: "xiaozhi:<deviceID>"
	deviceID := strings.TrimPrefix(msg.ChatID, "xiaozhi:")
	conn := ch.findConnectionByDevice(deviceID)
	if conn == nil {
		return nil, fmt.Errorf("no active connection for device %s: %w", deviceID, channels.ErrSendFailed)
	}

	// If we have a TTS provider, synthesize and stream audio to the device.
	if ch.ttsProvider != nil && msg.Content != "" {
		go ch.streamTTSToDevice(conn, msg.Content)
		return nil, nil
	}

	// Fallback: send as text-only LLM message.
	err := conn.writeJSON(xiaozhiMsg{
		Type:      msgTypeLLM,
		SessionID: conn.sessionID,
		Text:      msg.Content,
	})
	return nil, err
}

// handleUpgrade performs WebSocket upgrade and starts the connection lifecycle.
func (ch *XiaozhiChannel) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	if !ch.IsRunning() {
		http.Error(w, "channel not running", http.StatusServiceUnavailable)
		return
	}

	// Authenticate via Authorization header or query param.
	if !ch.authenticate(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Check connection limit.
	maxConns := ch.cfg.MaxConnections
	if maxConns <= 0 {
		maxConns = 100
	}
	if ch.currentConnCount() >= maxConns {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}

	conn, err := ch.upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.ErrorCF("xiaozhi", "WebSocket upgrade failed", map[string]any{"error": err.Error()})
		return
	}

	// Extract device ID from header (used for session persistence).
	deviceID := r.Header.Get("Device-Id")
	if deviceID == "" {
		deviceID = r.Header.Get("Client-Id")
	}
	if deviceID == "" {
		deviceID = uuid.New().String()
	}

	connCtx, connCancel := context.WithCancel(ch.ctx)
	xc := &xiaozhiConn{
		id:       uuid.New().String(),
		conn:     conn,
		deviceID: deviceID,
		cancel:   connCancel,
	}
	xc.state.Store(connStateIdle)

	// Disconnect any existing connection from the same device (one-connection-per-device).
	if old := ch.findConnectionByDevice(deviceID); old != nil {
		logger.InfoCF("xiaozhi", "Replacing existing connection for device", map[string]any{
			"device_id":   deviceID,
			"old_conn_id": old.id,
		})
		old.close()
		ch.removeConnection(old.id)
	}

	ch.addConnection(xc)

	logger.InfoCF("xiaozhi", "Device connected", map[string]any{
		"conn_id":   xc.id,
		"device_id": deviceID,
	})

	go ch.connectionLoop(connCtx, xc)
}

// connectionLoop handles the hello handshake then enters readLoop.
func (ch *XiaozhiChannel) connectionLoop(ctx context.Context, xc *xiaozhiConn) {
	defer func() {
		xc.close()
		ch.removeConnection(xc.id)
		logger.InfoCF("xiaozhi", "Device disconnected", map[string]any{
			"conn_id":   xc.id,
			"device_id": xc.deviceID,
		})
	}()

	// Wait for client hello with timeout.
	_ = xc.conn.SetReadDeadline(time.Now().Add(helloTimeoutSec * time.Second))
	_, rawMsg, err := xc.conn.ReadMessage()
	if err != nil {
		logger.ErrorCF("xiaozhi", "Hello timeout or read error", map[string]any{"error": err.Error()})
		return
	}

	var hello xiaozhiMsg
	if err := json.Unmarshal(rawMsg, &hello); err != nil || hello.Type != msgTypeHello {
		logger.ErrorCF("xiaozhi", "Invalid hello message", map[string]any{"error": "expected hello"})
		return
	}

	// Assign session ID based on device ID for persistence.
	xc.sessionID = "xiaozhi:" + xc.deviceID

	// Send server hello.
	sampleRate := defaultSampleRate
	frameDuration := defaultFrameDuration
	if hello.AudioParams != nil && hello.AudioParams.SampleRate > 0 {
		sampleRate = hello.AudioParams.SampleRate
	}

	reply := serverHello(xc.sessionID, sampleRate, frameDuration)
	if err := xc.writeJSON(reply); err != nil {
		logger.ErrorCF("xiaozhi", "Failed to send hello reply", map[string]any{"error": err.Error()})
		return
	}

	logger.InfoCF("xiaozhi", "Hello handshake complete", map[string]any{
		"device_id":  xc.deviceID,
		"session_id": xc.sessionID,
	})

	// Enter main read loop.
	ch.readLoop(ctx, xc)
}

// readLoop reads and dispatches WebSocket messages from a device.
func (ch *XiaozhiChannel) readLoop(ctx context.Context, xc *xiaozhiConn) {
	readTimeout := time.Duration(ch.cfg.ReadTimeout) * time.Second
	if readTimeout <= 0 {
		readTimeout = defaultReadSeconds * time.Second
	}

	_ = xc.conn.SetReadDeadline(time.Now().Add(readTimeout))
	xc.conn.SetPongHandler(func(string) error {
		_ = xc.conn.SetReadDeadline(time.Now().Add(readTimeout))
		return nil
	})

	// Start ping ticker.
	pingInterval := time.Duration(ch.cfg.PingInterval) * time.Second
	if pingInterval <= 0 {
		pingInterval = defaultPingSeconds * time.Second
	}
	go ch.pingLoop(xc, pingInterval)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgType, data, err := xc.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				logger.DebugCF("xiaozhi", "Read error", map[string]any{
					"conn_id": xc.id,
					"error":   err.Error(),
				})
			}
			return
		}

		_ = xc.conn.SetReadDeadline(time.Now().Add(readTimeout))

		switch msgType {
		case websocket.TextMessage:
			ch.handleTextMessage(ctx, xc, data)
		case websocket.BinaryMessage:
			ch.handleBinaryFrame(xc, data)
		}
	}
}

// handleTextMessage dispatches JSON control messages from the device.
func (ch *XiaozhiChannel) handleTextMessage(ctx context.Context, xc *xiaozhiConn, data []byte) {
	var msg xiaozhiMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		logger.DebugCF("xiaozhi", "Invalid JSON", map[string]any{"error": err.Error()})
		return
	}

	switch msg.Type {
	case msgTypeListen:
		ch.handleListen(ctx, xc, msg)
	case msgTypeAbort:
		ch.handleAbort(xc)
	default:
		logger.DebugCF("xiaozhi", "Unknown message type", map[string]any{"type": msg.Type})
	}
}

// handleListen processes listen start/stop/detect messages.
func (ch *XiaozhiChannel) handleListen(ctx context.Context, xc *xiaozhiConn, msg xiaozhiMsg) {
	switch msg.State {
	case stateStart:
		logger.DebugCF("xiaozhi", "Listen start", map[string]any{
			"device_id": xc.deviceID,
			"mode":      msg.Mode,
		})
		xc.state.Store(connStateListening)
		xc.cancelTTS() // Stop any active TTS when device starts listening.
		ch.startVoiceAccumulation(xc)

	case stateStop:
		logger.DebugCF("xiaozhi", "Listen stop", map[string]any{"device_id": xc.deviceID})
		ch.finishListening(ctx, xc)

	case stateDetect:
		// Wake word detected — treat like listen.start with optional wake word text.
		logger.DebugCF("xiaozhi", "Wake word detected", map[string]any{
			"device_id": xc.deviceID,
			"text":      msg.Text,
		})
		xc.state.Store(connStateListening)
		ch.startVoiceAccumulation(xc)
	}
}

// handleAbort cancels active TTS playback.
func (ch *XiaozhiChannel) handleAbort(xc *xiaozhiConn) {
	logger.DebugCF("xiaozhi", "Abort received", map[string]any{"device_id": xc.deviceID})
	xc.cancelTTS()
	xc.state.Store(connStateIdle)
}

// handleBinaryFrame processes incoming Opus audio frames from the device.
// Opus frames are typically 100-600 bytes; reject anything suspiciously large.
func (ch *XiaozhiChannel) handleBinaryFrame(xc *xiaozhiConn, data []byte) {
	const maxFrameSize = 8192 // 8KB — well above any valid Opus frame
	if len(data) == 0 || len(data) > maxFrameSize {
		return
	}
	xc.voiceMu.Lock()
	v := xc.voice
	xc.voiceMu.Unlock()

	if v != nil {
		v.Push(data)
	}
}

// finishListening finalizes voice accumulation, transcribes, and publishes to agent loop.
func (ch *XiaozhiChannel) finishListening(ctx context.Context, xc *xiaozhiConn) {
	xc.voiceMu.Lock()
	v := xc.voice
	xc.voice = nil
	xc.voiceMu.Unlock()

	if v == nil {
		xc.state.Store(connStateIdle)
		return
	}

	// Transcribe in background to not block the read loop.
	go func() {
		defer v.Cleanup()

		text, err := ch.transcribeVoice(ctx, v)
		if err != nil {
			logger.ErrorCF("xiaozhi", "Transcription failed", map[string]any{"error": err.Error()})
			xc.state.Store(connStateIdle)
			return
		}
		if text == "" {
			logger.DebugCF("xiaozhi", "Empty transcription, ignoring", nil)
			xc.state.Store(connStateIdle)
			return
		}

		// Send STT result to device for display.
		xc.writeJSON(xiaozhiMsg{
			Type:      msgTypeSTT,
			SessionID: xc.sessionID,
			Text:      text,
		})

		// Publish to picoclaw agent loop.
		chatID := "xiaozhi:" + xc.deviceID
		senderID := "xiaozhi-device"
		peer := bus.Peer{Kind: "direct", ID: chatID}

		sender := bus.SenderInfo{
			Platform:    "xiaozhi",
			PlatformID:  xc.deviceID,
			CanonicalID: identity.BuildCanonicalID("xiaozhi", xc.deviceID),
		}

		metadata := map[string]string{
			"platform":   "xiaozhi",
			"device_id":  xc.deviceID,
			"session_id": xc.sessionID,
			"is_voice":   "true",
		}

		ch.HandleMessage(ctx, peer, "", senderID, chatID, text, nil, metadata, sender)
	}()
}

// streamTTSToDevice synthesizes speech and streams Opus frames to the device.
func (ch *XiaozhiChannel) streamTTSToDevice(xc *xiaozhiConn, text string) {
	ttsCtx, ttsCancel := context.WithCancel(ch.ctx)

	xc.voiceMu.Lock()
	xc.ttsCancel = ttsCancel
	xc.voiceMu.Unlock()

	defer func() {
		ttsCancel()
		xc.state.Store(connStateIdle)
	}()

	xc.state.Store(connStateSpeaking)

	// Send sentence_start so device can display the response text.
	xc.writeJSON(xiaozhiMsg{
		Type:      msgTypeTTS,
		SessionID: xc.sessionID,
		State:     stateSentenceStart,
		Text:      text,
	})

	// Send tts.start.
	xc.writeJSON(xiaozhiMsg{
		Type:      msgTypeTTS,
		SessionID: xc.sessionID,
		State:     stateStart,
	})

	// Synthesize and stream.
	err := synthesizeAndStream(ttsCtx, ch.ttsProvider, xc, text)
	if err != nil {
		logger.ErrorCF("xiaozhi", "TTS streaming failed", map[string]any{
			"error":     err.Error(),
			"device_id": xc.deviceID,
		})
	}

	// Send tts.stop.
	xc.writeJSON(xiaozhiMsg{
		Type:      msgTypeTTS,
		SessionID: xc.sessionID,
		State:     stateStop,
	})
}

// pingLoop sends periodic ping frames to keep the connection alive.
func (ch *XiaozhiChannel) pingLoop(xc *xiaozhiConn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ch.ctx.Done():
			return
		case <-ticker.C:
			if xc.closed.Load() {
				return
			}
			xc.writeMu.Lock()
			err := xc.conn.WriteMessage(websocket.PingMessage, nil)
			xc.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// authenticate checks the request for a valid token via Authorization header.
func (ch *XiaozhiChannel) authenticate(r *http.Request) bool {
	token := ch.cfg.Token.String()
	if token == "" {
		// If no token configured, allow all connections.
		return true
	}

	auth := r.Header.Get("Authorization")
	if auth == token {
		return true
	}
	if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
		if after == token {
			return true
		}
	}

	// Query param fallback.
	if r.URL.Query().Get("token") == token {
		return true
	}

	return false
}

// Connection management helpers.

func (ch *XiaozhiChannel) addConnection(xc *xiaozhiConn) {
	ch.connsMu.Lock()
	defer ch.connsMu.Unlock()
	ch.connections[xc.id] = xc
}

func (ch *XiaozhiChannel) removeConnection(connID string) {
	ch.connsMu.Lock()
	defer ch.connsMu.Unlock()
	delete(ch.connections, connID)
}

func (ch *XiaozhiChannel) currentConnCount() int {
	ch.connsMu.RLock()
	defer ch.connsMu.RUnlock()
	return len(ch.connections)
}

func (ch *XiaozhiChannel) findConnectionByDevice(deviceID string) *xiaozhiConn {
	ch.connsMu.RLock()
	defer ch.connsMu.RUnlock()
	for _, c := range ch.connections {
		if c.deviceID == deviceID {
			return c
		}
	}
	return nil
}
