package mcpbridge

// A change the platform offers only to a client that can ask its user to
// confirm it, in the 2026-07-28 way: the call answers "input_required" with a
// form and a signed requestState, and the client retries it with the answer.
// A 2025 client asks its user differently — a server→client request,
// elicitation/create, over this same stdio — so for one that declared it can,
// the bridge speaks 2026-07-28 to the platform on its behalf and puts each form
// to the client in the 2025 way. The answer comes from the client's form only:
// whatever a request carries as inputResponses or requestState is dropped
// before the platform sees it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// modernVersion is the version the bridge speaks for a 2025 client that can ask.
const modernVersion = "2026-07-28"

const metaCapabilities = "io.modelcontextprotocol/clientCapabilities"
const metaClientInfo = "io.modelcontextprotocol/clientInfo"

// maxRounds caps how often one call may ask again (a production app's name
// typed wrong asks once more each time).
const maxRounds = 5

// confirmWait is how long a question waits for the user: the platform's
// requestState lapses after ten minutes anyway.
var confirmWait = 10 * time.Minute

// What the tool answers when no confirmation came; nothing was done either way.
const (
	cannotAsk    = "Your MCP client could not ask you to confirm this change, so nothing was done."
	noAnswer     = "No answer to the confirmation in time, so nothing was done."
	askedTooMuch = "The confirmation was asked too many times, so nothing was done."
)

// asking is the bridge's side of a 2025 client's confirmations.
type asking struct {
	mu         sync.Mutex
	can        bool            // initialize declared a form-capable elicitation
	clientInfo json.RawMessage // initialize's clientInfo, passed on in _meta
	next       int
	waiting    map[string]chan reply // our request id → the client's answer
	gone       chan struct{}         // closed when stdin ends: no answer will come
	goneOnce   sync.Once
}

// reply is the client's answer to one of the bridge's requests.
type reply struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (a *asking) done() chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.gone == nil {
		a.gone = make(chan struct{})
	}
	return a.gone
}

func (a *asking) stop() {
	gone := a.done()
	a.goneOnce.Do(func() { close(gone) })
}

