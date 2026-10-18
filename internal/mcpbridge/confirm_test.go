package mcpbridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeChanges is the platform's /mcp for changes: a 2026-07-28 tools/call of a
// change asks with a form until the answer confirms it (a production app also
// wants its name typed); every request it got is kept, as sent.
type fakeChanges struct {
	mu         sync.Mutex
	received   []map[string]any
	headers    []http.Header
	production bool
	states     int
	askForever bool
}

func (f *fakeChanges) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg map[string]any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &msg)
		f.mu.Lock()
		f.received = append(f.received, msg)
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		params, _ := msg["params"].(map[string]any)
		meta, _ := params["_meta"].(map[string]any)
		modern := meta[metaVersion] == modernVersion
		if modern && (r.Header.Get("MCP-Protocol-Version") != modernVersion || r.Header.Get("Mcp-Method") != msg["method"]) {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg["id"], "error": map[string]any{"code": -32020, "message": "Header mismatch"}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		result := f.answer(msg["method"], params, meta, modern)
		if modern {
			result["resultType"] = firstNonEmpty(result["resultType"], "complete")
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": result})
	}))
	t.Cleanup(server.Close)
	return server
}

func firstNonEmpty(value any, fallback string) any {
	if value == nil || value == "" {
		return fallback
	}
	return value
}

func (f *fakeChanges) answer(method any, params, meta map[string]any, modern bool) map[string]any {
	switch method {
	case "initialize":
		return map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
	case "tools/list":
		tools := []any{map[string]any{"name": "apps_get"}}
		capabilities, _ := meta[metaCapabilities].(map[string]any)
		if modern && capabilities["elicitation"] != nil {
			tools = append(tools, map[string]any{"name": "apps_restart"})
		}
		return map[string]any{"tools": tools}
	case "tools/call":
		if params["name"] != "apps_restart" {
			return text("read: " + fmt.Sprint(params["name"]))
		}
		if !modern {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": "cannot confirm"}}, "isError": true}
		}
		return f.change(params)
	}
	return map[string]any{}
}

func text(s string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}, "isError": false}
}

func (f *fakeChanges) change(params map[string]any) map[string]any {
	if params["requestState"] == nil || f.askForever {
		return f.ask("Restart shop (Shop / staging)?")
	}
	responses, _ := params["inputResponses"].(map[string]any)
	confirm, _ := responses["confirm"].(map[string]any)
	content, _ := confirm["content"].(map[string]any)
	switch {
	case confirm["action"] != "accept" || content["confirm"] == false:
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "Not done: the user declined."}}, "isError": true}
	case f.production && content["app_name"] != "erp":
		return f.ask("The name did not match. Restart erp (ERP / production)?")
	}
	result := text(`{"state":"started","run_id":41977}`)
	result["structuredContent"] = map[string]any{"state": "started", "run_id": 41977}
	return result
}

func (f *fakeChanges) ask(message string) map[string]any {
	f.mu.Lock()
	f.states++
	state := fmt.Sprintf("state-%d", f.states)
	f.mu.Unlock()
	properties := map[string]any{"confirm": map[string]any{"type": "boolean", "title": "Do it", "default": false}}
	if f.production {
		properties["app_name"] = map[string]any{"type": "string", "title": "App name"}
	}
	return map[string]any{
		"resultType": "input_required",
		"inputRequests": map[string]any{"confirm": map[string]any{
			"method": "elicitation/create",
			"params": map[string]any{
				"mode": "form", "message": message,
				"requestedSchema": map[string]any{"type": "object", "properties": properties, "required": []any{"confirm"}},
			},
		}},
		"requestState": state,
	}
}

func (f *fakeChanges) calls() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var calls []map[string]any
	for _, msg := range f.received {
		if msg["method"] == "tools/call" {
			calls = append(calls, msg)
		}
	}
	return calls
}

// client is a stdio MCP client driving a running Bridge through pipes.
type client struct {
	t     *testing.T
	in    *io.PipeWriter
	lines chan map[string]any
	done  chan error
}

type lineSink chan map[string]any

