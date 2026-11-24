package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// event is one JSON frame from the server, with the binary frame that follows an audio event.
type event struct {
	Type     string             `json:"type"`
	Turn     int64              `json:"turn"`
	Stage    string             `json:"stage"`
	Message  string             `json:"message"`
	Text     string             `json:"text"`
	Delta    string             `json:"delta"`
	Language *string            `json:"language"`
	Reply    string             `json:"reply"`
	Timings  map[string]float64 `json:"timings"`
	Format   string             `json:"format"`
	Index    int                `json:"index"`
	Part     int                `json:"part"`
	Rate     int                `json:"sample_rate"`
	Seconds  float64            `json:"seconds"`
	binary   []byte
	raw      map[string]any
}

type client struct {
	t    *testing.T
	conn *websocket.Conn
}

// dial serves the voice chat against the fake backends and connects to its WebSocket.
func dial(t *testing.T, cfg config) *client {
	t.Helper()
	srv := httptest.NewServer(newHandler(cfg))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.SetReadLimit(1 << 20)
	t.Cleanup(func() { _ = conn.CloseNow() })
	return &client{t, conn}
}

func (c *client) send(kind websocket.MessageType, data []byte) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.conn.Write(ctx, kind, data); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *client) sendJSON(v any) {
	c.t.Helper()
	data, _ := json.Marshal(v)
	c.send(websocket.MessageText, data)
}

func (c *client) next() event {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	kind, data, err := c.conn.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var e event
	if kind != websocket.MessageText || json.Unmarshal(data, &e) != nil || json.Unmarshal(data, &e.raw) != nil {
		c.t.Fatalf("unexpected frame %v %q", kind, data)
	}
	if e.Type == "audio" {
		if kind, e.binary, err = c.conn.Read(ctx); err != nil || kind != websocket.MessageBinary {
			c.t.Fatalf("audio event without a binary frame: %v %v", kind, err)
		}
	}
	return e
}

// until reads events up to and including the done of turn id.
func (c *client) until(id int64) []event {
	c.t.Helper()
	var events []event
	for {
		e := c.next()
		events = append(events, e)
		if e.Type == "done" && e.Turn == id {
			return events
		}
	}
}

func types(events []event) string {
	var out []string
	for _, e := range events {
		out = append(out, e.Type)
	}
	return strings.Join(out, " ")
}

// recorder keeps the bodies of the requests a fake backend received.
type recorder struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (r *recorder) wrap(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.mu.Unlock()
		h(w, req)
	}
}

func (r *recorder) all() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.bodies...)
}

func languages(names ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"languages": names})
	}
}

// blocking streams one delta, then holds the request open until the client aborts it, which closes aborted.
func blocking(aborted chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `data: {"choices": [{"delta": {"content": "Let me think"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(aborted)
	}
}

func waitClosed(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was not aborted", what)
	}
}

func TestRejects(t *testing.T) {
	_, cfg := startFake(t, nil)
	noVoice := cfg
	noVoice.voice = ""
	cases := []struct {
		name    string
		cfg     config
		frames  []any // a string is a text frame as is, []byte a binary frame, anything else JSON
		turn    int64
		problem string
	}{
		{"binary frame", cfg, []any{[]byte("RIFF")}, 0, "unexpected binary frame"},
		{"not JSON", cfg, []any{"{oops"}, 0, "invalid message"},
		{"unknown type", cfg, []any{map[string]any{"type": "hello", "id": 5}}, 5, `unknown message type "hello"`},
		{"empty turn", cfg, []any{map[string]any{"type": "turn", "id": 6, "text": "  "}}, 6, "Send audio or text"},
		{"no voice", noVoice, []any{map[string]any{"type": "turn", "id": 7, "text": "hi"}}, 7, "Select a voice"},
		{"audio as text", cfg, []any{map[string]any{"type": "turn", "id": 8, "audio": true}, "{}"}, 8, "expected the recording"},
		{"empty audio", cfg, []any{map[string]any{"type": "turn", "id": 9, "audio": true}, []byte{}}, 9, "expected the recording"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws := dial(t, c.cfg)
			for _, f := range c.frames {
				switch v := f.(type) {
				case string:
					ws.send(websocket.MessageText, []byte(v))
				case []byte:
					ws.send(websocket.MessageBinary, v)
				default:
					ws.sendJSON(v)
				}
			}
			events := ws.until(c.turn)
			if len(events) != 2 || events[0].Type != "error" || events[0].Stage != "request" ||
				events[0].Turn != c.turn || !strings.Contains(events[0].Message, c.problem) {
				t.Errorf("events = %+v, want a request error containing %q", events, c.problem)
			}
		})
	}
}

func TestCancel(t *testing.T) {
	aborted := make(chan struct{})
	f, cfg := startFake(t, handlers{fakeLLM: blocking(aborted)})
	c := dial(t, cfg)
	c.sendJSON(map[string]any{"type": "turn", "id": 10, "text": "Tell me a long story"})
	if e := c.next(); e.Type != "text" || e.Turn != 10 {
		t.Fatalf("first event = %+v", e)
	}
	c.sendJSON(map[string]any{"type": "cancel"})
	waitClosed(t, aborted, "the cancelled turn's LLM request")

	// The cancelled turn sends nothing more, not even done; the next turn runs normally.
	f.set(fakeLLM, sse("Okay."))
	f.set(fakeTTS, ndjson([]byte{1, 0}))
	c.sendJSON(map[string]any{"type": "turn", "id": 11, "text": "Never mind"})
	for _, e := range c.until(11) {
		if e.Turn != 11 {
			t.Errorf("event %s for cancelled turn %d", e.Type, e.Turn)
		}
	}
}

func TestNewTurnInterrupts(t *testing.T) {
	aborted := make(chan struct{})
	f, cfg := startFake(t, handlers{fakeLLM: blocking(aborted)})
	c := dial(t, cfg)
	c.sendJSON(map[string]any{"type": "turn", "id": 12, "text": "Tell me a long story"})
	if e := c.next(); e.Type != "text" {
		t.Fatalf("first event = %+v", e)
	}
	f.set(fakeLLM, sse("Sure."))
	f.set(fakeTTS, ndjson([]byte{1, 0}))
	c.sendJSON(map[string]any{"type": "turn", "id": 13, "text": "Actually, something short"})
	waitClosed(t, aborted, "the interrupted turn's LLM request")
	for _, e := range c.until(13) {
		if e.Turn != 13 {
			t.Errorf("event %s for interrupted turn %d", e.Type, e.Turn)
		}
	}
}

func TestDisconnectCancelsTurn(t *testing.T) {
	aborted := make(chan struct{})
	_, cfg := startFake(t, handlers{fakeLLM: blocking(aborted)})
	c := dial(t, cfg)
	c.sendJSON(map[string]any{"type": "turn", "id": 14, "text": "Tell me a long story"})
	c.next()
	_ = c.conn.CloseNow()
	waitClosed(t, aborted, "a dropped connection's LLM request")
}
