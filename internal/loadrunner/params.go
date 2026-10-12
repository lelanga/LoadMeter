package loadrunner

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/lelanga/loadmeter/internal/model"
)

// Parameter (.prm) handling. Key names follow commonly observed VuGen-generated
// files (see README): [parameter:Name] sections with Type, Table, ColumnName,
// Delimiter, SelectNextRow, GenerateNewVal/UpdateValueOn, OutOfRangePolicy, ...

type paramDef struct {
	name     string
	typ      string // raw Type value
	sec      *iniSection
	loc      string
	table    string
	column   string
	delim    string
	selectBy string // normalized SelectNextRow
	updateOn string // normalized GenerateNewVal / UpdateValueOn
	outRange string // normalized OutOfRangePolicy
}

const maxDataRows = 1_000_000

var sensitiveName = regexp.MustCompile(`(?i)pass|pwd|secret|token|apikey|api_key|credential`)

// loadParams reads a .prm file and maps its parameters onto the scenario.
func (im *importer) loadParams(prmName string, data []byte) {
	ini := parseINI(data)
	var tables []string
	byTable := map[string][]*paramDef{}
	for _, sec := range ini.sections {
		pfx, name, ok := strings.Cut(sec.name, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(pfx), "parameter") {
			if sec.name != "" {
				im.rep.Add("prm-section", sec.name, fmt.Sprintf("%s:%d", prmName, sec.line), model.Ignored, "section is not a [parameter:Name] definition")
			}
			continue
		}
		if v := sec.val("ParamName"); v != "" {
			name = v
		}
		name = strings.TrimSpace(name)
		p := &paramDef{
			name: name, typ: sec.val("Type"), sec: sec, loc: fmt.Sprintf("%s:%d", prmName, sec.line),
			table: sec.val("Table"), column: sec.val("ColumnName"), delim: sec.val("Delimiter"),
		}
		if v, ok := sec.first("SelectNextRow"); ok {
			p.selectBy = norm(v)
		}
		if v, ok := sec.first("UpdateValueOn", "GenerateNewVal"); ok {
			p.updateOn = norm(v)
		}
		if v, ok := sec.first("OutOfRangePolicy"); ok {
			p.outRange = norm(v)
		}
		im.params[name] = p
		im.known[name] = true
		switch t := norm(p.typ); t {
		case "table", "file":
			key := strings.ToLower(baseName(p.table))
			if key == "" {
				im.rep.Add("parameter", name, p.loc, model.Unsupported, "Table parameter without a Table= data file")
				im.rep.Manual = append(im.rep.Manual, fmt.Sprintf("Provide data for parameter %s (%s)", name, p.loc))
				im.sc.Variables[name] = ""
				continue
			}
			if _, seen := byTable[key]; !seen {
				tables = append(tables, key)
			}
			byTable[key] = append(byTable[key], p)
		default:
			im.mapFunctionParam(p, t)
		}
	}
	for _, key := range tables {
		im.loadTable(byTable[key])
	}
}

