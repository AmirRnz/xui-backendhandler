package xui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"example.com/xui-commerce/backend/internal/panelurl"
)

const SupportedWriteVersion = "3.8.5"

var ErrNotFound = errors.New("3x-ui client not found")

type Outcome string

const (
	Succeeded         Outcome = "success"
	DefinitiveNoWrite Outcome = "definitive_no_write"
	Unknown           Outcome = "unknown_or_partial"
)

type WriteResult struct {
	Outcome Outcome
	Err     error
}

type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

type ClientConfig struct {
	ID         string `json:"id"`
	Email      string `json:"email"`
	SubID      string `json:"subId"`
	Enable     bool   `json:"enable"`
	ExpiryTime int64  `json:"expiryTime"`
	LimitIP    int    `json:"limitIp"`
	LimitHWID  int    `json:"limitHwid"`
	TotalGB    int64  `json:"totalGB"`
	Flow       string `json:"flow"`
	Group      string `json:"group"`
	Comment    string `json:"comment"`
	TgID       int64  `json:"tgId"`
}
type addRequest struct {
	Client     ClientConfig `json:"client"`
	InboundIDs []int        `json:"inboundIds"`
}
type RemoteClient struct {
	ID          json.RawMessage `json:"id"`
	UUID        string          `json:"uuid"`
	Email       string          `json:"email"`
	SubID       string          `json:"subId"`
	Enable      bool            `json:"enable"`
	ExpiryTime  int64           `json:"expiryTime"`
	LimitIP     int             `json:"limitIp"`
	LimitHWID   int             `json:"limitHwid"`
	TotalGB     int64           `json:"totalGB"`
	LegacyTotal int64           `json:"total"`
	Flow        string          `json:"flow"`
	Group       string          `json:"group"`
	Comment     string          `json:"comment"`
	TgID        int64           `json:"tgId"`
	InboundIDs  []int           `json:"inboundIds"`
}
type envelope struct {
	Success bool            `json:"success"`
	Msg     string          `json:"msg"`
	Obj     json.RawMessage `json:"obj"`
}

func New(baseURL, apiToken string, timeout time.Duration) (*Client, error) {
	if err := panelurl.Validate(baseURL); err != nil {
		return nil, fmt.Errorf("invalid panel base URL: %w", err)
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid panel base URL: %w", err)
	}
	if strings.TrimSpace(apiToken) == "" {
		return nil, fmt.Errorf("panel API token is required")
	}
	if timeout <= 0 || timeout > 60*time.Second {
		timeout = 12 * time.Second
	}
	return &Client{base: u, token: apiToken, http: &http.Client{Timeout: timeout}}, nil
}

