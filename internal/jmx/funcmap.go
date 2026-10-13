package jmx

import (
	"fmt"
	"strings"

	"github.com/lelanga/loadmeter/internal/model"
)

// passFuncs maps JMeter functions to Load Meter built-ins that take the same
// arguments (including JMeter's optional result-variable argument), so they
// are rewritten by name only. Arguments are mapped recursively.
var passFuncs = map[string]string{
	"__V": "__v", "__eval": "__eval", "__evalVar": "__evalVar", "__isVarDefined": "__isVarDefined",
	"__split": "__split", "__intSum": "__intSum", "__longSum": "__longSum", "__changeCase": "__changeCase",
	"__digest": "__digest", "__base64Encode": "__base64Encode", "__base64Decode": "__base64Decode",
	"__escapeHtml": "__escapeHtml", "__unescapeHtml": "__unescapeHtml", "__escapeXml": "__escapeXml",
	"__urldecode": "__urldecode", "__unescape": "__unescape", "__char": "__char",
	"__escapeOroRegexpChars": "__escapeRegex", "__timeShift": "__timeShift", "__RandomDate": "__randomDate",
	"__dateTimeConvert": "__dateTimeConvert", "__RandomFromMultipleVars": "__randomFromVars",
}

// passNotes are compatibility notes for specific pass-through functions.
var passNotes = map[string]string{
	"__escapeOroRegexpChars": "quotes for Go RE2 (same metacharacters as Perl5 in practice)",
	"__base64Encode":         "JMeter needs the jmeter-plugins \"Custom JMeter Functions\" for this function",
	"__base64Decode":         "JMeter needs the jmeter-plugins \"Custom JMeter Functions\" for this function",
}

// localeArg is the index of the locale argument of date functions.
var localeArg = map[string]int{"__timeShift": 3, "__RandomDate": 3}

func joinCall(name string, args []string) string {
	return "${" + name + "(" + strings.Join(args, ",") + ")}"
}

func importPassthrough(r ref, raw string, vars map[string]string, add func(fn, status, format string, a ...any)) (string, bool) {
	name, ok := passFuncs[r.name]
	if !ok {
		return "", false
	}
	args := make([]string, len(r.args))
	for i, a := range r.args {
		v, sub := importFuncs(a, vars)
		for _, is := range sub {
			add(is.Fn, is.Status, "%s", is.Note)
		}
		args[i] = v
	}
	if n, ok := passNotes[r.name]; ok {
		status := model.Supported
		if strings.HasPrefix(r.name, "__escapeOro") {
			status = model.Partial
		}
		if status != model.Supported {
			add(r.name, status, "%s: %s", raw, n)
		}
	}
	if i, ok := localeArg[r.name]; ok && i < len(r.args) {
		if l := strings.TrimSpace(r.args[i]); l != "" && !strings.HasPrefix(strings.ToLower(l), "en") {
			add(r.name, model.Partial, "%s: locale %q ignored; Load Meter writes English month and day names", raw, l)
		}
	}
	return joinCall(name, args), true
}

func exportPassthrough(r ref, issues *[]fnIssue) (string, bool) {
	jm := ""
	for j, l := range passFuncs {
		if l == r.name {
			jm = j
			break
		}
	}
	if jm == "" {
		return "", false
	}
	args := make([]string, len(r.args))
	for i, a := range r.args {
		v, sub := exportFuncs(a)
		*issues = append(*issues, sub...)
		args[i] = v
	}
	if n, ok := passNotes[jm]; ok {
		*issues = append(*issues, fnIssue{Fn: r.name, Status: model.Partial, Note: fmt.Sprintf("%s -> %s: %s", r.name, jm, n)})
	}
	return joinCall(jm, args), true
}
