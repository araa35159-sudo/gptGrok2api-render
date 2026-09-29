package githubbackup

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/config"
)

// Backup keeps the small, mutable JSON state in an encrypted file in a
// separate GitHub repository. It is intended for a single running instance.
type Backup struct {
	client  *http.Client
	apiURL  string
	repo    string
	branch  string
	path    string
	token   string
	key     []byte
	mu      sync.Mutex
	sha     string
	lastSum [32]byte
}

type snapshot struct {
	Version int               `json:"version"`
	Files   map[string]string `json:"files"`
}

type envelope struct {
	Version    int    `json:"version"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

type contentResponse struct {
	SHA     string `json:"sha"`
	Content string `json:"content"`
}

// FromEnv returns nil when GitHub backup has not been configured. A partial
// configuration is an error so a deployment cannot silently lose persistence.
func FromEnv() (*Backup, error) {
	repo := strings.TrimSpace(os.Getenv("GITHUB_BACKUP_REPO"))
	token := strings.TrimSpace(os.Getenv("GITHUB_BACKUP_TOKEN"))
	encodedKey := strings.TrimSpace(os.Getenv("GITHUB_BACKUP_KEY"))
	if repo == "" && token == "" && encodedKey == "" {
		return nil, nil
	}
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || token == "" || encodedKey == "" {
		return nil, errors.New("GITHUB_BACKUP_REPO (owner/repo), GITHUB_BACKUP_TOKEN, and GITHUB_BACKUP_KEY must all be set")
	}
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("GITHUB_BACKUP_KEY must be a base64-encoded 32-byte key")
	}
	branch := strings.TrimSpace(os.Getenv("GITHUB_BACKUP_BRANCH"))
	if branch == "" {
		branch = "main"
	}
	path := strings.TrimSpace(os.Getenv("GITHUB_BACKUP_PATH"))
	if path == "" {
		path = "render-state.enc.json"
	}
	if !validRemotePath(path) {
		return nil, errors.New("GITHUB_BACKUP_PATH must be a relative file path without dot segments")
	}
	return &Backup{
		client: &http.Client{Timeout: 15 * time.Second},
		apiURL: "https://api.github.com",
		repo:   repo,
		branch: branch,
		path:   path,
		token:  token,
		key:    key,
	}, nil
}

func validRemotePath(path string) bool {
	if strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func (b *Backup) fileURL() string {
	parts := strings.Split(b.path, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return b.apiURL + "/repos/" + b.repo + "/contents/" + strings.Join(parts, "/")
}

func (b *Backup) request(ctx context.Context, method, endpoint string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return b.client.Do(req)
}

func responseError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("GitHub backup API returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
}

func (b *Backup) checkRepo(ctx context.Context) error {
	resp, err := b.request(ctx, http.MethodGet, b.apiURL+"/repos/"+b.repo, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError(resp)
	}
	branchURL := b.apiURL + "/repos/" + b.repo + "/branches/" + url.PathEscape(b.branch)
	branchResp, err := b.request(ctx, http.MethodGet, branchURL, nil)
	if err != nil {
		return err
	}
	defer branchResp.Body.Close()
	if branchResp.StatusCode != http.StatusOK {
		return responseError(branchResp)
	}
	return nil
}

func (b *Backup) load(ctx context.Context) ([]byte, string, bool, error) {
	endpoint := b.fileURL() + "?ref=" + url.QueryEscape(b.branch)
	resp, err := b.request(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", false, responseError(resp)
	}
	var result contentResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 20<<20)).Decode(&result); err != nil {
		return nil, "", false, err
	}
	if result.SHA == "" || result.Content == "" {
		return nil, "", false, errors.New("GitHub backup response is missing sha or content")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(result.Content, "\n", ""))
	return raw, result.SHA, true, err
}

func (b *Backup) encrypt(plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(b.key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return json.Marshal(envelope{1, base64.StdEncoding.EncodeToString(nonce), base64.StdEncoding.EncodeToString(aead.Seal(nil, nonce, plain, nil))})
}

func (b *Backup) decrypt(raw []byte) ([]byte, error) {
	var sealed envelope
	if err := json.Unmarshal(raw, &sealed); err != nil {
		return nil, err
	}
	if sealed.Version != 1 {
		return nil, fmt.Errorf("unsupported GitHub backup version %d", sealed.Version)
	}
	nonce, err := base64.StdEncoding.DecodeString(sealed.Nonce)
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(sealed.Ciphertext)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(b.key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, ciphertext, nil)
}

func statePaths(cfg config.Config) map[string]string {
	return map[string]string{
		"config.json":              cfg.ConfigPath,
		"accounts.json":            cfg.AccountsPath,
		"auth_keys.json":           cfg.AuthKeysPath,
		"oauth_accounts.json.enc":  cfg.OAuthPath,
		"register.json":            cfg.RegisterPath,
		"grok_accounts.json":       cfg.GrokAccountsPath,
		"tasks.json":               cfg.QueuePath,
		"image_tags.json":          filepath.Join(cfg.DataDir, "image_tags.json"),
		"prompt_sources.json":      filepath.Join(cfg.DataDir, "prompt_sources.json"),
		"cpa_config.json":          filepath.Join(cfg.DataDir, "cpa_config.json"),
		"sub2api_config.json":      filepath.Join(cfg.DataDir, "sub2api_config.json"),
		"editable_file_tasks.json": filepath.Join(cfg.DataDir, "editable_file_tasks.json"),
	}
}

func collect(cfg config.Config) ([]byte, error) {
	files := make(map[string]string)
	for name, path := range statePaths(cfg) {
		if path == "" {
			continue
		}
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		files[name] = base64.StdEncoding.EncodeToString(raw)
	}
	return json.Marshal(snapshot{Version: 1, Files: files})
}

// Restore must run before the server and account pool are constructed. A
// failed download or decryption stops startup instead of loading empty state.
func (b *Backup) Restore(ctx context.Context, cfg config.Config) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.checkRepo(ctx); err != nil {
		return false, err
	}
	raw, sha, found, err := b.load(ctx)
	if err != nil || !found {
		return found, err
	}
	plain, err := b.decrypt(raw)
	if err != nil {
		return false, fmt.Errorf("decrypt GitHub backup: %w", err)
	}
	var state snapshot
	if err := json.Unmarshal(plain, &state); err != nil || state.Version != 1 {
		return false, errors.New("invalid GitHub backup snapshot")
	}
	paths := statePaths(cfg)
	decoded := make(map[string][]byte)
	for name, encoded := range state.Files {
		if _, ok := paths[name]; !ok {
			return false, fmt.Errorf("unexpected file in GitHub backup: %s", name)
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return false, fmt.Errorf("decode %s: %w", name, err)
		}
		if !strings.HasSuffix(name, ".enc") && !json.Valid(data) {
			return false, fmt.Errorf("invalid JSON in GitHub backup: %s", name)
		}
		decoded[name] = data
	}
	for name, data := range decoded {
		if err := writeAtomic(paths[name], data); err != nil {
			return false, fmt.Errorf("restore %s: %w", name, err)
		}
	}
	b.sha = sha
	b.lastSum = sha256.Sum256(plain)
	return true, nil
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".restore-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Sync uploads only changed state. Concurrent calls within this process are
// serialized; GitHub's sha check protects against a stale remote revision.
func (b *Backup) Sync(ctx context.Context, cfg config.Config) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	plain, err := collect(cfg)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(plain)
	if sum == b.lastSum {
		return false, nil
	}
	sealed, err := b.encrypt(plain)
	if err != nil {
		return false, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		payload := map[string]any{
			"message": "Update encrypted Render state",
			"content": base64.StdEncoding.EncodeToString(sealed),
			"branch":  b.branch,
		}
		if b.sha != "" {
			payload["sha"] = b.sha
		}
		resp, err := b.request(ctx, http.MethodPut, b.fileURL(), payload)
		if err != nil {
			return false, err
		}
		if resp.StatusCode == http.StatusConflict {
			_ = resp.Body.Close()
			_, remoteSHA, found, err := b.load(ctx)
			if err != nil {
				return false, err
			}
			if found {
				b.sha = remoteSHA
			}
			continue
		}
		if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
			err = responseError(resp)
			_ = resp.Body.Close()
			return false, err
		}
		var result struct {
			Content contentResponse `json:"content"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result)
		_ = resp.Body.Close()
		if err != nil || result.Content.SHA == "" {
			return false, errors.New("GitHub backup upload did not return a sha")
		}
		b.sha = result.Content.SHA
		b.lastSum = sum
		return true, nil
	}
	return false, errors.New("GitHub backup changed concurrently; retry later")
}

// Run checks for changed files periodically. Failed uploads remain dirty and
// are retried on the next tick. The caller also syncs during graceful shutdown.
func (b *Backup) Run(ctx context.Context, cfg config.Config, logError func(error)) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			uploadCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, err := b.Sync(uploadCtx, cfg)
			cancel()
			if err != nil {
				logError(err)
			}
		}
	}
}
