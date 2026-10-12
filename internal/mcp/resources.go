package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lelanga/phoenix/docs"
	"github.com/lelanga/phoenix/internal/app"
)

const maxListedRuns = 50

var resourceTemplates = []map[string]any{
	{"uriTemplate": "phoenix://scenarios/{file}", "name": "scenario", "title": "Scenario JSON", "description": "A scenario file from the workspace, e.g. phoenix://scenarios/shop-login.json", "mimeType": "application/json"},
	{"uriTemplate": "phoenix://runs/{runId}/summary", "name": "run-summary", "title": "Run summary", "description": "Summary JSON of a finished run (metrics, SLAs, notes)", "mimeType": "application/json"},
	{"uriTemplate": "phoenix://runs/{runId}/report", "name": "run-report", "title": "Run report", "description": "Self-contained HTML report of a finished run", "mimeType": "text/html"},
	{"uriTemplate": "phoenix://docs/{name}", "name": "doc", "title": "Documentation", "description": "Phoenix docs. Available: scenario-format", "mimeType": "text/markdown"},
}

var scenarioFileRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,100}\.json$`)

// -32002 is the MCP "resource not found" code.
func notFound(uri string) error { return &rpcErr{-32002, "resource not found: " + uri} }

func (s *Server) resourcesList() (any, error) {
	list := []map[string]any{
		{"uri": "phoenix://docs/scenario-format", "name": "scenario-format", "title": "Scenario format (phoenix/v1)", "mimeType": "text/markdown"},
	}
	if scs, err := s.Svc.ListScenarios(); err == nil {
		for _, sc := range scs {
			r := map[string]any{"uri": "phoenix://scenarios/" + sc.File, "name": sc.File, "mimeType": "application/json"}
			if sc.Name != "" {
				r["title"] = sc.Name
			}
			if sc.Description != "" {
				r["description"] = sc.Description
			}
			list = append(list, r)
		}
	}
	if runs, err := s.Svc.ListRuns(); err == nil {
		for i, ri := range runs {
			if i >= maxListedRuns {
				break
			}
			list = append(list,
				map[string]any{"uri": "phoenix://runs/" + ri.RunID + "/summary", "name": ri.RunID + " summary", "title": "Summary of " + ri.Scenario, "mimeType": "application/json"},
				map[string]any{"uri": "phoenix://runs/" + ri.RunID + "/report", "name": ri.RunID + " report", "title": "Report of " + ri.Scenario, "mimeType": "text/html"})
		}
	}
	return map[string]any{"resources": list}, nil
}

func (s *Server) resourcesRead(params json.RawMessage) (any, error) {
	var p struct {
		URI string `json:"uri"`
	}
	if json.Unmarshal(params, &p) != nil || p.URI == "" {
		return nil, &rpcErr{-32602, "uri is required"}
	}
	u, err := url.Parse(p.URI)
	if err != nil || u.Scheme != "phoenix" || u.RawQuery != "" || u.Fragment != "" {
		return nil, &rpcErr{-32602, "unsupported resource uri " + p.URI}
	}
	// phoenix://<host>/<path...>; u.Host is the first segment.
	segs := append([]string{u.Host}, strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")...)
	if segs[len(segs)-1] == "" {
		segs = segs[:len(segs)-1]
	}
	for i := range segs {
		d, err := url.PathUnescape(segs[i])
		if err != nil || strings.ContainsAny(d, "/\\\x00") || d == ".." || d == "." {
			return nil, &rpcErr{-32602, "invalid resource uri"}
		}
		segs[i] = d
	}
	text, mime := "", ""
	switch {
	case len(segs) == 2 && segs[0] == "scenarios":
		if !scenarioFileRe.MatchString(segs[1]) {
			return nil, &rpcErr{-32602, "invalid scenario file name"}
		}
		sc, err := s.Svc.GetScenario(segs[1])
		if err != nil {
			return nil, mapResErr(err, p.URI)
		}
		b, _ := json.MarshalIndent(sc, "", "  ")
		text, mime = string(b), "application/json"
	case len(segs) == 3 && segs[0] == "runs" && (segs[2] == "summary" || segs[2] == "report"):
		if !app.ValidRunID(segs[1]) {
			return nil, &rpcErr{-32602, "invalid run id"}
		}
		if segs[2] == "summary" {
			sum, err := s.Svc.Summary(segs[1])
			if err != nil {
				return nil, mapResErr(err, p.URI)
			}
			b, _ := json.MarshalIndent(sum, "", "  ")
			text, mime = string(b), "application/json"
		} else {
			dir, err := s.Svc.RunDir(segs[1])
			if err != nil {
				return nil, &rpcErr{-32602, "invalid run id"}
			}
			b, err := os.ReadFile(filepath.Join(dir, "report.html"))
			if err != nil {
				return nil, mapResErr(err, p.URI)
			}
			text, mime = string(b), "text/html"
		}
	case len(segs) == 2 && segs[0] == "docs" && segs[1] == "scenario-format":
		b, err := docs.FS.ReadFile("SCENARIO_FORMAT.md")
		if err != nil {
			return nil, notFound(p.URI)
		}
		text, mime = string(b), "text/markdown"
	default:
		return nil, notFound(p.URI)
	}
	return map[string]any{"contents": []map[string]any{{"uri": p.URI, "mimeType": mime, "text": text}}}, nil
}

func mapResErr(err error, uri string) error {
	if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "not found") {
		return notFound(uri)
	}
	if strings.Contains(err.Error(), "invalid") {
		return &rpcErr{-32602, err.Error()}
	}
	return fmt.Errorf("reading %s: %w", uri, err)
}
