// Package mcpbridge serves the platform's MCP tools to a local AI client over
// stdio: each line on stdin is one JSON-RPC message, posted to <platform>/mcp
// with the token; each answer goes back as one line on stdout. For a 2025
// client that can show its user a form, the bridge also runs the platform's
// confirmation of a change with it (confirm.go).
package mcpbridge

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// metaVersion is where a 2026-07-28 request names its protocol version.
const metaVersion = "io.modelcontextprotocol/protocolVersion"

// mcpNamed are the methods whose params.name (or uri) the Mcp-Name header repeats.
var mcpNamed = map[string]bool{"tools/call": true, "prompts/get": true, "resources/read": true}

// Bridge forwards one client's messages to one platform's /mcp.
type Bridge struct {
	Endpoint  string // <platform>/mcp
	Token     string
	HTTP      *http.Client
	UserAgent string
	Out, Log  io.Writer
	mu        sync.Mutex // guards Out, Log and version
	// version is what a 2025 client agreed in initialize; later requests repeat it.
	version string
	asking  asking // a 2025 client's confirmations (confirm.go)
}

// Serve forwards every message until stdin closes, answering them concurrently
// (a client may run several tools at once).
func (b *Bridge) Serve(in io.Reader) error {
	reader := bufio.NewReaderSize(in, 1<<20)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer b.asking.stop() // first: a question still open will get no answer now
	for {
		line, err := reader.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			wg.Add(1)
			go func(message []byte) {
				defer wg.Done()
				b.forward(message)
			}(line)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type mcpMessage struct {
	ID     json.RawMessage
	Method string
	// request: an id and a method — the one kind of message that gets an
	// answer. A notification (no id) or the client's answer to the
	// platform (an id, no method) never does.
	request bool
	Params  struct {
		Name            string
		URI             string
		ProtocolVersion string
		Meta            map[string]json.RawMessage
		// initialize's: whether the client can show a form, and who it is.
		Capabilities map[string]json.RawMessage
		ClientInfo   json.RawMessage
	}
}

// parseMessage reads what the bridge needs of a message, leniently: an id is
// never lost to params of a shape the bridge does not know — the platform
// judges those, and its answer carries the id back.
func parseMessage(raw []byte) (*mcpMessage, error) {
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method json.RawMessage `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	msg := &mcpMessage{ID: envelope.ID, request: len(envelope.ID) > 0 && len(envelope.Method) > 0}
	_ = json.Unmarshal(envelope.Method, &msg.Method)
	var params map[string]json.RawMessage
	if json.Unmarshal(envelope.Params, &params) == nil {
		_ = json.Unmarshal(params["name"], &msg.Params.Name)
		_ = json.Unmarshal(params["uri"], &msg.Params.URI)
		_ = json.Unmarshal(params["protocolVersion"], &msg.Params.ProtocolVersion)
		_ = json.Unmarshal(params["_meta"], &msg.Params.Meta)
		_ = json.Unmarshal(params["capabilities"], &msg.Params.Capabilities)
		msg.Params.ClientInfo = params["clientInfo"]
	}
	return msg, nil
}

func (b *Bridge) forward(raw []byte) {
	if !json.Valid(raw) {
		b.reply(nil, -32700, "Parse error")
		return
	}
	msg, err := parseMessage(raw)
	if err != nil { // valid JSON, but no single message: a batch, a number
		b.reply(nil, -32600, "Invalid Request: one JSON-RPC message object per line")
		return
	}
	if b.asking.answered(msg, raw) || b.confirming(msg, raw) {
		return
	}
	req, err := b.request(msg, raw)
	if err != nil {
		b.fail(msg, err.Error())
		return
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		b.fail(msg, fmt.Sprintf("cannot reach %s: %v", b.Endpoint, err))
		return
	}
	defer resp.Body.Close()
	b.relay(msg, resp)
}

// request is the POST that carries raw to the platform, with the headers MCP
// repeats from the body.
func (b *Bridge) request(msg *mcpMessage, raw []byte) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, b.Endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", b.UserAgent)
	if msg.Method != "" {
		req.Header.Set("Mcp-Method", msg.Method)
	}
	if mcpNamed[msg.Method] {
		req.Header.Set("Mcp-Name", headerSafe(msg.Params.Name+msg.Params.URI))
	}
	if v := b.versionFor(msg); v != "" {
		req.Header.Set("MCP-Protocol-Version", v)
	}
	return req, nil
}

// fail answers a request with an internal error. Nothing may answer a
// notification or a response, so their failure goes to the log.
func (b *Bridge) fail(msg *mcpMessage, reason string) {
	if !msg.request {
		b.logf("cw mcp: %s: %s", describe(msg), reason)
		return
	}
	b.reply(msg.ID, -32603, reason)
}

func describe(msg *mcpMessage) string {
	if msg.Method != "" {
		return msg.Method
	}
	return "a response"
}

// versionFor is the MCP-Protocol-Version header: the request's own _meta version
// (2026-07-28), the one initialize offers, or the one initialize agreed.
func (b *Bridge) versionFor(msg *mcpMessage) string {
	var v string
	if raw, ok := msg.Params.Meta[metaVersion]; ok && json.Unmarshal(raw, &v) == nil && v != "" {
		return v
	}
	if msg.Method == "initialize" {
		return msg.Params.ProtocolVersion
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.version
}

// relay passes the platform's answer to msg on. A request always gets one:
// the platform's own, when it is a JSON-RPC response for msg's id, or an
// error saying what came instead.
func (b *Bridge) relay(msg *mcpMessage, resp *http.Response) {
	if !msg.request {
		if resp.StatusCode >= 300 {
			b.logf("cw mcp: %s refused %s (HTTP %d)", b.Endpoint, describe(msg), resp.StatusCode)
		}
		return
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b.relayEvents(msg, resp.Body)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if !b.isAnswer(msg, body) {
		b.reply(msg.ID, -32603, fmt.Sprintf("%s answered HTTP %d, not an MCP answer to this request — is the URL right?",
			b.Endpoint, resp.StatusCode))
		return
	}
	b.write(body)
}

// relayEvents passes on each message of an SSE response, in order, and
// answers the request itself when the stream ends — or breaks — without
// the platform's answer.
func (b *Bridge) relayEvents(msg *mcpMessage, body io.Reader) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 32<<20)
	answered := false
	var data []string
	flush := func() {
		if len(data) == 0 {
			return
		}
		event := []byte(strings.Join(data, "\n")) // an event's data lines, joined as SSE joins them
		data = data[:0]
		answered = b.isAnswer(msg, event) || answered
		b.write(event)
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			flush()
		}
	}
	if err := scanner.Err(); err != nil {
		// A cut-off event is not passed on: half a message is no message.
		b.reply(msg.ID, -32603, fmt.Sprintf("the stream from %s broke before its answer: %v", b.Endpoint, err))
		return
	}
	flush()
	if !answered {
		b.reply(msg.ID, -32603, fmt.Sprintf("the stream from %s ended without an answer to this request", b.Endpoint))
	}
}

// isAnswer reports whether message is the JSON-RPC response to msg — its id,
// and a result or an error — and keeps the protocol version initialize agreed.
func (b *Bridge) isAnswer(msg *mcpMessage, message []byte) bool {
	var answer struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(message, &answer) != nil || !sameID(answer.ID, msg.ID) || (answer.Result == nil && answer.Error == nil) {
		return false
	}
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if msg.Method == "initialize" && json.Unmarshal(answer.Result, &result) == nil && result.ProtocolVersion != "" {
		b.mu.Lock()
		b.version = result.ProtocolVersion
		b.mu.Unlock()
	}
	return true
}

// sameID compares two JSON-RPC ids as JSON values: 7 is 7, "7" is not.
func sameID(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	return json.Compact(&ca, a) == nil && json.Compact(&cb, b) == nil && bytes.Equal(ca.Bytes(), cb.Bytes())
}

func (b *Bridge) reply(id json.RawMessage, code int, message string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message},
	})
	b.write(out)
}

func (b *Bridge) logf(format string, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fmt.Fprintf(b.Log, format+"\n", args...)
}

// write puts one message on stdout as one line, as the stdio transport requires.
func (b *Bridge) write(message []byte) {
	var line bytes.Buffer
	if err := json.Compact(&line, message); err != nil {
		b.logf("cw mcp: dropped a malformed answer: %v", err)
		return
	}
	line.WriteByte('\n')
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Out.Write(line.Bytes())
}

// headerSafe is a value as MCP carries it in a header: plain printable ASCII
// as is, anything else in the "=?base64?…?=" form.
func headerSafe(value string) string {
	plain := value == strings.TrimSpace(value) &&
		!(strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?="))
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			plain = false
			break
		}
	}
	if plain {
		return value
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
}
