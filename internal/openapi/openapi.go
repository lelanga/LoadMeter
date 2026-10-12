// Package openapi converts OpenAPI 3.0/3.1 documents (JSON) into Phoenix
// scenarios. Only local "#/..." references are resolved; cyclic references are
// truncated; credentials are never imported (security schemes become
// ${secret_*} placeholders).
package openapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lelanga/phoenix/internal/model"
)

// MaxInput is the largest accepted document.
const MaxInput = 20 << 20

type Options struct {
	Name               string
	Tags               []string // include only operations with one of these tags
	ExcludeTags        []string
	OperationIDs       []string // include only these operationIds
	GroupBy            string   // "tag" (default) or "path"
	IncludeDeprecated  bool
	IncludeDestructive bool // DELETE operations
}

type importer struct {
	doc  map[string]any
	o    Options
	rep  *model.CompatReport
	sc   *model.Scenario
	vars map[string]string
}

var methods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// deref resolves a (possibly $ref) node; chains are limited to 32 hops.
func (im *importer) deref(v any) (any, error) {
	for i := 0; i < 32; i++ {
		m := asMap(v)
		ref, ok := m["$ref"].(string)
		if !ok {
			return v, nil
		}
		if !strings.HasPrefix(ref, "#/") {
			return nil, fmt.Errorf("external $ref %s is not supported", ref)
		}
		t, err := im.pointer(ref)
		if err != nil {
			return nil, err
		}
		v = t
	}
	return nil, errors.New("$ref chain too long or cyclic")
}

func (im *importer) pointer(ref string) (any, error) {
	var cur any = im.doc
	for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		if u, err := url.PathUnescape(seg); err == nil {
			seg = u
		}
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			n, ok := c[seg]
			if !ok {
				return nil, fmt.Errorf("unresolved $ref %s", ref)
			}
			cur = n
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(c) {
				return nil, fmt.Errorf("unresolved $ref %s", ref)
			}
			cur = c[i]
		default:
			return nil, fmt.Errorf("unresolved $ref %s", ref)
		}
	}
	return cur, nil
}

var nonIdent = regexp.MustCompile(`[^A-Za-z0-9_]+`)

func ident(s string) string {
	s = strings.Trim(nonIdent.ReplaceAllString(s, "_"), "_")
	if s == "" {
		return "x"
	}
	return s
}

func (im *importer) setVar(base, val string) string {
	name := ident(base)
	for i := 2; ; i++ {
		if cur, ok := im.vars[name]; !ok || cur == val {
			im.vars[name] = val
			return name
		}
		name = ident(base) + "_" + strconv.Itoa(i)
	}
}

func lower(in []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range in {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			m[s] = true
		}
	}
	return m
}

