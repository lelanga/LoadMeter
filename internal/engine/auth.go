package engine

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"net/http"
	"net/url"
	"strings"

	"github.com/lelanga/loadmeter/internal/model"
)

// HTTP authentication (JMeter AuthManager parity): basic and bearer are sent
// pre-emptively; digest (RFC 7616) answers a 401 challenge once and then
// reuses the challenge per VU with an incrementing nonce count. Credentials
// come from ${secret_*}/${__env()} references (enforced by Validate) and are
// redacted from inspector output.

// authCache is per-VU digest state keyed by rule index + host.
type authCache map[string]*digestChallenge

type digestChallenge struct {
	realm, nonce, opaque, algorithm string
	qop                             string // "auth" or "" (RFC 2069)
	userhash                        bool
	nc                              uint32
}

// matchAuth returns the rule with the longest URL prefix matching u.
func (vu *vuState) matchAuth(u *url.URL) (int, *model.AuthRule) {
	rules := vu.r.sc.Defaults.Auth
	if len(rules) == 0 {
		return -1, nil
	}
	full := u.String()
	best, bestLen := -1, -1
	for i := range rules {
		p := vu.authPrefix(&rules[i])
		if p == "" || !prefixMatch(full, p) {
			continue
		}
		if len(p) > bestLen {
			best, bestLen = i, len(p)
		}
	}
	if best < 0 {
		return -1, nil
	}
	return best, &rules[best]
}

func (vu *vuState) authPrefix(r *model.AuthRule) string {
	p, _ := vu.vc.Expand(r.URLPrefix)
	if !strings.Contains(p, "://") {
		base, _ := vu.vc.Expand(vu.r.sc.Defaults.BaseURL)
		base = strings.TrimRight(base, "/")
		if base == "" {
			return ""
		}
		if p != "" && !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		p = base + p
	}
	return p
}

// prefixMatch requires the prefix to end on a URL boundary, so
// "https://api.example.com" does not match "https://api.example.com.evil.test/".
func prefixMatch(full, prefix string) bool {
	if len(full) < len(prefix) || !strings.EqualFold(full[:len(prefix)], prefix) {
		return false
	}
	if len(full) == len(prefix) || strings.HasSuffix(prefix, "/") {
		return true
	}
	switch full[len(prefix)] {
	case '/', '?', '#':
		return true
	}
	return false
}

// applyAuth sets Authorization for the matching rule unless the scenario set
// the header explicitly. It returns the scheme applied ("" when none).
func (vu *vuState) applyAuth(req *http.Request, explicit bool) string {
	if explicit {
		return ""
	}
	idx, rule := vu.matchAuth(req.URL)
	if rule == nil {
		return ""
	}
	switch rule.Type {
	case model.AuthBasic:
		user, _ := vu.vc.Expand(rule.Username)
		pass, _ := vu.vc.Expand(rule.Password)
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
		return model.AuthBasic
	case model.AuthBearer:
		tok, _ := vu.vc.Expand(rule.Token)
		req.Header.Set("Authorization", "Bearer "+tok)
		return model.AuthBearer
	case model.AuthDigest:
		if ch := vu.auth[digestKey(idx, req.URL)]; ch != nil {
			if h, err := vu.digestHeader(rule, ch, req); err == nil {
				req.Header.Set("Authorization", h)
				return model.AuthDigest
			}
		}
	}
	return ""
}

func digestKey(idx int, u *url.URL) string {
	return fmt.Sprintf("%d|%s", idx, strings.ToLower(u.Host))
}

// digestChallengeFor parses a 401 response for a usable digest challenge for
// the rule matching the response URL and caches it; a nil challenge means a
// resend with credentials makes no sense.
func (vu *vuState) digestChallengeFor(resp *http.Response) (*model.AuthRule, *digestChallenge) {
	if resp.StatusCode != http.StatusUnauthorized || resp.Request == nil {
		return nil, nil
	}
	idx, rule := vu.matchAuth(resp.Request.URL)
	if rule == nil || rule.Type != model.AuthDigest {
		return nil, nil
	}
	realm, _ := vu.vc.Expand(rule.Realm)
	var best *digestChallenge
	for _, c := range parseChallenges(resp.Header.Values("WWW-Authenticate")) {
		if !strings.EqualFold(c.scheme, "digest") {
			continue
		}
		ch := &digestChallenge{realm: c.params["realm"], nonce: c.params["nonce"], opaque: c.params["opaque"],
			algorithm: strings.ToUpper(c.params["algorithm"]), userhash: strings.EqualFold(c.params["userhash"], "true")}
		if ch.algorithm == "" {
			ch.algorithm = "MD5"
		}
		if ch.nonce == "" || digestHash(ch.algorithm) == nil || (realm != "" && ch.realm != realm) {
			continue
		}
		if q, ok := c.params["qop"]; ok {
			for _, o := range strings.Split(q, ",") {
				if strings.EqualFold(strings.TrimSpace(o), "auth") {
					ch.qop = "auth"
				}
			}
			if ch.qop == "" {
				continue // only auth-int offered: not supported
			}
		}
		if best == nil || digestRank(ch.algorithm) > digestRank(best.algorithm) {
			best = ch
		}
	}
	if best == nil {
		return nil, nil
	}
	key := digestKey(idx, resp.Request.URL)
	if old := vu.auth[key]; old != nil && old.nonce == best.nonce && old.realm == best.realm && old.algorithm == best.algorithm {
		best.nc = old.nc // same nonce: keep counting
	}
	vu.auth[key] = best
	return rule, best
}

