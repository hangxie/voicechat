package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	backendTimeout = 120 * time.Second
	// A single-slot TTS answers 409 while busy and Ray Serve 503 or 429 when its queue is full; wait this long for a slot.
	ttsBusyWait = 20 * time.Second
	// A failed TTS /health is retried this often rather than on every turn, so an unreachable TTS does not delay turns.
	languagesRetry = time.Minute
)

var ttsBusyStatuses = []int{http.StatusConflict, http.StatusTooManyRequests, http.StatusServiceUnavailable}

type backendError struct {
	name    string
	status  int
	message string
}

func (e *backendError) Error() string {
	return fmt.Sprintf("%s returned %d: %s", e.name, e.status, e.message)
}

type backends struct {
	cfg    config
	client *http.Client

	mu          sync.Mutex
	languages   map[string]bool
	languagesAt time.Time
}

func newBackends(cfg config) *backends { return &backends{cfg: cfg, client: &http.Client{}} }

// post sends a request and returns the response once its status is 2xx; cancelling ctx aborts it, which makes
// a TTS that watches for disconnects drop a synthesis in progress.
func (b *backends) post(ctx context.Context, name, url, token, contentType string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer closeBody(resp)
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2000))
		return nil, &backendError{name, resp.StatusCode, errorMessage(detail)}
	}
	return resp, nil
}

// errorMessage pulls the message out of an OpenAI-style {"error": {"message": ...}} or {"error": "..."} body.
func errorMessage(body []byte) string {
	var parsed struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error != nil {
		var nested struct {
			Message string `json:"message"`
		}
		var plain string
		if json.Unmarshal(parsed.Error, &nested) == nil && nested.Message != "" {
			return nested.Message
		}
		if json.Unmarshal(parsed.Error, &plain) == nil {
			return plain
		}
	}
	return string(body)
}

func (b *backends) ttsGet(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.ttsURL+path, nil)
	if err != nil {
		return err
	}
	if b.cfg.ttsToken != "" {
		req.Header.Set("Authorization", "Bearer "+b.cfg.ttsToken)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer closeBody(resp)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("TTS %s returned %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (b *backends) voices(ctx context.Context) ([]string, error) {
	var voices []string
	return voices, b.ttsGet(ctx, "/voices", &voices)
}

// ttsLanguages lists the languages TTS accepts, read from its /health once it answers.
func (b *backends) ttsLanguages(ctx context.Context) map[string]bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.languages != nil || time.Since(b.languagesAt) < languagesRetry {
		return b.languages
	}
	b.languagesAt = time.Now()
	var health struct {
		Languages []string `json:"languages"`
	}
	if err := b.ttsGet(ctx, "/health", &health); err != nil {
		return nil
	}
	b.languages = map[string]bool{}
	for _, l := range health.Languages {
		b.languages[l] = true
	}
	return b.languages
}

// closeBody closes a response body whose close error carries nothing worth acting on once the body is read.
func closeBody(resp *http.Response) { _ = resp.Body.Close() }

// transcriptionForm is the multipart body of an OpenAI-style transcription request.
func transcriptionForm(audio []byte) ([]byte, string, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("response_format", "verbose_json"); err != nil {
		return nil, "", err
	}
	if err := form.WriteField("model", "whisper"); err != nil {
		return nil, "", err
	}
	part, err := form.CreateFormFile("file", "speech.wav")
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(audio); err != nil {
		return nil, "", err
	}
	if err := form.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), form.FormDataContentType(), nil
}

type transcript struct {
	Text     string `json:"text"`
	Language string `json:"language"`
}

func (b *backends) transcribe(ctx context.Context, audio []byte) (transcript, error) {
	var result transcript
	body, contentType, err := transcriptionForm(audio)
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, backendTimeout)
	defer cancel()
	resp, err := b.post(ctx, "STT", b.cfg.sttURL+"/audio/transcriptions", b.cfg.sttToken, contentType, body)
	if err != nil {
		return result, err
	}
	defer closeBody(resp)
	err = json.NewDecoder(resp.Body).Decode(&result)
	result.Text = strings.TrimSpace(result.Text)
	return result, err
}

// chat streams the reply, calling onText for each piece; a failed connection or 5xx is retried once, which is safe
// because no text has arrived yet.
func (b *backends) chat(ctx context.Context, messages []message, onText func(string)) error {
	body, _ := json.Marshal(map[string]any{
		"model": b.cfg.llmModel, "messages": messages, "stream": true, "max_tokens": b.cfg.llmMaxTokens,
		// Thinking would be a long silent pause before the first word.
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	})
	ctx, cancel := context.WithTimeout(ctx, backendTimeout)
	defer cancel()
	var resp *http.Response
	var err error
	for attempt := 1; ; attempt++ {
		resp, err = b.post(ctx, "LLM", b.cfg.llmURL+"/chat/completions", b.cfg.llmToken, "application/json", body)
		var be *backendError
		if err == nil || attempt == 2 || ctx.Err() != nil || (errors.As(err, &be) && be.status < 500) {
			break
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
		}
	}
	if err != nil {
		return err
	}
	defer closeBody(resp)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if string(data) == "[DONE]" {
			break
		}
		var event struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			return err
		}
		if len(event.Choices) > 0 && event.Choices[0].Delta.Content != "" {
			onText(event.Choices[0].Delta.Content)
		}
	}
	return scanner.Err()
}

// audioPiece is synthesized speech: raw 16-bit PCM at sampleRate, or a whole WAV from a backend that does not stream.
type audioPiece struct {
	pcm        []byte
	sampleRate int
	wav        []byte
	rtf        string
	seconds    string
}

// synthesize requests one chunk as a stream and calls onAudio for each piece as it is generated, so playback starts
// about a second into the chunk instead of after all of it. A backend that answers with a whole WAV still works.
func (b *backends) synthesize(ctx context.Context, text, voice, language string, onAudio func(audioPiece)) error {
	body, _ := json.Marshal(map[string]any{"text": text, "voice": voice, "language": language, "stream": true})
	ctx, cancel := context.WithTimeout(ctx, backendTimeout)
	defer cancel()
	deadline := time.Now().Add(ttsBusyWait)
	var resp *http.Response
	for {
		var err error
		resp, err = b.post(ctx, "TTS", b.cfg.ttsURL+"/api/tts", b.cfg.ttsToken, "application/json", body)
		var be *backendError
		if err == nil {
			break
		}
		if !errors.As(err, &be) || !slices.Contains(ttsBusyStatuses, be.status) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer closeBody(resp)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-ndjson") {
		wav, err := io.ReadAll(resp.Body)
		if err == nil {
			onAudio(audioPiece{wav: wav, rtf: resp.Header.Get("X-RTF"), seconds: resp.Header.Get("X-Audio-Seconds")})
		}
		return err
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		var event struct {
			Type       string `json:"type"`
			PCM        []byte `json:"pcm"` // base64 in JSON
			SampleRate int    `json:"sample_rate"`
			Error      string `json:"error"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		switch event.Type {
		case "audio":
			onAudio(audioPiece{pcm: event.PCM, sampleRate: event.SampleRate})
		case "error":
			return &backendError{"TTS", 500, event.Error}
		case "done":
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return &backendError{"TTS", 502, "stream ended without finishing"}
}
