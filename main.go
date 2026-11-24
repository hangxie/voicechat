// Command voice-chat orchestrates a spoken conversation over a WebSocket: speech -> STT -> streamed LLM reply ->
// TTS per sentence, streamed back to the browser as it is generated.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

//go:embed index.html
var indexHTML []byte

const defaultSystemPrompt = "You are a friendly voice assistant in a spoken conversation. Reply in the language the user speaks. " +
	"Keep answers brief, one to three short sentences, unless asked for more detail. " +
	"Your words are read aloud, so use plain spoken sentences only: no markdown, lists, headings, code, emoji, or URLs. " +
	"Write numbers, units, and symbols the way they are said."

// By default clients are accepted from loopback only, which is where a reverse proxy on the same host connects from.
const defaultTrustedNetworks = "127.0.0.0/8,::1/128"

type config struct {
	sttURL, llmURL, ttsURL       string
	sttToken, llmToken, ttsToken string
	llmModel, voice              string
	systemPrompt                 string
	llmMaxTokens                 int
	addr                         string
	trusted                      []netip.Prefix
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func loadConfig() (config, error) {
	maxTokens, err := strconv.Atoi(env("LLM_MAX_TOKENS", "512"))
	if err != nil {
		return config{}, fmt.Errorf("LLM_MAX_TOKENS: %w", err)
	}
	trusted, err := parseNetworks(env("TRUSTED_NETWORKS", defaultTrustedNetworks))
	if err != nil {
		return config{}, fmt.Errorf("TRUSTED_NETWORKS: %w", err)
	}
	return config{
		sttURL:       strings.TrimRight(env("STT_URL", "http://localhost:5005/v1"), "/"),
		llmURL:       strings.TrimRight(env("LLM_URL", "http://localhost:8080/v1"), "/"),
		ttsURL:       strings.TrimRight(env("TTS_URL", "http://localhost:5004"), "/"),
		sttToken:     os.Getenv("STT_TOKEN"),
		llmToken:     os.Getenv("LLM_TOKEN"),
		ttsToken:     os.Getenv("TTS_TOKEN"),
		llmModel:     env("LLM_MODEL", "qwen3.5-9b-q8-uncensored"),
		voice:        os.Getenv("VOICE"),
		systemPrompt: env("SYSTEM_PROMPT", defaultSystemPrompt),
		llmMaxTokens: maxTokens,
		addr:         net.JoinHostPort(env("VOICE_CHAT_HOST", "127.0.0.1"), env("VOICE_CHAT_PORT", "5006")),
		trusted:      trusted,
	}, nil
}

// parseNetworks reads a comma-separated list of CIDR prefixes.
func parseNetworks(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, field := range strings.Split(list, ",") {
		if field = strings.TrimSpace(field); field == "" {
			continue
		}
		p, err := netip.ParsePrefix(field)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Masked())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no networks in %q", list)
	}
	return out, nil
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write reply: %v", err)
	}
}

func trusted(networks []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil || !slices.ContainsFunc(networks, func(p netip.Prefix) bool { return p.Contains(addr.Addr().Unmap()) }) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// newHandler serves the page, the JSON endpoints, and the WebSocket, to trusted clients only.
func newHandler(cfg config) http.Handler {
	b := newBackends(cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if _, err := w.Write(indexHTML); err != nil {
			log.Printf("write page: %v", err)
		}
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, map[string]string{
			"status": "ok", "stt": cfg.sttURL, "llm": cfg.llmURL,
			"llm_model": cfg.llmModel, "tts": cfg.ttsURL, "voice": cfg.voice,
		})
	})
	mux.HandleFunc("GET /api/voices", func(w http.ResponseWriter, r *http.Request) {
		voices, err := b.voices(r.Context())
		if err != nil {
			reply(w, http.StatusBadGateway, map[string]string{"error": "TTS unavailable: " + err.Error()})
			return
		}
		def := ""
		if slices.Contains(voices, cfg.voice) {
			def = cfg.voice
		} else if len(voices) > 0 {
			def = voices[0]
		}
		reply(w, http.StatusOK, map[string]any{"voices": voices, "default": def})
	})
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		// The default origin check requires the page's origin to match the Host header, which the proxy passes on.
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		conn.SetReadLimit(maxAudioBytes + 64*1024)
		peer := r.Header.Get("X-Real-IP")
		if peer == "" {
			peer = r.RemoteAddr
		}
		log.Printf("%s connected", peer)
		s := &session{conn: conn, b: b, cfg: cfg, peer: peer, out: make(chan frame, 256)}
		s.serve(r.Context())
		log.Printf("%s disconnected", peer)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusNotFound, map[string]string{"error": "Not found"})
	})
	return trusted(cfg.trusted, mux)
}

func main() {
	log.SetFlags(0) // journald adds timestamps
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Voice chat listening on %s for %v (STT %s, LLM %s %s, TTS %s)", cfg.addr, cfg.trusted, cfg.sttURL, cfg.llmURL, cfg.llmModel, cfg.ttsURL)
	server := &http.Server{Addr: cfg.addr, Handler: newHandler(cfg), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(server.ListenAndServe())
}
