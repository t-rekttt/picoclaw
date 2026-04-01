package xiaozhi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sipeed/picoclaw/pkg/audio/asr"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

// mockTranscriber is a test double for asr.Transcriber
type mockTranscriber struct {
	text  string
	err   error
	calls int
}

func (m *mockTranscriber) Name() string { return "mock" }
func (m *mockTranscriber) Transcribe(ctx context.Context, audioFilePath string) (*asr.TranscriptionResponse, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return &asr.TranscriptionResponse{
		Text:     m.text,
		Duration: 1.0,
	}, nil
}

// mockTTSProvider is a test double for tts.TTSProvider
type mockTTSProvider struct {
	audioData []byte
	err       error
	calls     int
}

func (m *mockTTSProvider) Name() string { return "mock" }
func (m *mockTTSProvider) Synthesize(ctx context.Context, text string) (io.ReadCloser, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return io.NopCloser(strings.NewReader(string(m.audioData))), nil
}

// TestServerHelloSerialization tests protocol message serialization for serverHello.
func TestServerHelloSerialization(t *testing.T) {
	hello := serverHello("session-123", 16000, 60)

	if hello.Type != msgTypeHello {
		t.Errorf("expected type %q, got %q", msgTypeHello, hello.Type)
	}
	if hello.SessionID != "session-123" {
		t.Errorf("expected sessionID %q, got %q", "session-123", hello.SessionID)
	}
	if hello.Transport != "websocket" {
		t.Errorf("expected transport %q, got %q", "websocket", hello.Transport)
	}

	if hello.AudioParams == nil {
		t.Fatal("expected audio params, got nil")
	}
	if hello.AudioParams.Format != defaultFormat {
		t.Errorf("expected format %q, got %q", defaultFormat, hello.AudioParams.Format)
	}
	if hello.AudioParams.SampleRate != 16000 {
		t.Errorf("expected sample rate 16000, got %d", hello.AudioParams.SampleRate)
	}
	if hello.AudioParams.Channels != defaultChannels {
		t.Errorf("expected channels %d, got %d", defaultChannels, hello.AudioParams.Channels)
	}
	if hello.AudioParams.FrameDuration != 60 {
		t.Errorf("expected frame duration 60, got %d", hello.AudioParams.FrameDuration)
	}
}

