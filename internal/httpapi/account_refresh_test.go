package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/config"
)

func TestAccountRefreshHTTPFlowPersistsRemoteFieldsAndRedactsSecrets(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-1" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "account@example.test", "id": "user-1"})
		case "/backend-api/conversation/init":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_model_slug": "gpt-5", "limits_progress": []any{map[string]any{"feature_name": "image_gen", "remaining": 9}}})
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "plus"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	root := t.TempDir()
	cfg := config.Config{
		RootDir:        root,
		DataDir:        filepath.Join(root, "data"),
		AccountsPath:   filepath.Join(root, "data", "accounts.json"),
		AuthKeysPath:   filepath.Join(root, "data", "auth_keys.json"),
		ConfigPath:     filepath.Join(root, "config.json"),
		StaticDir:      filepath.Join(root, "web_dist"),
		APIKey:         "api-secret",
		AdminKey:       "admin-secret",
		OpenAIBaseURL:  upstream.URL,
		OpenAIOAuthURL: upstream.URL + "/oauth/token",
	}
	server := New(cfg)
	_, _, _, err := server.store.AddAccounts(nil, []map[string]any{{
		"access_token": "access-1", "refresh_token": "refresh-secret", "id_token": "id-secret", "source_type": "chatgpt_web",
		"status": "异常", "invalid_count": 2, "status_reason_code": "auth_invalid", "last_error_kind": "auth_invalid",
		"last_error_status": 403, "last_refresh_error": "stale error", "cooldown_until": time.Now().Add(time.Hour).Format(time.RFC3339),
	}})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	startRequest := httptest.NewRequest(http.MethodPost, "/api/accounts/refresh", strings.NewReader(`{"access_tokens":["access-1"]}`))
	startRequest.Header.Set("Authorization", "Bearer admin-secret")
	startRequest.Header.Set("Content-Type", "application/json")
	startResponse := httptest.NewRecorder()
	handler.ServeHTTP(startResponse, startRequest)
	if startResponse.Code != http.StatusOK {
		t.Fatalf("start returned %d: %s", startResponse.Code, startResponse.Body.String())
	}
	var start struct {
		ProgressID string `json:"progress_id"`
	}
	if err := json.Unmarshal(startResponse.Body.Bytes(), &start); err != nil || start.ProgressID == "" {
		t.Fatalf("invalid start response: %s", startResponse.Body.String())
	}

	var progress map[string]any
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		request := httptest.NewRequest(http.MethodGet, "/api/accounts/refresh/progress/"+start.ProgressID, nil)
		request.Header.Set("Authorization", "Bearer admin-secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("progress returned %d: %s", response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), &progress); err != nil {
			t.Fatal(err)
		}
		if done, _ := progress["done"].(bool); done {
			break
		}
	}
	if done, _ := progress["done"].(bool); !done {
		t.Fatalf("refresh did not finish: %#v", progress)
	}
	if processed, _ := progress["processed"].(float64); processed != 1 {
		t.Fatalf("unexpected progress: %#v", progress)
	}
	result, _ := progress["result"].(map[string]any)
	if refreshed, _ := result["refreshed"].(float64); refreshed != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	encoded, _ := json.Marshal(progress)
	if strings.Contains(string(encoded), "refresh-secret") || strings.Contains(string(encoded), "id-secret") {
		t.Fatalf("progress leaked account secrets: %s", encoded)
	}
	account, err := server.store.AccountList()
	if err != nil || len(account) != 1 {
		t.Fatalf("stored account missing: %#v %v", account, err)
	}
	if account[0]["email"] != "account@example.test" || account[0]["quota"] != float64(9) || account[0]["status"] != "正常" {
		t.Fatalf("remote fields were not persisted: %#v", account[0])
	}
	if intValue(account[0]["invalid_count"]) != 0 || stringValue(account[0]["last_error_kind"]) != "" || stringValue(account[0]["cooldown_until"]) != "" {
		t.Fatalf("successful refresh kept stale abnormal markers: %#v", account[0])
	}
}

