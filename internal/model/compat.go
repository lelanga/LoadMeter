package model

// Support levels for compatibility reports.
const (
	Supported   = "supported"
	Partial     = "partial"
	Unsupported = "unsupported"
	Ignored     = "ignored" // intentionally not imported (e.g. UI-only listeners); never silent
)

// CompatItem describes how one source element was handled by an importer.
type CompatItem struct {
	Element  string `json:"element"`            // e.g. "HTTPSamplerProxy", "web_url"
	Name     string `json:"name,omitempty"`     // element name/label in source
	Location string `json:"location,omitempty"` // path or file:line in the source
	Status   string `json:"status"`             // supported | partial | unsupported | ignored
	Note     string `json:"note,omitempty"`     // what was lost / what to change manually
}

// CompatReport is produced by every importer. Importers must never drop
// elements silently: everything not fully converted appears here.
type CompatReport struct {
	Source     string       `json:"source"`
	Format     string       `json:"format"` // har | jmx | loadrunner
	Items      []CompatItem `json:"items"`
	Warnings   []string     `json:"warnings,omitempty"`
	Manual     []string     `json:"manualChanges,omitempty"`
	Confidence string       `json:"confidence"` // high | medium | low
	Validated  string       `json:"validation"` // not-run | passed | failed
}

func (r *CompatReport) Add(element, name, location, status, note string) {
	r.Items = append(r.Items, CompatItem{Element: element, Name: name, Location: location, Status: status, Note: note})
}

func (r *CompatReport) Counts() map[string]int {
	c := map[string]int{}
	for _, it := range r.Items {
		c[it.Status]++
	}
	return c
}

// ComputeConfidence sets Confidence from the share of unsupported/partial items.
func (r *CompatReport) ComputeConfidence() {
	c := r.Counts()
	total := len(r.Items) - c[Ignored]
	if total <= 0 {
		r.Confidence = "high"
		return
	}
	bad := float64(c[Unsupported]) + 0.5*float64(c[Partial])
	switch ratio := bad / float64(total); {
	case c[Unsupported] == 0 && ratio <= 0.1:
		r.Confidence = "high"
	case ratio <= 0.3:
		r.Confidence = "medium"
	default:
		r.Confidence = "low"
	}
	if r.Validated == "" {
		r.Validated = "not-run"
	}
}
