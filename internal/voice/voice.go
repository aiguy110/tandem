// Package voice renders transcript messages through Tandem's shared language
// model and a text-to-speech endpoint. All provider traffic originates in the daemon.
package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/languagemodel"
)

const (
	maxAudioResponse         = 32 << 20
	maxCombinedAudioResponse = 32 << 20
	maxSpeechChunkCharacters = 3500
	speechChunkDelimiter     = "[[TANDEM_SPEECH_CHUNK]]"
)

type Audio struct {
	Data     []byte
	MIMEType string
}

type Renderer interface {
	Render(context.Context, string) (Audio, error)
}

type Service struct {
	config    config.VoiceConfig
	completer languagemodel.Completer
	client    *http.Client
}

func New(cfg config.VoiceConfig, completer languagemodel.Completer) (*Service, error) {
	if !cfg.Enabled {
		return nil, errors.New("voice rendering is not configured; run tandem setup")
	}
	if completer == nil {
		return nil, errors.New("voice rendering requires a configured language model; run tandem setup")
	}
	for name, value := range map[string]string{
		"TTS endpoint": cfg.TTSEndpoint, "TTS model": cfg.TTSModel, "TTS voice": cfg.TTSVoice,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("voice %s is required", name)
		}
	}
	if !validEndpoint(cfg.TTSEndpoint) {
		return nil, fmt.Errorf("voice endpoint must be an absolute HTTP(S) URL: %q", cfg.TTSEndpoint)
	}
	if cfg.Instructions == "" {
		cfg.Instructions = config.DefaultVoicePreparationInstructions
	}
	if cfg.TTSFormat == "" {
		cfg.TTSFormat = "mp3"
	}
	return &Service{config: cfg, completer: completer, client: &http.Client{Timeout: 2 * time.Minute}}, nil
}

func (s *Service) Render(ctx context.Context, source string) (Audio, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return Audio{}, errors.New("message has no text to render")
	}
	spoken, err := s.completer.Complete(ctx, speechPreparationInstructions(s.config.Instructions), source)
	if err != nil {
		return Audio{}, fmt.Errorf("prepare message for speech: %w", err)
	}
	chunks, err := splitSpeechChunks(spoken)
	if err != nil {
		return Audio{}, err
	}
	return s.speakChunks(ctx, chunks)
}

// speechPreparationInstructions makes the language model's output safe for
// speech providers with relatively small input limits. The delimiter is kept
// out of the text sent to the provider below.
func speechPreparationInstructions(instructions string) string {
	return instructions + "\n\nWhen the response needs more than one speech request, split it at natural paragraph or sentence boundaries. " +
		"Put the literal delimiter " + speechChunkDelimiter + " on its own line between parts. " +
		fmt.Sprintf("Keep every part at most %d characters. Do not put the delimiter anywhere else.", maxSpeechChunkCharacters)
}

func splitSpeechChunks(spoken string) ([]string, error) {
	var chunks []string
	for _, part := range strings.Split(spoken, speechChunkDelimiter) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		chunks = append(chunks, splitOversizedChunk(part)...)
	}
	if len(chunks) == 0 {
		return nil, errors.New("speech preparation returned no text")
	}
	return chunks, nil
}

// splitOversizedChunk is a guard for language models that miss the delimiter
// instruction. Prefer a sentence or whitespace break, keeping an individual
// TTS request safely below the provider's 4,096-character limit.
func splitOversizedChunk(text string) []string {
	runes := []rune(strings.TrimSpace(text))
	var chunks []string
	for len(runes) > maxSpeechChunkCharacters {
		cut := maxSpeechChunkCharacters
		for i := maxSpeechChunkCharacters - 1; i >= maxSpeechChunkCharacters/2; i-- {
			if strings.ContainsRune(".!?", runes[i]) || unicode.IsSpace(runes[i]) {
				cut = i + 1
				break
			}
		}
		chunks = append(chunks, strings.TrimSpace(string(runes[:cut])))
		runes = []rune(strings.TrimLeftFunc(string(runes[cut:]), unicode.IsSpace))
	}
	if text := strings.TrimSpace(string(runes)); text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}

func (s *Service) speakChunks(ctx context.Context, chunks []string) (Audio, error) {
	var combined []byte
	mimeType := ""
	for index, chunk := range chunks {
		audio, err := s.speak(ctx, chunk)
		if err != nil {
			return Audio{}, fmt.Errorf("render speech chunk %d of %d: %w", index+1, len(chunks), err)
		}
		if mimeType == "" {
			mimeType = audio.MIMEType
		} else if audio.MIMEType != mimeType {
			return Audio{}, fmt.Errorf("speech chunks returned inconsistent content types: %q and %q", mimeType, audio.MIMEType)
		}
		if len(combined)+len(audio.Data) > maxCombinedAudioResponse {
			return Audio{}, errors.New("combined speech response exceeds 32 MiB")
		}
		combined = append(combined, audio.Data...)
	}
	return Audio{Data: combined, MIMEType: mimeType}, nil
}

func (s *Service) speak(ctx context.Context, input string) (Audio, error) {
	body := map[string]any{
		"model": s.config.TTSModel, "input": input, "voice": s.config.TTSVoice,
		"response_format": s.config.TTSFormat,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return Audio{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.TTSEndpoint, bytes.NewReader(encoded))
	if err != nil {
		return Audio{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.config.TTSAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.config.TTSAPIKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return Audio{}, fmt.Errorf("request speech: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAudioResponse+1))
	if err != nil {
		return Audio{}, fmt.Errorf("read speech: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Audio{}, speechProviderError(resp.StatusCode, data)
	}
	if len(data) == 0 {
		return Audio{}, errors.New("speech provider returned empty audio")
	}
	if len(data) > maxAudioResponse {
		return Audio{}, errors.New("speech provider response exceeds 32 MiB")
	}
	contentType := strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = formatMIME(s.config.TTSFormat)
	}
	if !strings.HasPrefix(contentType, "audio/") {
		return Audio{}, fmt.Errorf("speech provider returned unexpected content type %q", contentType)
	}
	return Audio{Data: data, MIMEType: contentType}, nil
}

func validEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func speechProviderError(status int, body []byte) error {
	message := strings.TrimSpace(string(body))
	var problem struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &problem) == nil {
		switch value := problem.Error.(type) {
		case string:
			message = value
		case map[string]any:
			if text, ok := value["message"].(string); ok {
				message = text
			}
		}
		if message == "" {
			message = problem.Message
		}
	}
	if len(message) > 500 {
		message = message[:500] + "…"
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return fmt.Errorf("speech request failed (%d): %s", status, message)
}

func formatMIME(format string) string {
	switch strings.ToLower(format) {
	case "wav":
		return "audio/wav"
	case "opus":
		return "audio/opus"
	case "flac":
		return "audio/flac"
	case "aac":
		return "audio/aac"
	case "pcm":
		return "audio/L16"
	default:
		if value := mime.TypeByExtension("." + format); strings.HasPrefix(value, "audio/") {
			return value
		}
		return "audio/mpeg"
	}
}
