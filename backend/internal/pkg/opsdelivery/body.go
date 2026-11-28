package opsdelivery

import (
	"bytes"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// Body observes protocol payloads only after the downstream writer accepts them.
// It retains at most one incomplete event; snapshots are never added to deltas.
type Body struct {
	Session                           *Session
	ImageOnly                         bool
	line, data                        []byte
	dropping                          bool
	streamed, done, failed, wroteText bool
}

const maxEventBytes = 2 << 20

func (b *Body) Frame(at time.Time, raw []byte) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("[DONE]")) {
		b.done = true
		return
	}
	if !gjson.ValidBytes(raw) {
		b.Session.Missing()
		return
	}
	root := gjson.ParseBytes(raw)
	typ := root.Get("type").String()
	var count Counts
	add := func(text gjson.Result, tokens gjson.Result) {
		if text.Type != gjson.String || text.Str == "" {
			return
		}
		n := int64(utf8.RuneCountInString(text.Str))
		count.Chars += n
		// Chat logprobs carry one entry per emitted token. Require an exact byte-for-byte match.
		if tokens.IsArray() {
			var reconstructed strings.Builder
			var entries int64
			valid := true
			tokens.ForEach(func(_, v gjson.Result) bool {
				bs := v.Get("bytes")
				if !bs.IsArray() {
					valid = false
					return false
				}
				bs.ForEach(func(_, x gjson.Result) bool { reconstructed.WriteByte(byte(x.Int())); return true })
				entries++
				return true
			})
			if valid && entries > 0 && reconstructed.String() == text.Str {
				count.Tokens += entries
				return
			}
		}
		count.UnknownChars += n
	}
	switch typ {
	case "response.output_text.delta", "response.refusal.delta":
		add(root.Get("delta"), root.Get("logprobs"))
	case "content_block_delta":
		if root.Get("delta.type").String() == "text_delta" {
			add(root.Get("delta.text"), gjson.Result{})
		}
	case "content_block_start":
		if root.Get("content_block.type").String() == "text" {
			add(root.Get("content_block.text"), gjson.Result{})
		}
	case "error", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		b.failed = true
	case "response.completed", "response.done":
		b.done = true
		switch root.Get("response.status").String() {
		case "failed", "incomplete", "cancelled", "canceled":
			b.failed = true
		}
		if !b.wroteText {
			responseText(root.Get("response.output"), add)
		}
	case "message_stop", "image_generation.completed", "image_edit.completed":
		b.done = true
	default:
		if root.Get("error").Exists() {
			b.failed = true
		}
		root.Get("choices").ForEach(func(_, v gjson.Result) bool {
			add(v.Get("delta.content"), v.Get("logprobs.content"))
			add(v.Get("delta.refusal"), v.Get("logprobs.refusal"))
			add(v.Get("message.content"), v.Get("logprobs.content"))
			add(v.Get("message.refusal"), v.Get("logprobs.refusal"))
			add(v.Get("text"), gjson.Result{})
			return true
		})
		root.Get("candidates").ForEach(func(_, v gjson.Result) bool {
			v.Get("content.parts").ForEach(func(_, part gjson.Result) bool {
				if !part.Get("thought").Bool() {
					add(part.Get("text"), gjson.Result{})
				}
				return true
			})
			if v.Get("finishReason").String() != "" {
				b.done = true
			}
			return true
		})
		if !b.streamed {
			root.Get("content").ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "text" {
					add(part.Get("text"), gjson.Result{})
				}
				return true
			})
			responseText(root.Get("output"), add)
			switch root.Get("status").String() {
			case "failed", "incomplete", "cancelled", "canceled":
				b.failed = true
			}
		}
	}
	if count.Chars > 0 {
		b.wroteText = true
		b.Session.Output(at, count)
	}
}
func responseText(output gjson.Result, add func(gjson.Result, gjson.Result)) {
	output.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "message" {
			item.Get("content").ForEach(func(_, part gjson.Result) bool {
				switch part.Get("type").String() {
				case "output_text":
					add(part.Get("text"), part.Get("logprobs"))
				case "refusal":
					add(part.Get("refusal"), gjson.Result{})
				}
				return true
			})
		}
		return true
	})
}
func (b *Body) Write(at time.Time, contentType string, p []byte) {
	if b.ImageOnly {
		return // The image handler records its delivery result after forwarding.
	}
	if strings.HasPrefix(contentType, "application/json") {
		if !b.dropping {
			if len(p)+len(b.data) > maxEventBytes {
				b.Session.Missing()
				b.dropping = true
				b.data = nil
			} else {
				b.data = append(b.data, p...)
				if gjson.ValidBytes(b.data) {
					b.Frame(at, b.data)
					b.data = nil
				}
			}
		}
		return
	}
	if !strings.HasPrefix(contentType, "text/event-stream") {
		return
	}
	b.streamed = true
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		part := p
		if i >= 0 {
			part = p[:i]
		}
		if len(b.line)+len(part)+len(b.data) > maxEventBytes {
			b.dropping = true
			b.Session.Missing()
			b.line = nil
			b.data = nil
		}
		if !b.dropping {
			b.line = append(b.line, part...)
		}
		if i < 0 {
			return
		}
		p = p[i+1:]
		line := bytes.TrimSuffix(b.line, []byte{'\r'})
		if !b.dropping && len(line) == 0 {
			if len(b.data) > 0 {
				b.Frame(at, b.data)
				b.data = b.data[:0]
			}
		} else if !b.dropping && bytes.HasPrefix(line, []byte("data:")) {
			if len(b.data) > 0 {
				b.data = append(b.data, '\n')
			}
			b.data = append(b.data, bytes.TrimPrefix(line[5:], []byte{' '})...)
		} else if b.dropping && i == 0 {
			b.dropping = false
		}
		b.line = b.line[:0]
	}
}
func (b *Body) Outcome(status int, canceled, limited, streamFailure bool) string {
	switch {
	case canceled:
		return "cancelled"
	case limited:
		return "limited"
	case status >= 400 || b.failed || streamFailure || b.streamed && !b.done:
		return "failed"
	default:
		return "completed"
	}
}
func (b *Body) StreamFailed() bool { return b.streamed && (b.failed || !b.done) }

func (s *Session) WSFrame(payload []byte, err error) {
	if s == nil {
		return
	}
	if err != nil {
		s.Finish(time.Now(), "cancelled")
		return
	}
	body := Body{Session: s, streamed: true, wroteText: s.hasOutput()}
	now := time.Now()
	body.Frame(now, payload)
	if body.failed {
		s.FailAttempt(int(gjson.GetBytes(payload, "status").Int()))
	}
	if body.done || body.failed {
		s.Finish(now, body.Outcome(200, false, false, false))
	}
}
func (s *Session) hasOutput() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.row.Chars > 0 }
