package service

import (
	"io"

	"github.com/lelanga/loadmeter/internal/accesslog"
	"github.com/lelanga/loadmeter/internal/model"
	"github.com/lelanga/loadmeter/internal/safety"
)

// AccessLogImport is the result of importing an access log.
type AccessLogImport struct {
	File    string              `json:"file"`
	Report  *model.CompatReport `json:"report"`
	Profile *accesslog.Profile  `json:"profile"`
}

// ImportAccessLog parses an access log (plain or gzip) and saves a replay
// scenario. The replay target must be loopback or already allowlisted (policy
// file or --allow-host); this method never widens the allowlist.
func (s *Service) ImportAccessLog(name string, r io.Reader, source string, o accesslog.Options) (*AccessLogImport, error) {
	if o.Name == "" {
		o.Name = name
	}
	o.AllowHosts = append(append([]string(nil), o.AllowHosts...), s.Policy().AllowedHosts...)
	sc, rep, prof, err := accesslog.Import(r, source, o)
	if err != nil {
		return nil, invalid("%v", err)
	}
	file := s.uniqueFile(sc.Name)
	if _, err := s.SaveScenario(file, sc, "import"); err != nil {
		return nil, err
	}
	safety.Audit("import.accesslog", map[string]any{"file": file, "format": prof.Format, "lines": prof.Lines,
		"replayable": prof.Replayable, "scale": o.Scale, "window": prof.Window.Spec, "target": sc.Defaults.BaseURL})
	return &AccessLogImport{File: file, Report: rep, Profile: prof}, nil
}