func TestAccountRefreshAcceptsPublicAccountRef(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-public-ref" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "public@example.test", "id": "user-public"})
		case "/backend-api/conversation/init":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_model_slug": "gpt-5", "limits_progress": []any{map[string]any{"feature_name": "image_gen", "remaining": 12}}})
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "plus"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	server := newAccountRefreshTestServer(t, upstream.URL)
	_, _, _, err := server.store.AddAccounts(nil, []map[string]any{{
		"access_token": "access-public-ref", "email": "public@example.test", "user_id": "user-public-ref",
		"login_password": "password-public-ref", "two_factor_secret": "JBSWY3DPEHPK3PXP", "source_type": "chatgpt_web",
	}})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	listRequest := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	listRequest.Header.Set("Authorization", "Bearer admin-secret")
	listResponse := httptest.NewRecorder()
	handler.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list returned %d: %s", listResponse.Code, listResponse.Body.String())
	}
	var listPayload struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &listPayload); err != nil || len(listPayload.Items) != 1 {
		t.Fatalf("invalid list response: %s", listResponse.Body.String())
	}
	if stringValue(listPayload.Items[0]["access_token"]) != "" {
		t.Fatalf("list leaked access token: %#v", listPayload.Items[0])
	}
	if stringValue(listPayload.Items[0]["login_password"]) != "" || stringValue(listPayload.Items[0]["two_factor_secret"]) != "" {
		t.Fatalf("list leaked account credentials: %#v", listPayload.Items[0])
	}
	if hasToken, _ := listPayload.Items[0]["has_access_token"].(bool); !hasToken {
		t.Fatalf("list did not expose access token availability: %#v", listPayload.Items[0])
	}
	if preview := stringValue(listPayload.Items[0]["token_preview"]); preview == "" || strings.Contains(preview, "access-public-ref") {
		t.Fatalf("list did not expose a safe token preview: %#v", listPayload.Items[0])
	}
	ref := stringValue(listPayload.Items[0]["account_ref"])
	if ref == "" {
		t.Fatalf("list did not expose account_ref: %#v", listPayload.Items[0])
	}

	tokenRequest := httptest.NewRequest(http.MethodPost, "/api/accounts/token", strings.NewReader(`{"account_ref":"`+ref+`"}`))
	tokenRequest.Header.Set("Authorization", "Bearer admin-secret")
	tokenResponse := httptest.NewRecorder()
	handler.ServeHTTP(tokenResponse, tokenRequest)
	if tokenResponse.Code != http.StatusOK {
		t.Fatalf("token endpoint returned %d: %s", tokenResponse.Code, tokenResponse.Body.String())
	}
	var tokenPayload map[string]any
	if err := json.Unmarshal(tokenResponse.Body.Bytes(), &tokenPayload); err != nil {
		t.Fatalf("invalid token response: %s", tokenResponse.Body.String())
	}
	if got := stringValue(tokenPayload["access_token"]); got != "access-public-ref" {
		t.Fatalf("token endpoint did not return the full token: %#v", tokenPayload)
	}
	if stringValue(tokenPayload["user_id"]) != "user-public-ref" || stringValue(tokenPayload["login_password"]) != "password-public-ref" || stringValue(tokenPayload["two_factor_secret"]) != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("token endpoint did not return editable credentials: %#v", tokenPayload)
	}

	progress := runAccountRefreshForTest(t, handler, []string{ref})
	result, _ := progress["result"].(map[string]any)
	if refreshed, _ := result["refreshed"].(float64); refreshed != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestAccessTokenRefreshUsesCredentialLoginAndRotatesOriginalAccount(t *testing.T) {
	loginCalls := 0
	loginService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loginCalls++
		if r.Header.Get("X-Internal-Key") != "login-service-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["email"] != "login@example.test" || body["password"] != "password-value" || body["totp_secret"] != "JBSWY3DPEHPK3PXP" {
			t.Fatal("unexpected login request")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "access-new", "refresh_token": "refresh-new", "id_token": "id-new"})
	}))
	defer loginService.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-new" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/backend-api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "login@example.test", "id": "user-new"})
		case "/backend-api/conversation/init":
			_ = json.NewEncoder(w).Encode(map[string]any{"limits_progress": []any{map[string]any{"feature_name": "image_gen", "remaining": 8}}})
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "plus"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	server := newAccountRefreshTestServer(t, upstream.URL)
	server.credentialLogin.URL = loginService.URL
	server.credentialLogin.Key = "login-service-secret"
	_, _, _, err := server.store.AddAccounts(nil, []map[string]any{{"access_token": "access-old", "email": "login@example.test", "login_password": "password-value", "two_factor_secret": "JBSWY3DPEHPK3PXP", "group_id": "stable", "source_type": "chatgpt_web"}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/refresh-at", strings.NewReader("{\"access_tokens\":[\"access-old\"]}"))
	req.Header.Set("Authorization", "Bearer admin-secret")
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("refresh-at returned %d", response.Code)
	}
	if loginCalls != 1 {
		t.Fatalf("credential login calls = %d", loginCalls)
	}
	for _, secret := range []string{"password-value", "JBSWY3DPEHPK3PXP", "access-new", "refresh-new"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("refresh response leaked credentials")
		}
	}
	items, err := server.store.AccountList()
	if err != nil || len(items) != 1 {
		t.Fatalf("original account was not preserved")
	}
	account := items[0]
	if stringValue(account["access_token"]) != "access-new" || stringValue(account["refresh_token"]) != "refresh-new" || stringValue(account["id_token"]) != "id-new" {
		t.Fatal("tokens not rotated")
	}
	if stringValue(account["login_password"]) != "password-value" || stringValue(account["two_factor_secret"]) != "JBSWY3DPEHPK3PXP" || stringValue(account["group_id"]) != "stable" {
		t.Fatal("original fields not preserved")
	}
}