// noteInitialize keeps whether a 2025 client can put a form to its user:
// capabilities.elicitation empty (2025-06-18) or with "form" (2025-11-25).
func (a *asking) noteInitialize(msg *mcpMessage) {
	var elicitation map[string]json.RawMessage
	raw, declared := msg.Params.Capabilities["elicitation"]
	can := declared && json.Unmarshal(raw, &elicitation) == nil && elicitation != nil
	if _, form := elicitation["form"]; can && len(elicitation) > 0 && !form {
		can = false // URL mode only: no form to confirm in
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.can = can
	a.clientInfo = msg.Params.ClientInfo
}

func (a *asking) canAsk() (bool, json.RawMessage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.can, a.clientInfo
}

// answered hands the client's answer to the bridge's request it answers, and
// reports whether msg was one: such an answer is the bridge's, never the platform's.
func (a *asking) answered(msg *mcpMessage, raw []byte) bool {
	if msg.Method != "" || len(msg.ID) == 0 {
		return false
	}
	var id string
	if json.Unmarshal(msg.ID, &id) != nil {
		return false
	}
	a.mu.Lock()
	ch, ok := a.waiting[id]
	delete(a.waiting, id)
	a.mu.Unlock()
	if !ok {
		return false
	}
	var answer reply
	_ = json.Unmarshal(raw, &answer)
	ch <- answer
	return true
}

// confirming takes over msg when it is a 2025 tools/list or tools/call from a
// client that can ask, and reports whether it did.
func (b *Bridge) confirming(msg *mcpMessage, raw []byte) bool {
	if msg.Method == "initialize" {
		b.asking.noteInitialize(msg)
		return false
	}
	can, clientInfo := b.asking.canAsk()
	if !can || !msg.request || (msg.Method != "tools/list" && msg.Method != "tools/call") {
		return false
	}
	if _, modern := msg.Params.Meta[metaVersion]; modern {
		return false // a 2026-07-28 request runs its own round trip
	}
	var envelope map[string]json.RawMessage
	params := map[string]json.RawMessage{}
	if json.Unmarshal(raw, &envelope) != nil {
		return false
	}
	// A request may leave params out (the TypeScript SDK's tools/list does).
	if sent := envelope["params"]; len(sent) > 0 && !bytes.Equal(sent, []byte("null")) {
		if json.Unmarshal(sent, &params) != nil || params == nil {
			return false // not a shape to rewrite: let the platform judge it as sent
		}
	}
	// Only the form the bridge itself puts to the user may confirm.
	delete(params, "inputResponses")
	delete(params, "requestState")
	params["_meta"] = modernMeta(params["_meta"], clientInfo)
	envelope["params"] = mustJSON(params)
	if msg.Method == "tools/list" {
		answer, err := b.exchange(envelope)
		if err != nil {
			b.fail(msg, err.Error())
			return true
		}
		b.write(asLegacy(answer))
		return true
	}
	b.callConfirmed(msg, envelope, params)
	return true
}

// modernMeta is the request's own _meta with what a 2026-07-28 request names:
// its version, the client, and that it can ask in a form.
func modernMeta(raw, clientInfo json.RawMessage) json.RawMessage {
	meta := map[string]json.RawMessage{}
	_ = json.Unmarshal(raw, &meta)
	meta[metaVersion] = mustJSON(modernVersion)
	meta[metaCapabilities] = json.RawMessage(`{"elicitation":{"form":{}}}`)
	if len(clientInfo) > 0 {
		meta[metaClientInfo] = clientInfo
	}
	return mustJSON(meta)
}

// callConfirmed runs a tools/call to its end: each "input_required" goes to the
// user as a form, and the call is retried with the answer, until it completes.
func (b *Bridge) callConfirmed(msg *mcpMessage, envelope, params map[string]json.RawMessage) {
	for round := 0; ; round++ {
		answer, err := b.exchange(envelope)
		if err != nil {
			b.fail(msg, err.Error())
			return
		}
		var required struct {
			Result struct {
				ResultType    string                     `json:"resultType"`
				InputRequests map[string]json.RawMessage `json:"inputRequests"`
				RequestState  json.RawMessage            `json:"requestState"`
			} `json:"result"`
		}
		if json.Unmarshal(answer, &required) != nil || required.Result.ResultType != "input_required" {
			b.write(asLegacy(answer))
			return
		}
		if round == maxRounds {
			b.toolError(msg, askedTooMuch)
			return
		}
		responses, refusal := b.ask(required.Result.InputRequests)
		if refusal != "" {
			b.toolError(msg, refusal)
			return
		}
		params["inputResponses"] = mustJSON(responses)
		params["requestState"] = required.Result.RequestState
		envelope["params"] = mustJSON(params)
	}
}

// ask puts each form the platform sent to the user, and returns their answers
// by the platform's keys — or why there are none.
func (b *Bridge) ask(requests map[string]json.RawMessage) (map[string]json.RawMessage, string) {
	responses := map[string]json.RawMessage{}
	for key, raw := range requests {
		var request struct {
			Method string                     `json:"method"`
			Params map[string]json.RawMessage `json:"params"`
		}
		if json.Unmarshal(raw, &request) != nil || request.Method != "elicitation/create" {
			return nil, cannotAsk
		}
		var mode string
		_ = json.Unmarshal(request.Params["mode"], &mode)
		if mode != "" && mode != "form" {
			return nil, cannotAsk
		}
		// The 2025 shape: a message and a flat schema; the mode is the default.
		form := map[string]json.RawMessage{
			"message": request.Params["message"], "requestedSchema": request.Params["requestedSchema"],
		}
		answer, refusal := b.askClient(form)
		if refusal != "" {
			return nil, refusal
		}
		responses[key] = answer
	}
	return responses, ""
}

// askClient sends the client an elicitation/create and waits for its answer.
func (b *Bridge) askClient(form map[string]json.RawMessage) (json.RawMessage, string) {
	gone := b.asking.done()
	answer := make(chan reply, 1)
	b.asking.mu.Lock()
	b.asking.next++
	id := fmt.Sprintf("cw-confirm-%d", b.asking.next)
	if b.asking.waiting == nil {
		b.asking.waiting = map[string]chan reply{}
	}
	b.asking.waiting[id] = answer
	b.asking.mu.Unlock()
	forget := func() {
		b.asking.mu.Lock()
		delete(b.asking.waiting, id)
		b.asking.mu.Unlock()
	}
	b.write(mustJSON(map[string]any{"jsonrpc": "2.0", "id": id, "method": "elicitation/create", "params": form}))
	timer := time.NewTimer(confirmWait)
	defer timer.Stop()
	select {
	case got := <-answer:
		if len(got.Error) > 0 || len(got.Result) == 0 {
			return nil, cannotAsk
		}
		return got.Result, ""
	case <-timer.C:
		forget()
		return nil, noAnswer
	case <-gone:
		forget()
		return nil, noAnswer
	}
}

// exchange posts one 2026-07-28 request and returns the platform's answer to
// it; a notification it streams on the way is passed to the client.
func (b *Bridge) exchange(envelope map[string]json.RawMessage) ([]byte, error) {
	raw := mustJSON(envelope)
	msg, err := parseMessage(raw)
	if err != nil {
		return nil, err
	}
	req, err := b.request(msg, raw)
	if err != nil {
		return nil, err
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %v", b.Endpoint, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		for _, event := range events(body) {
			if b.isAnswer(msg, event) {
				return event, nil
			}
			b.write(event)
		}
	} else if b.isAnswer(msg, body) {
		return body, nil
	}
	return nil, fmt.Errorf("%s answered HTTP %d, not an MCP answer to this request — is the URL right?",
		b.Endpoint, resp.StatusCode)
}

// events are the data of each event in an SSE body.
func events(body []byte) [][]byte {
	var out [][]byte
	var data []string
	flush := func() {
		if len(data) > 0 {
			out = append(out, []byte(strings.Join(data, "\n")))
			data = data[:0]
		}
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			flush()
		}
	}
	flush()
	return out
}

// asLegacy is a 2026-07-28 answer as a 2025 client reads it: no resultType.
func asLegacy(answer []byte) []byte {
	var envelope map[string]json.RawMessage
	var result map[string]json.RawMessage
	if json.Unmarshal(answer, &envelope) != nil || json.Unmarshal(envelope["result"], &result) != nil || result == nil {
		return answer
	}
	delete(result, "resultType")
	envelope["result"] = mustJSON(result)
	return mustJSON(envelope)
}

// toolError answers msg with a failed tool call the model can read.
func (b *Bridge) toolError(msg *mcpMessage, text string) {
	b.write(mustJSON(map[string]any{
		"jsonrpc": "2.0", "id": msg.ID,
		"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": true},
	}))
}

func mustJSON(value any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		panic(err) // only values this file builds: maps of raw JSON and strings
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}
