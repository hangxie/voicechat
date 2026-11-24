package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBackends stands in for the STT, LLM, and TTS services on one test server. Handlers are swapped with set
// between requests; an endpoint without one answers 501.
type fakeBackends struct {
	mu       sync.Mutex
	handlers map[string]http.HandlerFunc
}

const (
	fakeSTT    = "POST /stt/audio/transcriptions"
	fakeLLM    = "POST /llm/chat/completions"
	fakeTTS    = "POST /tts/api/tts"
	fakeHealth = "GET /tts/health"
	fakeVoices = "GET /tts/voices"
)

func (f *fakeBackends) set(pattern string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.handlers == nil {
		f.handlers = map[string]http.HandlerFunc{}
	}
	f.handlers[pattern] = h
}

// startFake serves handlers, keyed by endpoint, and returns a config that points every backend at them.
func startFake(t *testing.T, handlers map[string]http.HandlerFunc) (*fakeBackends, config) {
	t.Helper()
	f := &fakeBackends{handlers: handlers}
	mux := http.NewServeMux()
	for _, pattern := range []string{fakeSTT, fakeLLM, fakeTTS, fakeHealth, fakeVoices} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			h := f.handlers[pattern]
			f.mu.Unlock()
			if h == nil {
				http.Error(w, "not faked", http.StatusNotImplemented)
				return
			}
			h(w, r)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, config{
		sttURL: srv.URL + "/stt", llmURL: srv.URL + "/llm", ttsURL: srv.URL + "/tts",
		sttToken: "stt-token", llmToken: "llm-token", ttsToken: "tts-token",
		llmModel: "test-model", voice: "test-voice", systemPrompt: "Be brief.", llmMaxTokens: 64,
		trusted: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
	}
}

type handlers = map[string]http.HandlerFunc

// sse answers like a streaming OpenAI chat completion with one event per delta.
func sse(deltas ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range deltas {
			data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": d}}}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

// ndjson answers like a streaming TTS: one audio event per piece of PCM, then done.
func ndjson(pieces ...[]byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		for _, p := range pieces {
			_ = enc.Encode(map[string]any{"type": "audio", "pcm": p, "sample_rate": 24000})
		}
		_ = enc.Encode(map[string]any{"type": "done"})
	}
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { http.Error(w, body, code) }
}

// sequence answers the nth request with the nth handler, repeating the last one, and counts requests.
func sequence(count *atomic.Int32, handlers ...http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n := int(count.Add(1)) - 1
		handlers[min(n, len(handlers)-1)](w, r)
	}
}

func TestErrorMessage(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"error": {"message": "model not found"}}`, "model not found"},
		{`{"error": "busy"}`, "busy"},
		{`{"error": {"code": 5}}`, `{"error": {"code": 5}}`},
		{`{"detail": "x"}`, `{"detail": "x"}`},
		{"plain text", "plain text"},
	}
	for _, c := range cases {
		if got := errorMessage([]byte(c.body)); got != c.want {
			t.Errorf("errorMessage(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestTranscribe(t *testing.T) {
	f, cfg := startFake(t, nil)
	f.set(fakeSTT, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer stt-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.FormValue("response_format"); got != "verbose_json" {
			t.Errorf("response_format = %q", got)
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("file: %v", err)
		}
		if data, _ := io.ReadAll(file); string(data) != "RIFF" {
			t.Errorf("file = %q", data)
		}
		_, _ = io.WriteString(w, `{"text": "  hello there \n", "language": "english"}`)
	})
	b := newBackends(cfg)
	got, err := b.transcribe(context.Background(), []byte("RIFF"))
	if err != nil || got != (transcript{"hello there", "english"}) {
		t.Fatalf("transcribe = %+v, %v", got, err)
	}

	f.set(fakeSTT, status(http.StatusBadRequest, `{"error": {"message": "bad audio"}}`))
	_, err = b.transcribe(context.Background(), []byte("RIFF"))
	var be *backendError
	if !errors.As(err, &be) || be.status != http.StatusBadRequest || !strings.Contains(err.Error(), "bad audio") {
		t.Fatalf("transcribe error = %v", err)
	}
}

func TestChat(t *testing.T) {
	f, cfg := startFake(t, nil)
	f.set(fakeLLM, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model     string    `json:"model"`
			Messages  []message `json:"messages"`
			Stream    bool      `json:"stream"`
			MaxTokens int       `json:"max_tokens"`
			Kwargs    struct {
				EnableThinking *bool `json:"enable_thinking"`
			} `json:"chat_template_kwargs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("request: %v", err)
		}
		if r.Header.Get("Authorization") != "Bearer llm-token" || req.Model != "test-model" || !req.Stream ||
			req.MaxTokens != 64 || req.Kwargs.EnableThinking == nil || *req.Kwargs.EnableThinking || len(req.Messages) != 2 {
			t.Errorf("unexpected request %+v", req)
		}
		// Comments, blank lines, and empty deltas are skipped; nothing after [DONE] is read.
		_, _ = fmt.Fprint(w, ": keep-alive\n\n")
		sse("Hello", "", " there.")(w, r)
		_, _ = fmt.Fprint(w, "data: not json\n\n")
	})
	var got []string
	err := newBackends(cfg).chat(context.Background(), []message{{"system", "s"}, {"user", "hi"}}, func(d string) { got = append(got, d) })
	if err != nil || strings.Join(got, "|") != "Hello| there." {
		t.Fatalf("chat = %q, %v", got, err)
	}
}