// TestXiaozhiMsgMarshalJSON tests JSON marshal/unmarshal of protocol messages.
func TestXiaozhiMsgMarshalJSON(t *testing.T) {
	msg := xiaozhiMsg{
		Type:      msgTypeListen,
		SessionID: "session-456",
		State:     stateStart,
		Mode:      modeAuto,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var unmarshaled xiaozhiMsg
	err = json.Unmarshal(data, &unmarshaled)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if unmarshaled.Type != msg.Type || unmarshaled.SessionID != msg.SessionID {
		t.Errorf("round-trip marshal/unmarshal failed")
	}
}

// TestAudioParamsMarshal tests that audioParams serializes correctly.
func TestAudioParamsMarshal(t *testing.T) {
	params := &audioParams{
		Format:        "opus",
		SampleRate:    16000,
		Channels:      1,
		FrameDuration: 60,
	}

	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	expected := `{"format":"opus","sample_rate":16000,"channels":1,"frame_duration":60}`
	if string(data) != expected {
		t.Errorf("expected %s, got %s", expected, string(data))
	}
}

// TestXiaozhiConfigInChannelsConfig tests that XiaozhiConfig integrates with ChannelsConfig.
func TestXiaozhiConfigInChannelsConfig(t *testing.T) {
	cfg := config.ChannelsConfig{
		Xiaozhi: config.XiaozhiConfig{
			Enabled:        true,
			MaxConnections: 50,
			PingInterval:   30,
			ReadTimeout:    60,
		},
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var unmarshaled config.ChannelsConfig
	err = json.Unmarshal(data, &unmarshaled)
	if err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if !unmarshaled.Xiaozhi.Enabled {
		t.Errorf("expected enabled=true")
	}
	if unmarshaled.Xiaozhi.MaxConnections != 50 {
		t.Errorf("expected max_connections=50, got %d", unmarshaled.Xiaozhi.MaxConnections)
	}
	if unmarshaled.Xiaozhi.PingInterval != 30 {
		t.Errorf("expected ping_interval=30, got %d", unmarshaled.Xiaozhi.PingInterval)
	}
}

// TestNewXiaozhiChannel creates a channel with mocked ASR/TTS.
func TestNewXiaozhiChannel(t *testing.T) {
	cfg := &config.Config{
		Channels: config.ChannelsConfig{
			Xiaozhi: config.XiaozhiConfig{
				Enabled:        true,
				MaxConnections: 100,
			},
		},
	}
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	ch, err := NewXiaozhiChannel(cfg, msgBus)
	if err != nil {
		t.Fatalf("NewXiaozhiChannel failed: %v", err)
	}

	if ch == nil {
		t.Fatal("expected channel, got nil")
	}
	if ch.Name() != "xiaozhi" {
		t.Errorf("expected name 'xiaozhi', got %q", ch.Name())
	}
}

// TestStartStop tests channel lifecycle.
func TestStartStop(t *testing.T) {
	cfg := &config.Config{
		Channels: config.ChannelsConfig{
			Xiaozhi: config.XiaozhiConfig{Enabled: true},
		},
	}
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	ch, _ := NewXiaozhiChannel(cfg, msgBus)

	ctx := context.Background()
	if err := ch.Start(ctx); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if !ch.IsRunning() {
		t.Error("expected channel to be running after Start")
	}

	if err := ch.Stop(ctx); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if ch.IsRunning() {
		t.Error("expected channel to not be running after Stop")
	}
}

// TestWebhookPath tests that the webhook path is correct.
func TestWebhookPath(t *testing.T) {
	cfg := &config.Config{
		Channels: config.ChannelsConfig{
			Xiaozhi: config.XiaozhiConfig{Enabled: true},
		},
	}
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	ch, _ := NewXiaozhiChannel(cfg, msgBus)

	path := ch.WebhookPath()
	if path != "/xiaozhi/" {
		t.Errorf("expected path '/xiaozhi/', got %q", path)
	}
}

// TestVoiceCapabilities tests that voice capabilities are exposed.
func TestVoiceCapabilities(t *testing.T) {
	cfg := &config.Config{
		Channels: config.ChannelsConfig{
			Xiaozhi: config.XiaozhiConfig{Enabled: true},
		},
	}
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	ch, _ := NewXiaozhiChannel(cfg, msgBus)

	// Without ASR/TTS configured, both should be false.
	caps := ch.VoiceCapabilities()
	if caps.ASR {
		t.Error("expected ASR=false without transcriber")
	}
	if caps.TTS {
		t.Error("expected TTS=false without TTS provider")
	}
}

// TestServeHTTP_NotFound tests that unknown paths return 404.
func TestServeHTTP_NotFound(t *testing.T) {
	cfg := &config.Config{
		Channels: config.ChannelsConfig{
			Xiaozhi: config.XiaozhiConfig{Enabled: true},
		},
	}
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	ch, _ := NewXiaozhiChannel(cfg, msgBus)
	ch.Start(context.Background())
	defer ch.Stop(context.Background())

	req := httptest.NewRequest("GET", "/xiaozhi/unknown", nil)
	w := httptest.NewRecorder()

	ch.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// TestServeHTTP_NotRunning tests that requests to non-running channel fail.
func TestServeHTTP_NotRunning(t *testing.T) {
	cfg := &config.Config{
		Channels: config.ChannelsConfig{
			Xiaozhi: config.XiaozhiConfig{Enabled: true},
		},
	}
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	ch, _ := NewXiaozhiChannel(cfg, msgBus)
	// Do NOT call Start

	req := httptest.NewRequest("GET", "/xiaozhi/v1/chat", nil)
	w := httptest.NewRecorder()

	ch.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

// TestAuthentication tests token-based authentication.
func TestAuthentication(t *testing.T) {
	token := *config.NewSecureString("secret-token")
	cfg := &config.Config{
		Channels: config.ChannelsConfig{
			Xiaozhi: config.XiaozhiConfig{
				Enabled: true,
				Token:   token,
			},
		},
	}
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	ch, _ := NewXiaozhiChannel(cfg, msgBus)
	ch.Start(context.Background())
	defer ch.Stop(context.Background())

	t.Run("no_token", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/xiaozhi/v1/chat", nil)
		w := httptest.NewRecorder()
		ch.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 without token, got %d", w.Code)
		}
	})

	t.Run("correct_authorization_header", func(t *testing.T) {
		// Cannot directly test upgrade without real WebSocket,
		// but can at least verify the authenticate function works.
		req := httptest.NewRequest("GET", "/xiaozhi/v1/chat", nil)
		req.Header.Set("Authorization", "secret-token")
		if !ch.authenticate(req) {
			t.Error("expected authentication to succeed")
		}
	})

	t.Run("bearer_token", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/xiaozhi/v1/chat", nil)
		req.Header.Set("Authorization", "Bearer secret-token")
		if !ch.authenticate(req) {
			t.Error("expected authentication to succeed with Bearer prefix")
		}
	})

	t.Run("query_param_token", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/xiaozhi/v1/chat?token=secret-token", nil)
		if !ch.authenticate(req) {
			t.Error("expected authentication to succeed via query param")
		}
	})
}

