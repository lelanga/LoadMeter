package service

import (
	"bytes"
	"path/filepath"

	"github.com/lelanga/phoenix/internal/k6"
	"github.com/lelanga/phoenix/internal/model"
	"github.com/lelanga/phoenix/internal/openapi"
	"github.com/lelanga/phoenix/internal/safety"
)

// ImportOpenAPI converts an OpenAPI 3.x JSON document into a saved scenario.
func (s *Service) ImportOpenAPI(name string, data []byte, opts openapi.Options) (*BasicImport, error) {
	if opts.Name == "" {
		opts.Name = name
	}
	sc, rep, err := openapi.Import(bytes.NewReader(data), name+".json", opts)
	if err != nil {
		return nil, invalid("%v", err)
	}
	file := s.uniqueFile(sc.Name)
	if _, err := s.SaveScenario(file, sc, "import"); err != nil {
		return nil, err
	}
	safety.Audit("import.openapi", map[string]any{"file": file, "confidence": rep.Confidence})
	return &BasicImport{File: file, Report: rep}, nil
}

// ExportK6 renders a workspace scenario as a k6 script. outDir (optional) is the
// directory the script will be written to; it is used to keep data file paths valid.
func (s *Service) ExportK6(file, outDir string) ([]byte, *model.CompatReport, error) {
	sc, err := s.GetScenario(file)
	if err != nil {
		return nil, nil, err
	}
	return k6.Export(sc, k6.Options{DataPrefix: s.k6DataPrefix(outDir)})
}

func (s *Service) k6DataPrefix(outDir string) string {
	if outDir == "" {
		return ""
	}
	rel, err := filepath.Rel(outDir, s.WS.ScenariosDir())
	if err != nil || rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

// ExportK6Scenario renders an in-memory scenario (used by the CLI for files outside the workspace).
func ExportK6Scenario(sc *model.Scenario, o k6.Options) ([]byte, *model.CompatReport, error) {
	return k6.Export(sc, o)
}
