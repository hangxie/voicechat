package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Expected values come from the Python prototype this service was ported from, including its quirks: a URL swallows
// the punctuation that follows it up to the next space, and a chunk of punctuation alone still goes to TTS. Cases are
// in English; full-width punctuation stays where the chunker treats it specially, and language detection keeps CJK
// text because detecting the script is its job.

func TestSpoken(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"**Hello**, I am *your assistant*.", "Hello, I am your assistant."},
		{"See [the link](https://x.com/a) and https://example.com/path, then go on.", "See the link and then go on."},
		{"# Title\n- first item\n2. second item", "Title first item second item"},
		{"Great😀👍🏻!", "Great!"},
		{"  many   spaces\tand\nnewlines  ", "many spaces and newlines"},
		{"`code` ~struck~ > quote | table", "code struck quote table"},
		{"Pi is 3.14, right?", "Pi is 3.14, right?"},
		{"１. full-width numbered item", "full-width numbered item"},
		{"Address https://a.b/c\u3000then more", "Address then more"},
	} {
		if got := spoken(c.in); got != c.want {
			t.Errorf("spoken(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestChunker(t *testing.T) {
	long := strings.Repeat("a", 140) + "，" + strings.Repeat("b", 20) + "，c"
	for _, c := range []struct {
		parts []string
		want  [][]string // one entry per part, then one for the final flush
	}{
		{[]string{"Sure, ", "the movie is about", " the national volleyball team. Do you", " want to talk about it?"}, [][]string{{}, {}, {"Sure, the movie is about the national volleyball team."}, {}, {"Do you want to talk about it?"}}},
		{[]string{"Hello", " there", ", how are", " you? I'm", " fine."}, [][]string{{}, {}, {"Hello there,"}, {"how are you?"}, {}, {"I'm fine."}}},
		{[]string{"Pi is 3", ".14 and", " e is 2.71. Done"}, [][]string{{}, {}, {"Pi is 3.14 and e is 2.71."}, {"Done"}}},
		{[]string{"Really？！", "Great……", "Let's go。"}, [][]string{{}, {"Really？！"}, {"Great……"}, {"Let's go。"}}},
		{[]string{"He said “", "hello。”Then", " he left。"}, [][]string{{}, {"He said “hello。”"}, {}, {"Then he left。"}}},
		{[]string{"One", " two", " three four five six,", " and then it goes on,", " until the end"}, [][]string{{}, {}, {}, {"One two three four five six,"}, {}, {"and then it goes on, until the end"}}},
		{[]string{"OK.", " ", "Yes!", "\n", "No"}, [][]string{{}, {"OK."}, {}, {}, {"Yes!"}, {"No"}}},
		{[]string{long}, [][]string{{strings.Repeat("a", 140) + "，"}, {strings.Repeat("b", 20) + "，c"}}},
		{[]string{".", " ... ok", "."}, [][]string{{}, {". ..."}, {}, {"ok."}}},
	} {
		ch := newChunker()
		var got [][]string
		for _, p := range c.parts {
			got = append(got, ch.feed(p, false))
		}
		got = append(got, ch.feed("", true))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("chunks of %q = %q, want %q", c.parts, got, c.want)
		}
	}
}

func TestReplyLanguage(t *testing.T) {
	for _, c := range []struct{ text, detected, want string }{
		{"你好", "", "chinese"},
		{"こんにちは", "chinese", "japanese"},
		{"안녕하세요", "", "korean"},
		{"漢字とかな", "", "japanese"},
		{"hello", "english", "english"},
		{"hello", "", ""},
		{"bonjour", "french", "french"},
		{"123", "", ""},
		{"中文 and English", "english", "chinese"},
	} {
		if got := replyLanguage(c.text, c.detected); got != c.want {
			t.Errorf("replyLanguage(%q, %q) = %q, want %q", c.text, c.detected, got, c.want)
		}
	}
}

func TestWithLanguage(t *testing.T) {
	for _, c := range []struct{ text, language, want string }{
		{"Hello", "english", "Hello\n\n(Reply in English.)"},
		{"hi", "", "hi\n\n(Reply in the language of this message, whatever language earlier turns used.)"},
		{"salut", "french", "salut\n\n(Reply in French.)"},
	} {
		if got := withLanguage(c.text, c.language); got != c.want {
			t.Errorf("withLanguage(%q, %q) = %q, want %q", c.text, c.language, got, c.want)
		}
	}
}

func TestTrimHistory(t *testing.T) {
	msgs := func(n int) []message {
		m := make([]message, n)
		for i := range m {
			m[i] = message{Role: []string{"user", "assistant"}[i%2], Content: fmt.Sprint(i)}
		}
		return m
	}
	// length in -> length kept, first kept message
	for _, c := range []struct{ n, kept, first int }{
		{20, 20, 0}, {80, 80, 0}, {81, 41, 40}, {82, 42, 40}, {119, 79, 40}, {120, 80, 40}, {121, 41, 80}, {161, 41, 120},
	} {
		got := trimHistory(msgs(c.n))
		if len(got) != c.kept || got[0].Content != fmt.Sprint(c.first) || got[0].Role != "user" {
			t.Errorf("trimHistory(%d) kept %d from %s/%s, want %d from %d/user", c.n, len(got), got[0].Content, got[0].Role, c.kept, c.first)
		}
	}
	mixed := []message{{"system", "x"}, {"user", ""}, {"user", "a"}, {"tool", "y"}, {"assistant", "b"}}
	if got := trimHistory(mixed); !reflect.DeepEqual(got, []message{{"user", "a"}, {"assistant", "b"}}) {
		t.Errorf("trimHistory dropped the wrong messages: %v", got)
	}
}