func TestChatRetry(t *testing.T) {
	cases := []struct {
		name     string
		handlers []http.HandlerFunc
		requests int32
		wantErr  string
	}{
		{"5xx once then ok", []http.HandlerFunc{status(503, "loading"), sse("ok")}, 2, ""},
		{"5xx twice", []http.HandlerFunc{status(500, "boom")}, 2, "LLM returned 500: boom"},
		{"4xx is not retried", []http.HandlerFunc{status(400, `{"error": "too long"}`)}, 1, "LLM returned 400: too long"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var count atomic.Int32
			f, cfg := startFake(t, nil)
			f.set(fakeLLM, sequence(&count, c.handlers...))
			err := newBackends(cfg).chat(context.Background(), nil, func(string) {})
			if count.Load() != c.requests {
				t.Errorf("requests = %d, want %d", count.Load(), c.requests)
			}
			if (err == nil) != (c.wantErr == "") || (err != nil && !strings.Contains(err.Error(), c.wantErr)) {
				t.Errorf("err = %v, want %q", err, c.wantErr)
			}
		})
	}
}

func TestChatBadEvent(t *testing.T) {
	f, cfg := startFake(t, nil)
	f.set(fakeLLM, func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, "data: {oops\n\n") })
	if err := newBackends(cfg).chat(context.Background(), nil, func(string) {}); err == nil {
		t.Fatal("chat accepted a malformed event")
	}
}

