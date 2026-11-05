// Package jsonstream incrementally extracts the string values of a flat JSON
// object while it is still being streamed (ARCHITECTURE §7.6). It exists only
// to show a live, *unvalidated* preview of model output: the complete text is
// always decoded and validated separately once the stream ends, and nothing
// the extractor produces is ever persisted or trusted.
//
// The extractor is a small state machine over bytes. Chunks may split the
// input anywhere — inside a key, inside an escape sequence (\n, \uXXXX,
// surrogate pairs) or inside a multi-byte UTF-8 rune — and the emitted deltas
// are always complete, valid UTF-8. Only top-level properties named in the
// allow-list are reported; values of other properties (and non-string
// values) are skipped. Malformed input stops extraction silently (the final
// strict decode reports the problem); the extractor never panics and never
// buffers more than the configured limit.
package jsonstream

import (
	"unicode/utf16"
	"unicode/utf8"
)

// Delta is the next fragment of one field's decoded string value.
type Delta struct {
	Field string
	Text  string
}

type state int

const (
	stBeforeObject state = iota // expect '{'
	stBeforeKey                 // expect '"' or '}'
	stKey                       // inside a key string
	stKeyEscape                 // after '\' in a key
	stAfterKey                  // expect ':'
	stBeforeValue               // expect a value
	stString                    // inside a string value
	stEscape                    // after '\' in a string value
	stUnicode                   // reading \uXXXX hex digits
	stSkipValue                 // inside a non-string value (number, literal, nested)
	stSkipString                // inside a string nested in a skipped value
	stSkipEscape                // after '\' in such a string
	stAfterValue                // expect ',' or '}'
	stDone                      // after the closing '}'
	stFailed                    // malformed input: extraction stopped
)

// Extractor extracts allowed string fields from a streamed JSON object.
// The zero value is not usable; use New. Not safe for concurrent use.
type Extractor struct {
	allowed map[string]bool
	limit   int
	seen    int

	st     state
	key    []byte
	field  string // current field if allowed, else ""
	out    []byte // pending decoded bytes of the current field
	hex    int    // hex digits read of \uXXXX
	cp     rune   // code point being read
	high   rune   // pending high surrogate (0 if none)
	depth  int    // nesting depth of a skipped value
	emit   []Delta
	maxKey int
}

// New returns an extractor that reports the given top-level fields and stops
// after limit input bytes (limit ≤ 0 means no limit).
func New(limit int, fields ...string) *Extractor {
	m := make(map[string]bool, len(fields))
	maxKey := 0
	for _, f := range fields {
		m[f] = true
		if len(f) > maxKey {
			maxKey = len(f)
		}
	}
	return &Extractor{allowed: m, limit: limit, maxKey: maxKey}
}

// Failed reports whether extraction stopped because of malformed input or
// the size limit.
func (x *Extractor) Failed() bool { return x.st == stFailed }

// Write consumes the next chunk and returns the deltas it completed, in
// order. Consecutive bytes of one field within a chunk are merged into one
// delta. Bytes of an incomplete rune or escape are held until a later chunk
// completes them.
func (x *Extractor) Write(chunk string) []Delta {
	x.emit = x.emit[:0]
	if x.st == stFailed || x.st == stDone {
		return nil
	}
	if x.limit > 0 && x.seen+len(chunk) > x.limit {
		chunk = chunk[:max(0, x.limit-x.seen)]
		defer func() { x.fail() }()
	}
	x.seen += len(chunk)
	for i := 0; i < len(chunk) && x.st != stFailed; i++ {
		x.step(chunk[i])
	}
	x.flush(false)
	if len(x.emit) == 0 {
		return nil
	}
	return append([]Delta(nil), x.emit...)
}

