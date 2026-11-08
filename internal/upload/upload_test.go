package upload

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTheFileGoesOnlyOverHTTPSOrToThisMachine(t *testing.T) {
	for raw, fine := range map[string]bool{
		"https://dw.example.com/upload/abc?token=x": true,
		"http://127.0.0.1:8080/upload/abc":          true,
		"http://localhost/upload/abc":               true,
		"http://dw.example.com/upload/abc?token=x":  false,
		"ftp://dw.example.com/upload/abc":           false,
		"not a url":                                 false,
	} {
		err := CheckURL(raw)
		if (err == nil) != fine {
			t.Errorf("%s: %v", raw, err)
		}
		if err != nil && strings.Contains(err.Error(), "token=") {
			t.Errorf("%s: the signature was repeated: %v", raw, err)
		}
	}
}

func TestAPieceCarriesItsRangeAndNoCredentialButTheSignature(t *testing.T) {
	var got struct{ method, contentRange, auth, body, token string }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.method, got.contentRange, got.auth = r.Method, r.Header.Get("Content-Range"), r.Header.Get("Authorization")
		got.body, got.token = string(body), r.URL.Query().Get("token")
		w.Write([]byte(`{"offset": 7, "size": 10, "complete": false}`))
	}))
	defer server.Close()
	target := &Target{URL: server.URL + "/upload/abc?token=sig", HTTP: NewHTTPClient()}
	var told int64
	state, err := target.Put(context.Background(), strings.NewReader("0123456789"), 3, 6, 10, func(n int64) { told = n })
	if err != nil || state != (State{Offset: 7, Size: 10}) {
		t.Fatalf("state %+v, %v", state, err)
	}
	if got.method != http.MethodPut || got.contentRange != "bytes 3-6/10" || got.body != "3456" || got.auth != "" || got.token != "sig" {
		t.Errorf("sent %+v", got)
	}
	if told != 4 {
		t.Errorf("told %d bytes sent", told)
	}
}

func TestAnswersThatAreNotTheStateAreRefusals(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   Refused
	}{
		{409, `{"offset": 20, "size": 25, "complete": false}`, Refused{Status: 409, State: &State{Offset: 20, Size: 25}}},
		{409, `{"detail": "Another request is sending this upload"}`, Refused{Status: 409, Detail: "Another request is sending this upload"}},
		{507, `{"detail": "Not enough free disk on the server for this file"}`, Refused{Status: 507, Detail: "Not enough free disk on the server for this file"}},
		{422, `{"detail": [{"loc": ["query", "token"]}]}`, Refused{Status: 422, Detail: `[{"loc": ["query", "token"]}]`}},
		{502, "<html>Bad Gateway</html>", Refused{Status: 502, Detail: "<html>Bad Gateway</html>"}},
		{200, "<html>a login page</html>", Refused{Status: 200, Detail: "not an answer from DownloaderWady"}},
	}
	for _, c := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		}))
		_, err := (&Target{URL: server.URL, HTTP: NewHTTPClient()}).Status(context.Background())
		server.Close()
		var refused *Refused
		if !errors.As(err, &refused) || refused.Status != c.want.Status || refused.Detail != c.want.Detail ||
			(c.want.State == nil) != (refused.State == nil) || (refused.State != nil && *refused.State != *c.want.State) {
			t.Errorf("%d %s: %#v", c.status, c.body, err)
		}
	}
}

func TestANetworkErrorNamesTheHostNotTheSignature(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	address := server.URL
	server.Close() // nothing answers there any more
	_, err := (&Target{URL: address + "/upload/abc?token=secret-signature", HTTP: NewHTTPClient()}).Status(context.Background())
	var network *NetworkError
	if !errors.As(err, &network) {
		t.Fatalf("%#v", err)
	}
	if strings.Contains(err.Error(), "secret-signature") || !strings.Contains(err.Error(), strings.TrimPrefix(address, "http://")) {
		t.Errorf("%v", err)
	}
}

func TestCancelAsksTheServerToDropTheFile(t *testing.T) {
	method := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		w.Write([]byte(`{"canceled": true}`))
	}))
	defer server.Close()
	if err := (&Target{URL: server.URL, HTTP: NewHTTPClient()}).Cancel(context.Background()); err != nil || method != http.MethodDelete {
		t.Errorf("%s: %v", method, err)
	}
}