func (s lineSink) Write(p []byte) (int, error) {
	for _, line := range bytes.Split(bytes.TrimSpace(p), []byte("\n")) {
		var msg map[string]any
		if json.Unmarshal(line, &msg) == nil {
			s <- msg
		}
	}
	return len(p), nil
}

func startClient(t *testing.T, server *httptest.Server) *client {
	t.Helper()
	r, w := io.Pipe()
	c := &client{t: t, in: w, lines: make(chan map[string]any, 64), done: make(chan error, 1)}
	b := &Bridge{Endpoint: server.URL + "/mcp", Token: goodToken, HTTP: server.Client(), UserAgent: "cw/test",
		Out: lineSink(c.lines), Log: io.Discard}
	go func() { c.done <- b.Serve(r) }()
	t.Cleanup(func() { w.Close() })
	return c
}

func (c *client) send(format string, args ...any) {
	c.t.Helper()
	if _, err := fmt.Fprintf(c.in, format+"\n", args...); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) next() map[string]any {
	c.t.Helper()
	select {
	case msg := <-c.lines:
		return msg
	case <-time.After(5 * time.Second):
		c.t.Fatal("no message from the bridge")
		return nil
	}
}

// initialize is a 2025 handshake; capabilities is the client's, as JSON.
func (c *client) initialize(capabilities string) {
	c.t.Helper()
	c.send(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":%s,"clientInfo":{"name":"test-client","version":"1"}}}`, capabilities)
	if answer := c.next(); answer["id"] != 0.0 {
		c.t.Fatalf("initialize answered %v", answer)
	}
}

// elicitation is the next message, which must be the bridge asking the client.
func (c *client) elicitation() map[string]any {
	c.t.Helper()
	msg := c.next()
	if msg["method"] != "elicitation/create" || msg["id"] == nil {
		c.t.Fatalf("expected an elicitation/create request, got %v", msg)
	}
	return msg
}

func (c *client) answerElicitation(request map[string]any, result string) {
	c.t.Helper()
	id, _ := json.Marshal(request["id"])
	c.send(`{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result)
}

func toolNames(t *testing.T, answer map[string]any) []string {
	t.Helper()
	result, _ := answer["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	var names []string
	for _, tool := range tools {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}

func resultOf(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	result, ok := answer["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", answer)
	}
	return result
}

func TestAClientThatCanAskIsOfferedChangesAndConfirmsThem(t *testing.T) {
	platform := &fakeChanges{}
	c := startClient(t, platform.serve(t))
	c.initialize(`{"elicitation":{}}`)

	// As the TypeScript SDK sends it: no params at all.
	c.send(`{"method":"tools/list","jsonrpc":"2.0","id":1}`)
	listed := c.next()
	if names := toolNames(t, listed); strings.Join(names, ",") != "apps_get,apps_restart" {
		t.Errorf("tools %v", names)
	}
	if _, typed := resultOf(t, listed)["resultType"]; typed {
		t.Error("a 2025 client gets no resultType")
	}

	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"apps_restart","arguments":{"id":7}}}`)
	asked := c.elicitation()
	params := asked["params"].(map[string]any)
	if params["message"] != "Restart shop (Shop / staging)?" || params["requestedSchema"] == nil {
		t.Errorf("asked %v", params)
	}
	if _, mode := params["mode"]; mode {
		t.Error("the 2025 form carries no mode")
	}
	c.answerElicitation(asked, `{"action":"accept","content":{"confirm":true}}`)
	done := resultOf(t, c.next())
	if done["isError"] != false || done["structuredContent"].(map[string]any)["state"] != "started" {
		t.Errorf("result %v", done)
	}
	if _, typed := done["resultType"]; typed {
		t.Error("the final result carries no resultType")
	}

	calls := platform.calls()
	if len(calls) != 2 {
		t.Fatalf("calls %v", calls)
	}
	retry := calls[1]["params"].(map[string]any)
	meta := retry["_meta"].(map[string]any)
	if meta[metaVersion] != modernVersion || meta[metaClientInfo].(map[string]any)["name"] != "test-client" {
		t.Errorf("meta %v", meta)
	}
	if retry["requestState"] != "state-1" || retry["inputResponses"].(map[string]any)["confirm"].(map[string]any)["action"] != "accept" {
		t.Errorf("retry %v", retry)
	}
	if retry["name"] != "apps_restart" || retry["arguments"].(map[string]any)["id"] != 7.0 {
		t.Errorf("the retry is the same call: %v", retry)
	}
}

func TestADeclinedChangeIsNotDone(t *testing.T) {
	for _, answer := range []string{`{"action":"decline"}`, `{"action":"cancel"}`, `{"action":"accept","content":{"confirm":false}}`} {
		platform := &fakeChanges{}
		c := startClient(t, platform.serve(t))
		c.initialize(`{"elicitation":{"form":{}}}`)
		c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"apps_restart","arguments":{"id":7}}}`)
		c.answerElicitation(c.elicitation(), answer)
		result := resultOf(t, c.next())
		content := result["content"].([]any)[0].(map[string]any)
		if result["isError"] != true || content["text"] != "Not done: the user declined." {
			t.Errorf("%s: %v", answer, result)
		}
	}
}

func TestAProductionAppAsksAgainUntilItsNameIsTyped(t *testing.T) {
	platform := &fakeChanges{production: true}
	c := startClient(t, platform.serve(t))
	c.initialize(`{"elicitation":{}}`)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"apps_restart","arguments":{"id":8}}}`)
	c.answerElicitation(c.elicitation(), `{"action":"accept","content":{"confirm":true,"app_name":"shop"}}`)
	again := c.elicitation()
	if msg := again["params"].(map[string]any)["message"]; msg != "The name did not match. Restart erp (ERP / production)?" {
		t.Errorf("asked again with %v", msg)
	}
	c.answerElicitation(again, `{"action":"accept","content":{"confirm":true,"app_name":"erp"}}`)
	if result := resultOf(t, c.next()); result["isError"] != false {
		t.Errorf("result %v", result)
	}
	if calls := platform.calls(); len(calls) != 3 || calls[2]["params"].(map[string]any)["requestState"] != "state-2" {
		t.Errorf("each retry carries the latest state: %v", calls)
	}
}

