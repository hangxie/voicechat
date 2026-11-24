package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestTextTurn(t *testing.T) {
	var llm, tts recorder
	_, cfg := startFake(t, handlers{
		fakeLLM:    llm.wrap(sse("Sure, the movie is about", " volleyball. Do you want", " to talk about it?")),
		fakeTTS:    tts.wrap(ndjson([]byte{1, 0, 2, 0}, []byte{3, 0})),
		fakeHealth: languages("english"),
	})
	c := dial(t, cfg)
	history := []message{{"user", "Hi"}, {"assistant", "Hello!"}}
	c.sendJSON(map[string]any{"type": "turn", "id": 1, "text": "Tell me about the movie", "history": history})
	events := c.until(1)

	var reply strings.Builder
	var chunks []string
	for _, e := range events {
		if e.Turn != 1 {
			t.Errorf("event for turn %d", e.Turn)
		}
		switch e.Type {
		case "text":
			reply.WriteString(e.Delta)
		case "audio":
			if e.Format != "pcm" || e.Rate != 24000 || len(e.binary) == 0 {
				t.Errorf("audio event %+v", e)
			}
			if e.Part == 0 {
				chunks = append(chunks, e.Text)
			}
		case "error":
			t.Errorf("error %s: %s", e.Stage, e.Message)
		}
	}
	done := events[len(events)-1]
	if done.Reply != reply.String() || done.Reply != "Sure, the movie is about volleyball. Do you want to talk about it?" {
		t.Errorf("reply = %q, deltas = %q", done.Reply, reply.String())
	}
	for _, mark := range []string{"llm_first_token", "first_audio", "llm_done"} {
		if _, ok := done.Timings[mark]; !ok {
			t.Errorf("timings lack %s: %v", mark, done.Timings)
		}
	}
	if want := []string{"Sure, the movie is about volleyball.", "Do you want to talk about it?"}; strings.Join(chunks, "|") != strings.Join(want, "|") {
		t.Errorf("chunks = %q, want %q", chunks, want)
	}

	// The LLM sees the system prompt, the history, and the new message with its language note.
	raw, _ := json.Marshal(llm.all()[0]["messages"])
	var sent []message
	_ = json.Unmarshal(raw, &sent)
	messages, _ := json.Marshal(sent)
	want, _ := json.Marshal([]message{
		{"system", "Be brief."},
		history[0], history[1],
		{"user", withLanguage("Tell me about the movie", "")},
	})
	if string(messages) != string(want) {
		t.Errorf("messages = %s\nwant %s", messages, want)
	}
	// Each chunk is spoken in the configured voice; with no detected language, TTS picks one.
	for _, body := range tts.all() {
		if body["voice"] != "test-voice" || body["language"] != "auto" {
			t.Errorf("TTS request %v", body)
		}
	}
	if n := len(tts.all()); n != 2 {
		t.Errorf("TTS requests = %d, want 2", n)
	}
}

func TestAudioTurn(t *testing.T) {
	var tts recorder
	f, cfg := startFake(t, handlers{
		fakeSTT: func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"text": " Good morning. ", "language": "english"}`)
		},
		fakeLLM:    sse("Morning!"),
		fakeTTS:    tts.wrap(ndjson([]byte{1, 0})),
		fakeHealth: languages("english", "chinese"),
	})
	c := dial(t, cfg)
	c.sendJSON(map[string]any{"type": "turn", "id": 2, "audio": true, "voice": "other"})
	c.send(websocket.MessageBinary, []byte("RIFF...."))
	events := c.until(2)
	if got := types(events); got != "transcript text audio done" {
		t.Fatalf("events = %s", got)
	}
	if e := events[0]; e.Text != "Good morning." || e.Language == nil || *e.Language != "english" {
		t.Errorf("transcript = %+v", e)
	}
	if _, ok := events[3].Timings["stt"]; !ok {
		t.Errorf("timings lack stt: %v", events[3].Timings)
	}
	if body := tts.all()[0]; body["voice"] != "other" || body["language"] != "english" {
		t.Errorf("TTS request %v", body)
	}

	// Silence transcribes to nothing, which ends the turn without asking the LLM.
	f.set(fakeSTT, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"text": "  "}`) })
	f.set(fakeLLM, status(500, "should not be called"))
	c.sendJSON(map[string]any{"type": "turn", "id": 3, "audio": true})
	c.send(websocket.MessageBinary, []byte("RIFF...."))
	if got := types(c.until(3)); got != "transcript done" {
		t.Errorf("silent turn events = %s", got)
	}
}

func TestTurnFailures(t *testing.T) {
	cases := []struct {
		name   string
		fakes  handlers
		audio  bool
		events string
		stage  string
	}{
		{"stt", handlers{fakeSTT: status(500, "stt down")}, true, "error done", "stt"},
		{"llm", handlers{fakeLLM: status(400, "context too long")}, false, "error done", "llm"},
		// After a TTS failure the rest of the reply still streams as text, and the turn still ends.
		{"tts", handlers{fakeLLM: sse("One. ", "Two. ", "Three."), fakeTTS: status(500, "tts down")}, false, "text error text text done", "tts"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, cfg := startFake(t, c.fakes)
			ws := dial(t, cfg)
			ws.sendJSON(map[string]any{"type": "turn", "id": 4, "text": "hi", "audio": c.audio})
			if c.audio {
				ws.send(websocket.MessageBinary, []byte("RIFF"))
			}
			events := ws.until(4)
			var stages []string
			for _, e := range events {
				if e.Type == "error" {
					stages = append(stages, e.Stage)
				}
			}
			// Text events may arrive before or after the error, so compare counts rather than order.
			if len(events) != len(strings.Fields(c.events)) || strings.Join(stages, ",") != c.stage {
				t.Errorf("events = %s, error stages = %v; want %s with stage %s", types(events), stages, c.events, c.stage)
			}
		})
	}
}

func TestWAVTurn(t *testing.T) {
	_, cfg := startFake(t, handlers{
		fakeLLM: sse("Hi there."),
		fakeTTS: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "audio/wav")
			w.Header().Set("X-RTF", "0.25")
			_, _ = io.WriteString(w, "RIFFwav")
		},
	})
	c := dial(t, cfg)
	c.sendJSON(map[string]any{"type": "turn", "id": 15, "text": "hello"})
	events := c.until(15)
	if got := types(events); got != "text audio done" {
		t.Fatalf("events = %s", got)
	}
	e := events[1]
	if e.Format != "wav" || string(e.binary) != "RIFFwav" || e.Text != "Hi there." || e.raw["rtf"] != 0.25 {
		t.Errorf("audio event = %+v", e)
	}
	// A header the backend left out is null rather than zero.
	if seconds, ok := e.raw["seconds"]; !ok || seconds != nil {
		t.Errorf("seconds = %v, %v; want null", seconds, ok)
	}
}
