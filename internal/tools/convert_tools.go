package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/lelanga/phoenix/internal/openapi"
	"github.com/lelanga/phoenix/internal/service"
)

func init() {
	Register(
		Tool{Name: "import_openapi", Description: "Import an OpenAPI 3.0/3.1 JSON document into a scenario: one transaction per tag (or path), a request per operation with example-based bodies, status assertions, and ${secret_*} placeholders for security schemes (no credentials imported). Returns a compatibility report. Deprecated and DELETE operations are skipped unless included.",
			InputSchema: obj(map[string]any{
				"path":               str("Path to the OpenAPI .json file (inside the workspace imports folder or current directory)"),
				"name":               str("Scenario name"),
				"tags":               strs("Only include operations with one of these tags"),
				"excludeTags":        strs("Skip operations with any of these tags"),
				"operationIds":       strs("Only include these operationIds"),
				"groupBy":            map[string]any{"type": "string", "enum": []string{"tag", "path"}, "description": "One transaction per tag (default) or per path"},
				"includeDeprecated":  boolean("Include deprecated operations (default false)"),
				"includeDestructive": boolean("Include DELETE operations (default false)"),
			}, "path"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				p, err := importPath(s, getStr(a, "path"))
				if err != nil {
					return nil, err
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return nil, err
				}
				name := getStr(a, "name")
				if name == "" {
					name = strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
				}
				return s.ImportOpenAPI(name, b, openapi.Options{
					Name: getStr(a, "name"), Tags: getStrs(a, "tags"), ExcludeTags: getStrs(a, "excludeTags"),
					OperationIDs: getStrs(a, "operationIds"), GroupBy: getStr(a, "groupBy"),
					IncludeDeprecated: getBool(a, "includeDeprecated"), IncludeDestructive: getBool(a, "includeDestructive"),
				})
			}},
		Tool{Name: "export_k6", Description: "Export a scenario to a k6 JavaScript file in the workspace exports folder, with a report of anything that could not be represented. The script has no remote imports. k6 does not enforce Phoenix's host allowlist.",
			InputSchema: obj(map[string]any{"file": fileProp}, "file"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				dir := filepath.Join(s.WS.Dir, "exports")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return nil, err
				}
				b, rep, err := s.ExportK6(getStr(a, "file"), dir)
				if err != nil {
					return nil, err
				}
				out := filepath.Join(dir, strings.TrimSuffix(filepath.Base(getStr(a, "file")), ".json")+".k6.js")
				if err := os.WriteFile(out, b, 0o644); err != nil {
					return nil, err
				}
				return map[string]any{"path": out, "report": rep}, nil
			}},
	)
}
