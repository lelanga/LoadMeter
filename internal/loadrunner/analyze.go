package loadrunner

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lelanga/phoenix/internal/model"
)

// Portability verdicts.
const (
	Portable            = "portable"
	PortableWithChanges = "portable-with-changes"
	NotPortable         = "not-portable"
)

// Analysis is a static portability assessment of a VuGen script. It is built
// purely from the script text; nothing is compiled or executed.
type Analysis struct {
	Name        string          `json:"name"`
	UsrFile     string          `json:"usrFile,omitempty"`
	Protocols   []string        `json:"protocols"`
	WebProtocol bool            `json:"webProtocol"`
	Files       []*FileFindings `json:"files"`
	// RuntimeFunctions counts calls to proprietary LoadRunner runtime APIs
	// (lr_*, web_*, and other protocol APIs such as ctrx_*, lrs_*) by name.
	RuntimeFunctions map[string]int `json:"runtimeFunctions"`
	// ProtocolFunctions is the subset belonging to non-web protocols, keyed by protocol.
	ProtocolFunctions map[string]int  `json:"protocolFunctions,omitempty"`
	CustomCode        []Finding       `json:"customCode,omitempty"`
	ExternalFiles     []string        `json:"externalFiles,omitempty"`
	Correlations      []Correlation   `json:"correlations,omitempty"`
	Parameters        []ParameterInfo `json:"parameters,omitempty"`
	Transactions      []string        `json:"transactions,omitempty"`
	Requests          int             `json:"requests"`
	Compat            map[string]int  `json:"compat,omitempty"` // compat report counts by status
	Confidence        string          `json:"confidence,omitempty"`
	Assessment        string          `json:"assessment"`
	Reasons           []string        `json:"reasons,omitempty"`
}

// FileFindings are the dependency findings for one script file.
type FileFindings struct {
	File             string         `json:"file"`
	Role             string         `json:"role"` // usr, init, action, end, globals, parameters, data, runtime-settings, other
	Lines            int            `json:"lines,omitempty"`
	Functions        []string       `json:"functions,omitempty"` // C functions defined in the file
	RuntimeFunctions map[string]int `json:"runtimeFunctions,omitempty"`
	CustomCode       []Finding      `json:"customCode,omitempty"`
	Includes         []string       `json:"includes,omitempty"`
	Correlations     []Correlation  `json:"correlations,omitempty"`
	Transactions     []string       `json:"transactions,omitempty"`
	Parameters       []string       `json:"parameters,omitempty"` // {Param} references
	ParseErrors      []string       `json:"parseErrors,omitempty"`
}

type Finding struct {
	Location string `json:"location"`
	Kind     string `json:"kind"` // control-flow, declaration, c-call, custom-function, global, ...
	Excerpt  string `json:"excerpt,omitempty"`
}

type Correlation struct {
	Param    string `json:"param"`
	Function string `json:"function"`
	Kind     string `json:"kind"` // boundary, jsonpath, regex, xpath, header
	Location string `json:"location"`
	Expr     string `json:"expr,omitempty"`
}

type ParameterInfo struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Source  string `json:"source"` // file:line of the definition
	Mapping string `json:"mapping,omitempty"`
}

func (a *Analysis) file(name string) *FileFindings {
	for _, f := range a.Files {
		if f.File == name {
			return f
		}
	}
	f := &FileFindings{File: name, RuntimeFunctions: map[string]int{}}
	a.Files = append(a.Files, f)
	return f
}

func (a *Analysis) addTx(file, name string) {
	ff := a.file(file)
	ff.Transactions = append(ff.Transactions, name)
	for _, t := range a.Transactions {
		if t == name {
			return
		}
	}
	a.Transactions = append(a.Transactions, name)
}

