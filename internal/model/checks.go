package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// MatchIndex selects which extractor match is kept: 0 = first, n > 0 = nth
// (1-based), -1 = random, MatchAll = every match. In JSON it is a number or
// one of the strings "all", "random", "first" (or a numeric string).
//
// JMeter numbers matches differently (0 = random, -1 = all); the JMX importer
// and exporter translate.
type MatchIndex int

// Match index values with a special meaning.
const (
	MatchFirst  MatchIndex = 0
	MatchRandom MatchIndex = -1
	MatchAll    MatchIndex = -2
)

func (m MatchIndex) MarshalJSON() ([]byte, error) {
	if m == MatchAll {
		return []byte(`"all"`), nil
	}
	return []byte(strconv.Itoa(int(m))), nil
}

func (m *MatchIndex) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, `"`) {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "all":
			*m = MatchAll
			return nil
		case "random":
			*m = MatchRandom
			return nil
		case "first", "":
			*m = MatchFirst
			return nil
		}
	}
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || v < -1 {
		return fmt.Errorf(`match must be a number >= -1, "all", "random" or "first", got %s`, string(b))
	}
	*m = MatchIndex(v)
	return nil
}

// Extractor sources: what text a regex or boundary extractor reads.
const (
	SrcBody           = "body"           // response body (default)
	SrcHeaders        = "headers"        // response status line and headers ("Name: value" lines)
	SrcRequestHeaders = "requestHeaders" // request headers as sent
	SrcURL            = "url"            // final request URL (after redirects)
	SrcStatus         = "status"         // response status code
	SrcMessage        = "message"        // response reason phrase ("OK")
)

// ExtractorSources lists the valid Extractor.Source values.
var ExtractorSources = []string{SrcBody, SrcHeaders, SrcRequestHeaders, SrcURL, SrcStatus, SrcMessage}

// DefaultMaxMatches caps how many matches an "all" extractor stores.
const DefaultMaxMatches = 1000

// ExtractorOptions are the JMeter-parity extractor fields. They are embedded
// in Extractor, so they appear directly in the extractor JSON object.
type ExtractorOptions struct {
	// Template (regex only) builds the value from groups: "$1$-$2$" ($0$ is the
	// whole match). It replaces Group when set.
	Template string `json:"template,omitempty"`
	// Source (regex and boundary): body (default), headers, requestHeaders,
	// url, status or message.
	Source string `json:"source,omitempty"`
	// MaxMatches caps the number of matches stored with match "all"
	// (default 1000).
	MaxMatches int `json:"maxMatches,omitempty"`
}

// New assertion types (see Assertion).
const (
	AsEquals = "equals" // the text (Source, default body) equals Value exactly
	AsSize   = "size"   // byte size compared with Op against Value; Expr: body (default)|full
	AsHash   = "hash"   // hex digest of the body equals Value; Expr: md5|sha1|sha256|sha512 (default md5)
)

// Assertion sources: what text contains/not_contains/regex/equals check.
var AssertionSources = []string{SrcBody, SrcHeaders, SrcRequestHeaders, SrcURL, SrcStatus, SrcMessage}

// AssertionOptions are the JMeter-parity assertion fields, embedded in Assertion.
type AssertionOptions struct {
	// Not inverts the result (JMeter "Not" / "Invert assertion").
	Not bool `json:"not,omitempty"`
	// Message replaces the generated failure message (JMeter custom failure message).
	Message string `json:"message,omitempty"`
	// Source (contains, not_contains, regex, equals): body (default), headers,
	// requestHeaders, url, status or message.
	Source string `json:"source,omitempty"`
	// Op (size): ==, !=, <, <=, >, >= (default ==).
	Op string `json:"op,omitempty"`
	// Regex (jsonpath): Value is a regular expression that must match the
	// whole value instead of being compared for equality.
	Regex bool `json:"regex,omitempty"`
	// ExpectNull (jsonpath): the path must exist and be JSON null.
	ExpectNull bool `json:"expectNull,omitempty"`
}

