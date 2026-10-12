package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/lelanga/loadmeter/internal/accesslog"
	"github.com/lelanga/loadmeter/internal/service"
)

func init() {
	Register(Tool{Name: "import_access_log",
		Description: "Build a replay scenario from a web-server access log (nginx/Apache combined or common, AWS ALB, CloudFront; plain or .gz). Produces a weighted endpoint mix (path ids normalised to {id}) driven by a ramping arrival-rate workload that follows the logged per-minute request rate, scaled (default 10%). GET/HEAD only unless includeWrites; query strings dropped unless keepQuery (secrets redacted). Reports endpoints found, mix, rate curve, skipped lines and everything ignored, and a fitted think-time distribution when sessions allow. baseUrl is required; non-loopback targets must already be in the safety allowlist. Replaying at production needs authorization. The scenario is saved, never run.",
		InputSchema: obj(map[string]any{
			"path":          str("Path to the access log (inside the workspace imports folder or current directory)"),
			"baseUrl":       str("Replay target, e.g. http://127.0.0.1:8099 (required; logs rarely contain the host)"),
			"name":          str("Scenario name"),
			"format":        map[string]any{"type": "string", "enum": []string{"auto", "nginx", "apache", "alb", "cloudfront"}, "description": "Log format (default auto)"},
			"scale":         num("Fraction of the logged rate to replay (default 0.1, max 10)"),
			"window":        str("peak (busiest 15 minutes, default), avg, or HH:MM-HH:MM in the log's clock"),
			"top":           num("Keep the N most frequent endpoints in the mix (default 20)"),
			"includeWrites": boolean("Also replay POST/PUT/PATCH/DELETE (sent with an empty body; default false)"),
			"keepQuery":     boolean("Keep the most common query string per endpoint, secrets redacted (default false)"),
			"includeStatic": boolean("Replay static assets (default false)"),
			"includeErrors": boolean("Replay requests that returned 4xx/5xx (default false)"),
			"journey":       boolean("One iteration = one reconstructed session with fitted think time (default false)"),
			"sessionCookie": str("Cookie name identifying sessions (CloudFront logs with cs(Cookie))"),
		}, "path", "baseUrl"),
		Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
			p, err := importPath(s, getStr(a, "path"))
			if err != nil {
				return nil, err
			}
			f, err := os.Open(p)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			name := getStr(a, "name")
			if name == "" {
				name = strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
			}
			return s.ImportAccessLog(name, f, filepath.Base(p), accesslog.Options{
				Format: getStr(a, "format"), BaseURL: getStr(a, "baseUrl"), Name: getStr(a, "name"),
				Scale: getNum(a, "scale"), Window: getStr(a, "window"), Top: int(getNum(a, "top")),
				IncludeWrites: getBool(a, "includeWrites"), KeepQuery: getBool(a, "keepQuery"),
				IncludeStatic: getBool(a, "includeStatic"), IncludeErrors: getBool(a, "includeErrors"),
				Journey: getBool(a, "journey"), SessionCookie: getStr(a, "sessionCookie"),
			})
		}})
}