// protocolPrefixes maps API prefixes of non-web VuGen protocols to a
// protocol label. Calls with these prefixes need the licensed runtime.
var protocolPrefixes = []struct{ prefix, proto string }{
	{"ctrx_", "Citrix ICA"},
	{"sapgui_", "SAP GUI"},
	{"sap_", "SAP Web"},
	{"lrs_", "Windows Sockets"},
	{"lrd_", "ODBC/Database"},
	{"lr_db_", "Database"},
	{"nca_", "Oracle NCA"},
	{"truclient_", "TruClient"},
	{"tc_", "TruClient"},
	{"rdp_", "RDP"},
	{"jms_", "JMS"},
	{"mms_", "Media Player"},
	{"smtp_", "SMTP"},
	{"pop3_", "POP3"},
	{"imap_", "IMAP"},
	{"ftp_", "FTP"},
	{"ldap_", "LDAP"},
	{"mldap_", "LDAP"},
	{"TE_", "Terminal Emulator"},
	{"flex_", "Flex"},
	{"amf_", "AMF"},
	{"siebel_", "Siebel"},
	{"soa_", "Web Services"},
	{"soap_request", "Web Services"},
	{"web_service_call", "Web Services"},
	{"web_websocket_", "WebSocket"},
	{"mobile_", "Mobile"},
	{"rte_", "Terminal Emulator"},
	{"mqtt_", "MQTT"},
	{"lr_java_", "Java"},
}

func protocolOf(fn string) string {
	for _, p := range protocolPrefixes {
		if strings.HasPrefix(fn, p.prefix) {
			return p.proto
		}
	}
	return ""
}

func isRuntimeFunc(fn string) bool {
	return strings.HasPrefix(fn, "lr_") || strings.HasPrefix(fn, "web_") || protocolOf(fn) != ""
}

var cKeywords = map[string]bool{"if": true, "for": true, "while": true, "switch": true, "return": true, "sizeof": true, "do": true}

var standardIncludes = map[string]bool{
	"lrun.h": true, "web_api.h": true, "globals.h": true, "as_web.h": true, "lrw_custom_body.h": true,
	"vdf.h": true, "stdio.h": true, "stdlib.h": true, "string.h": true, "time.h": true, "math.h": true,
	"lrs.h": true, "lrd.h": true, "ctrxfuncs.h": true, "sapgui.h": true,
}

// scanFile records token-level findings: runtime function calls (including
// those nested inside expressions and control flow), defined functions,
// includes and {Param} references.
func (im *importer) scanFile(f *cFile, role string) {
	ff := im.an.file(f.name)
	ff.Role = role
	ff.Lines = len(f.lines)
	for _, e := range f.errors {
		ff.ParseErrors = append(ff.ParseErrors, e.Error())
		im.rep.Warnings = append(im.rep.Warnings, fmt.Sprintf("%s:%d: %s", f.name, e.line, e.msg))
	}
	for _, fn := range f.funcs {
		ff.Functions = append(ff.Functions, fn.name)
	}
	toks := f.toks
	for i := 0; i+1 < len(toks); i++ {
		t := toks[i]
		if t.kind != tIdent || !toks[i+1].is(tPunct, "(") || cKeywords[t.text] {
			continue
		}
		if isRuntimeFunc(t.text) {
			ff.RuntimeFunctions[t.text]++
			im.an.RuntimeFunctions[t.text]++
			if p := protocolOf(t.text); p != "" {
				im.an.ProtocolFunctions[p]++
			}
		}
	}
	for _, d := range f.directives {
		if strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(d.text, "#")), "include") {
			inc := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(d.text, "#")), "include"))
			inc = strings.Trim(inc, `"<> `)
			ff.Includes = append(ff.Includes, inc)
			if !standardIncludes[strings.ToLower(baseName(inc))] {
				im.external(inc)
				st, note := model.Partial, "non-standard include; its declarations are not imported"
				if strings.HasSuffix(strings.ToLower(inc), ".c") {
					st, note = model.Unsupported, "included C source file is not imported"
				}
				im.rep.Add("#include", inc, im.loc(f, d.line), st, note)
				ff.CustomCode = append(ff.CustomCode, Finding{Location: im.loc(f, d.line), Kind: "include", Excerpt: d.text})
			}
		}
	}
	seen := map[string]bool{}
	for _, t := range toks {
		if t.kind != tString {
			continue
		}
		for _, n := range braceRefs(t.val, im.lb, im.rb) {
			if !seen[n] {
				seen[n] = true
				ff.Parameters = append(ff.Parameters, n)
			}
		}
	}
}