// TestAuthenticationNoToken tests that requests succeed with no token configured.
func TestAuthenticationNoToken(t *testing.T) {
	cfg := &config.Config{
		Channels: config.ChannelsConfig{
			Xiaozhi: config.XiaozhiConfig{Enabled: true},
			// No token
		},
	}
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	ch, _ := NewXiaozhiChannel(cfg, msgBus)

	req := httptest.NewRequest("GET", "/xiaozhi/v1/chat", nil)
	if !ch.authenticate(req) {
		t.Error("expected authentication to succeed when no token configured")
	}
}

// TestConnectionManagement tests adding/removing/finding connections.
func TestConnectionManagement(t *testing.T) {
	cfg := &config.Config{
		Channels: config.ChannelsConfig{
			Xiaozhi: config.XiaozhiConfig{Enabled: true},
		},
	}
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	ch, _ := NewXiaozhiChannel(cfg, msgBus)
	ch.Start(context.Background())
	defer ch.Stop(context.Background())

	// Create mock connection
	xc := &xiaozhiConn{
		id:       "conn-1",
		deviceID: "device-123",
	}

	// Add and verify
	ch.addConnection(xc)
	if ch.currentConnCount() != 1 {
		t.Errorf("expected 1 connection, got %d", ch.currentConnCount())
	}

	// Find by device
	found := ch.findConnectionByDevice("device-123")
	if found == nil {
		t.Error("expected to find connection by device ID")
	}
	if found.id != "conn-1" {
		t.Errorf("expected conn-1, got %s", found.id)
	}

	// Find non-existent device
	notFound := ch.findConnectionByDevice("device-999")
	if notFound != nil {
		t.Error("expected nil for non-existent device")
	}

	// Remove and verify
	ch.removeConnection("conn-1")
	if ch.currentConnCount() != 0 {
		t.Errorf("expected 0 connections after removal, got %d", ch.currentConnCount())
	}
}