func (x *Extractor) fail() {
	x.st = stFailed
	x.out = x.out[:0]
	x.high = 0
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (x *Extractor) step(c byte) {
	switch x.st {
	case stBeforeObject:
		switch {
		case isSpace(c):
		case c == '{':
			x.st = stBeforeKey
		default:
			x.fail()
		}
	case stBeforeKey:
		switch {
		case isSpace(c):
		case c == '"':
			x.key = x.key[:0]
			x.st = stKey
		case c == '}':
			x.st = stDone
		default:
			x.fail()
		}
	case stKey:
		switch {
		case c == '"':
			x.st = stAfterKey
		case c == '\\':
			// Escaped keys never name an allowed field (field names are
			// plain ASCII); keep reading but poison the key.
			x.key = append(x.key, 0)
			x.st = stKeyEscape
		case c < 0x20:
			x.fail()
		default:
			if len(x.key) <= x.maxKey {
				x.key = append(x.key, c)
			}
		}
	case stKeyEscape:
		x.st = stKey
	case stAfterKey:
		switch {
		case isSpace(c):
		case c == ':':
			x.field = ""
			if len(x.key) <= x.maxKey && x.allowed[string(x.key)] {
				x.field = string(x.key)
			}
			x.st = stBeforeValue
		default:
			x.fail()
		}
	case stBeforeValue:
		switch {
		case isSpace(c):
		case c == '"':
			if x.field == "" {
				x.st = stSkipString
				x.depth = 0
			} else {
				x.st = stString
			}
		case c == '{' || c == '[':
			x.depth = 1
			x.st = stSkipValue
		case c == '}' || c == ',' || c == ']' || c == ':':
			x.fail()
		default:
			x.depth = 0
			x.st = stSkipValue
		}
	case stString:
		switch {
		case c == '"':
			x.flush(true)
			x.st = stAfterValue
		case c == '\\':
			x.st = stEscape
		case c < 0x20:
			x.fail()
		default:
			x.pendingHigh()
			x.out = append(x.out, c)
		}
	case stEscape:
		var r byte
		switch c {
		case '"', '\\', '/':
			r = c
		case 'b':
			r = '\b'
		case 'f':
			r = '\f'
		case 'n':
			r = '\n'
		case 'r':
			r = '\r'
		case 't':
			r = '\t'
		case 'u':
			x.hex, x.cp = 0, 0
			x.st = stUnicode
			return
		default:
			x.fail()
			return
		}
		x.pendingHigh()
		x.out = append(x.out, r)
		x.st = stString
	case stUnicode:
		v, ok := hexVal(c)
		if !ok {
			x.fail()
			return
		}
		x.cp = x.cp<<4 | rune(v)
		x.hex++
		if x.hex < 4 {
			return
		}
		x.st = stString
		x.codePoint(x.cp)
	case stSkipValue:
		switch {
		case x.depth == 0 && (c == ',' || c == '}' || isSpace(c)):
			x.st = stAfterValue
			x.step(c)
		case c == '"':
			x.st = stSkipString
		case c == '{' || c == '[':
			x.depth++
		case c == '}' || c == ']':
			x.depth--
			if x.depth < 0 {
				x.fail()
			} else if x.depth == 0 {
				x.st = stAfterValue
			}
		}
	case stSkipString:
		switch c {
		case '"':
			if x.depth == 0 {
				x.st = stAfterValue
			} else {
				x.st = stSkipValue
			}
		case '\\':
			x.st = stSkipEscape
		}
	case stSkipEscape:
		x.st = stSkipString
	case stAfterValue:
		switch {
		case isSpace(c):
		case c == ',':
			x.st = stBeforeKey
		case c == '}':
			x.st = stDone
		default:
			x.fail()
		}
	case stDone:
		if !isSpace(c) {
			x.fail()
		}
	}
}

// codePoint appends a decoded \uXXXX code point, pairing surrogates as
// encoding/json does (a lone surrogate becomes U+FFFD).
func (x *Extractor) codePoint(r rune) {
	switch {
	case utf16.IsSurrogate(r) && r < 0xDC00: // high surrogate
		x.pendingHigh()
		x.high = r
	case utf16.IsSurrogate(r): // low surrogate
		if x.high != 0 {
			x.out = utf8.AppendRune(x.out, utf16.DecodeRune(x.high, r))
			x.high = 0
		} else {
			x.out = utf8.AppendRune(x.out, utf8.RuneError)
		}
	default:
		x.pendingHigh()
		x.out = utf8.AppendRune(x.out, r)
	}
}

// pendingHigh resolves a high surrogate that was not followed by a low one.
func (x *Extractor) pendingHigh() {
	if x.high != 0 {
		x.out = utf8.AppendRune(x.out, utf8.RuneError)
		x.high = 0
	}
}

// flush emits the complete runes of the current field. With end = true the
// string ended: a dangling high surrogate becomes U+FFFD and everything is
// emitted. Otherwise a trailing incomplete rune (or surrogate) is held for
// the next chunk. Invalid bytes become U+FFFD, as with encoding/json.
func (x *Extractor) flush(end bool) {
	if end {
		x.pendingHigh()
	}
	if x.field == "" {
		x.out = x.out[:0]
		return
	}
	n := len(x.out)
	if !end {
		n = completePrefix(x.out)
	}
	if n == 0 {
		return
	}
	text := toValidUTF8(x.out[:n])
	if k := len(x.emit); k > 0 && x.emit[k-1].Field == x.field {
		x.emit[k-1].Text += text
	} else {
		x.emit = append(x.emit, Delta{Field: x.field, Text: text})
	}
	x.out = append(x.out[:0], x.out[n:]...)
}

// completePrefix returns the length of the longest prefix of b that does not
// end inside an incomplete (but so far valid) UTF-8 sequence.
func completePrefix(b []byte) int {
	n := len(b)
	for i := 1; i <= utf8.UTFMax && i <= n; i++ {
		c := b[n-i]
		if utf8.RuneStart(c) {
			if !utf8.FullRune(b[n-i:]) {
				return n - i
			}
			return n
		}
	}
	return n
}

func toValidUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	out := make([]byte, 0, len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		out = utf8.AppendRune(out, r) // RuneError for invalid bytes
		b = b[size:]
	}
	return string(out)
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
