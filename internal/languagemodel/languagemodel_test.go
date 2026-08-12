package languagemodel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aiguy110/tandem/internal/config"
)

func TestCompleteUsesSharedCompatibleChatCompletionsContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("request = %s authorization=%q", r.Method, r.Header.Get("Authorization"))
		}
		var body struct {
			Model    string                           `json:"model"`
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "shared-model" || len(body.Messages) != 2 || body.Messages[0].Content != "instructions" || body.Messages[1].Content != "input" {
			t.Fatalf("body = %+v", body)
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":" generated text "}}]}`))
	}))
	defer server.Close()
	client, err := New(config.LanguageModelConfig{Endpoint: server.URL, APIKey: "secret", Model: "shared-model"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Complete(context.Background(), "instructions", "input")
	if err != nil || got != "generated text" {
		t.Fatalf("Complete = %q, %v", got, err)
	}
}
