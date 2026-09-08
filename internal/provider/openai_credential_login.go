package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type CredentialLoginClient struct {
	URL  string
	Key  string
	HTTP *http.Client
}
type CredentialLoginError struct {
	Code      string
	Message   string
	Retryable bool
	Terminal  bool
}

func (e *CredentialLoginError) Error() string {
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	return "OpenAI credential login failed"
}
func (c *CredentialLoginClient) Available() bool {
	return c != nil && strings.TrimSpace(c.URL) != "" && strings.TrimSpace(c.Key) != ""
}
func (c *CredentialLoginClient) Login(ctx context.Context, account map[string]any, proxyURL string) (AccountRefreshResult, error) {
	if !c.Available() {
		return AccountRefreshResult{}, errors.New("账号密码登录服务未配置")
	}
	email := aiString(account, "email", "username", "account_email")
	password := aiString(account, "login_password", "password", "account_password")
	totp := aiString(account, "two_factor_secret", "totp_secret", "2fa_secret", "2fa")
	if email == "" || password == "" || totp == "" {
		return AccountRefreshResult{}, errors.New("账号缺少邮箱、登录密码或 2FA 密钥")
	}
	raw, err := json.Marshal(map[string]string{"email": email, "password": password, "totp_secret": totp, "proxy": strings.TrimSpace(proxyURL)})
	if err != nil {
		return AccountRefreshResult{}, errors.New("encode credential login request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/login-token", bytes.NewReader(raw))
	if err != nil {
		return AccountRefreshResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Internal-Key", c.Key)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return AccountRefreshResult{}, fmt.Errorf("OpenAI 密码登录服务请求失败: %w", err)
	}
	defer response.Body.Close()
	var payload map[string]any
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload)
	if response.StatusCode != http.StatusOK {
		message := aiString(payload, "message")
		if message == "" {
			message = fmt.Sprintf("OpenAI 密码登录失败 (HTTP %d)", response.StatusCode)
		}
		retryable, _ := payload["retryable"].(bool)
		terminal, _ := payload["terminal"].(bool)
		return AccountRefreshResult{}, &CredentialLoginError{Code: aiString(payload, "code"), Message: message, Retryable: retryable, Terminal: terminal}
	}
	accessToken := aiString(payload, "access_token")
	if decodeErr != nil || accessToken == "" {
		return AccountRefreshResult{}, errors.New("OpenAI 密码登录服务未返回 access_token")
	}
	return AccountRefreshResult{AccessToken: accessToken, RefreshToken: aiString(payload, "refresh_token"), IDToken: aiString(payload, "id_token")}, nil
}