func TestChatCancel(t *testing.T) {
	aborted := make(chan struct{})
	f, cfg := startFake(t, nil)
	f.set(fakeLLM, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `data: {"choices": [{"delta": {"content": "first"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(aborted)
	})
	ctx, cancel := context.WithCancel(context.Background())
	err := newBackends(cfg).chat(ctx, nil, func(string) { cancel() })
	if err == nil {
		t.Fatal("chat finished after being cancelled")
	}
	select {
	case <-aborted:
	case <-time.After(5 * time.Second):
		t.Fatal("LLM request was not aborted")
	}
}

func TestSynthesize(t *testing.T) {
	f, cfg := startFake(t, nil)
	b := newBackends(cfg)
	collect := func() ([]audioPiece, error) {
		var pieces []audioPiece
		err := b.synthesize(context.Background(), "Hi.", "v", "english", func(p audioPiece) { pieces = append(pieces, p) })
		return pieces, err
	}

	f.set(fakeTTS, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.Header.Get("Authorization") != "Bearer tts-token" || req["text"] != "Hi." || req["voice"] != "v" ||
			req["language"] != "english" || req["stream"] != true {
			t.Errorf("unexpected request %v", req)
		}
		ndjson([]byte{1, 2}, []byte{3, 4, 5, 6})(w, r)
	})
	pieces, err := collect()
	if err != nil || len(pieces) != 2 || string(pieces[1].pcm) != "\x03\x04\x05\x06" || pieces[0].sampleRate != 24000 {
		t.Fatalf("streamed = %+v, %v", pieces, err)
	}

	f.set(fakeTTS, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("X-RTF", "0.31")
		w.Header().Set("X-Audio-Seconds", "1.5")
		_, _ = io.WriteString(w, "RIFFwav")
	})
	pieces, err = collect()
	if err != nil || len(pieces) != 1 || string(pieces[0].wav) != "RIFFwav" || pieces[0].rtf != "0.31" || pieces[0].seconds != "1.5" {
		t.Fatalf("wav = %+v, %v", pieces, err)
	}

	f.set(fakeTTS, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = fmt.Fprintln(w, `{"type": "error", "error": "out of memory"}`)
	})
	if _, err = collect(); err == nil || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("error event = %v", err)
	}

	f.set(fakeTTS, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = fmt.Fprintln(w, `{"type": "audio", "pcm": "AQI=", "sample_rate": 24000}`)
	})
	if _, err = collect(); err == nil || !strings.Contains(err.Error(), "without finishing") {
		t.Fatalf("truncated stream = %v", err)
	}

	f.set(fakeTTS, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = fmt.Fprintln(w, `not json`)
	})
	if _, err = collect(); err == nil {
		t.Fatal("malformed stream accepted")
	}
}

func TestSynthesizeBusy(t *testing.T) {
	var count atomic.Int32
	f, cfg := startFake(t, nil)
	f.set(fakeTTS, sequence(&count, status(409, "busy"), status(503, "busy"), ndjson([]byte{1, 2})))
	b := newBackends(cfg)
	pieces := 0
	if err := b.synthesize(context.Background(), "Hi.", "v", "auto", func(audioPiece) { pieces++ }); err != nil || pieces != 1 {
		t.Fatalf("synthesize = %d pieces, %v", pieces, err)
	}
	if count.Load() != 3 {
		t.Errorf("requests = %d, want 3", count.Load())
	}

	count.Store(0)
	f.set(fakeTTS, sequence(&count, status(500, "broken")))
	if err := b.synthesize(context.Background(), "Hi.", "v", "auto", func(audioPiece) {}); err == nil || count.Load() != 1 {
		t.Fatalf("a 500 was retried or accepted: %d requests, %v", count.Load(), err)
	}

	// A cancel while waiting for a busy TTS ends the wait.
	count.Store(0)
	f.set(fakeTTS, sequence(&count, status(409, "busy")))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := b.synthesize(ctx, "Hi.", "v", "auto", func(audioPiece) {}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy wait err = %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("busy wait ignored the cancel")
	}
}

func TestTTSLanguages(t *testing.T) {
	var count atomic.Int32
	f, cfg := startFake(t, nil)
	f.set(fakeHealth, sequence(&count, status(502, "down"), func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"languages": ["english", "chinese"]}`)
	}))
	b := newBackends(cfg)
	ctx := context.Background()

	if got := b.ttsLanguages(ctx); got != nil {
		t.Fatalf("languages after a failed lookup = %v", got)
	}
	if b.ttsLanguages(ctx); count.Load() != 1 {
		t.Fatalf("failed lookup retried within %v", languagesRetry)
	}
	b.languagesAt = time.Now().Add(-languagesRetry)
	if got := b.ttsLanguages(ctx); !got["chinese"] || got["french"] {
		t.Fatalf("languages = %v", got)
	}
	if b.ttsLanguages(ctx); count.Load() != 2 {
		t.Fatalf("languages looked up again after success: %d requests", count.Load())
	}
}

func TestVoices(t *testing.T) {
	f, cfg := startFake(t, nil)
	f.set(fakeVoices, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tts-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `["a", "b"]`)
	})
	b := newBackends(cfg)
	if got, err := b.voices(context.Background()); err != nil || strings.Join(got, ",") != "a,b" {
		t.Fatalf("voices = %v, %v", got, err)
	}
	f.set(fakeVoices, status(500, "x"))
	if _, err := b.voices(context.Background()); err == nil {
		t.Fatal("voices accepted a 500")
	}
}
