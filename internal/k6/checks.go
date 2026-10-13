package k6

import "github.com/lelanga/loadmeter/internal/model"

// parityNotes reports extractor/assertion features of the model that the k6
// export does not reproduce (JMeter-parity options; see model/checks.go).
func (e *exporter) parityNotes(rq *model.Request) {
	if rq.IgnoreStatus {
		e.note("ignoreStatus is not exported: k6 counts status >= 400 in http_req_failed")
	}
	for _, x := range rq.Extract {
		if x.Match == model.MatchAll {
			e.note("extractor " + x.Name + ": match \"all\" exported as the first match (no name_1..N variables)")
		}
		if x.Template != "" || (x.Source != "" && x.Source != model.SrcBody) {
			e.note("extractor " + x.Name + ": template/source not exported; k6 reads group 1 of the body")
		}
	}
	for _, a := range rq.Assert {
		switch {
		case a.Type == model.AsEquals || a.Type == model.AsSize || a.Type == model.AsHash:
			e.note(a.Type + " assertion not exported to k6 (dropped)")
		case a.Not || a.Regex || a.ExpectNull || (a.Source != "" && a.Source != model.SrcBody):
			e.note(a.Type + " assertion: not/regex/expectNull/source are not exported to k6; the check is written without them")
		}
	}
}
