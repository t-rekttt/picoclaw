package xiaozhi

// Xiaozhi WebSocket protocol message types.
// Protocol reference: xiaozhi-esp32 docs/websocket.md
const (
	// JSON message types.
	msgTypeHello  = "hello"
	msgTypeListen = "listen"
	msgTypeSTT    = "stt"
	msgTypeTTS    = "tts"
	msgTypeLLM    = "llm"
	msgTypeAbort  = "abort"

	// Listen states.
	stateStart  = "start"
	stateStop   = "stop"
	stateDetect = "detect"

	// TTS states.
	stateSentenceStart = "sentence_start"

	// Listening modes.
	modeAuto     = "auto"
	modeManual   = "manual"
	modeRealtime = "realtime"

	// Connection states.
	connStateIdle      = "idle"
	connStateListening = "listening"
	connStateSpeaking  = "speaking"

	// Default audio parameters.
	defaultSampleRate    = 16000
	defaultChannels      = 1
	defaultFrameDuration = 60 // milliseconds
	defaultFormat        = "opus"

	// Timeouts.
	helloTimeoutSec    = 10
	silenceTimeoutMs   = 1500
	sessionTimeoutSec  = 120
	defaultPingSeconds = 30
	defaultReadSeconds = 60
)

// xiaozhiMsg is the wire format for all xiaozhi protocol JSON messages.
type xiaozhiMsg struct {
	Type        string          `json:"type"`
	SessionID   string          `json:"session_id,omitempty"`
	State       string          `json:"state,omitempty"`
	Mode        string          `json:"mode,omitempty"`
	Text        string          `json:"text,omitempty"`
	Version     int             `json:"version,omitempty"`
	Transport   string          `json:"transport,omitempty"`
	AudioParams *audioParams    `json:"audio_params,omitempty"`
	Features    map[string]bool `json:"features,omitempty"`
	Emotion     string          `json:"emotion,omitempty"`
	Reason      string          `json:"reason,omitempty"`
}

// audioParams describes the audio encoding parameters exchanged in the hello handshake.
type audioParams struct {
	Format        string `json:"format"`
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	FrameDuration int    `json:"frame_duration"`
}

// serverHello builds the server's hello response message.
func serverHello(sessionID string, sampleRate, frameDuration int) xiaozhiMsg {
	return xiaozhiMsg{
		Type:      msgTypeHello,
		SessionID: sessionID,
		Transport: "websocket",
		AudioParams: &audioParams{
			Format:        defaultFormat,
			SampleRate:    sampleRate,
			Channels:      defaultChannels,
			FrameDuration: frameDuration,
		},
	}
}