// Import converts an OpenAPI JSON document.
func Import(r io.Reader, source string, o Options) (*model.Scenario, *model.CompatReport, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxInput+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > MaxInput {
		return nil, nil, fmt.Errorf("OpenAPI document exceeds the %d MB limit", MaxInput>>20)
	}
	trim := bytes.TrimSpace(data)
	if len(trim) == 0 || trim[0] != '{' {
		return nil, nil, errors.New("only JSON OpenAPI documents are supported (YAML is not); convert first, e.g. `yq -o=json spec.yaml > spec.json`")
	}
	dec := json.NewDecoder(bytes.NewReader(trim))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("not valid JSON: %w", err)
	}
	ver, _ := doc["openapi"].(string)
	if _, sw := doc["swagger"]; sw {
		return nil, nil, errors.New("Swagger 2.0 is not supported; convert to OpenAPI 3.x first")
	}
	if !strings.HasPrefix(ver, "3.0") && !strings.HasPrefix(ver, "3.1") {
		return nil, nil, fmt.Errorf("unsupported or missing \"openapi\" version %q (expected 3.0.x or 3.1.x)", ver)
	}
	paths := asMap(doc["paths"])
	if len(paths) == 0 {
		return nil, nil, errors.New("document has no paths")
	}
	info := asMap(doc["info"])
	title, _ := info["title"].(string)
	name := o.Name
	if name == "" {
		name = title
	}
	if name == "" {
		name = strings.TrimSuffix(source, ".json")
	}

	im := &importer{doc: doc, o: o, vars: map[string]string{}}
	im.sc = model.NewScenario(name)
	im.sc.Variables = im.vars
	im.sc.Workload = model.Workload{Executor: model.ExecConstantVUs, VUs: 1, Duration: model.Duration(30 * time.Second)}
	im.sc.Metadata = map[string]string{"importer": "openapi", "source": source, "openapiVersion": ver}
	if title != "" {
		im.sc.Metadata["title"] = title
	}
	if v, _ := info["version"].(string); v != "" {
		im.sc.Metadata["apiVersion"] = v
	}
	if d, _ := info["description"].(string); d != "" {
		if len(d) > 300 {
			d = d[:300]
		}
		im.sc.Description = d
	}
	im.rep = &model.CompatReport{Source: source, Format: "openapi"}
	im.server()

	for _, k := range []string{"webhooks"} {
		if w := asMap(doc[k]); len(w) > 0 {
			im.rep.Add(k, "", k, model.Unsupported, "webhooks are callbacks into the client and cannot be load tested; skipped")
		}
	}

	// Collect operations.
	type opRef struct {
		path, method string
		op, item     map[string]any
	}
	var ops []opRef
	pkeys := make([]string, 0, len(paths))
	for p := range paths {
		pkeys = append(pkeys, p)
	}
	sort.Strings(pkeys)
	for _, p := range pkeys {
		itemRaw, err := im.deref(paths[p])
		item := asMap(itemRaw)
		if err != nil || item == nil {
			im.rep.Add("path", p, "paths."+p, model.Unsupported, "path item could not be resolved: "+errString(err))
			continue
		}
		found := false
		for _, mth := range methods {
			if op := asMap(item[mth]); op != nil {
				ops = append(ops, opRef{p, mth, op, item})
				found = true
			}
		}
		if !found {
			im.rep.Add("path", p, "paths."+p, model.Ignored, "path item has no operations")
		}
	}

	incTags, excTags, ids := lower(o.Tags), lower(o.ExcludeTags), map[string]bool{}
	for _, s := range o.OperationIDs {
		ids[strings.TrimSpace(s)] = true
	}
	groups := map[string][]model.Step{}
	var order []string
	for _, r := range ops {
		loc := "paths." + r.path + "." + r.method
		opID, _ := r.op["operationId"].(string)
		label := strings.ToUpper(r.method) + " " + r.path
		if opID != "" {
			label = opID
		}
		var tags []string
		if ts, ok := r.op["tags"].([]any); ok {
			for _, t := range ts {
				if s, ok := t.(string); ok {
					tags = append(tags, s)
				}
			}
		}
		skip := func(why string) { im.rep.Add("operation", label, loc, model.Ignored, why) }
		if len(ids) > 0 && !ids[opID] {
			skip("excluded by operationId filter")
			continue
		}
		if len(incTags) > 0 {
			ok := false
			for _, t := range tags {
				ok = ok || incTags[strings.ToLower(t)]
			}
			if !ok {
				skip("excluded by tag filter")
				continue
			}
		}
		excluded := false
		for _, t := range tags {
			excluded = excluded || excTags[strings.ToLower(t)]
		}
		if excluded {
			skip("excluded by --exclude-tags")
			continue
		}
		if dep, _ := r.op["deprecated"].(bool); dep && !o.IncludeDeprecated {
			skip("deprecated operation skipped (use --include-deprecated)")
			continue
		}
		if r.method == "delete" && !o.IncludeDestructive {
			skip("destructive DELETE skipped (use --include-destructive)")
			continue
		}
		step, status, notes := im.operation(r.path, r.method, r.op, r.item, label)
		im.rep.Add("operation", label, loc, status, strings.Join(notes, "; "))
		if step == nil {
			continue
		}
		grp := "default"
		if o.GroupBy == "path" {
			grp = r.path
		} else if len(tags) > 0 {
			grp = tags[0]
			if len(tags) > 1 {
				im.rep.Warnings = append(im.rep.Warnings, fmt.Sprintf("%s has %d tags; grouped under the first (%s)", label, len(tags), tags[0]))
			}
		} else {
			grp = "untagged"
		}
		if _, ok := groups[grp]; !ok {
			order = append(order, grp)
		}
		groups[grp] = append(groups[grp], *step)
	}
	if len(order) == 0 {
		return nil, im.rep, errors.New("no operations left to import after applying filters")
	}
	// Order groups: top-level tag order first, then the rest alphabetically.
	rank := map[string]int{}
	if ts, ok := doc["tags"].([]any); ok {
		for i, t := range ts {
			if n, _ := asMap(t)["name"].(string); n != "" {
				rank[n] = i + 1
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		ri, rj := rank[order[i]], rank[order[j]]
		if (ri == 0) != (rj == 0) {
			return rj == 0
		}
		if ri != rj {
			return ri < rj
		}
		return order[i] < order[j]
	})
	for _, g := range order {
		im.sc.Steps = append(im.sc.Steps, model.Step{Type: model.StepTransaction, Name: g, Steps: groups[g]})
	}
	if len(im.secretsUsed()) > 0 {
		im.rep.Manual = append(im.rep.Manual, "Set the secret variables ("+strings.Join(im.secretsUsed(), ", ")+") before running, e.g. with ${__env(PHOENIX_...)} or --set; no credentials were imported.")
	}
	if probs := im.sc.Validate(); len(probs) > 0 {
		im.rep.Validated = "failed"
		im.rep.Warnings = append(im.rep.Warnings, probs...)
	} else {
		im.rep.Validated = "passed"
	}
	im.rep.ComputeConfidence()
	return im.sc, im.rep, nil
}

