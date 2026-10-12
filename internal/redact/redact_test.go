package redact

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestMask(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""}, {"a", "****"}, {"abcd", "****"}, {"abcde", "ab…(5)"}, {"abcdefghijkl", "ab…(12)"},
		{"abcdefghijklm", "abcd…(13 chars)"},
	}
	for _, tc := range tests {
		if got := Mask(tc.in); got != tc.want {
			t.Errorf("Mask(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
	long := strings.Repeat("x", 1234)
	if got := Mask(long); !strings.Contains(got, "1234 chars") || strings.Contains(got, long[:10]) {
		t.Errorf("long mask %q", got)
	}
}

func TestSensitiveName(t *testing.T) {
	yes := []string{"Authorization", "X-Api-Key", "api_key", "password", "Set-Cookie", "client_secret", "access_token", "sessionid", "X-CSRF-Token", "PIN", "otp"}
	no := []string{"Content-Type", "Accept", "User-Agent", "name", "id", "limit", "pinned"}
	for _, n := range yes {
		if !SensitiveName(n) {
			t.Errorf("%q should be sensitive", n)
		}
	}
	for _, n := range no {
		if SensitiveName(n) {
			t.Errorf("%q should not be sensitive", n)
		}
	}
}

func TestHeaders(t *testing.T) {
	if Headers(nil) != nil {
		t.Error("nil in, nil out")
	}
	in := map[string]string{"Authorization": "Bearer abcdefghijklmnop", "Content-Type": "application/json", "Cookie": "sid=1234567890abc"}
	out := Headers(in)
	if out["Content-Type"] != "application/json" {
		t.Error("benign header changed")
	}
	for _, k := range []string{"Authorization", "Cookie"} {
		if !strings.HasPrefix(out[k], "[REDACTED") || strings.Contains(out[k], "abcdefghijklmnop") || strings.Contains(out[k], "1234567890abc") {
			t.Errorf("%s -> %q", k, out[k])
		}
	}
	if in["Authorization"] != "Bearer abcdefghijklmnop" {
		t.Error("input map must not be mutated")
	}
}

func TestBodyJSON(t *testing.T) {
	in := `{"user":"bob","password":"hunter2hunter2","nested":{"api_key":"abcdef123456","items":[{"token":"t0k3nt0k3nt0k3n","ok":1}]},"n":5,"secret":123}`
	out := Body(in)
	if strings.Contains(out, "hunter2") || strings.Contains(out, "abcdef123456") || strings.Contains(out, "t0k3nt0k3n") {
		t.Fatalf("leak: %s", out)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("output must remain valid JSON: %v (%s)", err, out)
	}
	if v["user"] != "bob" || v["n"] != float64(5) {
		t.Errorf("benign fields changed: %v", v)
	}
	if v["secret"] != "[REDACTED]" {
		t.Errorf("non-string secret: %v", v["secret"])
	}
	if arr := Body(`[{"password":"abcdefghij"}]`); strings.Contains(arr, "abcdefghij") {
		t.Errorf("array body leak %s", arr)
	}
}

func TestBodyFormAndFallback(t *testing.T) {
	out := Body("user=bob&password=hunter2&token=abc123")
	q, err := url.ParseQuery(out)
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("user") != "bob" || q.Get("password") != "[REDACTED]" || q.Get("token") != "[REDACTED]" {
		t.Errorf("form: %s", out)
	}
	// non-JSON/non-form text falls through to Text()
	txt := Body("hello Bearer abcdefghijklmnop world")
	if strings.Contains(txt, "abcdefghijklmnop") {
		t.Errorf("text fallback leak %q", txt)
	}
	// broken JSON falls back
	if got := Body(`{"password":"abcdefgh"`); strings.Contains(got, "abcdefgh") {
		t.Errorf("broken JSON leak %q", got)
	}
	if Body("") != "" {
		t.Error("empty")
	}
}

func TestText(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV"
	tests := []struct {
		name, in string
		gone     string // substring that must not survive
		keep     string // substring that must survive
	}{
		{"bearer", "Authorization: Bearer abcdef1234567890xyz", "abcdef1234567890xyz", "Bearer"},
		{"basic", "auth Basic dXNlcjpwYXNzd29yZA==", "dXNlcjpwYXNzd29yZA", ""},
		{"jwt", "token is " + jwt + " end", "SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV", "end"},
		{"kv password", `password=hunter2&x=1`, "hunter2", "x=1"},
		{"json kv", `{"client_secret": "s3cr3tvalue", "a": "b"}`, "s3cr3tvalue", `"a": "b"`},
		{"visa luhn", "card 4111 1111 1111 1111 ok", "4111", "ok"},
		{"visa dashes", "card 4111-1111-1111-1111", "4111", ""},
		{"visa plain", "card 4111111111111111.", "4111111111111111", ""},
		{"amex", "378282246310005", "378282246310005", ""},
		{"non-luhn", "order 4111111111111112 shipped", "", "4111111111111112"},
		{"short number", "id 12345678", "", "12345678"},
		{"timestamp", "ts 1700000000000 done", "", "1700000000000"},
		{"plain", "nothing to hide here", "", "nothing to hide here"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Text(tc.in)
			if tc.gone != "" && strings.Contains(got, tc.gone) {
				t.Errorf("%q survived in %q", tc.gone, got)
			}
			if tc.keep != "" && !strings.Contains(got, tc.keep) {
				t.Errorf("%q lost in %q", tc.keep, got)
			}
		})
	}
	if got := Text("4111111111111111"); got != "[REDACTED-CARD]" {
		t.Errorf("card marker %q", got)
	}
	if got := Text("password=abc"); !strings.Contains(got, "[REDACTED]") {
		t.Errorf("kv marker %q", got)
	}
}

func TestPII(t *testing.T) {
	got := PII("mail alice.smith+x@example.co.uk and bob@x.io, pw password=zzz123")
	if strings.Contains(got, "alice") || strings.Contains(got, "bob@") || strings.Contains(got, "zzz123") {
		t.Errorf("PII leak %q", got)
	}
	if strings.Count(got, "[EMAIL]") != 2 {
		t.Errorf("expected 2 email markers: %q", got)
	}
	if Text("mail a@b.com") != "mail a@b.com" {
		t.Error("Text alone must not mask e-mail")
	}
}

func TestURL(t *testing.T) {
	tests := []struct {
		in       string
		gone     []string
		keep     []string
		identity bool
	}{
		{"http://h/p?token=abc123&page=2", []string{"abc123"}, []string{"page=2", "token=REDACTED"}, false},
		{"http://h/p?api_key=K&password=P&q=x", []string{"=K&", "=P&"}, []string{"q=x"}, false},
		{"http://h/p?page=2", nil, nil, true},
		{"http://h/p", nil, nil, true},
		{"::not a url", nil, nil, true},
	}
	for _, tc := range tests {
		got := URL(tc.in)
		if tc.identity && got != tc.in {
			t.Errorf("%q changed to %q", tc.in, got)
		}
		for _, g := range tc.gone {
			if strings.Contains(got, g) {
				t.Errorf("%q still has %q", got, g)
			}
		}
		for _, k := range tc.keep {
			if !strings.Contains(got, k) {
				t.Errorf("%q lost %q", got, k)
			}
		}
	}
}