func (c *Client) request(ctx context.Context, method, path string, body any) (envelope, error) {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return envelope{}, err
		}
		buf = bytes.NewReader(b)
	}
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), buf)
	if err != nil {
		return envelope{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return envelope{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return envelope{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return envelope{}, fmt.Errorf("3x-ui returned HTTP %d", resp.StatusCode)
	}
	var env envelope
	if err = json.Unmarshal(data, &env); err != nil {
		return envelope{}, fmt.Errorf("decode 3x-ui response: %w", err)
	}
	return env, nil
}

func (c *Client) CheckWriteReadiness(ctx context.Context) error {
	env, err := c.request(ctx, http.MethodGet, "/panel/api/server/getPanelUpdateInfo", nil)
	if err != nil {
		return fmt.Errorf("read panel write capability: %w", err)
	}
	if !env.Success {
		return fmt.Errorf("panel readiness rejected: %s", bounded(env.Msg))
	}
	var obj struct {
		CurrentVersion string `json:"currentVersion"`
	}
	if err = json.Unmarshal(env.Obj, &obj); err != nil {
		return err
	}
	if !sameVersion(obj.CurrentVersion, SupportedWriteVersion) {
		return fmt.Errorf("panel writes disabled: require 3x-ui %s, found %q", SupportedWriteVersion, obj.CurrentVersion)
	}
	// The lightweight options endpoint verifies authenticated panel access before writes.
	opt, err := c.request(ctx, http.MethodGet, "/panel/api/inbounds/options", nil)
	if err != nil {
		return fmt.Errorf("verify panel inbound access: %w", err)
	}
	if !opt.Success {
		return fmt.Errorf("panel inbound access rejected: %s", bounded(opt.Msg))
	}
	return nil
}
func sameVersion(got, want string) bool {
	got = strings.TrimSpace(strings.TrimPrefix(got, "v"))
	return got == want || strings.HasPrefix(got, want+"-") || strings.HasPrefix(got, want+"+")
}

func (c *Client) GetClient(ctx context.Context, email string) (*RemoteClient, error) {
	env, err := c.request(ctx, http.MethodGet, "/panel/api/clients/get/"+url.PathEscape(email), nil)
	if err != nil {
		return nil, err
	}
	if !env.Success {
		if strings.Contains(strings.ToLower(env.Msg), "not found") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read panel client: %s", bounded(env.Msg))
	}
	if len(env.Obj) == 0 || string(env.Obj) == "null" {
		return nil, ErrNotFound
	}
	var out RemoteClient
	if err = json.Unmarshal(env.Obj, &out); err != nil {
		return nil, err
	}
	if out.Email == "" {
		return nil, ErrNotFound
	}
	if out.Email != email {
		return nil, fmt.Errorf("panel returned a different email for client lookup")
	}
	return &out, nil
}

func (c *Client) Add(ctx context.Context, config ClientConfig, inbounds []int) WriteResult {
	if err := c.CheckWriteReadiness(ctx); err != nil {
		return WriteResult{Outcome: DefinitiveNoWrite, Err: err}
	}
	env, err := c.request(ctx, http.MethodPost, "/panel/api/clients/add", addRequest{Client: config, InboundIDs: inbounds})
	if err == nil && env.Success {
		return WriteResult{Outcome: Succeeded}
	}
	if err == nil {
		err = fmt.Errorf("panel add returned success=false: %s", bounded(env.Msg))
	}
	return WriteResult{Outcome: Unknown, Err: err}
}
func (c *Client) Attach(ctx context.Context, email string, inbounds []int) error {
	env, err := c.request(ctx, http.MethodPost, "/panel/api/clients/"+url.PathEscape(email)+"/attach", map[string]any{"inboundIds": inbounds})
	if err != nil {
		return err
	}
	if !env.Success {
		return fmt.Errorf("panel attach returned success=false: %s", bounded(env.Msg))
	}
	return nil
}
func (c *Client) Delete(ctx context.Context, email string) WriteResult {
	if err := c.CheckWriteReadiness(ctx); err != nil {
		return WriteResult{Outcome: DefinitiveNoWrite, Err: err}
	}
	env, err := c.request(ctx, http.MethodPost, "/panel/api/clients/del/"+url.PathEscape(email)+"?keepTraffic=0", nil)
	if err == nil && env.Success {
		return WriteResult{Outcome: Succeeded}
	}
	if err == nil {
		err = fmt.Errorf("panel delete returned success=false: %s", bounded(env.Msg))
	}
	return WriteResult{Outcome: Unknown, Err: err}
}
func (c *Client) SubscriptionLinks(ctx context.Context, subID string) ([]string, error) {
	env, err := c.request(ctx, http.MethodGet, "/panel/api/clients/subLinks/"+url.PathEscape(subID), nil)
	if err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("panel subscription links unavailable: %s", bounded(env.Msg))
	}
	var links []string
	if len(env.Obj) > 0 && string(env.Obj) != "null" {
		if err = json.Unmarshal(env.Obj, &links); err != nil {
			return nil, err
		}
	}
	return links, nil
}

func UUIDOf(r *RemoteClient) string {
	if r == nil {
		return ""
	}
	if r.UUID != "" {
		return r.UUID
	}
	var s string
	if json.Unmarshal(r.ID, &s) == nil {
		return s
	}
	return ""
}
func (r *RemoteClient) TrafficLimit() int64 {
	if r == nil {
		return 0
	}
	if r.TotalGB != 0 {
		return r.TotalGB
	}
	return r.LegacyTotal
}
func bounded(s string) string {
	if len(s) > 500 {
		s = s[:500]
	}
	return strings.TrimSpace(s)
}
