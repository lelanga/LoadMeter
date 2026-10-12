package loadrunner

import (
	"strings"
)

// A minimal, order-preserving INI reader for .usr, .prm, default.cfg and
// default.usp files. Keys and section names compare case-insensitively;
// surrounding double quotes are stripped from values.

type iniKV struct {
	key, val string
	line     int
}

type iniSection struct {
	name string
	line int
	keys []iniKV
}

type iniFile struct {
	sections []*iniSection
}

func parseINI(b []byte) *iniFile {
	text := strings.TrimPrefix(string(b), "\xef\xbb\xbf")
	f := &iniFile{}
	cur := &iniSection{name: "", line: 0}
	f.sections = append(f.sections, cur)
	for n, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || line[0] == ';' || line[0] == '#' {
			continue
		}
		if line[0] == '[' {
			end := strings.LastIndexByte(line, ']')
			if end < 0 {
				end = len(line)
			}
			cur = &iniSection{name: strings.TrimSpace(line[1:end]), line: n + 1}
			f.sections = append(f.sections, cur)
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			cur.keys = append(cur.keys, iniKV{key: strings.TrimSpace(line), line: n + 1})
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			v = v[1 : len(v)-1]
		}
		cur.keys = append(cur.keys, iniKV{key: strings.TrimSpace(k), val: v, line: n + 1})
	}
	return f
}

// section returns the first section with the given name (case-insensitive).
func (f *iniFile) section(name string) *iniSection {
	if f == nil {
		return nil
	}
	for _, s := range f.sections {
		if strings.EqualFold(s.name, name) {
			return s
		}
	}
	return nil
}

func (s *iniSection) get(key string) (string, bool) {
	if s == nil {
		return "", false
	}
	for _, kv := range s.keys {
		if strings.EqualFold(kv.key, key) {
			return kv.val, true
		}
	}
	return "", false
}

func (s *iniSection) val(key string) string {
	v, _ := s.get(key)
	return v
}

// first returns the value of the first key present.
func (s *iniSection) first(keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := s.get(k); ok {
			return v, true
		}
	}
	return "", false
}

// norm lowercases and strips spaces, underscores, dashes and slashes so that
// "Each iteration", "EachIteration" and "each_iteration" compare equal.
func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch r {
		case ' ', '_', '-', '/', '\t', '(', ')':
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
