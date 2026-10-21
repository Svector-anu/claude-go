package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client talks to a Server over its Unix socket.
type Client struct {
	http *http.Client
}

// APIError is a non-2xx answer from the server.
type APIError struct {
	Status int
	Detail ErrorDetail
}

func (e *APIError) Error() string { return fmt.Sprintf("%s: %s", e.Detail.Code, e.Detail.Message) }

// NewClient returns a client for the socket at path.
func NewClient(path string) *Client {
	return &Client{http: &http.Client{
		Timeout: acceptTimeout + 5*time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
			DisableKeepAlives: true,
		},
	}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://claude-go"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var eb ErrorBody
		if json.Unmarshal(data, &eb) != nil {
			return &APIError{Status: resp.StatusCode, Detail: ErrorDetail{Code: CodeInternal, Message: string(data)}}
		}
		return &APIError{Status: resp.StatusCode, Detail: eb.Error}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Health checks the controller is up and speaks this API.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	return h, c.do(ctx, http.MethodGet, "/v1/health", nil, &h)
}

// Status returns the session status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	return s, c.do(ctx, http.MethodGet, "/v1/status", nil, &s)
}

// Accounts lists the registered accounts.
func (c *Client) Accounts(ctx context.Context) ([]AccountView, error) {
	var r AccountsResponse
	return r.Accounts, c.do(ctx, http.MethodGet, "/v1/accounts", nil, &r)
}

// History returns the most recent handoff outcomes.
func (c *Client) History(ctx context.Context, limit int) ([]HistoryEntry, error) {
	var r HistoryResponse
	return r.Entries, c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/history?limit=%d", limit), nil, &r)
}

// Handoff requests a handoff to account to.
func (c *Client) Handoff(ctx context.Context, requestID, to string) (HandoffAccepted, error) {
	var a HandoffAccepted
	return a, c.do(ctx, http.MethodPost, "/v1/handoff", HandoffRequest{RequestID: requestID, To: to}, &a)
}

// ReportUsage sends the current account's rate-limit windows.
func (c *Client) ReportUsage(ctx context.Context, windows []Window) error {
	return c.do(ctx, http.MethodPost, "/v1/usage", UsageReport{Windows: windows}, nil)
}