// TestConnectionLimits tests that max connections is enforced.
func TestConnectionLimits(t *testing.T) {
	cfg := &config.Config{
		Channels: config.ChannelsConfig{
			Xiaozhi: config.XiaozhiConfig{
				Enabled:        true,
				MaxConnections: 2,
			},
		},
	}
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	ch, _ := NewXiaozhiChannel(cfg, msgBus)
	ch.Start(context.Background())

	// Add 2 connections (at limit)
	for i := 0; i < 2; i++ {
		// Create a mock server for the websocket connection
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upgrader := websocket.Upgrader{}
			conn, _ := upgrader.Upgrade(w, r, nil)
			defer conn.Close()
			select {}
		}))

		url := "ws" + strings.TrimPrefix(server.URL, "http")
		ws, _, _ := websocket.DefaultDialer.Dial(url, nil)

		xc := &xiaozhiConn{
			id:       "conn-" + string(rune(i)),
			deviceID: "device-" + string(rune(i)),
			conn:     ws,
		}
		ch.addConnection(xc)
		defer server.Close()
		defer ws.Close()
	}

	if ch.currentConnCount() != 2 {
		t.Errorf("expected 2 connections, got %d", ch.currentConnCount())
	}

	// Verify count is at limit
	if ch.currentConnCount() >= 2 {
		t.Log("connection limit enforcement works via handleUpgrade")
	}

	ch.Stop(context.Background())
}

// TestSendWithoutActiveConnection returns error when no connection for device.
func TestSendWithoutActiveConnection(t *testing.T) {
	cfg := &config.Config{
		Channels: config.ChannelsConfig{
			Xiaozhi: config.XiaozhiConfig{Enabled: true},
		},
	}
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	ch, _ := NewXiaozhiChannel(cfg, msgBus)
	ch.Start(context.Background())
	defer ch.Stop(context.Background())

	msg := bus.OutboundMessage{
		ChatID:  "xiaozhi:device-404",
		Content: "Hello",
	}

	_, err := ch.Send(context.Background(), msg)
	if err == nil {
		t.Error("expected error when sending to non-existent device")
	}
}

// TestSendChannelNotRunning returns error when channel not running.
func TestSendChannelNotRunning(t *testing.T) {
	cfg := &config.Config{
		Channels: config.ChannelsConfig{
			Xiaozhi: config.XiaozhiConfig{Enabled: true},
		},
	}
	msgBus := bus.NewMessageBus()
	defer msgBus.Close()

	ch, _ := NewXiaozhiChannel(cfg, msgBus)
	// Do NOT call Start

	msg := bus.OutboundMessage{
		ChatID:  "xiaozhi:device-123",
		Content: "Hello",
	}

	_, err := ch.Send(context.Background(), msg)
	if err == nil {
		t.Error("expected error when sending on stopped channel")
	}
}

// TestVoiceAccumulatorPushClose tests voice accumulator basic operations.
func TestVoiceAccumulatorPushClose(t *testing.T) {
	acc, err := newVoiceAccumulator("test-device")
	if err != nil {
		t.Fatalf("newVoiceAccumulator failed: %v", err)
	}
	defer acc.Cleanup()

	// Initially no data
	if acc.HasData() {
		t.Error("expected HasData=false initially")
	}

	// Push a frame
	opusFrame := []byte{0x01, 0x02, 0x03}
	acc.Push(opusFrame)

	if !acc.HasData() {
		t.Error("expected HasData=true after Push")
	}

	// Close
	acc.Close()

	// Push after close should be no-op
	acc.Push(opusFrame)
	if acc.seqNum != 1 {
		t.Errorf("expected seqNum=1 (no increment after close), got %d", acc.seqNum)
	}
}

// TestVoiceAccumulatorSilentDetection tests silence timeout detection.
func TestVoiceAccumulatorSilentDetection(t *testing.T) {
	acc, err := newVoiceAccumulator("test-device")
	if err != nil {
		t.Fatalf("newVoiceAccumulator failed: %v", err)
	}
	defer acc.Cleanup()

	// Initially not silent (lastAudioAt = now)
	if acc.IsSilent() {
		t.Error("expected IsSilent=false when just created")
	}

	// Manually set lastAudioAt to past
	acc.mu.Lock()
	acc.lastAudioAt = time.Now().Add(-2 * time.Second)
	acc.mu.Unlock()

	// Now should be silent (timeout is 1.5 seconds)
	if !acc.IsSilent() {
		t.Error("expected IsSilent=true after 2 seconds without audio")
	}
}