func TestOnlyTheFormConfirms(t *testing.T) {
	platform := &fakeChanges{}
	c := startClient(t, platform.serve(t))
	c.initialize(`{"elicitation":{}}`)
	// A request that brings its own answer and state: dropped, the user is asked.
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"apps_restart","arguments":{"id":7},"requestState":"state-9","inputResponses":{"confirm":{"action":"accept","content":{"confirm":true}}}}}`)
	c.answerElicitation(c.elicitation(), `{"action":"decline"}`)
	c.next()
	first := platform.calls()[0]["params"].(map[string]any)
	if _, ok := first["requestState"]; ok {
		t.Errorf("the platform saw the request's own state: %v", first)
	}
	if _, ok := first["inputResponses"]; ok {
		t.Errorf("the platform saw the request's own answer: %v", first)
	}
}

func TestAClientThatCannotAskGetsWhatItGotBefore(t *testing.T) {
	for _, capabilities := range []string{`{}`, `{"elicitation":{"url":{}}}`, `{"roots":{}}`} {
		platform := &fakeChanges{}
		c := startClient(t, platform.serve(t))
		c.initialize(capabilities)
		c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
		if names := toolNames(t, c.next()); strings.Join(names, ",") != "apps_get" {
			t.Errorf("%s: tools %v", capabilities, names)
		}
		platform.mu.Lock()
		header := platform.headers[len(platform.headers)-1].Get("MCP-Protocol-Version")
		_, meta := platform.received[len(platform.received)-1]["params"].(map[string]any)["_meta"]
		platform.mu.Unlock()
		if header != "2025-11-25" || meta {
			t.Errorf("%s: forwarded as %q, _meta %v — not as before", capabilities, header, meta)
		}
	}
}

func TestAModernClientRunsItsOwnRoundTrip(t *testing.T) {
	platform := &fakeChanges{}
	c := startClient(t, platform.serve(t))
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"elicitation":{"form":{},"url":{}}}}`
	c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"apps_restart","arguments":{"id":7},%s}}`, meta)
	asked := resultOf(t, c.next())
	if asked["resultType"] != "input_required" || asked["requestState"] != "state-1" || asked["inputRequests"] == nil {
		t.Fatalf("a 2026-07-28 client sees input_required as sent: %v", asked)
	}
	request := asked["inputRequests"].(map[string]any)["confirm"].(map[string]any)["params"].(map[string]any)
	if request["mode"] != "form" {
		t.Errorf("as the platform sent it, mode included: %v", request)
	}
	retry := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"apps_restart","arguments":{"id":7},"requestState":"state-1","inputResponses":{"confirm":{"action":"accept","content":{"confirm":true}}},%s}}`, meta)
	c.send("%s", retry)
	done := resultOf(t, c.next())
	if done["resultType"] != "complete" || done["isError"] != false {
		t.Errorf("result %v", done)
	}
	calls := platform.calls()
	if len(calls) != 2 {
		t.Fatalf("calls %v", calls)
	}
	var sent map[string]any
	json.Unmarshal([]byte(retry), &sent)
	if fmt.Sprint(calls[1]) != fmt.Sprint(sent) {
		t.Errorf("the retry reaches the platform unchanged:\n sent %v\n got  %v", sent, calls[1])
	}
}

