// Package param detects business inputs that should come from test data and
// generates synthetic datasets. It never fabricates real customer records.
package param

import (
	"crypto/sha1"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lelanga/phoenix/internal/model"
	"github.com/lelanga/phoenix/internal/redact"
)

type Use struct {
	Step  string `json:"step"`
	Field string `json:"field"` // form:name | query:name | json:key
}

type Suggestion struct {
	ID        string `json:"id"`
	Column    string `json:"column"`
	Kind      string `json:"kind"` // username | password | email | account | search | product | amount | date | reference
	Recorded  string `json:"recordedValue"`
	Uses      []Use  `json:"uses"`
	Mode      string `json:"suggestedMode"` // unique | random | sequential | per-vu
	Generator string `json:"generator"`
	Sensitive bool   `json:"sensitive"`
	Why       string `json:"why"`
	Risk      string `json:"risk"`
	raw       string
}

var kinds = []struct {
	kind, mode, gen string
	re              *regexp.Regexp
}{
	{"password", "per-vu", "copy of the recorded test password (supply real test-account passwords)", regexp.MustCompile(`(?i)^(pass(word)?|pwd|passwd)$`)},
	{"email", "unique", "perf.user.N@example.test (reserved .test domain)", regexp.MustCompile(`(?i)e-?mail`)},
	{"username", "unique", "perfuserNNNN", regexp.MustCompile(`(?i)^(user(name)?|login|uname|user_?id)$`)},
	{"account", "sequential", "ACC-NNNNNN (synthetic)", regexp.MustCompile(`(?i)(account|acct|customer|cust)(_?id|no|number)?$`)},
	{"search", "random", "words from a small synthetic vocabulary", regexp.MustCompile(`(?i)^(q|query|search|term|keyword|s)$`)},
	{"product", "random", "recorded product IDs (from responses) or SKU-NNN", regexp.MustCompile(`(?i)(product|sku|item)(_?id)?$`)},
	{"amount", "random", "random amount 1.00–500.00", regexp.MustCompile(`(?i)(amount|price|total|qty|quantity)$`)},
	{"date", "sequential", "dates starting tomorrow (ISO 8601)", regexp.MustCompile(`(?i)(date|dob|from|to|checkin|checkout)$`)},
	{"reference", "unique", "REF-<run>-NNNN", regexp.MustCompile(`(?i)(ref|reference|order_?ref|invoice)(_?no|number)?$`)},
}

func classify(field string) (string, string, string) {
	for _, k := range kinds {
		if k.re.MatchString(field) {
			return k.kind, k.mode, k.gen
		}
	}
	return "", "", ""
}

// Detect scans requests for parameterizable inputs.
func Detect(sc *model.Scenario) []Suggestion {
	byCol := map[string]*Suggestion{}
	var order []string
	add := func(step, field, name, value string) {
		if value == "" || strings.Contains(value, "${") {
			return
		}
		kind, mode, gen := classify(name)
		if kind == "" {
			return
		}
		col := strings.ToLower(regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(name, "_"))
		s := byCol[col]
		if s == nil {
			s = &Suggestion{Column: col, Kind: kind, Mode: mode, Generator: gen, raw: value, Sensitive: kind == "password" || kind == "email"}
			s.Recorded = value
			if s.Sensitive {
				s.Recorded = redact.Mask(value)
			}
			s.Why = fmt.Sprintf("Field %q carries %s input. Replaying one recorded value for every virtual user creates unrealistic caching, locking or duplicate-key behaviour.", name, kind)
			switch kind {
			case "password":
				s.Risk = "Passwords must belong to dedicated test accounts. The generated file contains the recorded test password; replace it with real test credentials and keep the file private."
			case "username", "email":
				s.Risk = "Synthetic usernames must exist in the test environment before the run; generate or provision them first."
			default:
				s.Risk = "Synthetic values may not exist in the system under test; validate with a debug run."
			}
			byCol[col] = s
			order = append(order, col)
		}
		s.Uses = append(s.Uses, Use{Step: step, Field: field})
	}
	for _, st := range sc.AllRequests() {
		r := st.Request
		for _, kv := range r.Form {
			add(st.Name, "form:"+kv.Name, kv.Name, kv.Value)
		}
		if u, err := url.Parse(r.URL); err == nil {
			for k, vs := range u.Query() {
				for _, v := range vs {
					add(st.Name, "query:"+k, k, v)
				}
			}
		}
		if b := strings.TrimSpace(r.Body); strings.HasPrefix(b, "{") {
			var m map[string]any
			if json.Unmarshal([]byte(b), &m) == nil {
				keys := make([]string, 0, len(m))
				for k := range m {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					if s, ok := m[k].(string); ok {
						add(st.Name, "json:"+k, k, s)
					}
				}
			}
		}
	}
	out := make([]Suggestion, 0, len(order))
	for _, c := range order {
		s := byCol[c]
		h := sha1.Sum([]byte(s.Column + "|" + s.Kind))
		s.ID = hex.EncodeToString(h[:])[:10]
		out = append(out, *s)
	}
	return out
}

