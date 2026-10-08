package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kingswady/cwcli/internal/config"
	"github.com/kingswady/cwcli/internal/platform"
)

const goodToken = "cwk_test-token-0123456789"

type memoryStore map[string]string

func (m memoryStore) Get(u string) (string, error) {
	if token, ok := m[u]; ok {
		return token, nil
	}
	return "", config.ErrNoToken
}
func (m memoryStore) Set(u, token string) (string, error) { m[u] = token; return "memory", nil }
func (m memoryStore) Delete(u string) error               { delete(m, u); return nil }

// fakeAPI answers like /api/v1: routes map a path to the "data" it returns.
func fakeAPI(t *testing.T, routes map[string]any) *httptest.Server {
	t.Helper()
	server, _ := recordedAPI(t, routes)
	return server
}

// requests are what a fake API was asked, each as "path?query" (the query
// sorted by url.Values.Encode, like the route keys).
type requests struct {
	mu   sync.Mutex
	seen []string
}

func (r *requests) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, strings.TrimPrefix(req.URL.Path, "/api/v1")+"?"+req.URL.Query().Encode())
}

// to is the requests whose path is path, in order.
func (r *requests) to(path string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var matching []string
	for _, seen := range r.seen {
		if strings.HasPrefix(seen, path+"?") {
			matching = append(matching, seen)
		}
	}
	return matching
}

// recordedAPI is fakeAPI that also records every request.
func recordedAPI(t *testing.T, routes map[string]any) (*httptest.Server, *requests) {
	t.Helper()
	seen := &requests{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.add(r)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+goodToken {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"invalid_token","message":"Missing, revoked or expired API token"}}`))
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/api/v1")
		if r.URL.RawQuery != "" {
			if _, ok := routes[key+"?"+r.URL.RawQuery]; ok {
				key += "?" + r.URL.RawQuery
			}
		}
		data, ok := routes[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"code":"not_found","message":"No API function at ` + key + `"}}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)
	return server, seen
}

type harness struct {
	app    *App
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	env    map[string]string
}

func newHarness(t *testing.T, server *httptest.Server) *harness {
	t.Helper()
	h := &harness{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, env: map[string]string{}}
	if server != nil {
		h.env["CW_URL"] = server.URL
		h.env["CW_TOKEN"] = goodToken
	}
	h.app = &App{
		version:    "dev",
		stdin:      strings.NewReader(""),
		stdout:     h.stdout,
		stderr:     h.stderr,
		getenv:     func(key string) string { return h.env[key] },
		configDir:  t.TempDir(),
		secrets:    memoryStore{},
		httpClient: platform.NewHTTPClient(),
		interrupt:  func() (context.Context, func()) { return context.WithCancel(context.Background()) },
		wait:       func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil },
		// Output captured into a buffer is never a terminal: plain unless asked.
		stdoutIsTerminal: func() bool { return false },
		stdinIsTerminal:  func() bool { return false },
		clock:            time.Now,
	}
	return h
}

func (h *harness) run(args ...string) int { return h.app.Run(args) }

func TestParseArgsAcceptsFlagsAfterPositionals(t *testing.T) {
	h := newHarness(t, nil)
	fs := h.app.newFlags("x")
	asJSON := fs.Bool("json", false, "")
	positional, err := parseArgs(fs, []string{"show", "shop", "--json"})
	if err != nil || !*asJSON || strings.Join(positional, " ") != "show shop" {
		t.Fatalf("got %v %v %v", positional, *asJSON, err)
	}
}

func TestAHTMLPageIsNotThePlatform(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>login</html>"))
	}))
	t.Cleanup(server.Close)
	h := newHarness(t, server)
	if code := h.run("servers"); code != 1 || !strings.Contains(h.stderr.String(), "is the URL right?") {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
}

func TestAParameterTheAPIRefusesIsAUsageError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"code":"invalid_parameter","message":"limit is out of range"}}`))
	}))
	t.Cleanup(server.Close)
	h := newHarness(t, server)
	if code := h.run("apps", "--limit", "0"); code != 2 {
		t.Fatalf("exit %d: %s", code, h.stderr)
	}
}
