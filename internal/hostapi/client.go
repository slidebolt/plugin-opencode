// Package hostapi is a client for the opencode serve API.
package hostapi

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
)

// username is what opencode serve expects unless configured otherwise.
const username = "opencode"

// Client talks to one OpenCode server.
type Client struct {
	base     string
	password string
	http     *http.Client
}

// New creates a Client. Prompts can run for minutes, so calls use their context
// rather than a client timeout.
func New(baseURL, password string) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, errors.New("OPENCODE_HOST_URL is required")
	}
	if password == "" {
		return nil, errors.New("OPENCODE_HOST_PASSWORD is required")
	}
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		return nil, fmt.Errorf("invalid OpenCode URL: %w", err)
	}
	return &Client{base: baseURL, password: password, http: &http.Client{}}, nil
}

// Health checks the server is reachable.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/global/health", nil, nil)
}

// CreateSession starts a conversation and returns its ID.
func (c *Client) CreateSession(ctx context.Context, title string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/session", map[string]string{"title": title}, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("opencode returned no session id")
	}
	return out.ID, nil
}

// Abort stops whatever a session is doing.
func (c *Client) Abort(ctx context.Context, sessionID string) error {
	return c.do(ctx, http.MethodPost, "/session/"+url.PathEscape(sessionID)+"/abort", nil, nil)
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Prompt sends text and blocks until the agent has finished, returning its reply.
func (c *Client) Prompt(ctx context.Context, sessionID, text string) (string, error) {
	var out struct {
		Info struct {
			Error json.RawMessage `json:"error"`
		} `json:"info"`
		Parts []textPart `json:"parts"`
	}
	body := map[string]any{"parts": []textPart{{Type: "text", Text: text}}}
	if err := c.do(ctx, http.MethodPost, "/session/"+url.PathEscape(sessionID)+"/message", body, &out); err != nil {
		return "", err
	}
	if msg := errorMessage(out.Info.Error); msg != "" {
		return "", errors.New(msg)
	}
	var reply []string
	for _, p := range out.Parts {
		if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			reply = append(reply, strings.TrimSpace(p.Text))
		}
	}
	return strings.Join(reply, "\n"), nil
}

func errorMessage(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var e struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return string(raw)
	}
	if e.Data.Message != "" {
		return e.Data.Message
	}
	if e.Name != "" {
		return e.Name
	}
	return string(raw)
}

func (c *Client) do(ctx context.Context, method, path string, payload, target any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(username, c.password)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("opencode HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if target == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
