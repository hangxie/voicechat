package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard) // the server logs every connection and turn
	os.Exit(m.Run())
}

func TestHTTP(t *testing.T) {
	f, cfg := startFake(t, handlers{fakeVoices: func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `["a", "test-voice"]`)
	}})
	cfg.trusted = append(cfg.trusted, netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("192.168.0.0/24"))
	h := newHandler(cfg)
	get := func(path, remote string) (int, string) {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	const local = "127.0.0.1:40000"

	if code, body := get("/", local); code != 200 || !strings.Contains(body, "<html") {
		t.Errorf("page = %d", code)
	}
	if code, body := get("/health", local); code != 200 || !strings.Contains(body, `"llm_model":"test-model"`) {
		t.Errorf("health = %d %s", code, body)
	}
	if code, body := get("/api/voices", local); code != 200 || !strings.Contains(body, `"default":"test-voice"`) {
		t.Errorf("voices = %d %s", code, body)
	}
	f.set(fakeVoices, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `["a", "b"]`) })
	if code, body := get("/api/voices", local); code != 200 || !strings.Contains(body, `"default":"a"`) {
		t.Errorf("voices without the configured one = %d %s", code, body)
	}
	f.set(fakeVoices, status(500, "down"))
	if code, body := get("/api/voices", local); code != http.StatusBadGateway || !strings.Contains(body, "TTS unavailable") {
		t.Errorf("voices with TTS down = %d %s", code, body)
	}
	if code, body := get("/nope", local); code != 404 || !strings.Contains(body, "Not found") {
		t.Errorf("unknown path = %d %s", code, body)
	}
	for _, remote := range []string{"192.168.0.50:1234", "[::1]:1234", "[::ffff:127.0.0.1]:1234"} {
		if code, _ := get("/health", remote); code != 200 {
			t.Errorf("%s refused: %d", remote, code)
		}
	}
	for _, remote := range []string{"10.0.0.5:1234", "192.168.1.5:1234", "[2001:db8::1]:1234", "garbage"} {
		if code, _ := get("/health", remote); code != http.StatusForbidden {
			t.Errorf("%s allowed: %d", remote, code)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	// Empty counts as unset; clearing these also hides the ones the Makefile exports for make run.
	for _, name := range []string{"STT_URL", "TTS_URL", "VOICE_CHAT_PORT", "SYSTEM_PROMPT"} {
		t.Setenv(name, "")
	}
	t.Setenv("LLM_URL", "http://llm:8080/v1/")
	t.Setenv("LLM_MAX_TOKENS", "256")
	t.Setenv("VOICE_CHAT_HOST", "")
	t.Setenv("VOICE", "")
	t.Setenv("TRUSTED_NETWORKS", "")
	cfg, err := loadConfig()
	if err != nil || cfg.llmURL != "http://llm:8080/v1" || cfg.llmMaxTokens != 256 || cfg.addr != "127.0.0.1:5006" ||
		cfg.sttURL != "http://localhost:5005/v1" || cfg.systemPrompt != defaultSystemPrompt || cfg.voice != "" ||
		fmt.Sprint(cfg.trusted) != "[127.0.0.0/8 ::1/128]" {
		t.Errorf("config = %+v, %v", cfg, err)
	}

	t.Setenv("TRUSTED_NETWORKS", "127.0.0.1/8, 192.168.0.0/24,")
	if cfg, err = loadConfig(); err != nil || fmt.Sprint(cfg.trusted) != "[127.0.0.0/8 192.168.0.0/24]" {
		t.Errorf("trusted = %v, %v", cfg.trusted, err)
	}
	for name, value := range map[string]string{"TRUSTED_NETWORKS": "192.168.0.1", "LLM_MAX_TOKENS": "lots"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s=%q: err = %v", name, value, err)
			}
		})
	}
	t.Setenv("TRUSTED_NETWORKS", " , ")
	if _, err := loadConfig(); err == nil {
		t.Error("an empty network list was accepted")
	}
}
