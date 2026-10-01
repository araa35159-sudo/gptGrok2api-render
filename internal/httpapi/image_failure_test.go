package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

func TestImageFailuresReleaseCapacityAndPreserveResults(t *testing.T) {
	for _, tc := range []struct {
		name, mode              string
		count, requests, status int
	}{
		{"rejected_content_keeps_account_available", "blocked", 1, 2, 422},
		{"single_account_retry_preserves_gateway_error", "gateway", 1, 1, 502},
		{"completed_image_survives_rejected_sibling", "partial", 2, 1, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := testConfig()
			cfg.RootDir, cfg.DataDir = root, root
			cfg.ConfigPath = filepath.Join(root, "config.json")
			cfg.AccountsPath = filepath.Join(root, "accounts.json")
			cfg.AuthKeysPath = filepath.Join(root, "keys.json")
			cfg.ImageDataDir = filepath.Join(root, "images")
			cfg.ImageMaxConcurrency, cfg.ImageAccountLimit = 2, 1
			cfg.ChatMaxRetries = 2
			cfg.ChatRetryCodes = map[int]bool{422: true, 502: true}
			server := New(cfg)
			if _, _, _, err := server.store.AddAccounts(nil, []map[string]any{{"access_token": "jwt.header.payload", "pool": "basic", "status": "正常"}}); err != nil {
				t.Fatal(err)
			}
			const fileID = "file_000000001234567890abcdef12345678"
			var generations atomic.Int32
			var upstream *httptest.Server
			upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/":
					if tc.mode == "gateway" {
						http.Error(w, "fixture gateway failure", 502)
						return
					}
					_, _ = io.WriteString(w, `<html data-build="test"></html>`)
				case "/backend-api/sentinel/chat-requirements/prepare":
					_, _ = io.WriteString(w, `{"prepare_token":"prepare"}`)
				case "/backend-api/sentinel/chat-requirements/finalize":
					_, _ = io.WriteString(w, `{"token":"requirements"}`)
				case "/backend-api/f/conversation/prepare":
					_, _ = io.WriteString(w, `{"conduit_token":"conduit"}`)
				case "/backend-api/f/conversation":
					ordinal := generations.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					if tc.mode == "partial" && ordinal == 1 {
						_, _ = io.WriteString(w, "data: {\"conversation_id\":\"result\"}\n\ndata: [DONE]\n\n")
					} else {
						_, _ = io.WriteString(w, "data: {\"message\":{\"status\":\"blocked\"}}\n\n")
					}
				case "/backend-api/conversation/result":
					writeHTTPAPIGeneratedImageConversation(w, fileID)
				case "/backend-api/files/" + fileID + "/download":
					_ = json.NewEncoder(w).Encode(map[string]any{"download_url": upstream.URL + "/blob"})
				case "/blob":
					w.Header().Set("Content-Type", "image/png")
					_, _ = w.Write(tinyPNG)
				default:
					t.Errorf("unexpected upstream request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			server.openAIImage = provider.NewOpenAIImage(upstream.URL, upstream.Client(), nil, time.Second)
			for index := 0; index < tc.requests; index++ {
				body, _ := json.Marshal(map[string]any{"model": "gpt-image-2.5", "prompt": "test", "n": tc.count})
				request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(string(body)))
				request.Header.Set("Authorization", "Bearer api-secret")
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				if response.Code != tc.status || strings.Contains(response.Body.String(), "no available accounts") {
					t.Fatalf("unexpected response: %d %s", response.Code, response.Body.String())
				}
				if tc.mode == "gateway" && !strings.Contains(response.Body.String(), "fixture gateway failure") {
					t.Fatalf("original upstream failure was lost: %s", response.Body.String())
				}
				if tc.mode == "partial" {
					var value struct {
						Data []map[string]any `json:"data"`
					}
					if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil || len(value.Data) != 1 {
						t.Fatalf("completed output was discarded: %s", response.Body.String())
					}
				}
				if len(server.imageSlots) != 0 {
					t.Fatal("finished request held a concurrency slot")
				}
			}
			if tc.mode == "blocked" && generations.Load() != 2 {
				t.Fatalf("content rejection was retried: %d generations", generations.Load())
			}
		})
	}
}
