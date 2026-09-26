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
	"regexp"
	"strconv"
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
	// Extra carries panel-owned client fields (including authentication secrets)
	// across full-row updates. It is never persisted or logged by the backend.
	Extra map[string]json.RawMessage `json:"-"`
}
type addRequest struct {
	Client     ClientConfig `json:"client"`
	InboundIDs []int        `json:"inboundIds"`
}
type RemoteClient struct {
	ID          json.RawMessage            `json:"id"`
	UUID        string                     `json:"uuid"`
	Email       string                     `json:"email"`
	SubID       string                     `json:"subId"`
	Enable      bool                       `json:"enable"`
	ExpiryTime  int64                      `json:"expiryTime"`
	LimitIP     int                        `json:"limitIp"`
	LimitHWID   int                        `json:"limitHwid"`
	TotalGB     int64                      `json:"totalGB"`
	LegacyTotal int64                      `json:"total"`
	Flow        string                     `json:"flow"`
	Group       string                     `json:"group"`
	Comment     string                     `json:"comment"`
	TgID        int64                      `json:"tgId"`
	InboundIDs  []int                      `json:"inboundIds"`
	Extra       map[string]json.RawMessage `json:"-"`
}

func (r *RemoteClient) UnmarshalJSON(data []byte) error {
	type remoteAlias RemoteClient
	var decoded remoteAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = RemoteClient(decoded)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"id", "uuid", "email", "subId", "enable", "expiryTime", "limitIp", "limitHwid", "totalGB", "total", "flow", "group", "comment", "tgId", "inboundIds"} {
		delete(fields, name)
	}
	r.Extra = fields
	return nil
}

func (c ClientConfig) MarshalJSON() ([]byte, error) {
	type configAlias ClientConfig
	raw, err := json.Marshal(configAlias(c))
	if err != nil {
		return nil, err
	}
	fields := make(map[string]json.RawMessage)
	if err = json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for key, value := range c.Extra {
		if _, known := fields[key]; !known {
			fields[key] = value
		}
	}
	// These fields are part of the full client row. Include zero values too so
	// a replacement update cannot silently discard a customer's existing value.
	for key, value := range map[string]any{
		"id": c.ID, "email": c.Email, "subId": c.SubID, "enable": c.Enable,
		"expiryTime": c.ExpiryTime, "limitIp": c.LimitIP, "limitHwid": c.LimitHWID,
		"totalGB": c.TotalGB, "flow": c.Flow, "group": c.Group,
		"comment": c.Comment, "tgId": c.TgID,
	} {
		encoded, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return nil, marshalErr
		}
		fields[key] = encoded
	}
	return json.Marshal(fields)
}

// InboundOption is the lightweight dropdown projection returned by 3x-ui.
// Keep only fields used by the bot's plan editor; the panel owns the remaining
// capability metadata in its public schema.
type InboundOption struct {
	ID       int    `json:"id"`
	Remark   string `json:"remark"`
	Tag      string `json:"tag"`
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	Enable   bool   `json:"enable"`
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

// ListClients returns the complete client projection required to check panel
// wide email, UUID, and subscription ID collisions. It intentionally fails if
// the panel returns an untyped/incomplete object.
func (c *Client) ListClients(ctx context.Context) ([]RemoteClient, error) {
	env, err := c.request(ctx, http.MethodGet, "/panel/api/clients/list", nil)
	if err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("panel client list rejected: %s", bounded(env.Msg))
	}
	var out []RemoteClient
	if len(env.Obj) == 0 || string(env.Obj) == "null" || json.Unmarshal(env.Obj, &out) != nil {
		return nil, errors.New("panel client list response is untyped or incomplete")
	}
	for _, client := range out {
		if client.Email == "" || client.UUID == "" || client.SubID == "" {
			return nil, errors.New("panel client list omitted email, UUID, or subscription ID")
		}
	}
	return out, nil
}

type InboundAttachment struct {
	ID      int            `json:"id"`
	Clients []RemoteClient `json:"clientStats"`
}

