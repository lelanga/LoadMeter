package vars

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// dateTok is one Java SimpleDateFormat token: a run of the same pattern letter,
// or literal text (quoted or not a letter).
type dateTok struct {
	letter byte // 0 for literal
	n      int  // run length
	lit    string
}

func tokenizeJavaDate(p string) ([]dateTok, error) {
	var out []dateTok
	for i := 0; i < len(p); {
		ch := p[i]
		switch {
		case ch == '\'':
			// '' is a literal quote; 'text' is literal text ('' inside is a quote).
			if i+1 < len(p) && p[i+1] == '\'' {
				out = append(out, dateTok{lit: "'"})
				i += 2
				continue
			}
			var b strings.Builder
			j := i + 1
			for ; j < len(p); j++ {
				if p[j] == '\'' {
					if j+1 < len(p) && p[j+1] == '\'' {
						b.WriteByte('\'')
						j++
						continue
					}
					break
				}
				b.WriteByte(p[j])
			}
			if j >= len(p) {
				return nil, fmt.Errorf("date format %q: unterminated quote", p)
			}
			out = append(out, dateTok{lit: b.String()})
			i = j + 1
		case ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z':
			j := i
			for j < len(p) && p[j] == ch {
				j++
			}
			if !strings.ContainsRune("GyYMLwWDdFEuaHkKhmsSzZX", rune(ch)) {
				return nil, fmt.Errorf("date format %q: unsupported pattern letter %q", p, ch)
			}
			out = append(out, dateTok{letter: ch, n: j - i})
			i = j
		default:
			j := i
			for j < len(p) && p[j] != '\'' && !(p[j] >= 'a' && p[j] <= 'z' || p[j] >= 'A' && p[j] <= 'Z') {
				j++
			}
			out = append(out, dateTok{lit: p[i:j]})
			i = j
		}
	}
	return out, nil
}

func pad(v, n int) string {
	s := strconv.Itoa(v)
	for len(s) < n {
		s = "0" + s
	}
	return s
}

// FormatJavaDate formats t with a Java SimpleDateFormat pattern (English names).
// Supported letters: G y Y M L w W D d F E u a H k K h m s S z Z X.
func FormatJavaDate(t time.Time, pattern string) (string, error) {
	toks, err := tokenizeJavaDate(pattern)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, tk := range toks {
		if tk.letter == 0 {
			b.WriteString(tk.lit)
			continue
		}
		n := tk.n
		switch tk.letter {
		case 'G':
			b.WriteString("AD")
		case 'y', 'Y':
			y := t.Year()
			if tk.letter == 'Y' {
				y, _ = t.ISOWeek()
			}
			if n == 2 {
				b.WriteString(pad(y%100, 2))
			} else {
				b.WriteString(pad(y, n))
			}
		case 'M', 'L':
			switch {
			case n >= 4:
				b.WriteString(t.Month().String())
			case n == 3:
				b.WriteString(t.Month().String()[:3])
			default:
				b.WriteString(pad(int(t.Month()), n))
			}
		case 'w':
			_, w := t.ISOWeek()
			b.WriteString(pad(w, n))
		case 'W':
			b.WriteString(pad((t.Day()-1+int(time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).Weekday()))/7+1, n))
		case 'D':
			b.WriteString(pad(t.YearDay(), n))
		case 'd':
			b.WriteString(pad(t.Day(), n))
		case 'F':
			b.WriteString(pad((t.Day()-1)/7+1, n))
		case 'E':
			if n >= 4 {
				b.WriteString(t.Weekday().String())
			} else {
				b.WriteString(t.Weekday().String()[:3])
			}
		case 'u':
			wd := int(t.Weekday())
			if wd == 0 {
				wd = 7
			}
			b.WriteString(pad(wd, n))
		case 'a':
			if t.Hour() < 12 {
				b.WriteString("AM")
			} else {
				b.WriteString("PM")
			}
		case 'H':
			b.WriteString(pad(t.Hour(), n))
		case 'k':
			h := t.Hour()
			if h == 0 {
				h = 24
			}
			b.WriteString(pad(h, n))
		case 'K':
			b.WriteString(pad(t.Hour()%12, n))
		case 'h':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			b.WriteString(pad(h, n))
		case 'm':
			b.WriteString(pad(t.Minute(), n))
		case 's':
			b.WriteString(pad(t.Second(), n))
		case 'S':
			b.WriteString(pad(t.Nanosecond()/int(time.Millisecond), n))
		case 'z':
			b.WriteString(t.Format("MST"))
		case 'Z':
			b.WriteString(t.Format("-0700"))
		case 'X':
			switch n {
			case 1:
				b.WriteString(t.Format("Z07"))
			case 2:
				b.WriteString(t.Format("Z0700"))
			default:
				b.WriteString(t.Format("Z07:00"))
			}
		}
	}
	return b.String(), nil
}

