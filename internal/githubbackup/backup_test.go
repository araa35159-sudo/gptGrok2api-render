package githubbackup

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/auucoder/gptgrok2api-go/internal/config"
)

func testConfig(root string) config.Config {
	data := filepath.Join(root, "data")
	return config.Config{
		DataDir:          data,
		ConfigPath:       filepath.Join(data, "config.json"),
		AccountsPath:     filepath.Join(data, "accounts.json"),
		AuthKeysPath:     filepath.Join(data, "auth_keys.json"),
		OAuthPath:        filepath.Join(data, "oauth_accounts.json.enc"),
		QueuePath:        filepath.Join(data, "tasks.json"),
		RegisterPath:     filepath.Join(data, "register.json"),
		GrokAccountsPath: filepath.Join(data, "grok_accounts.json"),
	}
}

func TestEncryptedSnapshotRoundTrip(t *testing.T) {
	var mu sync.Mutex
	var stored []byte
	var writes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/repos/owner/data":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/repos/owner/data/branches/main":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/repos/owner/data/contents/render-state.enc.json" && r.Method == http.MethodGet:
			if stored == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"sha": fmt.Sprintf("sha-%d", writes), "content": base64.StdEncoding.EncodeToString(stored)})
		case r.URL.Path == "/repos/owner/data/contents/render-state.enc.json" && r.Method == http.MethodPut:
			var body struct {
				Content string `json:"content"`
				SHA     string `json:"sha"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode upload: %v", err)
				return
			}
			if stored != nil && body.SHA != fmt.Sprintf("sha-%d", writes) {
				w.WriteHeader(http.StatusConflict)
				return
			}
			var err error
			stored, err = base64.StdEncoding.DecodeString(body.Content)
			if err != nil {
				t.Errorf("decode content: %v", err)
				return
			}
			writes++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"content": map[string]string{"sha": fmt.Sprintf("sha-%d", writes)}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	key := sha256.Sum256([]byte("test backup key"))
	makeBackup := func() *Backup {
		return &Backup{client: server.Client(), apiURL: server.URL, repo: "owner/data", branch: "main", path: "render-state.enc.json", token: "test", key: key[:]}
	}
	first := testConfig(t.TempDir())
	if err := os.MkdirAll(first.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first.AccountsPath, []byte(`[{"access_token":"secret-token"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	backup := makeBackup()
	found, err := backup.Restore(context.Background(), first)
	if err != nil || found {
		t.Fatalf("initial restore: found=%v err=%v", found, err)
	}
	uploaded, err := backup.Sync(context.Background(), first)
	if err != nil || !uploaded {
		t.Fatalf("upload: uploaded=%v err=%v", uploaded, err)
	}
	if strings.Contains(string(stored), "secret-token") {
		t.Fatal("plaintext credential appeared in GitHub content")
	}
	uploaded, err = backup.Sync(context.Background(), first)
	if err != nil || uploaded || writes != 1 {
		t.Fatalf("unchanged state generated commit: uploaded=%v writes=%d err=%v", uploaded, writes, err)
	}
	second := testConfig(t.TempDir())
	found, err = makeBackup().Restore(context.Background(), second)
	if err != nil || !found {
		t.Fatalf("restore: found=%v err=%v", found, err)
	}
	raw, err := os.ReadFile(second.AccountsPath)
	if err != nil || string(raw) != `[{"access_token":"secret-token"}]` {
		t.Fatalf("restored accounts: %q %v", raw, err)
	}
}

func TestInvalidBackupKeyStopsStartup(t *testing.T) {
	t.Setenv("GITHUB_BACKUP_REPO", "owner/data")
	t.Setenv("GITHUB_BACKUP_TOKEN", "token")
	t.Setenv("GITHUB_BACKUP_KEY", "not-a-key")
	if _, err := FromEnv(); err == nil {
		t.Fatal("invalid encryption key was accepted")
	}
}
