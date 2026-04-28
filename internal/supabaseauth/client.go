package supabaseauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

type AuthRequest struct {
	Email    string         `json:"email"`
	Password string         `json:"password"`
	Metadata map[string]any `json:"data,omitempty"`
}

type VerifyOTPRequest struct {
	Email string `json:"email,omitempty"`
	Token string `json:"token"`
	Type  string `json:"type"`
}

type AuthResponse struct {
	AccessToken  string         `json:"access_token,omitempty"`
	RefreshToken string         `json:"refresh_token,omitempty"`
	TokenType    string         `json:"token_type,omitempty"`
	ExpiresIn    int            `json:"expires_in,omitempty"`
	User         map[string]any `json:"user,omitempty"`
}

type ErrorResponse struct {
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
	Msg              string `json:"msg,omitempty"`
	Message          string `json:"message,omitempty"`
}

func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) Enabled() bool {
	return c.baseURL != "" && c.apiKey != ""
}

func (c *Client) SignUp(ctx context.Context, req AuthRequest) (*AuthResponse, int, error) {
	return c.postJSON(ctx, "/auth/v1/signup", req)
}

func (c *Client) Login(ctx context.Context, req AuthRequest) (*AuthResponse, int, error) {
	return c.postJSON(ctx, "/auth/v1/token?grant_type=password", req)
}

func (c *Client) VerifyOTP(ctx context.Context, req VerifyOTPRequest) (*AuthResponse, int, error) {
	return c.postJSON(ctx, "/auth/v1/verify", req)
}

func (c *Client) postJSON(ctx context.Context, path string, payload any) (*AuthResponse, int, error) {
	if !c.Enabled() {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("supabase auth is not configured")
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("apikey", c.apiKey)
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("request supabase auth: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode >= 400 {
		var e ErrorResponse
		_ = json.NewDecoder(res.Body).Decode(&e)
		msg := firstNonEmpty(e.ErrorDescription, e.Message, e.Msg, e.Error, "supabase auth request failed")
		return nil, res.StatusCode, fmt.Errorf("%s", msg)
	}

	var out AuthResponse
	if err = json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("decode supabase auth response: %w", err)
	}
	return &out, res.StatusCode, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
