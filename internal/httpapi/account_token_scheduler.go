package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/provider"
)

// accountTokenRefreshScheduler keeps OAuth access tokens ahead of expiry.
// The setting is intentionally separate from the slower survival probe: a
// probe verifies account metadata, while this loop rotates credentials.
func (s *Server) accountTokenRefreshScheduler() {
	// Start soon after boot so an already-near-expiry token is repaired without
	// waiting for the first full polling interval.
	timer := time.NewTimer(30 * time.Second)
	<-timer.C
	for {
		s.refreshExpiringAccountTokens()
		timer.Reset(60 * time.Second)
		<-timer.C
	}
}

func (s *Server) refreshExpiringAccountTokens() {
	s.tokenRefreshMu.Lock()
	if s.tokenRefreshRunning {
		s.tokenRefreshMu.Unlock()
		return
	}
	s.tokenRefreshRunning = true
	s.tokenRefreshMu.Unlock()
	defer func() {
		s.tokenRefreshMu.Lock()
		s.tokenRefreshRunning = false
		s.tokenRefreshMu.Unlock()
	}()

	items, err := s.store.AccountList()
	if err != nil {
		log.Printf("automatic account token refresh: list accounts: %v", err)
		return
	}
	minutes := 5
	if value, configErr := s.store.Config(); configErr == nil {
		if parsed, parseErr := strconv.Atoi(strings.TrimSpace(stringValue(value["refresh_account_interval_minute"]))); parseErr == nil && parsed > 0 {
			minutes = parsed
		}
	}
	if minutes < 1 {
		minutes = 1
	}
	if minutes > 60 {
		minutes = 60
	}
	window := time.Duration(minutes+2) * time.Minute
	concurrency := accountRefreshConcurrency()
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	credentialScheduled := 0
	for _, account := range items {
		account := account
		token := accountToken(account)
		refreshToken := strings.TrimSpace(stringValue(account["refresh_token"]))
		if token == "" {
			continue
		}
		needsRefresh := provider.TokenExpiresWithin(token, window)
		if refreshToken == "" {
			needsRefresh = accountNeedsCredentialLogin(account, token)
			if needsRefresh {
				if credentialScheduled >= 6 {
					continue
				}
				credentialScheduled++
			}
		}
		if !needsRefresh || tokenRefreshCoolingDown(account) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.refreshOneExpiringToken(account)
		}()
	}
	wg.Wait()
}

func (s *Server) refreshOneExpiringToken(account map[string]any) {
	oldToken := accountToken(account)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result, err := s.refreshAccountAccessToken(ctx, account)
	if err != nil {
		updates := accountRefreshFailureUpdates(err)
		updates["token_refresh_next_retry_at"] = time.Now().UTC().Add(15 * time.Minute).Format(time.RFC3339)
		if strings.TrimSpace(stringValue(account["refresh_token"])) == "" {
			retryAfter := 15 * time.Minute
			var loginErr *provider.CredentialLoginError
			if errors.As(err, &loginErr) && loginErr.Terminal {
				retryAfter = 6 * time.Hour
			}
			updates["credential_login_next_retry_at"] = time.Now().UTC().Add(retryAfter).Format(time.RFC3339)
			if errors.As(err, &loginErr) {
				updates["credential_login_error_code"] = loginErr.Code
			}
		}
		if len(updates) > 0 {
			if _, _, updateErr := s.store.UpdateAccount(oldToken, updates); updateErr != nil {
				log.Printf("automatic account token refresh: save failure for %s: %v", tokenPreview(oldToken), updateErr)
			}
		}
		log.Printf("automatic account token refresh failed for %s: %v", tokenPreview(oldToken), safeRefreshError(err))
		return
	}
	if _, _, err = s.store.RotateAccountTokens(oldToken, result.AccessToken, result.RefreshToken, result.IDToken, result.Fields); err != nil {
		log.Printf("automatic account token refresh: rotate %s: %v", tokenPreview(oldToken), err)
	}
}

func accountNeedsCredentialLogin(account map[string]any, token string) bool {
	email, password, totp := accountLoginCredentials(account)
	if email == "" || password == "" || totp == "" {
		return false
	}
	if provider.TokenExpiresWithin(token, 0) {
		return true
	}
	reason := strings.ToLower(strings.TrimSpace(stringValue(account["status_reason_code"])))
	return intValue(account["last_error_status"]) == http.StatusUnauthorized ||
		reason == "account_invalid" || reason == "account_relogin_required" ||
		strings.EqualFold(stringValue(account["last_error_kind"]), "auth_invalid")
}

func tokenRefreshCoolingDown(account map[string]any) bool {
	for _, key := range []string{"token_refresh_next_retry_at", "credential_login_next_retry_at"} {
		raw := strings.TrimSpace(stringValue(account[key]))
		if raw == "" {
			continue
		}
		value, err := time.Parse(time.RFC3339, raw)
		if err == nil && time.Now().Before(value) {
			return true
		}
	}
	return false
}
