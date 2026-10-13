package extract

import (
	"strconv"
	"strings"
)

// RegexMatch is one regex match: Groups[0] is the whole match, Groups[i] group i.
type RegexMatch struct {
	Groups []string
}

// RegexAll returns up to limit matches (limit <= 0: no limit) with all groups.
func RegexAll(text, expr string, limit int) ([]RegexMatch, error) {
	re, err := compile(expr)
	if err != nil {
		return nil, err
	}
	n := -1
	if limit > 0 {
		n = limit
	}
	var out []RegexMatch
	for _, m := range re.FindAllStringSubmatch(text, n) {
		out = append(out, RegexMatch{Groups: m})
	}
	return out, nil
}

// Template renders a JMeter-style template: $N$ is replaced by group N
// ($0$ = whole match); unknown groups render as empty text. Other text is
// copied as written.
func (m RegexMatch) Template(tpl string) string {
	if !strings.Contains(tpl, "$") {
		return tpl
	}
	var b strings.Builder
	for i := 0; i < len(tpl); i++ {
		if tpl[i] == '$' {
			j := i + 1
			for j < len(tpl) && tpl[j] >= '0' && tpl[j] <= '9' {
				j++
			}
			if j > i+1 && j < len(tpl) && tpl[j] == '$' {
				g, _ := strconv.Atoi(tpl[i+1 : j])
				if g < len(m.Groups) {
					b.WriteString(m.Groups[g])
				}
				i = j
				continue
			}
		}
		b.WriteByte(tpl[i])
	}
	return b.String()
}

// Group returns group g, or "" when the pattern has fewer groups.
func (m RegexMatch) Group(g int) string {
	if g >= 0 && g < len(m.Groups) {
		return m.Groups[g]
	}
	return ""
}
