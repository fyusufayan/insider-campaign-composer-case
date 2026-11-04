package logging

import (
	"log/slog"
	"regexp"
	"strings"
)

// Phone is a phone number that is masked whenever it is logged.
type Phone string

// LogValue implements slog.LogValuer.
func (p Phone) LogValue() slog.Value { return slog.StringValue(MaskPhone(string(p))) }

// Email is an email address that is masked whenever it is logged.
type Email string

// LogValue implements slog.LogValuer.
func (e Email) LogValue() slog.Value { return slog.StringValue(MaskEmail(string(e))) }

// MaskPhone keeps a leading "+" with the first two digits and the last two digits:
// "+905321234567" → "+90********67". Short values are masked entirely.
func MaskPhone(p string) string {
	r := []rune(p)
	keepHead := 0
	if len(r) > 0 && r[0] == '+' {
		keepHead = 3
	}
	const keepTail = 2
	if len(r) < keepHead+keepTail+4 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:keepHead]) + strings.Repeat("*", len(r)-keepHead-keepTail) + string(r[len(r)-keepTail:])
}

// MaskEmail keeps the first rune of the local part and the domain:
// "ayse.yilmaz@example.com" → "a***@example.com". Values without "@" are masked entirely.
func MaskEmail(e string) string {
	at := strings.LastIndex(e, "@")
	if at <= 0 {
		return strings.Repeat("*", len([]rune(e)))
	}
	first := []rune(e[:at])[0]
	return string(first) + "***" + e[at:]
}

var (
	emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// E.164 numbers; the leading "+" avoids masking ids, counts and timestamps.
	phonePattern = regexp.MustCompile(`\+[1-9][0-9]{7,14}`)
)

// MaskText masks every email address and E.164 phone number embedded in s.
// It is the safety net applied to all logged strings (messages, attributes,
// error texts); code should still log contacts through Phone and Email.
func MaskText(s string) string {
	if !strings.ContainsAny(s, "@+") {
		return s
	}
	s = emailPattern.ReplaceAllStringFunc(s, MaskEmail)
	return phonePattern.ReplaceAllStringFunc(s, MaskPhone)
}
