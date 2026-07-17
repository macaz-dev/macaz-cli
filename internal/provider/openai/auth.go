package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/macaz-dev/macaz-cli/internal/secrets"
)

const (
	accountClientID          = "app_EMoamEEZ73f0CkXaXp7hrann"
	accountIssuer            = "https://auth.openai.com"
	accountDeviceURL         = accountIssuer + "/codex/device"
	accountDeviceUserCode    = accountIssuer + "/api/accounts/deviceauth/usercode"
	accountDeviceToken       = accountIssuer + "/api/accounts/deviceauth/token"
	accountTokenEndpoint     = accountIssuer + "/oauth/token"
	accountResponsesEndpoint = "https://chatgpt.com/backend-api/codex/responses"
	accountModelsEndpoint    = "https://chatgpt.com/backend-api/codex/models"
	accountDeviceCallback    = accountIssuer + "/deviceauth/callback"
	accountPollingSafety     = 3 * time.Second
	accountRefreshSkew       = 60 * time.Second
	accountCredentialType    = "openai_account_oauth"
	accountCredentialMethod  = "chatgpt_headless"
	accountClientVersion     = "0.144.5"
)

type DeviceAuthorization struct {
	URL      string
	UserCode string
	Interval time.Duration
}

type accountAuth struct {
	httpClient *http.Client
	mu         sync.Mutex
}

type accountCredentials struct {
	Type      string `json:"type"`
	Method    string `json:"method"`
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	ExpiresAt int64  `json:"expires_at"`
	AccountID string `json:"account_id,omitempty"`
}

type accountTokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

type pendingAuthorization struct {
	DeviceAuthID string
	UserCode     string
	Interval     time.Duration
}

func newAccountAuth(client *http.Client) *accountAuth {
	return &accountAuth{httpClient: client}
}

func AuthorizeSubscription(ctx context.Context, client *http.Client, ready func(DeviceAuthorization) error) error {
	auth := newAccountAuth(client)
	pending, err := auth.start(ctx)
	if err != nil {
		return err
	}
	if err := ready(DeviceAuthorization{
		URL:      accountDeviceURL,
		UserCode: pending.UserCode,
		Interval: pending.Interval,
	}); err != nil {
		return err
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		credential, waiting, err := auth.poll(ctx, pending)
		if err != nil {
			return err
		}
		if !waiting {
			return saveAccountCredential(credential)
		}
		timer.Reset(pending.Interval)
	}
}