// RequestCheckOptions holds request-level response-check settings, embedded in Request.
type RequestCheckOptions struct {
	// IgnoreStatus stops a status >= 400 from failing the request by itself
	// (JMeter "Ignore Status"); assertions still apply.
	IgnoreStatus bool `json:"ignoreStatus,omitempty"`
}

// AssertionTypes lists every valid Assertion.Type.
var AssertionTypes = []string{AsStatus, AsContains, AsNotContains, AsRegex, AsJSONPath, AsMaxLatency, AsHeader, AsEquals, AsSize, AsHash}

// SizeOps lists valid size-assertion operators.
var SizeOps = []string{"==", "!=", "<", "<=", ">", ">="}

// HashAlgorithms lists valid hash-assertion algorithms.
var HashAlgorithms = []string{"md5", "sha1", "sha256", "sha512"}

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

func validateExtractor(where string, e Extractor) []string {
	var p []string
	if e.Match < MatchAll {
		p = append(p, fmt.Sprintf("%s: extractor %q: match %d is invalid", where, e.Name, e.Match))
	}
	if e.MaxMatches < 0 {
		p = append(p, fmt.Sprintf("%s: extractor %q: maxMatches must be >= 0", where, e.Name))
	}
	if e.Source != "" {
		if e.Type != ExRegex && e.Type != ExBoundary {
			p = append(p, fmt.Sprintf("%s: extractor %q: source is only valid for regex and boundary", where, e.Name))
		} else if !oneOf(e.Source, ExtractorSources) {
			p = append(p, fmt.Sprintf("%s: extractor %q: unknown source %q (expected one of %v)", where, e.Name, e.Source, ExtractorSources))
		}
	}
	if e.Template != "" && e.Type != ExRegex {
		p = append(p, fmt.Sprintf("%s: extractor %q: template is only valid for regex", where, e.Name))
	}
	return p
}

func validateAssertion(where string, a Assertion) []string {
	var p []string
	if !oneOf(a.Type, AssertionTypes) {
		return []string{fmt.Sprintf("%s: unknown assertion type %q", where, a.Type)}
	}
	if a.Source != "" {
		switch a.Type {
		case AsContains, AsNotContains, AsRegex, AsEquals:
			if !oneOf(a.Source, AssertionSources) {
				p = append(p, fmt.Sprintf("%s: %s assertion: unknown source %q (expected one of %v)", where, a.Type, a.Source, AssertionSources))
			}
		default:
			p = append(p, fmt.Sprintf("%s: source is not valid on a %s assertion", where, a.Type))
		}
	}
	if a.Op != "" && a.Type != AsSize {
		p = append(p, fmt.Sprintf("%s: op is only valid on a size assertion", where))
	}
	if (a.Regex || a.ExpectNull) && a.Type != AsJSONPath {
		p = append(p, fmt.Sprintf("%s: regex/expectNull are only valid on a jsonpath assertion", where))
	}
	switch a.Type {
	case AsSize:
		if a.Op != "" && !oneOf(a.Op, SizeOps) {
			p = append(p, fmt.Sprintf("%s: size op %q must be one of %v", where, a.Op, SizeOps))
		}
		if a.Expr != "" && a.Expr != "body" && a.Expr != "full" {
			p = append(p, fmt.Sprintf("%s: size expr %q must be body or full", where, a.Expr))
		}
		if !strings.Contains(a.Value, "${") {
			if n, err := strconv.ParseInt(strings.TrimSpace(a.Value), 10, 64); err != nil || n < 0 {
				p = append(p, fmt.Sprintf("%s: size value %q must be a non-negative number of bytes", where, a.Value))
			}
		}
	case AsHash:
		if a.Expr != "" && !oneOf(strings.ToLower(a.Expr), HashAlgorithms) {
			p = append(p, fmt.Sprintf("%s: hash expr %q must be one of %v", where, a.Expr, HashAlgorithms))
		}
		if a.Value == "" {
			p = append(p, fmt.Sprintf("%s: hash assertion needs the expected hex digest in value", where))
		}
	case AsJSONPath:
		if a.Regex && a.ExpectNull {
			p = append(p, fmt.Sprintf("%s: jsonpath assertion cannot set both regex and expectNull", where))
		}
	}
	return p
}
