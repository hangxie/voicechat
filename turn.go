package main

import (
	"context"
	"errors"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// turnTimings are seconds from when the turn arrived, shared by the LLM and TTS workers.
type turnTimings struct {
	start time.Time
	mu    sync.Mutex
	marks map[string]float64
}

// mark records name the first time it is called and returns its value.
func (t *turnTimings) mark(name string) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if v, ok := t.marks[name]; ok {
		return v
	}
	v := round2(time.Since(t.start).Seconds())
	t.marks[name] = v
	return v
}

func (t *turnTimings) has(name string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.marks[name]
	return ok
}

func (t *turnTimings) snapshot() map[string]float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]float64, len(t.marks))
	for k, v := range t.marks {
		out[k] = v
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// turn is one exchange: speech to text, the reply streamed from the LLM and cut into chunks, and each chunk
// synthesized in order while later ones are still being generated. Cancelling ctx stops every stage.
type turn struct {
	s       *session
	ctx     context.Context
	msg     clientMessage
	timings *turnTimings
}

func (s *session) runTurn(ctx context.Context, msg clientMessage, audio []byte) {
	t := &turn{s: s, ctx: ctx, msg: msg, timings: &turnTimings{start: time.Now(), marks: map[string]float64{}}}
	reply := ""
	defer func() { t.finish(reply) }()

	text, detected := msg.Text, ""
	if audio != nil {
		var ok bool
		if text, detected, ok = t.transcribe(audio); !ok {
			return
		}
	}
	replyIn := replyLanguage(text, detected)
	messages := append([]message{{"system", s.cfg.systemPrompt}}, trimHistory(msg.History)...)
	messages = append(messages, message{"user", withLanguage(text, replyIn)})

	chunks := make(chan string, 64)
	var replyText strings.Builder
	go t.generate(messages, chunks, &replyText)
	t.speak(chunks, replyIn)
	reply = replyText.String() // generate has finished: it closes chunks last
}

// emit sends an event for this turn, unless the turn has been cancelled and the page has moved on.
func (t *turn) emit(event map[string]any, binary []byte) {
	if t.ctx.Err() == nil {
		event["turn"] = t.msg.ID
		t.s.send(t.ctx, frame{event, binary})
	}
}

func (t *turn) fail(stage string, err error) {
	if t.ctx.Err() == nil && !errors.Is(err, context.Canceled) {
		t.emit(map[string]any{"type": "error", "stage": stage, "message": err.Error()}, nil)
	}
}

// finish ends every turn with done, so the page knows it is over, and logs its timings but never its words.
func (t *turn) finish(reply string) {
	t.emit(map[string]any{"type": "done", "reply": reply, "timings": t.timings.snapshot()}, nil)
	marks := t.timings.snapshot()
	mark := func(name string) string {
		if v, ok := marks[name]; ok {
			return strconv.FormatFloat(v, 'f', 2, 64)
		}
		return "-"
	}
	outcome := "done"
	if t.ctx.Err() != nil {
		outcome = "interrupted"
	}
	log.Printf("%s turn %d %s: stt=%s first_token=%s first_audio=%s llm_done=%s", t.s.peer, t.msg.ID, outcome,
		mark("stt"), mark("llm_first_token"), mark("first_audio"), mark("llm_done"))
}

// transcribe returns the recording's text and detected language; ok is false when the turn should end here.
func (t *turn) transcribe(audio []byte) (text, language string, ok bool) {
	result, err := t.s.b.transcribe(t.ctx, audio)
	if err != nil {
		t.fail("stt", err)
		return "", "", false
	}
	seconds := t.timings.mark("stt")
	t.emit(map[string]any{"type": "transcript", "text": result.Text, "language": nullable(result.Language), "seconds": seconds}, nil)
	return result.Text, result.Language, result.Text != ""
}

// generate streams the reply as text events and hands finished chunks to speak, closing chunks when done.
func (t *turn) generate(messages []message, chunks chan<- string, reply *strings.Builder) {
	defer close(chunks)
	ch := newChunker()
	err := t.s.b.chat(t.ctx, messages, func(delta string) {
		if reply.Len() == 0 {
			t.timings.mark("llm_first_token")
		}
		reply.WriteString(delta)
		t.emit(map[string]any{"type": "text", "delta": delta}, nil)
		for _, c := range ch.feed(delta, false) {
			chunks <- c
		}
	})
	if err == nil {
		for _, c := range ch.feed("", true) {
			chunks <- c
		}
	} else {
		t.fail("llm", err)
	}
	t.timings.mark("llm_done")
}

// speak synthesizes chunks one at a time and in order, since a TTS replica serves one request at a time. After a
// failure or cancel it keeps draining so generate never blocks. The TTS language is looked up on the first chunk
// rather than before the LLM starts, so a slow TTS cannot delay the first token.
func (t *turn) speak(chunks <-chan string, replyIn string) {
	language, failed, index := "", false, 0
	for chunk := range chunks {
		if failed || t.ctx.Err() != nil {
			continue
		}
		if language == "" {
			language = "auto"
			if t.s.b.ttsLanguages(t.ctx)[replyIn] {
				language = replyIn
			}
		}
		part := 0
		err := t.s.b.synthesize(t.ctx, chunk, t.msg.Voice, language, func(p audioPiece) {
			t.emit(t.audioEvent(index, part, chunk, p))
			part++
		})
		if err != nil {
			failed = true
			t.fail("tts", err)
		}
		index++
	}
}

// audioEvent describes one piece of speech; the piece itself follows as a binary frame. The chunk's text rides on
// its first piece.
func (t *turn) audioEvent(index, part int, chunk string, p audioPiece) (map[string]any, []byte) {
	event := map[string]any{"type": "audio", "index": index, "part": part}
	if part == 0 {
		event["text"] = chunk
	}
	if !t.timings.has("first_audio") {
		event["first_audio"] = t.timings.mark("first_audio")
	}
	if p.wav != nil {
		event["format"], event["seconds"], event["rtf"] = "wav", parseFloat(p.seconds), parseFloat(p.rtf)
		return event, p.wav
	}
	event["format"], event["sample_rate"] = "pcm", p.sampleRate
	event["seconds"] = round2(float64(len(p.pcm)) / 2 / float64(p.sampleRate))
	return event, p.pcm
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func parseFloat(s string) any {
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v
	}
	return nil
}
