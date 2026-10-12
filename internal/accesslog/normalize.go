package accesslog

import (
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// IDKind classifies a path segment that was replaced by {id}.
type IDKind string

const (
	IDNumeric IDKind = "numeric"
	IDUUID    IDKind = "uuid"
	IDHash    IDKind = "hash"
)

var (
	uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexRe  = regexp.MustCompile(`^[0-9a-fA-F]+$`)
)

// classifySegment reports whether a path segment looks like an identifier.
// Rules: all digits; UUID; hex string of 16+ chars (or 8+ mixing digits and
// letters a-f); any 20+ char token mixing letters and digits (opaque ids).
func classifySegment(seg string) (IDKind, bool) {
	n := len(seg)
	if n == 0 {
		return "", false
	}
	allDigits := true
	hasDigit, hasAlpha := false, false
	for i := 0; i < n; i++ {
		c := seg[i]
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			hasAlpha = true
			allDigits = false
		default:
			allDigits = false
		}
	}
	switch {
	case allDigits:
		return IDNumeric, true
	case uuidRe.MatchString(seg):
		return IDUUID, true
	case hexRe.MatchString(seg) && (n >= 16 || (n >= 8 && hasDigit && hasAlpha)):
		return IDHash, true
	case n >= 20 && hasDigit && hasAlpha && !strings.ContainsAny(seg, ".%"):
		return IDHash, true
	}
	return "", false
}

// IDSlot describes one {id} position in a normalised path.
type IDSlot struct {
	Index   int    `json:"index"` // 0-based segment index
	Kind    IDKind `json:"kind"`
	Min     int64  `json:"min,omitempty"` // numeric range observed
	Max     int64  `json:"max,omitempty"`
	Len     int    `json:"len,omitempty"` // length of the longest value seen (hash ids)
	Example string `json:"-"`             // first value seen; never printed or stored
}

// Normalize returns the endpoint path template, the raw query (without '?')
// and the id slots found. The path is cleaned (".." removed, one leading
// slash, trailing slash dropped except for "/"). Unparseable escapes are kept.
func Normalize(target string) (tmpl, query string, ids []IDSlot) {
	p := target
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p, query = p[:i], p[i+1:]
	}
	if i := strings.IndexByte(p, '#'); i >= 0 {
		p = p[:i]
	}
	if u, err := url.PathUnescape(p); err == nil {
		p = u
	}
	p = path.Clean("/" + p)
	if p == "/" {
		return "/", query, nil
	}
	segs := strings.Split(p[1:], "/")
	for i, s := range segs {
		kind, ok := classifySegment(s)
		if !ok {
			continue
		}
		slot := IDSlot{Index: i, Kind: kind, Example: s}
		if kind == IDNumeric {
			if v, err := strconv.ParseInt(s, 10, 64); err == nil {
				slot.Min, slot.Max = v, v
			}
		}
		ids = append(ids, slot)
		segs[i] = "{id}"
	}
	return "/" + strings.Join(segs, "/"), query, ids
}

var staticExt = map[string]bool{
	".css": true, ".js": true, ".mjs": true, ".map": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".svg": true, ".ico": true, ".webp": true, ".avif": true, ".woff": true, ".woff2": true, ".ttf": true, ".eot": true,
	".otf": true, ".mp4": true, ".webm": true, ".mp3": true, ".pdf": true, ".zip": true, ".gz": true, ".txt": true,
	".xml": true, ".bmp": true, ".swf": true,
}

// isStatic reports a static-asset path by extension.
func isStatic(p string) bool {
	i := strings.LastIndexByte(p, '.')
	if i < 0 || strings.LastIndexByte(p, '/') > i {
		return false
	}
	return staticExt[strings.ToLower(p[i:])]
}