func digestRank(alg string) int {
	switch strings.TrimSuffix(alg, "-SESS") {
	case "SHA-512-256":
		return 3
	case "SHA-256":
		return 2
	}
	return 1
}

func digestHash(alg string) func() hash.Hash {
	switch strings.TrimSuffix(strings.ToUpper(alg), "-SESS") {
	case "MD5":
		return md5.New
	case "SHA-256":
		return sha256.New
	case "SHA-512-256":
		return sha512.New512_256
	}
	return nil
}

// digestHeader computes the RFC 7616 Authorization value for req and
// increments the challenge's nonce count.
func (vu *vuState) digestHeader(rule *model.AuthRule, ch *digestChallenge, req *http.Request) (string, error) {
	newH := digestHash(ch.algorithm)
	if newH == nil {
		return "", fmt.Errorf("unsupported digest algorithm %q", ch.algorithm)
	}
	h := func(s string) string {
		x := newH()
		x.Write([]byte(s))
		return hex.EncodeToString(x.Sum(nil))
	}
	user, _ := vu.vc.Expand(rule.Username)
	pass, _ := vu.vc.Expand(rule.Password)
	ch.nc++
	nc := fmt.Sprintf("%08x", ch.nc)
	var cb [16]byte
	_, _ = rand.Read(cb[:])
	cnonce := hex.EncodeToString(cb[:])
	uri := req.URL.RequestURI()
	ha1 := h(user + ":" + ch.realm + ":" + pass)
	if strings.HasSuffix(ch.algorithm, "-SESS") {
		ha1 = h(ha1 + ":" + ch.nonce + ":" + cnonce)
	}
	ha2 := h(req.Method + ":" + uri)
	var response string
	if ch.qop == "auth" {
		response = h(ha1 + ":" + ch.nonce + ":" + nc + ":" + cnonce + ":auth:" + ha2)
	} else {
		response = h(ha1 + ":" + ch.nonce + ":" + ha2)
	}
	name := user
	if ch.userhash {
		name = h(user + ":" + ch.realm)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `Digest username=%s, realm=%s, nonce=%s, uri=%s, algorithm=%s, response=%s`,
		quote(name), quote(ch.realm), quote(ch.nonce), quote(uri), ch.algorithm, quote(response))
	if ch.opaque != "" {
		fmt.Fprintf(&b, ", opaque=%s", quote(ch.opaque))
	}
	if ch.qop == "auth" {
		fmt.Fprintf(&b, ", qop=auth, nc=%s, cnonce=%s", nc, quote(cnonce))
	}
	if ch.userhash {
		b.WriteString(", userhash=true")
	}
	return b.String(), nil
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

type challenge struct {
	scheme string
	params map[string]string
}

// parseChallenges parses WWW-Authenticate values (RFC 9110 §11.6.1), which
// may hold several comma-separated challenges with quoted parameters.
func parseChallenges(values []string) []challenge {
	var out []challenge
	for _, v := range values {
		p := &authParser{s: v}
		var cur *challenge
		for {
			p.skip()
			if p.done() {
				break
			}
			tok := p.token()
			if tok == "" {
				p.i++ // stray character
				continue
			}
			p.skipSpaces()
			if !p.done() && p.s[p.i] == '=' {
				// auth-param of the current challenge (or token68 padding).
				p.i++
				p.skipSpaces()
				val := p.value()
				if cur != nil {
					cur.params[strings.ToLower(tok)] = val
				}
				continue
			}
			// A new scheme.
			out = append(out, challenge{scheme: tok, params: map[string]string{}})
			cur = &out[len(out)-1]
		}
	}
	return out
}

type authParser struct {
	s string
	i int
}

func (p *authParser) done() bool { return p.i >= len(p.s) }

func (p *authParser) skipSpaces() {
	for !p.done() && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

func (p *authParser) skip() {
	for !p.done() && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == ',') {
		p.i++
	}
}

func (p *authParser) token() string {
	start := p.i
	for !p.done() {
		c := p.s[p.i]
		if c == ' ' || c == '\t' || c == ',' || c == '=' || c == '"' {
			break
		}
		p.i++
	}
	return p.s[start:p.i]
}

func (p *authParser) value() string {
	if p.done() {
		return ""
	}
	if p.s[p.i] != '"' {
		return p.token()
	}
	p.i++
	var b strings.Builder
	for !p.done() {
		c := p.s[p.i]
		p.i++
		switch c {
		case '\\':
			if !p.done() {
				b.WriteByte(p.s[p.i])
				p.i++
			}
		case '"':
			return b.String()
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