func TestOtherRequestsAreServedWhileTheUserDecides(t *testing.T) {
	platform := &fakeChanges{}
	c := startClient(t, platform.serve(t))
	c.initialize(`{"elicitation":{}}`)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"apps_restart","arguments":{"id":7}}}`)
	asked := c.elicitation()
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"apps_get","arguments":{"id":7}}}`)
	read := c.next()
	if read["id"] != 3.0 || resultOf(t, read)["content"].([]any)[0].(map[string]any)["text"] != "read: apps_get" {
		t.Fatalf("the read waited for the form: %v", read)
	}
	c.answerElicitation(asked, `{"action":"accept","content":{"confirm":true}}`)
	if done := c.next(); done["id"] != 2.0 || resultOf(t, done)["isError"] != false {
		t.Errorf("change %v", done)
	}
}

func TestAClientThatFailsToAskDoesNothing(t *testing.T) {
	platform := &fakeChanges{}
	c := startClient(t, platform.serve(t))
	c.initialize(`{"elicitation":{}}`)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"apps_restart","arguments":{"id":7}}}`)
	asked := c.elicitation()
	id, _ := json.Marshal(asked["id"])
	c.send(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"Method not found"}}`, id)
	result := resultOf(t, c.next())
	if result["isError"] != true || result["content"].([]any)[0].(map[string]any)["text"] != cannotAsk {
		t.Errorf("result %v", result)
	}
	if len(platform.calls()) != 1 {
		t.Errorf("nothing retried: %v", platform.calls())
	}
}

func TestAskingIsCappedAndWaitsNotForever(t *testing.T) {
	platform := &fakeChanges{askForever: true}
	c := startClient(t, platform.serve(t))
	c.initialize(`{"elicitation":{}}`)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"apps_restart","arguments":{"id":7}}}`)
	for range maxRounds {
		c.answerElicitation(c.elicitation(), `{"action":"accept","content":{"confirm":true}}`)
	}
	if result := resultOf(t, c.next()); result["content"].([]any)[0].(map[string]any)["text"] != askedTooMuch {
		t.Errorf("result %v", result)
	}

	saved := confirmWait
	confirmWait = 50 * time.Millisecond
	t.Cleanup(func() { confirmWait = saved })
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"apps_restart","arguments":{"id":7}}}`)
	c.elicitation() // never answered
	if result := resultOf(t, c.next()); result["content"].([]any)[0].(map[string]any)["text"] != noAnswer {
		t.Errorf("result %v", result)
	}
}

func TestStdinClosingEndsAPendingQuestion(t *testing.T) {
	platform := &fakeChanges{}
	c := startClient(t, platform.serve(t))
	c.initialize(`{"elicitation":{}}`)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"apps_restart","arguments":{"id":7}}}`)
	c.elicitation()
	c.in.Close()
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve waited for an answer that cannot come")
	}
	if len(platform.calls()) != 1 {
		t.Errorf("nothing retried: %v", platform.calls())
	}
}
