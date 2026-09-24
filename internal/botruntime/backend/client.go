package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}
type APIError struct {
	Status        int
	Code, Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("backend %s: %s", e.Code, e.Message) }
func New(baseURL, token string, timeout time.Duration) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if !strings.HasPrefix(baseURL, "https://") && !strings.HasPrefix(baseURL, "http://") {
		return nil, fmt.Errorf("BACKEND_URL must be an HTTP(S) URL")
	}
	if token == "" {
		return nil, fmt.Errorf("BACKEND_TOKEN is required")
	}
	if timeout < time.Second || timeout > time.Minute {
		timeout = 15 * time.Second
	}
	return &Client{BaseURL: baseURL, Token: token, HTTP: &http.Client{Timeout: timeout}}, nil
}
func (c *Client) Call(ctx context.Context, method, path string, telegramID int64, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if telegramID > 0 {
		req.Header.Set("X-Actor-Telegram-ID", fmt.Sprint(telegramID))
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("backend request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return fmt.Errorf("read backend response failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return &APIError{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if out != nil && len(data) > 0 {
		if err = json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode backend response failed")
		}
	}
	return nil
}
