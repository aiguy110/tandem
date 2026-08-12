package voice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aiguy110/tandem/internal/config"
	"github.com/aiguy110/tandem/internal/languagemodel"
)

func TestRenderUsesSharedLanguageModelAndSpeechRequests(t *testing.T) {
	var languageModelSeen, speechSeen bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v1/chat/completions":
			languageModelSeen = true
			var body struct {
				Model    string                           `json:"model"`
				Messages []struct{ Role, Content string } `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Model != "cleaner" || len(body.Messages) != 2 || body.Messages[1].Content != "**Hello**, world." {
				t.Errorf("language-model body = %+v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "Hello, world."}}}})
		case "/v1/audio/speech":
			speechSeen = true
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["model"] != "speaker" || body["voice"] != "sky" || body["input"] != "Hello, world." || body["response_format"] != "mp3" {
				t.Errorf("speech body = %+v", body)
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte("audio-data"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	language, err := languagemodel.New(config.LanguageModelConfig{Endpoint: server.URL + "/v1/chat/completions", APIKey: "secret", Model: "cleaner"})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(config.VoiceConfig{Enabled: true, Instructions: "clean it", TTSEndpoint: server.URL + "/v1/audio/speech", TTSAPIKey: "secret", TTSModel: "speaker", TTSVoice: "sky", TTSFormat: "mp3"}, language)
	if err != nil {
		t.Fatal(err)
	}
	audio, err := svc.Render(context.Background(), "**Hello**, world.")
	if err != nil {
		t.Fatal(err)
	}
	if !languageModelSeen || !speechSeen || string(audio.Data) != "audio-data" || audio.MIMEType != "audio/mpeg" {
		t.Fatalf("render = %+v, languageModel=%v speech=%v", audio, languageModelSeen, speechSeen)
	}
}

func TestRenderSurfacesProviderProblem(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad credential"}}`))
	}))
	defer server.Close()
	language, err := languagemodel.New(config.LanguageModelConfig{Endpoint: server.URL, Model: "cleaner"})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := New(config.VoiceConfig{Enabled: true, TTSEndpoint: server.URL, TTSModel: "tts", TTSVoice: "voice"}, language)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Render(context.Background(), "hello")
	if err == nil || !strings.Contains(err.Error(), "bad credential") {
		t.Fatalf("Render error = %v", err)
	}
}

func TestNewRejectsInvalidEndpoint(t *testing.T) {
	_, err := New(config.VoiceConfig{Enabled: true, TTSEndpoint: "localhost:1", TTSModel: "tts", TTSVoice: "voice"}, fakeCompleter{})
	if err == nil || !strings.Contains(err.Error(), "absolute HTTP") {
		t.Fatalf("New error = %v", err)
	}
}

type fakeCompleter struct{}

func (fakeCompleter) Complete(context.Context, string, string) (string, error) {
	return "spoken text", nil
}