// JavaToGoLayout converts a Java SimpleDateFormat pattern into a Go time
// layout, for parsing. Letters Go cannot parse (G Y w W F u k K) and literal
// text that would be read as a Go layout element are refused.
func JavaToGoLayout(pattern string) (string, error) {
	toks, err := tokenizeJavaDate(pattern)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for i, tk := range toks {
		if tk.letter == 0 {
			if goLayoutWord(tk.lit) {
				return "", fmt.Errorf("date format %q: literal %q cannot be expressed as a Go layout", pattern, tk.lit)
			}
			b.WriteString(tk.lit)
			continue
		}
		n := tk.n
		var s string
		switch tk.letter {
		case 'y':
			s = map[bool]string{true: "06", false: "2006"}[n == 2]
		case 'M', 'L':
			switch {
			case n >= 4:
				s = "January"
			case n == 3:
				s = "Jan"
			case n == 2:
				s = "01"
			default:
				s = "1"
			}
		case 'D':
			s = "002"
		case 'd':
			s = map[bool]string{true: "02", false: "2"}[n >= 2]
		case 'E':
			s = map[bool]string{true: "Monday", false: "Mon"}[n >= 4]
		case 'a':
			s = "PM"
		case 'H':
			s = "15"
		case 'h':
			s = map[bool]string{true: "03", false: "3"}[n >= 2]
		case 'm':
			s = map[bool]string{true: "04", false: "4"}[n >= 2]
		case 's':
			s = map[bool]string{true: "05", false: "5"}[n >= 2]
		case 'S':
			if i == 0 || toks[i-1].letter != 0 || (toks[i-1].lit != "." && toks[i-1].lit != ",") {
				return "", fmt.Errorf("date format %q: milliseconds (S) can only be parsed after '.' or ','", pattern)
			}
			s = strings.Repeat("0", n)
		case 'z':
			s = "MST"
		case 'Z':
			s = "-0700"
		case 'X':
			s = map[int]string{1: "Z07", 2: "Z0700"}[n]
			if s == "" {
				s = "Z07:00"
			}
		default:
			return "", fmt.Errorf("date format %q: pattern letter %q cannot be parsed", pattern, tk.letter)
		}
		b.WriteString(s)
	}
	return b.String(), nil
}

// goLayoutWord reports whether literal text would be misread by Go's layout parser.
func goLayoutWord(s string) bool {
	for _, w := range []string{"Jan", "Mon", "MST", "PM", "pm", "200", "15", "0", "1", "2", "3", "4", "5", "6", "7", "Z07", "-07", "+07"} {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// ParseISODuration parses the ISO-8601 durations accepted by Java's
// Duration.parse: [-]P[nD][T[nH][nM][n[.n]S]], each number optionally signed
// (PT-6H3M, -P1DT2H, PT0.5S).
func ParseISODuration(s string) (time.Duration, error) {
	orig := s
	bad := fmt.Errorf("invalid ISO-8601 duration %q", orig)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	} else {
		s = strings.TrimPrefix(s, "+")
	}
	if len(s) < 2 || (s[0] != 'P' && s[0] != 'p') {
		return 0, bad
	}
	s = s[1:]
	var total float64
	inTime, any := false, false
	for len(s) > 0 {
		if s[0] == 'T' || s[0] == 't' {
			if inTime {
				return 0, bad
			}
			inTime, s = true, s[1:]
			continue
		}
		j := 0
		for j < len(s) && (s[j] == '-' || s[j] == '+' || s[j] == '.' || s[j] >= '0' && s[j] <= '9') {
			j++
		}
		if j == 0 || j >= len(s) {
			return 0, bad
		}
		v, err := strconv.ParseFloat(s[:j], 64)
		if err != nil {
			return 0, bad
		}
		unit := s[j] | 0x20
		switch {
		case !inTime && unit == 'd':
			total += v * 86400
		case inTime && unit == 'h':
			total += v * 3600
		case inTime && unit == 'm':
			total += v * 60
		case inTime && unit == 's':
			total += v
		default:
			return 0, bad
		}
		any = true
		s = s[j+1:]
	}
	if !any {
		return 0, bad
	}
	if neg {
		total = -total
	}
	return time.Duration(total * float64(time.Second)), nil
}
