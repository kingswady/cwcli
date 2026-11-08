// Package upload sends a file to a server's DownloaderWady in pieces, for
// cw backups import. The platform signs one upload; its URL carries the
// signature, the upload's only credential, and the file goes straight to the
// app's server, never through the platform. No API token travels here.
package upload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kingswady/cwcli/internal/platform"
)

// DefaultPiece is the piece cw sends when the platform names none: under
// Cloudflare's 100 MB request cap.
const DefaultPiece = 50 << 20

// stallTimeout: a connection that takes no bytes for this long is given up,
// and the piece sent again.
const stallTimeout = 2 * time.Minute

// State is what the server holds of the file: the next piece starts at Offset.
type State struct {
	Offset   int64 `json:"offset"`
	Size     int64 `json:"size"`
	Complete bool  `json:"complete"`
}

// Target is one signed upload.
type Target struct {
	URL  string
	HTTP *http.Client
}

// Refused is an answer that is not the file's state: its HTTP status and what
// DownloaderWady said. A 409 that says where the file ends carries that in
// State: the next piece starts there.
type Refused struct {
	Status int
	Detail string
	State  *State
}

func (e *Refused) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Detail)
}

// NetworkError is a request that never got an answer: sending it again may work.
type NetworkError struct {
	Host string
	Err  error
}

func (e *NetworkError) Error() string { return fmt.Sprintf("cannot reach %s: %v", e.Host, e.Err) }
func (e *NetworkError) Unwrap() error { return e.Err }

// CheckURL accepts an upload address: https, or http to this machine. A
// database never crosses a network in the clear. The address is never
// repeated in the error: its query is the upload's signature.
func CheckURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return errors.New("the platform's upload address is not a URL")
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if platform.IsLoopback(parsed.Hostname()) {
			return nil
		}
	}
	return fmt.Errorf("the upload address for %s is not https: refusing to send the file in the clear", parsed.Hostname())
}

// NewHTTPClient is a client for 50 MB pieces on slow links: no limit on how
// long a piece takes, but one on a connection that stops moving (stallConn) and
// on an answer that does not come once the piece is in. HTTP/1.1 only, and no
// redirect: the file goes to the address the platform signed and nowhere else.
func NewHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := dialer.DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return &stallConn{Conn: conn}, nil
			},
			TLSHandshakeTimeout: 30 * time.Second,
			// The last piece is answered once the server has the whole file on disk.
			ResponseHeaderTimeout: 5 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// stallConn gives each write stallTimeout: one that cannot go out that long
// (the link went quiet, the server stopped reading) fails the piece.
type stallConn struct{ net.Conn }

func (c *stallConn) Write(p []byte) (int, error) {
	if err := c.Conn.SetWriteDeadline(time.Now().Add(stallTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

// Status asks where the file ends: where the next piece starts.
func (t *Target) Status(ctx context.Context) (State, error) {
	status, raw, err := t.do(ctx, http.MethodGet, nil, "")
	if err != nil {
		return State{}, err
	}
	return readState(status, raw)
}

// Put sends bytes start to end (both included) of file, of size bytes in all,
// read from the file as they go. sent is told how many of them have gone out.
func (t *Target) Put(ctx context.Context, file io.ReaderAt, start, end, size int64, sent func(int64)) (State, error) {
	body := &counter{r: io.NewSectionReader(file, start, end-start+1), sent: sent}
	// The transport may read on after an error: what it reads then is not news.
	defer body.closed.Store(true)
	status, raw, err := t.do(ctx, http.MethodPut, body, fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	if err != nil {
		return State{}, err
	}
	return readState(status, raw)
}

// Cancel asks the server to drop what it holds of the file.
func (t *Target) Cancel(ctx context.Context) error {
	status, raw, err := t.do(ctx, http.MethodDelete, nil, "")
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		return &Refused{Status: status, Detail: detail(raw, nil)}
	}
	return nil
}

func (t *Target) do(ctx context.Context, method string, body *counter, contentRange string) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = body
	}
	req, err := http.NewRequestWithContext(ctx, method, t.URL, reader)
	if err != nil {
		return 0, nil, errors.New("the platform's upload address is not a URL")
	}
	if body != nil {
		req.ContentLength = body.r.Size()
		req.Header.Set("Content-Range", contentRange)
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return 0, nil, t.failed(ctx, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, nil, t.failed(ctx, fmt.Errorf("reading the answer: %w", err))
	}
	return resp.StatusCode, raw, nil
}

// failed is a request that got no answer, without its URL: http's errors
// quote it, and its query is the upload's signature.
func (t *Target) failed(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var quoted *url.Error
	if errors.As(err, &quoted) {
		err = quoted.Err
	}
	host := "the server"
	if parsed, parseErr := url.Parse(t.URL); parseErr == nil && parsed.Host != "" {
		host = parsed.Host
	}
	return &NetworkError{Host: host, Err: err}
}

// readState is an answer as the file's state: 200 with it, or a refusal (a
// 409 that says where the file ends carries that too).
func readState(status int, raw []byte) (State, error) {
	var said struct {
		Offset   *int64          `json:"offset"`
		Size     int64           `json:"size"`
		Complete bool            `json:"complete"`
		Detail   json.RawMessage `json:"detail"`
	}
	parsed := json.Unmarshal(raw, &said) == nil
	if parsed && said.Offset != nil {
		state := State{Offset: *said.Offset, Size: said.Size, Complete: said.Complete}
		switch status {
		case http.StatusOK:
			return state, nil
		case http.StatusConflict:
			return state, &Refused{Status: status, State: &state}
		}
	}
	if status == http.StatusOK {
		return State{}, &Refused{Status: status, Detail: "not an answer from DownloaderWady"}
	}
	return State{}, &Refused{Status: status, Detail: detail(raw, said.Detail)}
}

// detail is what DownloaderWady said ({"detail": "…"}), else the start of the body.
func detail(raw []byte, said json.RawMessage) string {
	var text string
	if json.Unmarshal(said, &text) == nil && text != "" {
		return text
	}
	if len(said) > 0 {
		text = string(said)
	} else {
		text = strings.TrimSpace(string(raw))
	}
	text, _, _ = strings.Cut(text, "\n")
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return text
}

// counter tells sent how much of a piece has been read for the wire.
type counter struct {
	r      *io.SectionReader
	n      int64
	sent   func(int64)
	closed atomic.Bool
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if n > 0 && c.sent != nil && !c.closed.Load() {
		c.sent(c.n)
	}
	return n, err
}