func TestAccessTokenRefreshAcceptsATOnlyAndPreservesExistingRT(t *testing.T) {
	loginService := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Key") != "login-service-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "access-at-only"})
	}))
	defer loginService.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-at-only" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"email": "at-only@example.test", "id": "user-at-only"})
		case "/backend-api/conversation/init":
			_ = json.NewEncoder(w).Encode(map[string]any{"limits_progress": []any{map[string]any{"feature_name": "image_gen", "remaining": 6}}})
		case "/backend-api/accounts/check/v4-2023-04-27":
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"default": map[string]any{"account": map[string]any{"plan_type": "plus"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	server := newAccountRefreshTestServer(t, upstream.URL)
	server.credentialLogin.URL = loginService.URL
	server.credentialLogin.Key = "login-service-secret"
	_, _, _, err := server.store.AddAccounts(nil, []map[string]any{{
		"access_token": "access-at-old", "refresh_token": "refresh-keep", "email": "at-only@example.test",
		"login_password": "password-value", "two_factor_secret": "JBSWY3DPEHPK3PXP", "source_type": "chatgpt_web",
	}})
	if err != nil {
		t.Fatal(err)
	}
	progress := runAccountRefreshForTest(t, server.Handler(), []string{"access-at-old"})
	result, _ := progress["result"].(map[string]any)
	if refreshed, _ := result["refreshed"].(float64); refreshed != 1 {
		t.Fatalf("AT-only refresh failed: %#v", progress)
	}
	items, err := server.store.AccountList()
	if err != nil || len(items) != 1 {
		t.Fatalf("account count changed: %#v %v", items, err)
	}
	if stringValue(items[0]["access_token"]) != "access-at-only" || stringValue(items[0]["refresh_token"]) != "refresh-keep" {
		t.Fatalf("AT-only rotation did not preserve RT: %#v", items[0])
	}
}

func TestAccountRefreshMissingRefDoesNotMutateAccount(t *testing.T) {
	server := newAccountRefreshTestServer(t, "http://127.0.0.1.invalid")
	_, _, _, err := server.store.AddAccounts(nil, []map[string]any{{
		"access_token": "access-keep-normal", "email": "keep@example.test", "status": "正常", "quota": 7,
	}})
	if err != nil {
		t.Fatal(err)
	}
	progress := runAccountRefreshForTest(t, server.Handler(), []string{"missing@example.test"})
	result, _ := progress["result"].(map[string]any)
	errorsValue, _ := result["errors"].([]any)
	if len(errorsValue) != 1 {
		t.Fatalf("expected one lookup error: %#v", progress)
	}
	items, err := server.store.AccountList()
	if err != nil || len(items) != 1 {
		t.Fatalf("stored account missing: %#v %v", items, err)
	}
	if items[0]["status"] != "正常" || stringValue(items[0]["last_refresh_error"]) != "" {
		t.Fatalf("missing account ref mutated stored account: %#v", items[0])
	}
}

func TestAccountRefreshTransientErrorKeepsAccountNormal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary upstream failure", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	server := newAccountRefreshTestServer(t, upstream.URL)
	_, _, _, err := server.store.AddAccounts(nil, []map[string]any{{
		"access_token": "access-transient", "email": "transient@example.test", "status": "正常", "quota": 7,
	}})
	if err != nil {
		t.Fatal(err)
	}
	progress := runAccountRefreshForTest(t, server.Handler(), []string{"transient@example.test"})
	result, _ := progress["result"].(map[string]any)
	errorsValue, _ := result["errors"].([]any)
	if len(errorsValue) != 1 {
		t.Fatalf("expected one transient error: %#v", progress)
	}
	items, err := server.store.AccountList()
	if err != nil || len(items) != 1 {
		t.Fatalf("stored account missing: %#v %v", items, err)
	}
	if items[0]["status"] != "正常" || accountStatusCategory(items[0]) != "normal" {
		t.Fatalf("transient refresh changed account category: %#v", items[0])
	}
	if stringValue(items[0]["last_refresh_warning"]) == "" || stringValue(items[0]["last_refresh_error"]) != "" {
		t.Fatalf("transient refresh error fields were not classified safely: %#v", items[0])
	}
}

func newAccountRefreshTestServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{
		RootDir:        root,
		DataDir:        filepath.Join(root, "data"),
		AccountsPath:   filepath.Join(root, "data", "accounts.json"),
		AuthKeysPath:   filepath.Join(root, "data", "auth_keys.json"),
		ConfigPath:     filepath.Join(root, "config.json"),
		StaticDir:      filepath.Join(root, "web_dist"),
		APIKey:         "api-secret",
		AdminKey:       "admin-secret",
		OpenAIBaseURL:  upstreamURL,
		OpenAIOAuthURL: upstreamURL + "/oauth/token",
	}
	return New(cfg)
}

func runAccountRefreshForTest(t *testing.T, handler http.Handler, refs []string) map[string]any {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"access_tokens": refs})
	if err != nil {
		t.Fatal(err)
	}
	startRequest := httptest.NewRequest(http.MethodPost, "/api/accounts/refresh", strings.NewReader(string(raw)))
	startRequest.Header.Set("Authorization", "Bearer admin-secret")
	startRequest.Header.Set("Content-Type", "application/json")
	startResponse := httptest.NewRecorder()
	handler.ServeHTTP(startResponse, startRequest)
	if startResponse.Code != http.StatusOK {
		t.Fatalf("start returned %d: %s", startResponse.Code, startResponse.Body.String())
	}
	var start struct {
		ProgressID string `json:"progress_id"`
	}
	if err := json.Unmarshal(startResponse.Body.Bytes(), &start); err != nil || start.ProgressID == "" {
		t.Fatalf("invalid start response: %s", startResponse.Body.String())
	}

	var progress map[string]any
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		request := httptest.NewRequest(http.MethodGet, "/api/accounts/refresh/progress/"+start.ProgressID, nil)
		request.Header.Set("Authorization", "Bearer admin-secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("progress returned %d: %s", response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), &progress); err != nil {
			t.Fatal(err)
		}
		if done, _ := progress["done"].(bool); done {
			return progress
		}
	}
	t.Fatalf("refresh did not finish: %#v", progress)
	return nil
}
