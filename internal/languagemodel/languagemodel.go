// Package languagemodel provides Tandem's shared OpenAI-compatible Chat
// Completions client. Features such as voice preparation, summaries, and
// automatic titles use this boundary instead of owning provider credentials.
package languagemodel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aiguy110/tandem/internal/config"
)

const maxResponseBytes = 2 << 20

type Completer interface {
	Complete(ctx context.Context, instructions, input string) (string, error)
}

type Client struct {
	config config.LanguageModelConfig
	client *http.Client
}

func New(cfg config.LanguageModelConfig) (*Client, error) {
	for name, value := range map[string]string{"endpoint": cfg.Endpoint, "model": cfg.Model} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("language model %s is required", name)
		}
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("language model endpoint must be an absolute HTTP(S) URL: %q", cfg.Endpoint)
	}
	return &Client{config: cfg, client: &http.Client{Timeout: 2 * time.Minute}}, nil
}

func (c *Client) Complete(ctx context.Context, instructions, input string) (string, error) {
	body := map[string]any{
		"model": c.config.Model,
		"messages": []map[string]string{
			{"role": "system", "content": instructions},
			{"role": "user", "content": input},
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.Endpoint, bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request language model: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("read language model response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", providerError(resp.StatusCode, data)
	}
	if len(data) > maxResponseBytes {
		return "", errors.New("language model response is too large")
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return "", fmt.Errorf("decode language model response: %w", err)
	}
	if len(response.Choices) == 0 || strings.TrimSpace(response.Choices[0].Message.Content) == "" {
		return "", errors.New("language model returned no text")
	}
	return strings.TrimSpace(response.Choices[0].Message.Content), nil
}

func providerError(status int, body []byte) error {
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
	return fmt.Errorf("language model request failed (%d): %s", status, message)
}
