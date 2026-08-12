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

	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/languagemodel"
)

const (
	maxAudioResponse = 32 << 20
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
	spoken, err := s.completer.Complete(ctx, s.config.Instructions, source)
	if err != nil {
		return Audio{}, fmt.Errorf("prepare message for speech: %w", err)
	}
	return s.speak(ctx, spoken)
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
