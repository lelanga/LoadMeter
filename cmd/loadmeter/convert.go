package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lelanga/loadmeter/internal/app"
	"github.com/lelanga/loadmeter/internal/k6"
	"github.com/lelanga/loadmeter/internal/openapi"
)

func init() {
	register("import-openapi", "Import an OpenAPI 3.x JSON spec: loadmeter import-openapi spec.json [--name N --tags a,b --exclude-tags x --operations id1,id2 --group-by tag|path --include-deprecated --include-destructive --json]", cmdImportOpenAPI)
	register("export-k6", "Export a scenario to a k6 script: loadmeter export-k6 scenario.json [-o out.js]", cmdExportK6)
}

func cmdImportOpenAPI(args []string) error {
	fs := flag.NewFlagSet("import-openapi", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	name := fs.String("name", "", "scenario name")
	groupBy := fs.String("group-by", "tag", "one transaction per tag or path")
	destructive := fs.Bool("include-destructive", false, "include DELETE operations")
	deprecated := fs.Bool("include-deprecated", false, "include deprecated operations")
	asJSON := fs.Bool("json", false, "print JSON")
	var tags, exclude, ops stringList
	fs.Var(&tags, "tags", "only these tags (comma separated)")
	fs.Var(&exclude, "exclude-tags", "skip these tags")
	fs.Var(&ops, "operations", "only these operationIds")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter import-openapi spec.json")
	}
	b, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	svc, err := newService(*ws, nil)
	if err != nil {
		return err
	}
	n := *name
	if n == "" {
		n = strings.TrimSuffix(filepath.Base(fs.Arg(0)), filepath.Ext(fs.Arg(0)))
	}
	res, err := svc.ImportOpenAPI(n, b, openapi.Options{Name: *name, Tags: tags, ExcludeTags: exclude, OperationIDs: ops,
		GroupBy: *groupBy, IncludeDeprecated: *deprecated, IncludeDestructive: *destructive})
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(res)
	}
	fmt.Printf("scenario: %s\n", filepath.Join(svc.WS.ScenariosDir(), res.File))
	printCompat(res.Report)
	return nil
}

func cmdExportK6(args []string) error {
	fs := flag.NewFlagSet("export-k6", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory")
	out := fs.String("o", "", "output file (default <scenario>.k6.js)")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: loadmeter export-k6 scenario.json [-o out.js]")
	}
	arg := fs.Arg(0)
	if *out == "" {
		*out = strings.TrimSuffix(filepath.Base(arg), ".json") + ".k6.js"
	}
	outDir, err := filepath.Abs(filepath.Dir(*out))
	if err != nil {
		return err
	}
	var b []byte
	svc, err := newService(*ws, nil)
	if err != nil {
		return err
	}
	if st, serr := os.Stat(arg); serr == nil && !st.IsDir() {
		// A scenario file given by path: data files resolve relative to it.
		sc, err := app.LoadScenario(arg)
		if err != nil {
			return err
		}
		abs, _ := filepath.Abs(filepath.Dir(arg))
		prefix := ""
		if rel, err := filepath.Rel(outDir, abs); err == nil && rel != "." {
			prefix = filepath.ToSlash(rel)
		}
		bb, r, err := k6.Export(sc, k6.Options{DataPrefix: prefix})
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, bb, 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", *out)
		printCompat(r)
		return nil
	}
	bb, r, err := svc.ExportK6(filepath.Base(arg), outDir)
	if err != nil {
		return err
	}
	b = bb
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", *out)
	printCompat(r)
	return nil
}
