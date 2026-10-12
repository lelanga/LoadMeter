package summary

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Violation is a sentence that states a number which is not in the facts.
type Violation struct {
	Sentence string `json:"sentence"`
	Number   string `json:"number"`
}

func (v Violation) String() string { return fmt.Sprintf("%q in %q", v.Number, v.Sentence) }

var (
	reNumber    = regexp.MustCompile(`\d{1,3}(?:,\d{3})+(?:\.\d+)?|\d+(?:\.\d+)?`)
	reStatusCls = regexp.MustCompile(`(?i)\b[1-5]xx\b`)
	reListMark  = regexp.MustCompile(`(?m)^\s*(?:\d+[.)]|[-*•])\s+`)
	reSentence  = regexp.MustCompile(`[^.!?\n]+(?:\.\d[^.!?\n]*)*[.!?]*`)
)

type numTok struct {
	raw  string
	val  float64
	tol  float64 // how far the true value may be and still round to the text
	unit string  // ms | s | min | h when a time unit follows the number
}

// extractNumbers finds the numeric claims in text. Digits glued to a preceding
// letter (p95, HTTP2, req42) are identifiers, not claims. "12k", "1.5 million"
// and "12,000" are understood; precision comes from the written decimals, or
// from trailing zeros of values of 1,000 and up ("about 12,000").
func extractNumbers(text string) []numTok {
	text = reStatusCls.ReplaceAllString(text, " ")
	text = reListMark.ReplaceAllString(text, "")
	var out []numTok
	for _, loc := range reNumber.FindAllStringIndex(text, -1) {
		s, e := loc[0], loc[1]
		if s > 0 {
			p := text[s-1]
			if p == '_' || p >= 'a' && p <= 'z' || p >= 'A' && p <= 'Z' || p == '.' && s > 1 && text[s-2] >= '0' && text[s-2] <= '9' {
				continue
			}
		}
		raw := text[s:e]
		clean := strings.ReplaceAll(raw, ",", "")
		v, err := strconv.ParseFloat(clean, 64)
		if err != nil {
			continue
		}
		dec := 0
		if i := strings.IndexByte(clean, '.'); i >= 0 {
			dec = len(clean) - i - 1
		}
		tol := 0.5 * math.Pow(10, -float64(dec))
		rest := strings.ToLower(text[e:])
		switch {
		case strings.HasPrefix(rest, "k") && (len(rest) == 1 || !isAlpha(rest[1])):
			v, tol = v*1e3, tol*1e3
			raw += "k"
		case strings.HasPrefix(strings.TrimLeft(rest, " "), "million"):
			v, tol = v*1e6, tol*1e6
			raw += " million"
		case strings.HasPrefix(strings.TrimLeft(rest, " "), "thousand"):
			v, tol = v*1e3, tol*1e3
			raw += " thousand"
		case dec == 0 && v >= 1000:
			tz := 0
			for i := len(clean) - 1; i > 0 && clean[i] == '0'; i-- {
				tz++
			}
			tol = 0.5 * math.Pow(10, float64(tz))
		}
		out = append(out, numTok{raw: raw, val: v, tol: tol, unit: timeUnit(text[e:])})
	}
	return out
}

var reUnit = regexp.MustCompile(`^\s?([A-Za-z]+)`)

// timeUnit recognises a time unit right after a number.
func timeUnit(rest string) string {
	m := reUnit.FindStringSubmatch(rest)
	if m == nil {
		return ""
	}
	switch strings.ToLower(m[1]) {
	case "ms", "msec", "msecs", "millisecond", "milliseconds":
		return "ms"
	case "s", "sec", "secs", "second", "seconds":
		return "s"
	case "min", "mins", "minute", "minutes":
		return "min"
	case "h", "hr", "hrs", "hour", "hours":
		return "h"
	}
	return ""
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

var wordNums = map[string]float64{"two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
	"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15, "twenty": 20, "thirty": 30, "forty": 40,
	"fifty": 50, "sixty": 60, "seventy": 70, "eighty": 80, "ninety": 90, "hundred": 100}

var reWord = regexp.MustCompile(`(?i)\b[a-z]+\b`)

func wordNumbers(text string) []numTok {
	var out []numTok
	for _, w := range reWord.FindAllString(text, -1) {
		if v, ok := wordNums[strings.ToLower(w)]; ok {
			out = append(out, numTok{raw: w, val: v, tol: 0.5})
		}
	}
	return out
}

// allowedSet holds the numbers a summary may state. Time conversions are only
// applied to facts that really are durations (JSON keys ending in Ms or Sec),
// and only when the text puts a time unit after the number, so "3 minutes"
// cannot be justified by an unrelated 180.
type allowedSet struct {
	any []float64 // every number in the facts (numeric values and numbers inside fact strings)
	ms  []float64 // millisecond-valued facts
	sec []float64 // second-valued facts
}

func (f Facts) allowed() allowedSet {
	b, _ := json.Marshal(f)
	var doc any
	_ = json.Unmarshal(b, &doc)
	var a allowedSet
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				walk(k, c)
			}
		case []any:
			for _, c := range x {
				walk(key, c)
			}
		case float64:
			a.any = append(a.any, x)
			switch {
			case strings.HasSuffix(key, "Ms"):
				a.ms = append(a.ms, x)
			case strings.HasSuffix(key, "Sec"):
				a.sec = append(a.sec, x)
			}
		case string:
			for _, n := range extractNumbers(x) {
				a.any = append(a.any, n.val)
			}
		}
	}
	walk("", doc)
	return a
}

func near(n numTok, vals []float64, scale float64) bool {
	for _, a := range vals {
		if math.Abs(n.val-a*scale) <= n.tol+1e-9 {
			return true
		}
	}
	return false
}

func (a allowedSet) ok(n numTok) bool {
	switch n.unit {
	case "ms":
		return near(n, a.ms, 1) || near(n, a.sec, 1000) || near(n, a.any, 1)
	case "s":
		return near(n, a.sec, 1) || near(n, a.ms, 0.001) || near(n, a.any, 1)
	case "min":
		return near(n, a.sec, 1.0/60)
	case "h":
		return near(n, a.sec, 1.0/3600)
	}
	return near(n, a.any, 1)
}

// Verify returns the sentences of text that contain a number (digits, "12k",
// "3 million" or a spelled-out two..ninety) that is not in the facts within
// rounding. An empty result means every number is grounded.
//
// It checks numbers only. It cannot tell whether a true number is attached to
// the right thing, so the prompt also restricts the model to the supplied
// facts, and the template summary remains the reference text.
func Verify(text string, f Facts) []Violation {
	allowed := f.allowed()
	var out []Violation
	for _, s := range reSentence.FindAllString(text, -1) {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		toks := append(extractNumbers(s), wordNumbers(s)...)
		for _, n := range toks {
			if !allowed.ok(n) {
				out = append(out, Violation{Sentence: s, Number: n.raw})
				break
			}
		}
	}
	return out
}
