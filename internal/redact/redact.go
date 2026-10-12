// Package redact masks credentials and personal data before anything is shown
// in the UI, written to logs or sent to an AI provider.
package redact

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

var sensitiveName = regexp.MustCompile(`(?i)(pass(word|wd)?|pwd|secret|token|api[-_]?key|apikey|auth|session|cookie|credential|private|ssn|card[-_]?number|cvv|pin\b|otp|signature|bearer|jwt|refresh)`)

// SensitiveName reports whether a header/field/key name likely carries a secret.
func SensitiveName(name string) bool { return sensitiveName.MatchString(name) }

// Mask keeps a short prefix so users can recognise a value without exposing it.
func Mask(v string) string {
	switch {
	case v == "":
		return ""
	case len(v) <= 4:
		return "****"
	case len(v) <= 12:
		return v[:2] + "…(" + itoa(len(v)) + ")"
	default:
		return v[:4] + "…(" + itoa(len(v)) + " chars)"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func Headers(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if SensitiveName(k) {
			out[k] = "[REDACTED " + Mask(v) + "]"
		} else {
			out[k] = v
		}
	}
	return out
}

var (
	bearerRe = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9\-._~+/=]{8,}`)
	jwtRe    = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
	emailRe  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	cardRe   = regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`)
	kvRe     = regexp.MustCompile(`(?i)("?(?:password|passwd|pwd|secret|token|api_?key|access_token|refresh_token|client_secret)"?\s*[:=]\s*"?)([^"&\s,}]+)`)
)

// Text masks secrets in free text (logs, response bodies, scripts).
func Text(s string) string {
	s = bearerRe.ReplaceAllString(s, "$1 [REDACTED]")
	s = jwtRe.ReplaceAllString(s, "[REDACTED-JWT]")
	s = kvRe.ReplaceAllString(s, "$1[REDACTED]")
	s = cardRe.ReplaceAllStringFunc(s, func(m string) string {
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, m)
		if luhn(digits) {
			return "[REDACTED-CARD]"
		}
		return m
	})
	return s
}

// PII additionally masks e-mail addresses (used before sending data to cloud AI).
func PII(s string) string {
	return emailRe.ReplaceAllString(Text(s), "[EMAIL]")
}

func luhn(d string) bool {
	if len(d) < 13 {
		return false
	}
	sum, alt := 0, false
	for i := len(d) - 1; i >= 0; i-- {
		n := int(d[i] - '0')
		if alt {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		alt = !alt
	}
	return sum%10 == 0
}

// Body redacts JSON or form bodies structurally, falling back to Text.
func Body(s string) string {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		var v any
		if json.Unmarshal([]byte(t), &v) == nil {
			v = walk(v)
			if b, err := json.Marshal(v); err == nil {
				return Text(string(b))
			}
		}
	}
	if strings.Contains(t, "=") && !strings.ContainsAny(t, " <\n") {
		if q, err := url.ParseQuery(t); err == nil && len(q) > 0 {
			for k := range q {
				if SensitiveName(k) {
					q.Set(k, "[REDACTED]")
				}
			}
			return q.Encode()
		}
	}
	return Text(s)
}

func walk(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if SensitiveName(k) {
				if s, ok := val.(string); ok {
					x[k] = "[REDACTED " + Mask(s) + "]"
				} else {
					x[k] = "[REDACTED]"
				}
			} else {
				x[k] = walk(val)
			}
		}
	case []any:
		for i := range x {
			x[i] = walk(x[i])
		}
	}
	return v
}

// URL masks sensitive query parameters.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}
	q := u.Query()
	changed := false
	for k := range q {
		if SensitiveName(k) {
			q.Set(k, "REDACTED")
			changed = true
		}
	}
	if changed {
		u.RawQuery = q.Encode()
	}
	return u.String()
}