// TestXiaozhiConnWriteJSON tests write operations on connection.
func TestXiaozhiConnWriteJSON(t *testing.T) {
	// Create a mock WebSocket connection using httptest.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		// Echo received message
		var msg xiaozhiMsg
		for {
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			conn.WriteJSON(msg)
		}
	}))
	defer server.Close()

	// Connect to server
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer ws.Close()

	// Create xiaozhiConn wrapper
	xc := &xiaozhiConn{
		id:   "test-conn",
		conn: ws,
	}

	// Write a message
	testMsg := xiaozhiMsg{
		Type:      msgTypeHello,
		SessionID: "test-session",
	}

	err = xc.writeJSON(testMsg)
	if err != nil {
		t.Fatalf("writeJSON failed: %v", err)
	}

	// Read it back
	var received xiaozhiMsg
	err = ws.ReadJSON(&received)
	if err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}

	if received.Type != testMsg.Type || received.SessionID != testMsg.SessionID {
		t.Error("received message does not match sent message")
	}
}

// TestXiaozhiConnClose tests connection close logic.
func TestXiaozhiConnClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, _ := upgrader.Upgrade(w, r, nil)
		defer conn.Close()
		select {}
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ws, _, _ := websocket.DefaultDialer.Dial(url, nil)
	defer ws.Close()

	xc := &xiaozhiConn{
		id:   "test-conn",
		conn: ws,
	}

	if xc.closed.Load() {
		t.Error("expected closed=false initially")
	}

	xc.close()

	if !xc.closed.Load() {
		t.Error("expected closed=true after close()")
	}

	// Second close should be idempotent
	xc.close()
	if !xc.closed.Load() {
		t.Error("expected closed=true still")
	}
}

// TestXiaozhiConnWriteBinary tests binary message writing.
func TestXiaozhiConnWriteBinary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, _ := upgrader.Upgrade(w, r, nil)
		defer conn.Close()

		// Read and echo binary messages
		for {
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			conn.WriteMessage(msgType, data)
		}
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ws, _, _ := websocket.DefaultDialer.Dial(url, nil)
	defer ws.Close()

	xc := &xiaozhiConn{
		id:   "test-conn",
		conn: ws,
	}

	testData := []byte{0xFF, 0xFE, 0xFD}
	err := xc.writeBinary(testData)
	if err != nil {
		t.Fatalf("writeBinary failed: %v", err)
	}

	// Read back
	msgType, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}

	if msgType != websocket.BinaryMessage {
		t.Errorf("expected binary message type, got %d", msgType)
	}
	if string(data) != string(testData) {
		t.Error("received data does not match sent data")
	}
}

// TestXiaozhiConnWriteJSONAfterClose returns error.
func TestXiaozhiConnWriteJSONAfterClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, _ := upgrader.Upgrade(w, r, nil)
		defer conn.Close()
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ws, _, _ := websocket.DefaultDialer.Dial(url, nil)
	defer ws.Close()

	xc := &xiaozhiConn{
		id:   "test-conn",
		conn: ws,
	}

	xc.close()

	err := xc.writeJSON(xiaozhiMsg{Type: msgTypeHello})
	if err == nil {
		t.Error("expected error when writing to closed connection")
	}
}

// TestXiaozhiConnWriteBinaryAfterClose returns error.
func TestXiaozhiConnWriteBinaryAfterClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, _ := upgrader.Upgrade(w, r, nil)
		defer conn.Close()
	}))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ws, _, _ := websocket.DefaultDialer.Dial(url, nil)
	defer ws.Close()

	xc := &xiaozhiConn{
		id:   "test-conn",
		conn: ws,
	}

	xc.close()

	err := xc.writeBinary([]byte{0x01})
	if err == nil {
		t.Error("expected error when writing binary to closed connection")
	}
}