func errString(err error) string {
	if err == nil {
		return "not an object"
	}
	return err.Error()
}

func (im *importer) secretsUsed() []string {
	var out []string
	for k := range im.vars {
		if strings.HasPrefix(k, "secret_") {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

var serverVar = regexp.MustCompile(`\{([^{}]+)\}`)

func (im *importer) server() {
	base := "http://127.0.0.1:8099"
	if servers, ok := im.doc["servers"].([]any); ok && len(servers) > 0 {
		s := asMap(servers[0])
		u, _ := s["url"].(string)
		vs := asMap(s["variables"])
		u = serverVar.ReplaceAllStringFunc(u, func(m string) string {
			if d, ok := asMap(vs[m[1:len(m)-1]])["default"].(string); ok {
				return d
			}
			return m
		})
		if u != "" {
			if strings.Contains(u, "://") {
				base = strings.TrimRight(u, "/")
				im.rep.Add("servers", u, "servers[0]", model.Supported, "set as the baseUrl variable; the run-time safety allowlist still applies")
			} else {
				base += "/" + strings.Trim(u, "/")
				base = strings.TrimRight(base, "/")
				im.rep.Add("servers", u, "servers[0]", model.Partial, "relative server URL; baseUrl defaults to http://127.0.0.1:8099 plus that path")
			}
		}
		if len(servers) > 1 {
			im.rep.Add("servers", "", "servers[1:]", model.Ignored, fmt.Sprintf("%d additional servers ignored; only servers[0] is used", len(servers)-1))
		}
	} else {
		im.rep.Add("servers", "", "servers", model.Partial, "no servers declared; baseUrl defaults to http://127.0.0.1:8099")
	}
	im.vars["baseUrl"] = base
	im.sc.Defaults.BaseURL = "${baseUrl}"
	im.sc.Defaults.Timeout = model.Duration(30 * time.Second)
}