// ListInboundAttachments returns full inbound rows and clientStats (the slim
// endpoint is deliberately not used because it omits UUID and subId).
func (c *Client) ListInboundAttachments(ctx context.Context) ([]InboundAttachment, error) {
	env, err := c.request(ctx, http.MethodGet, "/panel/api/inbounds/list", nil)
	if err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("panel inbound list rejected: %s", bounded(env.Msg))
	}
	var out []InboundAttachment
	if len(env.Obj) == 0 || string(env.Obj) == "null" || json.Unmarshal(env.Obj, &out) != nil {
		return nil, errors.New("panel inbound list response is untyped or incomplete")
	}
	for _, in := range out {
		if in.ID <= 0 {
			return nil, errors.New("panel inbound list omitted inbound ID")
		}
		for _, cl := range in.Clients {
			if cl.Email == "" || cl.UUID == "" || cl.SubID == "" {
				return nil, errors.New("panel inbound clientStats omitted identity fields")
			}
		}
	}
	return out, nil
}

// ListInboundOptions returns known inbound IDs for validating archived
// attachment references before an instance restore.
func (c *Client) ListInboundOptions(ctx context.Context) ([]int, error) {
	options, err := c.ListInbounds(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(options))
	for _, option := range options {
		ids = append(ids, option.ID)
	}
	return ids, nil
}

// CustomerClientComment preserves the customer's configured device allowance
// in the 3x-ui comment while keeping the panel's own IP limit disabled. The
// "devices:" marker matches comments created by the legacy bots.
func CustomerClientComment(planName string, telegramID int64, deviceLimit int) string {
	planName = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.TrimSpace(planName))
	planName = strings.Join(strings.Fields(planName), " ")
	if planName == "" {
		planName = "VPN service"
	}
	if deviceLimit < 0 {
		deviceLimit = 0
	}
	return fmt.Sprintf("created by xui-backend, devices: %d, plan: %s, telegram_id: %d", deviceLimit, planName, telegramID)
}

var customerDeviceMarker = regexp.MustCompile(`(?i)(\bdevices\s*:\s*)\d+`)

// UpdateCustomerDeviceComment changes only the legacy-compatible device count
// marker, preserving panel/operator notes around it.
func UpdateCustomerDeviceComment(comment string, deviceLimit int) (string, error) {
	if deviceLimit < 0 {
		return "", errors.New("device limit cannot be negative")
	}
	matches := customerDeviceMarker.FindAllStringSubmatchIndex(comment, -1)
	if len(matches) != 1 {
		return "", errors.New("client comment must contain exactly one devices marker")
	}
	match := matches[0]
	return comment[:match[0]] + comment[match[2]:match[3]] + fmt.Sprint(deviceLimit) + comment[match[1]:], nil
}

func CustomerDeviceLimit(comment string) (int, error) {
	matches := customerDeviceMarker.FindAllStringSubmatchIndex(comment, -1)
	if len(matches) != 1 {
		return 0, errors.New("client comment must contain exactly one devices marker")
	}
	return strconv.Atoi(comment[matches[0][3]:matches[0][1]])
}

// ListInbounds returns the metadata needed to choose actual panel inbounds
// while building a plan. It uses the documented lightweight picker endpoint.
func (c *Client) ListInbounds(ctx context.Context) ([]InboundOption, error) {
	env, err := c.request(ctx, http.MethodGet, "/panel/api/inbounds/options", nil)
	if err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("panel inbound options rejected: %s", bounded(env.Msg))
	}
	var options []InboundOption
	if len(env.Obj) == 0 || string(env.Obj) == "null" || json.Unmarshal(env.Obj, &options) != nil {
		return nil, errors.New("panel inbound options response is untyped")
	}
	for _, option := range options {
		if option.ID <= 0 {
			return nil, errors.New("panel inbound options omitted inbound ID")
		}
	}
	return options, nil
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

// Update replaces the full client row in 3x-ui. The caller must first read the
// current client and carry its unowned fields through ClientConfig.Extra.
func (c *Client) Update(ctx context.Context, email string, config ClientConfig) WriteResult {
	if err := c.CheckWriteReadiness(ctx); err != nil {
		return WriteResult{Outcome: DefinitiveNoWrite, Err: err}
	}
	if strings.TrimSpace(email) == "" || config.Email != email {
		return WriteResult{Outcome: DefinitiveNoWrite, Err: errors.New("client update email must match the existing row")}
	}
	env, err := c.request(ctx, http.MethodPost, "/panel/api/clients/update/"+url.PathEscape(email), config)
	if err == nil && env.Success {
		return WriteResult{Outcome: Succeeded}
	}
	if err == nil {
		err = fmt.Errorf("panel update may have partially applied: %s", bounded(env.Msg))
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
