package xiaozhi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"

	"github.com/sipeed/picoclaw/pkg/audio"
	"github.com/sipeed/picoclaw/pkg/audio/tts"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// voiceAccumulator collects incoming Opus frames into an OGG file for transcription.
type voiceAccumulator struct {
	writer      *oggwriter.OggWriter
	file        string
	seqNum      uint32
	lastAudioAt time.Time
	mu          sync.Mutex
	closed      bool
}

// newVoiceAccumulator creates a new OGG file and accumulator.
func newVoiceAccumulator(deviceID string) (*voiceAccumulator, error) {
	filename := filepath.Join(os.TempDir(), fmt.Sprintf("xiaozhi_%s_%d.ogg", deviceID, time.Now().UnixNano()))
	writer, err := oggwriter.New(filename, defaultSampleRate, defaultChannels)
	if err != nil {
		return nil, fmt.Errorf("create ogg writer: %w", err)
	}
	return &voiceAccumulator{
		writer:      writer,
		file:        filename,
		lastAudioAt: time.Now(),
	}, nil
}

// Push writes an Opus frame into the OGG container.
func (v *voiceAccumulator) Push(opusFrame []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.closed {
		return
	}

	v.lastAudioAt = time.Now()
	v.seqNum++

	// Timestamp increments by samples-per-frame (sample_rate * frame_duration_ms / 1000).
	// Using uint32 arithmetic naturally wraps, matching RTP spec behavior.
	samplesPerFrame := uint32(defaultSampleRate * defaultFrameDuration / 1000)
	pkt := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: uint16(v.seqNum),
			Timestamp:      uint32(v.seqNum) * samplesPerFrame,
			SSRC:           1,
		},
		Payload: opusFrame,
	}

	if err := v.writer.WriteRTP(pkt); err != nil {
		logger.ErrorCF("xiaozhi", "Failed to write RTP to OGG", map[string]any{"error": err})
	}
}

// Close finalizes the OGG file.
func (v *voiceAccumulator) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.closed {
		v.writer.Close()
		v.closed = true
	}
}

// Cleanup removes the temp OGG file.
func (v *voiceAccumulator) Cleanup() {
	v.Close()
	os.Remove(v.file)
}

// IsSilent returns true if no audio was received for longer than the silence threshold.
func (v *voiceAccumulator) IsSilent() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return time.Since(v.lastAudioAt) > silenceTimeoutMs*time.Millisecond
}

// HasData returns true if at least one frame was written.
func (v *voiceAccumulator) HasData() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.seqNum > 0
}

// startVoiceAccumulation creates a new voice accumulator for the connection.
func (ch *XiaozhiChannel) startVoiceAccumulation(xc *xiaozhiConn) {
	// Close any previous accumulator.
	xc.closeVoice()

	v, err := newVoiceAccumulator(xc.deviceID)
	if err != nil {
		logger.ErrorCF("xiaozhi", "Failed to start voice accumulation", map[string]any{"error": err.Error()})
		return
	}

	xc.voiceMu.Lock()
	xc.voice = v
	xc.voiceMu.Unlock()

	// Start silence detection as a safety timeout.
	go ch.silenceWatcher(xc, v)
}

// silenceWatcher monitors for silence and auto-finishes listening.
func (ch *XiaozhiChannel) silenceWatcher(xc *xiaozhiConn, v *voiceAccumulator) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ch.ctx.Done():
			return
		case <-ticker.C:
			if xc.closed.Load() {
				return
			}

			// Check if voice accumulator was replaced or removed.
			xc.voiceMu.Lock()
			current := xc.voice
			xc.voiceMu.Unlock()
			if current != v {
				return // Accumulator was replaced, stop watching.
			}

			if v.HasData() && v.IsSilent() {
				logger.DebugCF("xiaozhi", "Silence detected, finishing listening", map[string]any{
					"device_id": xc.deviceID,
				})
				ch.finishListening(ch.ctx, xc)
				return
			}
		}
	}
}

// transcribeVoice finalizes the OGG file and sends it to the ASR transcriber.
func (ch *XiaozhiChannel) transcribeVoice(ctx context.Context, v *voiceAccumulator) (string, error) {
	if ch.transcriber == nil {
		return "", fmt.Errorf("no ASR transcriber configured")
	}

	v.Close()

	if !v.HasData() {
		return "", nil
	}

	logger.DebugCF("xiaozhi", "Transcribing voice", map[string]any{"file": v.file})

	res, err := ch.transcriber.Transcribe(ctx, v.file)
	if err != nil {
		return "", err
	}

	logger.InfoCF("xiaozhi", "Transcription result", map[string]any{
		"text":     res.Text,
		"duration": res.Duration,
	})

	return res.Text, nil
}

// synthesizeAndStream calls the TTS provider and streams Opus frames to the device connection.
func synthesizeAndStream(ctx context.Context, provider tts.TTSProvider, xc *xiaozhiConn, text string) error {
	reader, err := provider.Synthesize(ctx, text)
	if err != nil {
		return fmt.Errorf("tts synthesize: %w", err)
	}
	defer reader.Close()

	// Extract individual Opus frames from the OGG stream and send as binary WebSocket messages.
	return audio.DecodeOggOpus(reader, func(frame []byte) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if xc.closed.Load() {
			return fmt.Errorf("connection closed")
		}

		return xc.writeBinary(frame)
	})
}
