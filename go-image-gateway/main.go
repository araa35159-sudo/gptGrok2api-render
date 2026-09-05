package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

const queueKey = "image-gateway:queue"

type config struct {
	ListenAddr     string
	RedisAddr      string
	BackendURL     string
	SchedulerURL   string
	SchedulerKey   string
	MonitorURL     string
	LogURL         string
	Workers        int
	QueueCapacity  int64
	SyncWait       time.Duration
	BackendTimeout time.Duration
	TaskTTL        time.Duration
	MaxBodyBytes   int64
	AuthKeysFile   string
	AdminKey       string
	MaxAttempts    int
}

type task struct {
	ID            string          `json:"id"`
	Status        string          `json:"status"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	Authorization string          `json:"authorization,omitempty"`
	Result        json.RawMessage `json:"result,omitempty"`
	Error         string          `json:"error,omitempty"`
	StatusCode    int             `json:"status_code,omitempty"`
	Attempts      int             `json:"attempts"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type editFile struct {
	Name string `json:"name"`
	Mime string `json:"mime"`
	Data string `json:"data"`
}

type publicTask struct {
	ID         string          `json:"id"`
	Status     string          `json:"status"`
	StatusURL  string          `json:"status_url,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	StatusCode int             `json:"status_code,omitempty"`
	Attempts   int             `json:"attempts"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type reservation struct {
	ID           string `json:"reservation_id"`
	AccountEmail string `json:"account_email"`
	ProxySource  string `json:"proxy_source"`
	ProxyGroupID string `json:"proxy_group_id"`
	ProxyNodeID  string `json:"proxy_node_id"`
}

type server struct {
	cfg       config
	rdb       *redis.Client
	client    *http.Client
	started   time.Time
	accepted  atomic.Uint64
	completed atomic.Uint64
	failed    atomic.Uint64
	active    atomic.Int64
	wg        sync.WaitGroup
	waiters   sync.Map
	cancels   sync.Map
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(env(name, strconv.Itoa(fallback)))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

func envIntNonNegative(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func loadConfig() config {
	return config{
		ListenAddr:    env("IMAGE_GATEWAY_LISTEN", ":8080"),
		RedisAddr:     env("IMAGE_GATEWAY_REDIS_ADDR", "redis:6379"),
		BackendURL:    strings.TrimRight(env("IMAGE_GATEWAY_BACKEND_URL", "http://app/v1/images/generations"), "/"),
		SchedulerURL:  strings.TrimRight(env("IMAGE_GATEWAY_SCHEDULER_URL", "http://app/internal/image-scheduler"), "/"),
		SchedulerKey:  strings.TrimSpace(os.Getenv("IMAGE_GATEWAY_SCHEDULER_KEY")),
		MonitorURL:    strings.TrimRight(env("IMAGE_GATEWAY_MONITOR_URL", "http://app/internal/image-monitor"), "/"),
		LogURL:        strings.TrimRight(env("IMAGE_GATEWAY_LOG_URL", "http://app/internal/logs/call"), "/"),
		Workers:       envInt("IMAGE_GATEWAY_WORKERS", 1000),
		QueueCapacity: int64(envInt("IMAGE_GATEWAY_QUEUE_CAPACITY", 10000)),
		// Keep synchronous compatibility for fast requests, but return a task
		// before Cloudflare's roughly two-minute origin timeout for slow ones.
		SyncWait:       time.Duration(envIntNonNegative("IMAGE_GATEWAY_SYNC_WAIT_SECS", 90)) * time.Second,
		BackendTimeout: time.Duration(envInt("IMAGE_GATEWAY_BACKEND_TIMEOUT_SECS", 900)) * time.Second,
		TaskTTL:        time.Duration(envInt("IMAGE_GATEWAY_TASK_TTL_SECS", 86400)) * time.Second,
		MaxBodyBytes:   int64(envInt("IMAGE_GATEWAY_MAX_BODY_MB", 2)) << 20,
		AuthKeysFile:   env("IMAGE_GATEWAY_AUTH_KEYS_FILE", "/etc/image-gateway/auth_keys.json"),
		AdminKey:       strings.TrimSpace(os.Getenv("IMAGE_GATEWAY_AUTH_KEY")),
		MaxAttempts:    envInt("IMAGE_GATEWAY_MAX_ATTEMPTS", 1),
	}
}

func (s *server) authorized(header string) bool {
	token := strings.TrimSpace(header)
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = strings.TrimSpace(token[7:])
	}
	if token == "" {
		return false
	}
	if s.cfg.AdminKey != "" && token == s.cfg.AdminKey {
		return true
	}
	raw, err := os.ReadFile(filepath.Clean(s.cfg.AuthKeysFile))
	if err != nil {
		return false
	}
	var doc struct {
		Items []struct {
			KeyHash string `json:"key_hash"`
			Enabled bool   `json:"enabled"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	for _, item := range doc.Items {
		if item.Enabled && item.KeyHash == hash {
			return true
		}
	}
	return false
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func taskKey(id string) string { return "image-gateway:task:" + id }

func (s *server) saveTask(ctx context.Context, item task) error {
	item.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(item)
	ttl := s.cfg.TaskTTL
	// Keep terminal task records for the configured retention window as well.
	// A short fixed TTL made completed images appear to disappear while clients
	// were still polling the task URL.
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, taskKey(item.ID), raw, ttl).Err()
}

func (s *server) loadTask(ctx context.Context, id string) (task, error) {
	var item task
	raw, err := s.rdb.Get(ctx, taskKey(id)).Bytes()
	if err != nil {
		return item, err
	}
	err = json.Unmarshal(raw, &item)
	return item, err
}

func (s *server) cancelTask(id string) {
	if value, ok := s.cancels.Load(id); ok {
		if cancel, ok := value.(context.CancelFunc); ok {
			cancel()
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	item, err := s.loadTask(ctx, id)
	if err != nil || item.Status != "queued" {
		return
	}
	item.Status = "canceled"
	item.Error = "client disconnected"
	item.Payload = nil
	item.Authorization = ""
	_ = s.saveTask(ctx, item)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if !s.authorized(r.Header.Get("Authorization")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid API key"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 413, map[string]string{"error": "request body too large"})
		return
	}
	var check map[string]any
	if json.Unmarshal(raw, &check) != nil || strings.TrimSpace(fmt.Sprint(check["prompt"])) == "" {
		writeJSON(w, 400, map[string]string{"error": "valid JSON prompt is required"})
		return
	}
	s.enqueue(w, r, raw)
	return
}

func (s *server) enqueue(w http.ResponseWriter, r *http.Request, raw []byte) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	length, err := s.rdb.LLen(ctx, queueKey).Result()
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "queue unavailable"})
		return
	}
	if length >= s.cfg.QueueCapacity {
		writeJSON(w, 429, map[string]string{"error": "task queue is full"})
		return
	}
	now := time.Now().UTC()
	item := task{ID: newID(), Status: "queued", Payload: raw, Authorization: r.Header.Get("Authorization"), CreatedAt: now, UpdatedAt: now}
	if err := s.saveTask(ctx, item); err != nil {
		writeJSON(w, 503, map[string]string{"error": "unable to persist task"})
		return
	}
	ch := make(chan task, 1)
	s.waiters.Store(item.ID, ch)
	defer s.waiters.Delete(item.ID)
	if err := s.rdb.RPush(ctx, queueKey, item.ID).Err(); err != nil {
		writeJSON(w, 503, map[string]string{"error": "unable to enqueue task"})
		return
	}
	s.accepted.Add(1)
	// Cloudflare and other edge proxies can time out while an image is queued
	// or generated.  Async callers receive the task id immediately and poll
	// /v1/image-tasks/{id}; the worker still uses the same reservation flow.
	if wantsAsync(r) || s.cfg.SyncWait <= 0 {
		writeAcceptedTask(w, item)
		return
	}
	wait := s.syncWaitForRequest(r)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case done := <-ch:
		if done.Status == "success" {
			writeJSON(w, http.StatusOK, json.RawMessage(done.Result))
			return
		}
		status := done.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		errorType := "server_error"
		if status < 500 {
			errorType = "invalid_request_error"
		}
		writeJSON(w, status, map[string]any{"error": map[string]any{"message": done.Error, "type": errorType}})
	case <-timer.C:
		// Do not hold the public request open past the edge proxy deadline.
		// The task remains in Redis and can be polled until it completes.
		writeAcceptedTask(w, item)
	case <-r.Context().Done():
		// The caller (or an edge proxy such as Cloudflare) may close the
		// connection before image generation finishes. Keep the queued task
		// running so it can persist its final result for polling instead of
		// turning a successful upstream generation into a canceled/502 task.
		return
	}
}

func writeAcceptedTask(w http.ResponseWriter, item task) {
	w.Header().Set("Location", "/v1/image-tasks/"+item.ID)
	w.Header().Set("Retry-After", "2")
	writeJSON(w, http.StatusAccepted, publicTask{ID: item.ID, Status: item.Status, StatusURL: "/v1/image-tasks/" + item.ID, StatusCode: item.StatusCode, Attempts: item.Attempts, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt})
}

// Direct-IP callers are not behind Cloudflare's origin timeout, so keep their
// compatible endpoint open for the final result. The proxied public hostname
// still switches to a pollable task before Cloudflare can return HTTP 524.
func (s *server) syncWaitForRequest(r *http.Request) time.Duration {
	if r == nil {
		return s.cfg.SyncWait
	}
	host := strings.ToLower(strings.TrimSpace(r.Host))
	if forwarded := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))); forwarded != "" {
		host = forwarded
	}
	host = strings.Split(host, ":")[0]
	if host == "gpt.qkmss.com" {
		return s.cfg.SyncWait
	}
	return s.cfg.BackendTimeout
}

func wantsAsync(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.URL != nil && (r.URL.Path == "/v1/image-tasks/generations" || r.URL.Path == "/v1/image-tasks/edits") {
		return true
	}
	for _, value := range []string{r.URL.Query().Get("async"), r.Header.Get("X-Image-Async")} {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "on":
			return true
		}
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Prefer")), "respond-async")
}

func (s *server) submitEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if !s.authorized(r.Header.Get("Authorization")) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid API key"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 413, map[string]string{"error": "request body too large"})
		return
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '{' {
		s.submitJSONEditRaw(w, r, raw)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid multipart request"})
		return
	}
	form := r.MultipartForm
	if form == nil {
		writeJSON(w, 400, map[string]string{"error": "multipart form is required"})
		return
	}
	files := func(keys ...string) ([]editFile, error) {
		var out []editFile
		for _, key := range keys {
			for _, header := range form.File[key] {
				file, err := header.Open()
				if err != nil {
					return nil, err
				}
				data, err := io.ReadAll(io.LimitReader(file, 50<<20))
				file.Close()
				if err != nil {
					return nil, err
				}
				out = append(out, editFile{Name: header.Filename, Mime: header.Header.Get("Content-Type"), Data: base64.StdEncoding.EncodeToString(data)})
			}
		}
		return out, nil
	}
	images, err := files("image", "image[]")
	if err != nil || len(images) == 0 {
		writeJSON(w, 400, map[string]string{"error": "image file or image_url is required"})
		return
	}
	masks, err := files("mask")
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid mask"})
		return
	}
	payload := map[string]any{"_edit": true, "prompt": firstFormValue(form.Value["prompt"]), "model": firstFormValue(form.Value["model"]), "n": firstFormValue(form.Value["n"]), "size": firstFormValue(form.Value["size"]), "quality": firstFormValue(form.Value["quality"]), "response_format": firstFormValue(form.Value["response_format"]), "images": images, "mask": masks}
	if payload["model"] == "" {
		payload["model"] = "gpt-image-2"
	}
	if payload["n"] == "" {
		payload["n"] = "1"
	}
	if payload["size"] == "" {
		payload["size"] = "1024x1024"
	}
	if payload["response_format"] == "" {
		payload["response_format"] = "b64_json"
	}
	raw, _ = json.Marshal(payload)
	s.enqueue(w, r, raw)
	return
}

func (s *server) submitJSONEdit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 413, map[string]string{"error": "request body too large"})
		return
	}
	s.submitJSONEditRaw(w, r, raw)
}

func (s *server) submitJSONEditRaw(w http.ResponseWriter, r *http.Request, raw []byte) {
	var input map[string]json.RawMessage
	if json.Unmarshal(raw, &input) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON request"})
		return
	}
	if mask := input["mask"]; len(mask) > 0 && string(mask) != "null" && string(mask) != `""` {
		writeJSON(w, 400, map[string]string{"error": "mask is not supported yet"})
		return
	}
	values := []json.RawMessage{}
	for _, key := range []string{"image", "images", "images[]", "image_url", "image_url_parts"} {
		raw := input[key]
		if len(raw) == 0 {
			continue
		}
		var many []json.RawMessage
		if json.Unmarshal(raw, &many) == nil {
			values = append(values, many...)
		} else {
			values = append(values, raw)
		}
	}
	if len(values) == 0 {
		writeJSON(w, 400, map[string]string{"error": "image is required"})
		return
	}
	images := []editFile{}
	for i, value := range values {
		dataURL, ok := imageReferenceString(value)
		if !ok || strings.TrimSpace(dataURL) == "" {
			writeJSON(w, 400, map[string]string{"error": "image must be a string"})
			return
		}
		if strings.HasPrefix(strings.ToLower(dataURL), "http://") || strings.HasPrefix(strings.ToLower(dataURL), "https://") {
			resp, fetchErr := http.Get(dataURL)
			if fetchErr != nil || resp.StatusCode >= 400 {
				if resp != nil {
					resp.Body.Close()
				}
				writeJSON(w, 400, map[string]string{"error": "invalid image URL"})
				return
			}
			b, readErr := io.ReadAll(io.LimitReader(resp.Body, 50<<20+1))
			resp.Body.Close()
			if readErr != nil || len(b) > 50<<20 {
				writeJSON(w, 400, map[string]string{"error": "invalid image URL"})
				return
			}
			contentType := resp.Header.Get("Content-Type")
			if contentType == "" {
				contentType = "image/png"
			}
			dataURL = "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(b)
		}
		header, encoded, ok := strings.Cut(dataURL, ",")
		if !ok || !strings.HasPrefix(strings.ToLower(header), "data:") || !strings.Contains(strings.ToLower(header), ";base64") {
			if !strings.Contains(dataURL, ",") && looksLikeBase64(dataURL) {
				dataURL = "data:image/png;base64," + dataURL
				header, encoded, ok = strings.Cut(dataURL, ",")
			}
		}
		if !ok || !strings.HasPrefix(strings.ToLower(header), "data:") || !strings.Contains(strings.ToLower(header), ";base64") {
			writeJSON(w, 400, map[string]string{"error": "image must be a base64 data URL"})
			return
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": "invalid image base64 data"})
			return
		}
		mime := "image/png"
		if semi := strings.Index(header, ";"); semi > 5 {
			mime = header[5:semi]
		}
		images = append(images, editFile{Name: fmt.Sprintf("image-%d.png", i+1), Mime: mime, Data: base64.StdEncoding.EncodeToString(data)})
	}
	get := func(name, fallback string) string {
		var value string
		if json.Unmarshal(input[name], &value) == nil && value != "" {
			return value
		}
		return fallback
	}
	n := "1"
	if value := input["n"]; len(value) > 0 {
		var number int
		if json.Unmarshal(value, &number) == nil {
			n = strconv.Itoa(number)
		} else {
			var text string
			if json.Unmarshal(value, &text) == nil && text != "" {
				n = text
			}
		}
	}
	payload := map[string]any{"_edit": true, "prompt": get("prompt", ""), "model": get("model", "gpt-image-2"), "n": n, "size": get("size", "1024x1024"), "quality": get("quality", ""), "response_format": get("response_format", "b64_json"), "images": images}
	encoded, _ := json.Marshal(payload)
	s.enqueue(w, r, encoded)
}

// imageReferenceString accepts the string and object shapes used by OpenAI
// compatible clients: a direct URL/data URL, {url: ...}, or nested
// {image_url: {url: ...}}.
func imageReferenceString(raw json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text), true
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return "", false
	}
	for _, key := range []string{"url", "data_url", "data", "b64_json", "image_url", "image"} {
		value, ok := object[key]
		if !ok {
			continue
		}
		if text, ok := imageReferenceString(value); ok && text != "" {
			return text, true
		}
	}
	return "", false
}

func looksLikeBase64(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 32 {
		return false
	}
	for _, char := range value {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '+' || char == '/' || char == '=' || char == '-' || char == '_' || char == '\r' || char == '\n' {
			continue
		}
		return false
	}
	return true
}

func firstFormValue(values []string) string {
	if len(values) > 0 {
		return values[0]
	}
	return ""
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/image-tasks/")
	if id == "" || strings.Contains(id, "/") {
		writeJSON(w, 404, map[string]string{"error": "task not found"})
		return
	}
	item, err := s.loadTask(r.Context(), id)
	if errors.Is(err, redis.Nil) {
		writeJSON(w, 404, map[string]string{"error": "task not found"})
		return
	}
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": "task store unavailable"})
		return
	}
	writeJSON(w, 200, publicTask{ID: item.ID, Status: item.Status, StatusURL: "/v1/image-tasks/" + item.ID, Result: item.Result, Error: item.Error, StatusCode: item.StatusCode, Attempts: item.Attempts, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt})
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	err := s.rdb.Ping(ctx).Err()
	queued, _ := s.rdb.LLen(ctx, queueKey).Result()
	status := 200
	state := "ok"
	if err != nil {
		status, state = 503, "degraded"
	}
	writeJSON(w, status, map[string]any{"status": state, "redis_ok": err == nil, "workers": s.cfg.Workers, "active": s.active.Load(), "queued": queued, "accepted": s.accepted.Load(), "completed": s.completed.Load(), "failed": s.failed.Load(), "uptime_secs": int(time.Since(s.started).Seconds())})
}

func (s *server) worker(ctx context.Context, workerID int) {
	defer s.wg.Done()
	for {
		result, err := s.rdb.BLPop(ctx, 5*time.Second, queueKey).Result()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !errors.Is(err, redis.Nil) {
				log.Printf("worker=%d queue_error=%v", workerID, err)
				time.Sleep(time.Second)
			}
			continue
		}
		if len(result) != 2 {
			continue
		}
		s.runTask(ctx, result[1])
	}
}

func (s *server) runTask(parent context.Context, id string) {
	item, err := s.loadTask(parent, id)
	if err != nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil && item.Status == "running" {
			item.Status = "failed"
			item.Error = fmt.Sprintf("image worker panic: %v", recovered)
			item.Payload = nil
			item.Authorization = ""
			_ = s.saveTask(context.Background(), item)
			s.failed.Add(1)
			s.monitorFinish(item, "failed", item.Error)
			s.notify(item)
		}
	}()
	if item.Status == "canceled" {
		return
	}
	item.Status = "running"
	item.Attempts++
	_ = s.saveTask(parent, item)
	requestSummary := imageSummary(item.Payload)
	s.active.Add(1)
	defer s.active.Add(-1)
	if item.Attempts == 1 {
		s.monitorStart(item)
	}
	s.monitorStage(item.ID, "image_getting_account", map[string]any{"model": imageModel(item.Payload), "handler_queue_ms": time.Since(item.CreatedAt).Milliseconds()})
	taskCtx, taskCancel := context.WithCancel(parent)
	s.cancels.Store(id, taskCancel)
	defer func() {
		s.cancels.Delete(id)
		taskCancel()
	}()
	ctx, cancel := context.WithTimeout(taskCtx, s.cfg.BackendTimeout)
	defer cancel()
	lease, err := s.scheduler(ctx, "reserve", item.Payload)
	statusCode := 0
	if err == nil {
		var response json.RawMessage
		s.monitorStage(item.ID, "image_egress_ready", map[string]any{"model": imageModel(item.Payload)})
		s.monitorStage(item.ID, "image_starting_generation", map[string]any{"model": imageModel(item.Payload)})
		if isEditPayload(item.Payload) {
			response, statusCode, err = s.schedulerExecuteEdit(ctx, lease.ID, item.ID, item.Payload, item.Authorization)
		} else {
			response, statusCode, err = s.schedulerExecute(ctx, lease.ID, item.ID, item.Payload, item.Authorization)
		}
		// A 202 response is an acceptance/queue signal, not a completed
		// OpenAI image result. Never persist it as success, otherwise callers
		// receive a misleading 200 containing an async task payload.
		if err == nil {
			if statusCode == http.StatusAccepted {
				err = fmt.Errorf("image backend returned HTTP 202 before producing a result")
				statusCode = http.StatusBadGateway
			} else if !hasImageResultData(response) {
				err = errors.New("image backend returned no image data")
			}
		}
		// schedulerExecute returns the upstream status alongside an error for
		// non-2xx responses. Never expose an upstream 202 as a terminal task
		// status: it means accepted/pending, not a completed image result.
		if statusCode == http.StatusAccepted {
			statusCode = http.StatusBadGateway
		}
		// release is idempotent; execute also releases in its finally block.
		_ = s.schedulerRelease(context.Background(), lease.ID, false)
		if err == nil {
			item.Status, item.StatusCode, item.Result, item.Payload, item.Authorization = "success", http.StatusOK, response, nil, ""
			s.completed.Add(1)
			_ = s.saveTask(parent, item)
			s.monitorFinish(item, "success", "")
			s.logCall(item, requestSummary, "success", "", lease)
			s.notify(item)
			return
		}
	}
	if taskCtx.Err() != nil {
		item.Status, item.StatusCode, item.Error, item.Payload, item.Authorization = "canceled", 499, "client disconnected", nil, ""
		_ = s.saveTask(context.Background(), item)
		s.monitorFinish(item, "canceled", item.Error)
		s.notify(item)
		return
	}
	retryable := err != nil && (statusCode == 0 || statusCode == http.StatusTooManyRequests || statusCode >= 500)
	if retryable && item.Attempts < s.cfg.MaxAttempts {
		item.Status = "queued"
		_ = s.saveTask(parent, item)
		delay := time.Duration(1<<(item.Attempts-1)) * time.Second
		s.monitorStage(item.ID, "image_retry_wait", map[string]any{"model": imageModel(item.Payload), "retry_wait_ms": delay.Milliseconds(), "error": truncate(err)})
		go func(taskID string, wait time.Duration) {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			<-timer.C
			_ = s.rdb.RPush(context.Background(), queueKey, taskID).Err()
		}(item.ID, delay)
		return
	}
	if statusCode < 400 || statusCode > 599 {
		statusCode = http.StatusBadGateway
	}
	item.Status, item.StatusCode, item.Error, item.Payload, item.Authorization = "failed", statusCode, truncate(err), nil, ""
	s.failed.Add(1)
	_ = s.saveTask(parent, item)
	s.monitorFinish(item, "failed", item.Error)
	s.logCall(item, requestSummary, "failed", item.Error, lease)
	s.notify(item)
}

func hasImageResultData(raw []byte) bool {
	var envelope map[string]any
	if json.Unmarshal(raw, &envelope) != nil {
		return false
	}
	data, ok := envelope["data"].([]any)
	return ok && len(data) > 0
}

func (s *server) scheduler(ctx context.Context, action string, payload []byte) (reservation, error) {
	var empty reservation
	var body []byte
	if action == "reserve" {
		body = []byte(`{"model":"gpt-image-2"}`)
	} else {
		body = payload
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.SchedulerURL+"/reserve", bytes.NewReader(body))
	if err != nil {
		return empty, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-image-scheduler-key", s.cfg.SchedulerKey)
	req.Header.Set("X-Image-Gateway-Task", "1")
	resp, err := s.client.Do(req)
	if err != nil {
		return empty, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil || resp.StatusCode >= 300 {
		return empty, fmt.Errorf("scheduler reserve HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var value reservation
	if json.Unmarshal(raw, &value) != nil || value.ID == "" {
		return empty, errors.New("scheduler returned no reservation")
	}
	return value, nil
}

func (s *server) schedulerExecute(ctx context.Context, reservation, callID string, payload []byte, authorization string) (json.RawMessage, int, error) {
	var request map[string]any
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, 0, fmt.Errorf("invalid task payload: %w", err)
	}
	request["_call_id"] = callID
	request["_trace_image_perf"] = true
	// The reservation executor collects one final image response.  Normalize
	// client-side streaming requests here instead of failing them with a 400;
	// the public image endpoint remains OpenAI-compatible while the internal
	// account/proxy lease is always released deterministically.
	request["stream"] = false
	body, err := json.Marshal(map[string]any{"request": request})
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.SchedulerURL+"/"+reservation+"/execute", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-image-scheduler-key", s.cfg.SchedulerKey)
	req.Header.Set("Authorization", authorization)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("scheduler execute HTTP %d: %s", resp.StatusCode, string(raw))
	}
	if err := validateImageResponse(raw); err != nil {
		return nil, http.StatusBadGateway, err
	}
	return raw, resp.StatusCode, nil
}

func validateImageResponse(raw []byte) error {
	var envelope struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("image backend returned invalid JSON: %w", err)
	}
	if len(envelope.Data) == 0 {
		return errors.New("image backend response missing data")
	}
	return nil
}

func imageModel(payload []byte) string {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(payload, &body)
	if strings.TrimSpace(body.Model) == "" {
		return "gpt-image-2"
	}
	return body.Model
}
func imageSummary(payload []byte) string {
	var body struct {
		Prompt string `json:"prompt"`
	}
	_ = json.Unmarshal(payload, &body)
	return body.Prompt
}
func isEditPayload(payload []byte) bool {
	var body struct {
		Edit bool `json:"_edit"`
	}
	_ = json.Unmarshal(payload, &body)
	return body.Edit
}

func (s *server) schedulerExecuteEdit(ctx context.Context, reservation, callID string, payload []byte, authorization string) (json.RawMessage, int, error) {
	var body struct {
		Prompt, Model, N, Size, Quality, ResponseFormat string
		Images                                          []editFile `json:"images"`
		Mask                                            []editFile `json:"mask"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, 0, err
	}
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	_ = writer.WriteField("prompt", body.Prompt)
	_ = writer.WriteField("model", body.Model)
	_ = writer.WriteField("n", body.N)
	_ = writer.WriteField("size", body.Size)
	_ = writer.WriteField("quality", body.Quality)
	_ = writer.WriteField("response_format", body.ResponseFormat)
	_ = writer.WriteField("_call_id", callID)
	for _, item := range append(body.Images, body.Mask...) {
		data, err := base64.StdEncoding.DecodeString(item.Data)
		if err != nil {
			return nil, 0, err
		}
		field := "image[]"
		if containsFile(body.Mask, item) {
			field = "mask"
		}
		part, err := writer.CreateFormFile(field, item.Name)
		if err != nil {
			return nil, 0, err
		}
		if _, err = part.Write(data); err != nil {
			return nil, 0, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.SchedulerURL+"/"+reservation+"/execute-edit", &buf)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("x-image-scheduler-key", s.cfg.SchedulerKey)
	req.Header.Set("X-Image-Gateway-Task", "1")
	req.Header.Set("Authorization", authorization)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("scheduler execute-edit HTTP %d: %s", resp.StatusCode, string(raw))
	}
	if err := validateImageResponse(raw); err != nil {
		return nil, http.StatusBadGateway, err
	}
	return raw, resp.StatusCode, nil
}
func containsFile(files []editFile, target editFile) bool {
	for _, item := range files {
		if item.Data == target.Data {
			return true
		}
	}
	return false
}
func (s *server) monitor(ctx context.Context, action string, body map[string]any) {
	if s.cfg.MonitorURL == "" {
		return
	}
	s.monitorTo(ctx, s.cfg.MonitorURL+"/"+action, body)
}
func (s *server) monitorTo(ctx context.Context, endpoint string, body map[string]any) {
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return
	}
	request.Header.Set("Content-Type", "application/json")
	if s.cfg.SchedulerKey != "" {
		request.Header.Set("x-image-scheduler-key", s.cfg.SchedulerKey)
	}
	response, err := s.client.Do(request)
	if err == nil && response != nil {
		response.Body.Close()
	}
}
func (s *server) monitorStart(item task) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.monitor(ctx, "start", map[string]any{"call_id": item.ID, "endpoint": taskEndpoint(item), "model": imageModel(item.Payload), "summary": imageSummary(item.Payload)})
}
func (s *server) monitorStage(callID, event string, data map[string]any) {
	data["call_id"] = callID
	data["event"] = event
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.monitor(ctx, "stage", data)
}
func (s *server) monitorFinish(item task, status, errorText string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.monitor(ctx, "finish", map[string]any{"call_id": item.ID, "endpoint": taskEndpoint(item), "model": imageModel(item.Payload), "status": status, "duration_ms": time.Since(item.CreatedAt).Milliseconds(), "error": errorText})
}

func taskEndpoint(item task) string {
	var p map[string]any
	if json.Unmarshal(item.Payload, &p) == nil {
		if v, ok := p["_edit"].(bool); ok && v {
			return "/v1/images/edits"
		}
	}
	return "/v1/images/generations"
}
func (s *server) logCall(item task, summary, status, errorText string, lease reservation) {
	if s.cfg.LogURL == "" {
		return
	}
	body := map[string]any{"summary": summary, "detail": map[string]any{"call_id": item.ID, "endpoint": taskEndpoint(item), "model": imageModel(item.Payload), "status": status, "duration_ms": time.Since(item.CreatedAt).Milliseconds(), "attempts": item.Attempts, "error": errorText, "gateway": "go-image-gateway", "account_email": lease.AccountEmail, "proxy_source": lease.ProxySource, "proxy_group_id": lease.ProxyGroupID, "proxy_node_id": lease.ProxyNodeID, "has_proxy": lease.ProxySource != "", "request_text": summary, "perf": map[string]any{"total_ms": time.Since(item.CreatedAt).Milliseconds()}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s.monitorTo(ctx, s.cfg.LogURL, body)
}

func (s *server) schedulerRelease(ctx context.Context, reservation string, failed bool) error {
	body := []byte(`{"failed":false}`)
	if failed {
		body = []byte(`{"failed":true}`)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.SchedulerURL+"/"+reservation+"/release", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-image-scheduler-key", s.cfg.SchedulerKey)
	resp, err := s.client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	return err
}

func (s *server) notify(item task) {
	if value, ok := s.waiters.Load(item.ID); ok {
		value.(chan task) <- item
	}
}

func truncate(err error) string {
	if err == nil {
		return "unknown backend error"
	}
	value := err.Error()
	if len(value) > 1000 {
		return value[:1000]
	}
	return value
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("chatgpt2api-image-gateway reservation-sync 1.0")
		return
	}
	cfg := loadConfig()
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, PoolSize: max(32, cfg.Workers+16), MinIdleConns: 8})
	s := &server{cfg: cfg, rdb: rdb, started: time.Now(), client: &http.Client{Transport: &http.Transport{MaxIdleConns: cfg.Workers * 2, MaxIdleConnsPerHost: cfg.Workers, MaxConnsPerHost: cfg.Workers, IdleConnTimeout: 90 * time.Second}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis unavailable: %v", err)
	}
	for i := 0; i < cfg.Workers; i++ {
		s.wg.Add(1)
		go s.worker(ctx, i)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/v1/images/generations", s.submit)
	mux.HandleFunc("/v1/images/edits", s.submitEdit)
	mux.HandleFunc("/v1/image-tasks/edits", s.submitEdit)
	mux.HandleFunc("/v1/image-tasks/", s.status)
	httpServer := &http.Server{Addr: cfg.ListenAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	log.Printf("image gateway listening=%s workers=%d queue_capacity=%d", cfg.ListenAddr, cfg.Workers, cfg.QueueCapacity)
	log.Fatal(httpServer.ListenAndServe())
}