func (a *accountAuth) start(ctx context.Context) (pendingAuthorization, error) {
	body, _ := json.Marshal(map[string]string{"client_id": accountClientID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, accountDeviceUserCode, bytes.NewReader(body))
	if err != nil {
		return pendingAuthorization{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "macaz/dev")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return pendingAuthorization{}, fmt.Errorf("start OpenAI device authorization: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return pendingAuthorization{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return pendingAuthorization{}, fmt.Errorf("OpenAI device authorization failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var payload struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		Interval     string `json:"interval"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return pendingAuthorization{}, fmt.Errorf("decode OpenAI device authorization: %w", err)
	}
	if strings.TrimSpace(payload.DeviceAuthID) == "" || strings.TrimSpace(payload.UserCode) == "" {
		return pendingAuthorization{}, errors.New("OpenAI device authorization response is missing device id or user code")
	}
	seconds, _ := strconv.Atoi(strings.TrimSpace(payload.Interval))
	if seconds < 1 {
		seconds = 5
	}
	return pendingAuthorization{
		DeviceAuthID: payload.DeviceAuthID,
		UserCode:     payload.UserCode,
		Interval:     time.Duration(seconds)*time.Second + accountPollingSafety,
	}, nil
}

func (a *accountAuth) poll(ctx context.Context, pending pendingAuthorization) (accountCredentials, bool, error) {
	body, _ := json.Marshal(map[string]string{
		"device_auth_id": pending.DeviceAuthID,
		"user_code":      pending.UserCode,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, accountDeviceToken, bytes.NewReader(body))
	if err != nil {
		return accountCredentials{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "macaz/dev")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return accountCredentials{}, false, fmt.Errorf("poll OpenAI device authorization: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return accountCredentials{}, false, err
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return accountCredentials{}, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return accountCredentials{}, false, fmt.Errorf("OpenAI device authorization polling failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var device struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	if err := json.Unmarshal(raw, &device); err != nil {
		return accountCredentials{}, false, fmt.Errorf("decode OpenAI device token: %w", err)
	}
	if strings.TrimSpace(device.AuthorizationCode) == "" || strings.TrimSpace(device.CodeVerifier) == "" {
		return accountCredentials{}, false, errors.New("OpenAI device token response is missing authorization code or verifier")
	}
	tokens, err := a.exchange(ctx, device.AuthorizationCode, device.CodeVerifier)
	if err != nil {
		return accountCredentials{}, false, err
	}
	return credentialFromTokens(tokens, ""), false, nil
}

func (a *accountAuth) credentials(ctx context.Context) (accountCredentials, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw, err := secrets.Get(secrets.OpenAIAccount, "")
	if err != nil {
		return accountCredentials{}, fmt.Errorf("OpenAI Subscription is not connected: %w", err)
	}
	var credential accountCredentials
	if err := json.Unmarshal([]byte(raw), &credential); err != nil {
		return accountCredentials{}, fmt.Errorf("decode OpenAI Subscription credential: %w", err)
	}
	if credential.Type != accountCredentialType {
		return accountCredentials{}, errors.New("stored credential is not an OpenAI Subscription credential")
	}
	if credential.Access != "" && time.UnixMilli(credential.ExpiresAt).After(time.Now().Add(accountRefreshSkew)) {
		return credential, nil
	}
	if credential.Refresh == "" {
		return accountCredentials{}, errors.New("OpenAI Subscription refresh token is missing; reconnect with `macaz provider set`")
	}
	tokens, err := a.refresh(ctx, credential.Refresh)
	if err != nil {
		return accountCredentials{}, err
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = credential.Refresh
	}
	next := credentialFromTokens(tokens, credential.AccountID)
	if err := saveAccountCredential(next); err != nil {
		return accountCredentials{}, err
	}
	return next, nil
}

func (a *accountAuth) exchange(ctx context.Context, code, verifier string) (accountTokenResponse, error) {
	return a.requestToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {accountDeviceCallback},
		"client_id":     {accountClientID},
		"code_verifier": {verifier},
	})
}

func (a *accountAuth) refresh(ctx context.Context, refreshToken string) (accountTokenResponse, error) {
	return a.requestToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {accountClientID},
	})
}

func (a *accountAuth) requestToken(ctx context.Context, form url.Values) (accountTokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, accountTokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return accountTokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "macaz/dev")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return accountTokenResponse{}, fmt.Errorf("request OpenAI Subscription token: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return accountTokenResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return accountTokenResponse{}, fmt.Errorf("OpenAI Subscription token exchange failed: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var token accountTokenResponse
	if err := json.Unmarshal(raw, &token); err != nil {
		return accountTokenResponse{}, fmt.Errorf("decode OpenAI Subscription token: %w", err)
	}
	if token.AccessToken == "" {
		return accountTokenResponse{}, errors.New("OpenAI Subscription token response is missing access token")
	}
	return token, nil
}

func saveAccountCredential(credential accountCredentials) error {
	raw, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	return secrets.Set(secrets.OpenAIAccount, string(raw))
}

func credentialFromTokens(tokens accountTokenResponse, fallbackAccountID string) accountCredentials {
	expiresIn := tokens.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	accountID := extractAccountID(tokens.IDToken)
	if accountID == "" {
		accountID = extractAccountID(tokens.AccessToken)
	}
	if accountID == "" {
		accountID = fallbackAccountID
	}
	return accountCredentials{
		Type:      accountCredentialType,
		Method:    accountCredentialMethod,
		Access:    tokens.AccessToken,
		Refresh:   tokens.RefreshToken,
		ExpiresAt: time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli(),
		AccountID: accountID,
	}
}

func extractAccountID(token string) string {
	payload := jwtPayload(token)
	for _, key := range []string{"chatgpt_account_id", "account_id"} {
		if value, ok := payload[key].(string); ok && value != "" {
			return value
		}
	}
	if auth, ok := payload["https://api.openai.com/auth"].(map[string]any); ok {
		if value, ok := auth["chatgpt_account_id"].(string); ok {
			return value
		}
	}
	if organizations, ok := payload["organizations"].([]any); ok && len(organizations) > 0 {
		if organization, ok := organizations[0].(map[string]any); ok {
			if value, ok := organization["id"].(string); ok {
				return value
			}
		}
	}
	return ""
}

func jwtPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var payload map[string]any
	if decoder.Decode(&payload) != nil {
		return nil
	}
	return payload
}