// mapFunctionParam maps generated (non-file) parameter types onto Load Meter
// built-in functions.
func (im *importer) mapFunctionParam(p *paramDef, t string) {
	each := p.updateOn == "" || p.updateOn == "eachoccurrence"
	evalNote := ""
	if !each {
		evalNote = fmt.Sprintf("; LoadRunner updates it %q but Load Meter evaluates the function at every use", p.sec.val("GenerateNewVal")+p.sec.val("UpdateValueOn"))
	}
	status := func(base string) string {
		if !each {
			return model.Partial
		}
		return base
	}
	switch t {
	case "datetime", "date", "time", "datetimeparameter":
		f := p.sec.val("Format")
		expr, exact := "${__timestamp()}", f == ""
		if f != "" {
			if jf, ok := strftimeToJava(f); ok {
				expr, exact = "${__timestamp("+jf+")}", true
			}
		}
		st := status(model.Supported)
		note := "mapped to " + expr + evalNote
		if !exact {
			st = model.Partial
			note = fmt.Sprintf("format %q not convertible; mapped to %s (epoch milliseconds)%s", f, expr, evalNote)
		}
		if p.sec.val("Offset") != "" || strings.EqualFold(p.sec.val("WorkingDays"), "1") {
			st, note = model.Partial, note+"; date offset/working-day options are not supported"
		}
		im.funcs[p.name] = expr
		im.rep.Add("parameter:"+p.typ, p.name, p.loc, st, note)
	case "randomnumber", "random":
		lo, _ := p.sec.first("MinValue", "Min")
		hi, _ := p.sec.first("MaxValue", "Max")
		a, e1 := strconv.Atoi(strings.TrimSpace(lo))
		b, e2 := strconv.Atoi(strings.TrimSpace(hi))
		if e1 != nil || e2 != nil || b < a {
			im.funcs[p.name] = "${__randomInt(0,100)}"
			im.rep.Add("parameter:"+p.typ, p.name, p.loc, model.Partial, fmt.Sprintf("range %q..%q not understood; mapped to ${__randomInt(0,100)}", lo, hi))
			return
		}
		expr := fmt.Sprintf("${__randomInt(%d,%d)}", a, b)
		im.funcs[p.name] = expr
		note := "mapped to " + expr + evalNote
		st := status(model.Supported)
		if f := p.sec.val("Format"); f != "" && f != "%d" && f != "%lu" && f != "%01lu" && f != "%01d" {
			st, note = model.Partial, note+fmt.Sprintf("; number format %q not applied", f)
		}
		im.rep.Add("parameter:"+p.typ, p.name, p.loc, st, note)
	case "uniquenumber", "unique":
		im.funcs[p.name] = "${__counter()}"
		im.rep.Add("parameter:"+p.typ, p.name, p.loc, model.Partial,
			"mapped to ${__counter()} (global counter); Start/Block-size per Vuser and number format are not reproduced"+evalNote)
	case "iterationnumber", "iteration":
		im.funcs[p.name] = "${__iteration()}"
		im.rep.Add("parameter:"+p.typ, p.name, p.loc, model.Supported, "mapped to ${__iteration()}")
	case "vuserid", "vuser":
		im.funcs[p.name] = "${__vu()}"
		im.rep.Add("parameter:"+p.typ, p.name, p.loc, model.Supported, "mapped to ${__vu()} (Load Meter VU index)")
	case "groupname", "group", "loadgeneratorname", "host", "hostname", "loadgenerator":
		v := p.sec.val("OriginalValue")
		im.sc.Variables[p.name] = v
		im.rep.Add("parameter:"+p.typ, p.name, p.loc, model.Partial,
			fmt.Sprintf("Controller/host attribute has no Load Meter equivalent; set as constant variable %q", v))
	default:
		im.sc.Variables[p.name] = p.sec.val("OriginalValue")
		im.rep.Add("parameter:"+p.typ, p.name, p.loc, model.Unsupported,
			fmt.Sprintf("parameter type %q is not supported (custom/user-defined, XML or database parameters need the LoadRunner runtime); defined as an empty variable", p.typ))
		im.rep.Manual = append(im.rep.Manual, fmt.Sprintf("Provide a value for parameter %s (type %s, %s)", p.name, p.typ, p.loc))
	}
}

