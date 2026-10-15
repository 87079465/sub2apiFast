// Package logicc implements the Logicc web API protocol documented by
// https://github.com/JamesZhaoY/logicc2api (MIT, JamesZhaoY).
package logicc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/proxyutil"
)

type Credentials struct{ Token, ClientCookie, SessionID string }
type Client struct {
	HTTP              *http.Client
	BaseURL, ClerkURL string
	Credentials       Credentials
	Save              func(Credentials) error
}
type HTTPError struct {
	Status int
	Header http.Header
}

func (e *HTTPError) Error() string { return fmt.Sprintf("Logicc upstream returned HTTP %d", e.Status) }

func NewClient(creds Credentials, proxy string) (*Client, error) {
	creds.Token = strings.TrimSpace(creds.Token)
	creds.ClientCookie = strings.TrimSpace(creds.ClientCookie)
	creds.SessionID = strings.TrimSpace(creds.SessionID)
	if sid, _ := Claims(creds.Token); sid != "" {
		if creds.SessionID != "" && creds.SessionID != sid {
			return nil, fmt.Errorf("Logicc Session ID does not match the Session JWT")
		}
		creds.SessionID = sid
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, err
		}
		if err = proxyutil.ConfigureTransportProxy(transport, u); err != nil {
			return nil, err
		}
	}
	return &Client{Credentials: creds, BaseURL: "https://app.logicc.com/api", ClerkURL: "https://clerk.logicc.com", HTTP: &http.Client{Transport: transport, Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func Claims(token string) (string, int64) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", 0
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", 0
	}
	var claims struct {
		SID string `json:"sid"`
		Exp int64  `json:"exp"`
	}
	if json.Unmarshal(b, &claims) != nil {
		return "", 0
	}
	return claims.SID, claims.Exp
}

func (c *Client) Refresh(ctx context.Context) error {
	creds := c.Credentials
	if creds.SessionID == "" {
		creds.SessionID, _ = Claims(creds.Token)
	}
	if !strings.HasPrefix(creds.SessionID, "sess_") || strings.ContainsAny(creds.SessionID, "/?#\r\n") || creds.ClientCookie == "" || strings.ContainsAny(creds.ClientCookie, ";\r\n") {
		return fmt.Errorf("Logicc requires a valid __client cookie and Session ID to refresh its session")
	}
	refreshCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(refreshCtx, "POST", c.ClerkURL+"/v1/client/sessions/"+url.PathEscape(creds.SessionID)+"/tokens", nil)
	if err != nil {
		return err
	}
	req.AddCookie(&http.Cookie{Name: "__client", Value: creds.ClientCookie})
	req.Header.Set("Origin", "https://app.logicc.com")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("Logicc session refresh transport failed")
	}
	defer resp.Body.Close()
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "__client" {
			creds.ClientCookie = cookie.Value
		}
	}
	// Persist rotated cookies even when Clerk rejects the session token request.
	if creds.ClientCookie != c.Credentials.ClientCookie && c.Save != nil {
		if err := c.Save(creds); err != nil {
			return err
		}
	}
	c.Credentials = creds
	if resp.StatusCode != http.StatusOK {
		return &HTTPError{Status: resp.StatusCode, Header: resp.Header.Clone()}
	}
	var result struct {
		JWT string `json:"jwt"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return fmt.Errorf("invalid Logicc session response")
	}
	sid, expiry := Claims(result.JWT)
	if sid != creds.SessionID || expiry <= time.Now().Add(15*time.Second).Unix() {
		return fmt.Errorf("Logicc returned an expired or mismatched session token")
	}
	creds.Token = result.JWT
	if c.Save != nil {
		if err := c.Save(creds); err != nil {
			return err
		}
	}
	c.Credentials = creds
	return nil
}

func (c *Client) Request(ctx context.Context, method, path string, payload any, stream bool) (*http.Response, error) {
	_, exp := Claims(c.Credentials.Token)
	if exp <= time.Now().Add(15*time.Second).Unix() {
		if err := c.Refresh(ctx); err != nil {
			return nil, err
		}
	}
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/"+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Credentials.Token)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if stream {
			req.Header.Set("Accept", "text/event-stream")
			req.Header.Set("Stream-Response-Protocol", "ai-sdk-v6")
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("Logicc request transport failed")
		}
		if resp.StatusCode == 401 && attempt == 0 {
			resp.Body.Close()
			if err := c.Refresh(ctx); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, &HTTPError{Status: resp.StatusCode, Header: resp.Header.Clone()}
		}
		return resp, nil
	}
	return nil, fmt.Errorf("Logicc authentication failed")
}

type Model struct {
	ID        string `json:"id"`
	ModelID   string `json:"modelId"`
	Name      string `json:"name"`
	IsDefault bool   `json:"isDefault"`
}

func (c *Client) Models(ctx context.Context) ([]Model, error) {
	resp, err := c.Request(ctx, "GET", "models?language=en", nil, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var models []Model
	if json.Unmarshal(b, &models) != nil {
		var wrapper struct {
			Models []Model `json:"models"`
			Data   []Model `json:"data"`
		}
		if err := json.Unmarshal(b, &wrapper); err != nil {
			return nil, fmt.Errorf("invalid Logicc model catalog")
		}
		models = wrapper.Models
		if len(models) == 0 {
			models = wrapper.Data
		}
	}
	valid := make([]Model, 0, len(models))
	for _, m := range models {
		if m.ID == "" {
			m.ID = m.ModelID
		}
		if m.ID != "" {
			if m.Name == "" {
				m.Name = m.ID
			}
			valid = append(valid, m)
		}
	}
	if len(valid) == 0 {
		return nil, fmt.Errorf("Logicc returned no models")
	}
	return valid, nil
}

func (c *Client) Infer(ctx context.Context, model, prompt, assistant, effort string) (*http.Response, string, error) {
	if model == "default" {
		models, err := c.Models(ctx)
		if err != nil {
			return nil, "", err
		}
		model = models[0].ID
		for _, m := range models {
			if m.IsDefault {
				model = m.ID
				break
			}
		}
	}
	create := map[string]string{}
	if assistant != "" {
		create["assistantId"] = assistant
	}
	resp, err := c.Request(ctx, "POST", "chats", create, false)
	if err != nil {
		return nil, "", err
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if err != nil {
		return nil, "", err
	}
	var id string
	if json.Unmarshal(b, &id) != nil {
		var chat struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(b, &chat) != nil {
			return nil, "", fmt.Errorf("invalid Logicc chat response")
		}
		id = chat.ID
	}
	if id == "" || strings.ContainsAny(id, "/?#") {
		return nil, "", fmt.Errorf("Logicc returned an invalid chat ID")
	}
	payload := map[string]any{"modelId": model, "prompt": prompt, "isVoicePrompt": false}
	if effort != "" {
		payload["reasoningEffort"] = effort
	}
	resp, err = c.Request(ctx, "POST", "chats/"+url.PathEscape(id), payload, true)
	return resp, model, err
}
