package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestARateLimitSaysWhenToRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"code":"rate_limited","message":"Too many requests; retry in 12 s"}}`))
	}))
	t.Cleanup(server.Close)
	c := &Client{Base: server.URL, Token: "cwk_x", HTTP: NewHTTPClient(), UserAgent: "cw/test"}
	_, err := c.Get("/whoami", nil)
	var refused *APIError
	if !errors.As(err, &refused) || refused.Status != 429 || refused.RetryAfter != 12*time.Second {
		t.Fatalf("got %#v", err)
	}
}

func TestAPostCarriesItsKeyAndBodyAndReadsTheReason(t *testing.T) {
	var gotKey, gotType string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotType = r.Header.Get("Idempotency-Key"), r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&gotBody)
		if r.URL.Path == "/api/v1/runs/1/retry" {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":{"code":"not_retryable","message":"Newer run exists","reason":"newer_run"}}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"data":{"state":"started","run_id":5}}`))
	}))
	t.Cleanup(server.Close)
	c := &Client{Base: server.URL, Token: "cwk_x", HTTP: NewHTTPClient()}
	data, status, err := c.Post(context.Background(), "/apps/7/backup", map[string]any{"format": "dump"}, "cw-key-1")
	if err != nil || status != http.StatusAccepted || !strings.Contains(string(data), `"run_id":5`) {
		t.Fatalf("data %s status %d err %v", data, status, err)
	}
	if gotKey != "cw-key-1" || gotType != "application/json" || gotBody["format"] != "dump" {
		t.Errorf("key %q type %q body %v", gotKey, gotType, gotBody)
	}
	_, _, err = c.Post(context.Background(), "/runs/1/retry", map[string]any{}, "cw-key-2")
	var refused *APIError
	if !errors.As(err, &refused) || refused.Code != "not_retryable" || refused.Reason != "newer_run" {
		t.Errorf("err %#v", err)
	}
	gone := &Client{Base: "http://127.0.0.1:1", Token: "cwk_x", HTTP: NewHTTPClient()}
	_, _, err = gone.Post(context.Background(), "/apps/7/backup", map[string]any{}, "k")
	var network *NetworkError
	if !errors.As(err, &network) || !strings.Contains(err.Error(), "cannot reach") {
		t.Errorf("unreachable: %v", err)
	}
}
