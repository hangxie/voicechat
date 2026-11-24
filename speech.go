package main

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A chunk ends at sentence punctuation; ASCII stops need a following space so "3.14" stays whole. The first chunk may
// also end at a comma once it is long enough, so playback starts sooner, and so may any chunk that grows too long.
const (
	hardStops       = "。！？；…\n"
	softStops       = ".!?;"
	pauses          = "，,、：:"
	closers         = "\"'”’」』）)]"
	firstChunkChars = 12
	maxChunkChars   = 150
	minSpokenChars  = 2
)

// trailing punctuation is kept with the stop it follows, as in "……" or "?!".
var trailing = closers + hardStops + softStops

// The patterns use Unicode classes where Python's re is Unicode-aware and RE2's \s and \d are ASCII-only.
var markdown = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`), "$1"}, // [text](url) -> text
	{regexp.MustCompile(`https?://[^\s\v\p{Z}\x{85}]+`), ""},
	{regexp.MustCompile(`(?m)^[\s\v\p{Z}]{0,3}(#{1,6}|[-*+]|\p{Nd}+[.)])[\s\v\p{Z}]+`), ""}, // headings, bullets, numbered items
	{regexp.MustCompile("[*_`~#|>]+"), ""},
	{regexp.MustCompile(`[\x{1F000}-\x{1FAFF}\x{2600}-\x{27BF}\x{FE0F}\x{200D}]`), ""}, // emoji and their joiners
	{regexp.MustCompile(`[\s\v\p{Z}\x{85}]+`), " "},
}

// spoken removes the markdown, links, and emoji the model still sometimes writes, for TTS.
func spoken(text string) string {
	for _, m := range markdown {
		text = m.pattern.ReplaceAllString(text, m.replacement)
	}
	return strings.TrimSpace(text)
}

// chunker splits streamed LLM text into chunks small enough to synthesize while the rest is still generating.
type chunker struct {
	buffer []rune
	first  bool
}

func newChunker() *chunker { return &chunker{first: true} }

func (c *chunker) feed(text string, final bool) []string {
	c.buffer = append(c.buffer, []rune(text)...)
	b := c.buffer
	var chunks []string
	start := 0
	for i := 0; i < len(b); {
		ch, length := b[i], i+1-start
		limit := maxChunkChars
		if c.first {
			limit = firstChunkChars
		}
		cut := strings.ContainsRune(hardStops, ch) ||
			(strings.ContainsRune(softStops, ch) && i+1 < len(b) && unicode.IsSpace(b[i+1])) ||
			(strings.ContainsRune(pauses, ch) && length >= limit)
		if !cut {
			i++
			continue
		}
		end := i + 1
		for end < len(b) && strings.ContainsRune(trailing, b[end]) {
			end++
		}
		if end == len(b) && !final {
			break // the next token may still continue this run of punctuation
		}
		if utf8.RuneCountInString(spoken(string(b[start:end]))) >= minSpokenChars {
			chunks = append(chunks, string(b[start:end]))
			start, c.first = end, false
		}
		i = end
	}
	c.buffer = append([]rune(nil), b[start:]...)
	if final && spoken(string(c.buffer)) != "" {
		chunks = append(chunks, string(c.buffer))
		c.buffer = nil
	}
	out := []string{}
	for _, chunk := range chunks {
		if s := spoken(chunk); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// replyLanguage is the language to answer in: from the script of the text when it is unambiguous, else the
// STT-detected language, which may be empty.
func replyLanguage(text, detected string) string {
	in := func(lo, hi rune) bool {
		return strings.IndexFunc(text, func(r rune) bool { return r >= lo && r <= hi }) >= 0
	}
	switch {
	case in(0x3040, 0x30ff):
		return "japanese"
	case in(0xac00, 0xd7af):
		return "korean"
	case in(0x4e00, 0x9fff):
		return "chinese"
	}
	return detected
}

// withLanguage adds this turn's reply language to the user's message, which a small model otherwise drops once the
// history holds a reply in another language and copies that pattern instead. It rides on the latest message rather
// than the system prompt so the cached prefix survives a change of language; the template only allows a system
// message first.
func withLanguage(text, language string) string {
	if language == "" {
		return text + "\n\n(Reply in the language of this message, whatever language earlier turns used.)"
	}
	return text + "\n\n(Reply in " + capitalize(language) + ".)"
}

func capitalize(s string) string {
	r := []rune(strings.ToLower(s))
	if len(r) > 0 {
		r[0] = unicode.ToUpper(r[0])
	}
	return string(r)
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Past historyMax messages the oldest are dropped historyTrim at a time, so the start of the prompt stays the same
// for historyTrim messages and llama.cpp reuses its cached prefix; sliding one turn at a time would reprocess it
// every turn.
const (
	historyMax  = 80
	historyTrim = 40 // even, so the kept history starts on the same role
)

// trimHistory keeps the valid user and assistant turns, most recent last, trimmed in blocks past historyMax.
func trimHistory(messages []message) []message {
	turns := []message{}
	for _, m := range messages {
		if (m.Role == "user" || m.Role == "assistant") && m.Content != "" {
			turns = append(turns, m)
		}
	}
	excess := len(turns) - historyMax
	if excess <= 0 {
		return turns
	}
	drop := (excess + historyTrim - 1) / historyTrim * historyTrim // excess rounded up to whole blocks
	return turns[drop:]
}