var vocab = []string{"laptop", "chair", "coffee", "notebook", "headphones", "lamp", "backpack", "monitor", "keyboard", "bottle"}

func generate(s Suggestion, i int) string {
	switch s.Kind {
	case "password":
		return s.raw
	case "email":
		return fmt.Sprintf("perf.user.%04d@example.test", i+1)
	case "username":
		return fmt.Sprintf("perfuser%04d", i+1)
	case "account":
		return fmt.Sprintf("ACC-%06d", 100000+i)
	case "search":
		return vocab[i%len(vocab)]
	case "product":
		if s.raw != "" && i == 0 {
			return s.raw
		}
		return fmt.Sprintf("SKU-%03d", i%5+1)
	case "amount":
		return fmt.Sprintf("%d.%02d", 1+(i*37)%500, (i*13)%100)
	case "date":
		return time.Now().AddDate(0, 0, 1+i%60).Format("2006-01-02")
	case "reference":
		return fmt.Sprintf("REF-%s-%04d", time.Now().Format("0102"), i+1)
	}
	return s.raw
}

// Apply writes a synthetic CSV next to the scenario and rewrites the selected
// fields to ${column}. dir is the scenario directory; file is the CSV name.
func Apply(sc *model.Scenario, sugg []Suggestion, ids map[string]bool, dir, file string, rows int) ([]string, error) {
	if rows <= 0 {
		rows = 100
	}
	var sel []Suggestion
	for _, s := range sugg {
		if ids[s.ID] {
			sel = append(sel, s)
		}
	}
	if len(sel) == 0 {
		return nil, fmt.Errorf("no suggestions selected")
	}
	cols := make([]string, len(sel))
	for i, s := range sel {
		cols[i] = s.Column
	}
	f, err := os.OpenFile(dir+string(os.PathSeparator)+file, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	w := csv.NewWriter(f)
	_ = w.Write(cols)
	for i := 0; i < rows; i++ {
		row := make([]string, len(sel))
		for j, s := range sel {
			row[j] = generate(s, i)
		}
		_ = w.Write(row)
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	steps := map[string]*model.Step{}
	for _, st := range sc.AllRequests() {
		steps[st.Name] = st
	}
	var applied []string
	for _, s := range sel {
		for _, u := range s.Uses {
			st := steps[u.Step]
			if st == nil {
				continue
			}
			kind, name, _ := strings.Cut(u.Field, ":")
			r := st.Request
			switch kind {
			case "form":
				for i := range r.Form {
					if r.Form[i].Name == name {
						r.Form[i].Value = "${" + s.Column + "}"
					}
				}
			case "query":
				if pu, err := url.Parse(r.URL); err == nil {
					q := pu.Query()
					old := q.Get(name)
					r.URL = strings.Replace(r.URL, name+"="+url.QueryEscape(old), name+"=${__urlencode(${"+s.Column+"})}", 1)
				}
			case "json":
				var m map[string]any
				if json.Unmarshal([]byte(r.Body), &m) == nil {
					m[name] = "${" + s.Column + "}"
					b, _ := json.Marshal(m)
					r.Body = string(b)
				}
			}
		}
		applied = append(applied, s.ID)
	}
	mode := "sequential"
	for _, s := range sel {
		if s.Mode == "unique" {
			mode = "unique"
		}
	}
	name := strings.TrimSuffix(file, ".csv")
	// replace an existing data source with the same name
	out := sc.DataSources[:0]
	for _, d := range sc.DataSources {
		if d.Name != name {
			out = append(out, d)
		}
	}
	sens := false
	for _, s := range sel {
		sens = sens || s.Sensitive
	}
	onEx := "recycle"
	if mode == "unique" {
		onEx = "stop"
	}
	sc.DataSources = append(out, model.DataSource{Name: name, File: file, Mode: mode, OnExhaust: onEx, Sensitive: sens})
	return applied, nil
}
