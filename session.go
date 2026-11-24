package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const (
	maxAudioBytes = 10 * 1024 * 1024
	// Pings keep a quiet connection busy so the proxy's idle timeout does not close it, and find dead peers.
	pingEvery = 25 * time.Second
)

// clientMessage is a JSON text frame from the page. A turn with audio is followed by one binary frame holding the
// recording as WAV; "cancel" stops the turn in progress, as does starting a new one.
type clientMessage struct {
	Type    string    `json:"type"`
	ID      int64     `json:"id"`
	Text    string    `json:"text"`
	Audio   bool      `json:"audio"`
	History []message `json:"history"`
	Voice   string    `json:"voice"`
}

// frame is one event for the page, a JSON text frame, followed by a binary frame when it carries audio.
type frame struct {
	event  map[string]any
	binary []byte
}

type session struct {
	conn   *websocket.Conn
	b      *backends
	cfg    config
	peer   string
	out    chan frame
	cancel context.CancelFunc // of the turn in progress
}

func (s *session) serve(ctx context.Context) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	go s.write(ctx, stop)
	go s.ping(ctx, stop)
	for {
		kind, data, err := s.conn.Read(ctx)
		if err != nil {
			return // closed or failed; stop() cancels the turn in progress and its backend requests
		}
		if kind != websocket.MessageText {
			s.reject(0, "unexpected binary frame")
			continue
		}
		var msg clientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			s.reject(0, "invalid message: "+err.Error())
			continue
		}
		var audio []byte
		if msg.Type == "turn" && msg.Audio {
			kind, audio, err = s.conn.Read(ctx)
			if err != nil {
				return
			}
			if kind != websocket.MessageBinary || len(audio) == 0 {
				s.reject(msg.ID, "expected the recording as a binary frame")
				continue
			}
		}
		switch msg.Type {
		case "cancel":
			s.stopTurn()
		case "turn":
			s.stopTurn()
			if problem := s.validate(&msg, audio); problem != "" {
				s.reject(msg.ID, problem)
				continue
			}
			turnCtx, cancel := context.WithCancel(ctx)
			s.cancel = cancel
			go s.runTurn(turnCtx, msg, audio)
		default:
			s.reject(msg.ID, "unknown message type "+strconv.Quote(msg.Type))
		}
	}
}

func (s *session) stopTurn() {
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

func (s *session) validate(msg *clientMessage, audio []byte) string {
	if audio == nil && strings.TrimSpace(msg.Text) == "" {
		return "Send audio or text"
	}
	if msg.Voice == "" {
		msg.Voice = s.cfg.voice
	}
	if msg.Voice == "" {
		return "Select a voice"
	}
	return ""
}

// reject answers an invalid message with an error and a done, so the page ends that turn.
func (s *session) reject(id int64, problem string) {
	s.send(context.Background(), frame{event: map[string]any{"type": "error", "turn": id, "stage": "request", "message": problem}})
	s.send(context.Background(), frame{event: map[string]any{"type": "done", "turn": id, "reply": "", "timings": map[string]float64{}}})
}

func (s *session) send(ctx context.Context, f frame) {
	select {
	case s.out <- f:
	case <-ctx.Done():
	}
}

func (s *session) write(ctx context.Context, stop context.CancelFunc) {
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-s.out:
			data, _ := json.Marshal(f.event)
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := s.conn.Write(wctx, websocket.MessageText, data)
			if err == nil && f.binary != nil {
				err = s.conn.Write(wctx, websocket.MessageBinary, f.binary)
			}
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (s *session) ping(ctx context.Context, stop context.CancelFunc) {
	ticker := time.NewTicker(pingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := s.conn.Ping(pctx)
			cancel()
			if err != nil {
				stop()
				return
			}
		}
	}
}