// loadTable builds one DataSource from all parameters sharing a .dat file.
func (im *importer) loadTable(ps []*paramDef) {
	first := ps[0]
	data, fname, ok := im.fs.get(first.table)
	if !ok {
		for _, p := range ps {
			im.sc.Variables[p.name] = ""
			im.rep.Add("parameter:Table", p.name, p.loc, model.Unsupported, fmt.Sprintf("data file %q was not supplied", first.table))
		}
		im.rep.Manual = append(im.rep.Manual, fmt.Sprintf("Supply data file %s for parameters %s", first.table, paramNames(ps)))
		return
	}
	im.external(fname)
	delim := first.delim
	switch norm(delim) {
	case "", "comma":
		delim = ","
	case "tab", "\\t":
		delim = "\t"
	case "space":
		delim = " "
	case "semicolon":
		delim = ";"
	}
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	r.Comma = []rune(delim)[0]
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	var recs [][]string
	for len(recs) <= maxDataRows {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			im.rep.Warnings = append(im.rep.Warnings, fmt.Sprintf("%s: %v (rows after the error were not read)", fname, err))
			break
		}
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue
		}
		recs = append(recs, rec)
	}
	if len(recs) > maxDataRows {
		recs = recs[:maxDataRows]
		im.rep.Warnings = append(im.rep.Warnings, fmt.Sprintf("%s: truncated to %d rows", fname, maxDataRows))
	}
	if len(recs) == 0 {
		for _, p := range ps {
			im.sc.Variables[p.name] = ""
			im.rep.Add("parameter:Table", p.name, p.loc, model.Unsupported, fmt.Sprintf("data file %s is empty", fname))
		}
		return
	}
	header := recs[0]
	rows := recs[1:]
	// Map each parameter to a source column index.
	type col struct {
		name string
		src  int
	}
	var cols []col
	used := map[string]bool{}
	var drivers []*paramDef
	for _, p := range ps {
		idx := -1
		c := strings.TrimSpace(p.column)
		if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(strings.ToLower(c), "col"))); err == nil && strings.HasPrefix(strings.ToLower(c), "col") {
			idx = n - 1
		} else {
			for i, h := range header {
				if strings.EqualFold(strings.TrimSpace(h), c) {
					idx = i
					break
				}
			}
			if c == "" {
				idx = 0
			}
		}
		if idx < 0 || idx >= len(header) {
			im.sc.Variables[p.name] = ""
			im.rep.Add("parameter:Table", p.name, p.loc, model.Unsupported, fmt.Sprintf("column %q not found in %s", p.column, fname))
			continue
		}
		cols = append(cols, col{p.name, idx})
		used[p.name] = true
		if !strings.HasPrefix(p.selectBy, "samelineas") {
			drivers = append(drivers, p)
		}
	}
	if len(cols) == 0 {
		return
	}
	// Keep the remaining columns under their header names (harmless extras).
	for i, h := range header {
		h = strings.TrimSpace(h)
		mapped := false
		for _, c := range cols {
			if c.src == i {
				mapped = true
			}
		}
		if !mapped && h != "" && !used[h] && !im.known[h] {
			cols = append(cols, col{h, i})
			used[h] = true
		}
	}
	ds := model.DataSource{Name: dsName(fname), Delimiter: delim}
	for _, c := range cols {
		ds.Columns = append(ds.Columns, c.name)
		if sensitiveName.MatchString(c.name) {
			ds.Sensitive = true
		}
	}
	for _, rec := range rows {
		out := make([]string, len(cols))
		for i, c := range cols {
			if c.src < len(rec) {
				out[i] = rec[c.src]
			}
		}
		ds.Rows = append(ds.Rows, out)
	}
	if len(ds.Rows) == 0 {
		im.rep.Warnings = append(im.rep.Warnings, fmt.Sprintf("%s has a header but no data rows", fname))
		ds.Rows = [][]string{make([]string, len(cols))}
	}
	if len(drivers) == 0 {
		drivers = ps[:1]
	}
	drv := drivers[0]
	modeNote, modeStatus := "", model.Supported
	switch {
	case drv.selectBy == "" || drv.selectBy == "sequential":
		ds.Mode = "sequential"
		modeStatus = model.Partial
		modeNote = "SelectNextRow=Sequential: LoadRunner gives every Vuser its own cursor from row 1; Load Meter 'sequential' shares one cursor across VUs (use mode per-vu or an identical data file per VU if the same rows per VU are required)"
	case drv.selectBy == "random":
		ds.Mode = "random"
	case drv.selectBy == "unique":
		ds.Mode = "unique"
	default:
		ds.Mode = "sequential"
		modeStatus = model.Partial
		modeNote = fmt.Sprintf("SelectNextRow=%q not understood; using sequential", drv.sec.val("SelectNextRow"))
	}
	switch drv.updateOn {
	case "", "eachiteration":
		ds.Scope = "iteration"
	case "once":
		ds.Scope = "vu"
		if ds.Mode == "unique" {
			ds.Mode = "per-vu" // one distinct row per VU for the whole run
		}
	case "eachoccurrence":
		ds.Scope = "iteration"
		modeStatus = model.Partial
		modeNote = strings.TrimPrefix(modeNote+"; ", "; ") + "UpdateValueOn=Each occurrence: Load Meter advances rows once per iteration, not on every reference"
	default:
		ds.Scope = "iteration"
		modeStatus = model.Partial
		modeNote = strings.TrimPrefix(modeNote+"; ", "; ") + fmt.Sprintf("update policy %q not understood; using each iteration", drv.updateOn)
	}
	switch {
	case drv.outRange == "":
	case strings.Contains(drv.outRange, "abort"):
		ds.OnExhaust = "stop"
	case strings.Contains(drv.outRange, "cyclic"):
		ds.OnExhaust = "recycle"
	case strings.Contains(drv.outRange, "last"):
		ds.OnExhaust = "recycle"
		modeStatus = model.Partial
		modeNote = strings.TrimPrefix(modeNote+"; ", "; ") + "OutOfRangePolicy=Continue with last value is not available; rows recycle instead"
	}
	if len(drivers) > 1 {
		for _, d := range drivers[1:] {
			if d.selectBy != drv.selectBy || d.updateOn != drv.updateOn {
				modeStatus = model.Partial
				modeNote = strings.TrimPrefix(modeNote+"; ", "; ") + fmt.Sprintf("%s and %s advance independently in LoadRunner; Load Meter reads one row for both", drv.name, d.name)
			}
		}
	}
	im.sc.DataSources = append(im.sc.DataSources, ds)
	for _, p := range ps {
		if !used[p.name] {
			continue
		}
		st, note := model.Supported, fmt.Sprintf("column %q of %s -> data source %q", p.column, fname, ds.Name)
		if p == drv && modeStatus != model.Supported {
			st, note = modeStatus, note+"; "+modeNote
		}
		im.rep.Add("parameter:Table", p.name, p.loc, st, note)
	}
	if ds.Sensitive {
		im.rep.Add("dataSource", ds.Name, fname, model.Supported, "marked sensitive (column name suggests credentials); values are masked in logs")
	}
}

func paramNames(ps []*paramDef) string {
	var n []string
	for _, p := range ps {
		n = append(n, p.name)
	}
	return strings.Join(n, ", ")
}

func dsName(file string) string {
	b := strings.TrimSuffix(file, path.Ext(file))
	var out strings.Builder
	for _, r := range b {
		if r == '_' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	if out.Len() == 0 {
		return "data"
	}
	return out.String()
}

// strftimeToJava converts the strftime subset LoadRunner date parameters use
// into the SimpleDateFormat subset understood by ${__timestamp(fmt)}.
func strftimeToJava(f string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		c := f[i]
		if c != '%' {
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == ',' || c == '(' || c == ')' || c == '{' || c == '}' || c == '$' {
				return "", false // letters would be read as pattern letters; ',' splits function args
			}
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(f) {
			return "", false
		}
		i++
		switch f[i] {
		case 'Y':
			b.WriteString("yyyy")
		case 'm':
			b.WriteString("MM")
		case 'd':
			b.WriteString("dd")
		case 'H':
			b.WriteString("HH")
		case 'M':
			b.WriteString("mm")
		case 'S':
			b.WriteString("ss")
		case '%':
			b.WriteByte('%')
		default:
			return "", false
		}
	}
	return b.String(), true
}