func braceRefs(s, lb, rb string) []string {
	var out []string
	for {
		i := strings.Index(s, lb)
		if i < 0 {
			return out
		}
		j := strings.Index(s[i+len(lb):], rb)
		if j < 0 {
			return out
		}
		n := s[i+len(lb) : i+len(lb)+j]
		if identRe.MatchString(n) {
			out = append(out, n)
		}
		s = s[i+len(lb)+j+len(rb):]
	}
}

// finishAnalysis aggregates per-file findings and computes the verdict.
func (im *importer) finishAnalysis(protocolBlocked bool) {
	a := im.an
	for _, ff := range a.Files {
		a.CustomCode = append(a.CustomCode, ff.CustomCode...)
		a.Correlations = append(a.Correlations, ff.Correlations...)
	}
	names := make([]string, 0, len(im.params))
	for n := range im.params {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := im.params[n]
		m := im.funcs[n]
		if m == "" {
			for _, ds := range im.sc.DataSources {
				for _, c := range ds.Columns {
					if c == n {
						m = "dataSource " + ds.Name
					}
				}
			}
		}
		if m == "" {
			if _, ok := im.sc.Variables[n]; ok {
				m = "variable"
			}
		}
		a.Parameters = append(a.Parameters, ParameterInfo{Name: n, Type: p.typ, Source: p.loc, Mapping: m})
	}
	im.rep.ComputeConfidence()
	a.Compat = im.rep.Counts()
	a.Confidence = im.rep.Confidence
	protoCalls := 0
	for _, v := range a.ProtocolFunctions {
		protoCalls += v
	}
	switch {
	case protocolBlocked:
		a.Assessment = NotPortable
		a.Reasons = append(a.Reasons, "script protocol is not Web (HTTP/HTML): "+strings.Join(a.Protocols, ", "))
	case a.Requests == 0:
		a.Assessment = NotPortable
		a.Reasons = append(a.Reasons, "no HTTP requests could be imported")
	case protoCalls > a.Requests:
		a.Assessment = NotPortable
		a.Reasons = append(a.Reasons, fmt.Sprintf("%d non-web protocol calls outweigh %d HTTP requests", protoCalls, a.Requests))
	case a.Compat[model.Unsupported] > 0 || a.Compat[model.Partial] > 0:
		a.Assessment = PortableWithChanges
	default:
		a.Assessment = Portable
	}
	if a.Assessment != NotPortable {
		if n := a.Compat[model.Unsupported]; n > 0 {
			a.Reasons = append(a.Reasons, fmt.Sprintf("%d unsupported element(s) need manual changes", n))
		}
		if n := a.Compat[model.Partial]; n > 0 {
			a.Reasons = append(a.Reasons, fmt.Sprintf("%d element(s) converted with reduced fidelity", n))
		}
	}
	if len(a.CustomCode) > 0 {
		a.Reasons = append(a.Reasons, fmt.Sprintf("%d custom C code finding(s)", len(a.CustomCode)))
	}
	if protoCalls > 0 {
		var ps []string
		for p, n := range a.ProtocolFunctions {
			ps = append(ps, fmt.Sprintf("%s (%d)", p, n))
		}
		sort.Strings(ps)
		a.Reasons = append(a.Reasons, "non-web protocol API calls: "+strings.Join(ps, ", "))
	}
	sort.Strings(a.ExternalFiles)
}
