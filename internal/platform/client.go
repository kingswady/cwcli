// Package platform talks to the platform's customer API (/api/v1).
package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client calls one platform with one token.
type Client struct {
	Base      string
	Token     string
	HTTP      *http.Client
	UserAgent string
	// TokenExpires is the X-Token-Expires-At of the last answer (RFC 3339).
	TokenExpires string
}

// APIError is a refusal from the platform, with its machine-readable code.
type APIError struct {
	Status  int
	Code    string
	Message string
	// Reason refines Code where the platform gives one (a retry refused: newer_run, …).
	Reason string
	// RetryAfter is how long a rate-limited caller should wait (Retry-After), else 0.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	switch e.Code {
	case "invalid_token":
		return e.Message + " — run: cw login"
	case "function_not_allowed":
		return e.Message + " (give the token more access in My Settings → API Tokens)"
	}
	return e.Message
}

// NewHTTPClient never follows a redirect: the token goes to the URL given and
// nowhere else, and a redirect (to a login page, say) is never the API.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Get returns the response's "data" member.
func (c *Client) Get(path string, query url.Values) (json.RawMessage, error) {
	return c.GetContext(context.Background(), path, query)
}

// GetContext is Get that gives up when ctx ends (Ctrl-C), returning ctx.Err().
func (c *Client) GetContext(ctx context.Context, path string, query url.Values) (json.RawMessage, error) {
	target := c.Base + "/api/v1" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	data, _, err := c.do(ctx, http.MethodGet, target, nil, nil)
	return data, err
}

// Post asks the platform to change something: body as JSON, the request's
// Idempotency-Key (the same key again returns the first answer and changes
// nothing). It returns the "data" member and the HTTP status (202 when the
// change started, 200 when there was nothing to start).
func (c *Client) Post(ctx context.Context, path string, body any, key string) (json.RawMessage, int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	headers := map[string]string{"Content-Type": "application/json", "Idempotency-Key": key}
	return c.do(ctx, http.MethodPost, c.Base+"/api/v1"+path, payload, headers)
}

func (c *Client) do(ctx context.Context, method, target string, body []byte, headers map[string]string) (json.RawMessage, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, &NetworkError{Base: c.Base, Err: err}
	}
	defer resp.Body.Close()
	if expires := resp.Header.Get("X-Token-Expires-At"); expires != "" {
		c.TokenExpires = expires
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, &NetworkError{Base: c.Base, Err: fmt.Errorf("reading the response: %w", err)}
	}
	answer, err := c.decodeAnswer(resp, data)
	return answer, resp.StatusCode, err
}

func (c *Client) decodeAnswer(resp *http.Response, body []byte) (json.RawMessage, error) {
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, &APIError{
			Status: resp.StatusCode,
			Code:   "redirect",
			Message: fmt.Sprintf("%s does not serve the platform API here (it redirects to %s) — check the URL, "+
				"or whether the platform has the API enabled", c.Base, redirectPath(resp)),
		}
	}
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Reason  string `json:"reason"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, &APIError{
			Status:  resp.StatusCode,
			Code:    "bad_response",
			Message: fmt.Sprintf("%s did not answer as the platform API (HTTP %d) — is the URL right?", c.Base, resp.StatusCode),
		}
	}
	if envelope.Error != nil {
		return nil, &APIError{
			Status: resp.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message,
			Reason: envelope.Error.Reason, RetryAfter: retryAfter(resp),
		}
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return nil, &APIError{Status: resp.StatusCode, Code: "http_error", Message: fmt.Sprintf("HTTP %d from %s", resp.StatusCode, c.Base)}
	}
	return envelope.Data, nil
}

// NetworkError is a request that never got an answer: the platform could not be
// reached, or the connection broke. Asking again may work.
type NetworkError struct {
	Base string
	Err  error
}

func (e *NetworkError) Error() string { return fmt.Sprintf("cannot reach %s: %v", e.Base, e.Err) }
func (e *NetworkError) Unwrap() error { return e.Err }

// Decode reads JSON numbers as json.Number so ids print as integers.
func Decode(raw json.RawMessage, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(out)
}

// redirectPath is where a redirect points, without its query ("/web/login").
func redirectPath(resp *http.Response) string {
	target, err := resp.Location()
	if err != nil || target.Path == "" {
		return "another page"
	}
	return target.Path
}

// retryAfter reads a Retry-After of whole seconds (the platform sends no dates).
func retryAfter(resp *http.Response) time.Duration {
	seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
