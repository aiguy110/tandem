// Package voice renders transcript messages through OpenAI-compatible cleanup
// and text-to-speech endpoints. All provider traffic originates in the daemon.
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
)

const (
	maxJSONResponse  = 2 << 20
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
	config config.VoiceConfig
	client *http.Client
}

func New(cfg config.VoiceConfig) (*Service, error) {
	if !cfg.Enabled {
		return nil, errors.New("voice rendering is not configured; run tandem setup")
	}
	for name, value := range map[string]string{
		"cleanup endpoint": cfg.CleanupEndpoint, "cleanup model": cfg.CleanupModel,
		"TTS endpoint": cfg.TTSEndpoint, "TTS model": cfg.TTSModel, "TTS voice": cfg.TTSVoice,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("voice %s is required", name)
		}
	}
	for _, raw := range []string{cfg.CleanupEndpoint, cfg.TTSEndpoint} {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("voice endpoint must be an absolute HTTP(S) URL: %q", raw)
		}
	}
	if cfg.CleanupInstructions == "" {
		cfg.CleanupInstructions = config.DefaultVoiceCleanupInstructions
	}
	if cfg.TTSFormat == "" {
		cfg.TTSFormat = "mp3"
	}
	return &Service{config: cfg, client: &http.Client{Timeout: 2 * time.Minute}}, nil
}

func (s *Service) Render(ctx context.Context, source string) (Audio, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return Audio{}, errors.New("message has no text to render")
	}
	spoken, err := s.cleanup(ctx, source)
	if err != nil {
		return Audio{}, err
	}
	return s.speak(ctx, spoken)
}

func (s *Service) cleanup(ctx context.Context, source string) (string, error) {
	body := map[string]any{
		"model": s.config.CleanupModel,
		"messages": []map[string]string{
			{"role": "system", "content": s.config.CleanupInstructions},
			{"role": "user", "content": source},
		},
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := s.postJSON(ctx, s.config.CleanupEndpoint, s.config.CleanupAPIKey, body, maxJSONResponse, &response); err != nil {
		return "", fmt.Errorf("clean up message for speech: %w", err)
	}
	if len(response.Choices) == 0 || strings.TrimSpace(response.Choices[0].Message.Content) == "" {
		return "", errors.New("clean up message for speech: provider returned no text")
	}
	return strings.TrimSpace(response.Choices[0].Message.Content), nil
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
		return Audio{}, providerError("speech", resp.StatusCode, data)
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

func (s *Service) postJSON(ctx context.Context, endpoint, key string, body any, limit int64, target any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return providerError("provider", resp.StatusCode, data)
	}
	if int64(len(data)) > limit {
		return errors.New("provider response is too large")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode provider response: %w", err)
	}
	return nil
}

func providerError(stage string, status int, body []byte) error {
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
	return fmt.Errorf("%s request failed (%d): %s", stage, status, message)
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
