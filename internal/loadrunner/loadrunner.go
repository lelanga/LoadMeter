// Package loadrunner imports LoadRunner VuGen "Web - HTTP/HTML" scripts into
// Load Meter's canonical scenario model by static analysis of the script text.
//
// Nothing in a script is compiled or executed and no OpenText/Micro Focus
// runtime is needed or bundled: the importer tokenizes the C source, maps the
// documented web_* / lr_* calls it understands onto the IR and reports every
// other construct (custom C, control flow, other protocols) with file:line in
// the compatibility report. See README.md for the supported subset.
package loadrunner

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/lelanga/loadmeter/internal/model"
)

// ErrUnsupportedProtocol is returned (wrapped) when the .usr declares no
// web protocol. The CompatReport is still returned and explains why.
var ErrUnsupportedProtocol = errors.New("loadrunner: script protocol is not Web (HTTP/HTML)")

// ErrNoScript is returned when the input contains no .usr and no .c files.
var ErrNoScript = errors.New("loadrunner: no VuGen script files (.usr / .c) found")

const (
	maxFileBytes = 64 << 20
	maxFiles     = 5000
)

// fileSet indexes script files by lower-cased base name.
type fileSet struct {
	names []string          // original base names, sorted
	byKey map[string]string // lower base -> original base
	data  map[string][]byte // original base -> content
	depth map[string]int
}

func baseName(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	return path.Base(strings.TrimSpace(p))
}

func newFileSet(files map[string][]byte) *fileSet {
	fs := &fileSet{byKey: map[string]string{}, data: map[string][]byte{}, depth: map[string]int{}}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b := baseName(k)
		if b == "." || b == "/" || b == "" {
			continue
		}
		d := strings.Count(strings.ReplaceAll(k, `\`, "/"), "/")
		lk := strings.ToLower(b)
		if prev, ok := fs.byKey[lk]; ok && fs.depth[prev] <= d {
			continue // keep the shallowest copy (script root over data/ snapshots)
		}
		if prev, ok := fs.byKey[lk]; ok {
			delete(fs.data, prev)
		}
		fs.byKey[lk] = b
		fs.data[b] = files[k]
		fs.depth[b] = d
	}
	for _, b := range fs.byKey {
		fs.names = append(fs.names, b)
	}
	sort.Strings(fs.names)
	return fs
}

func (fs *fileSet) get(name string) ([]byte, string, bool) {
	b, ok := fs.byKey[strings.ToLower(baseName(name))]
	if !ok {
		return nil, "", false
	}
	return fs.data[b], b, true
}

func (fs *fileSet) withExt(ext string) []string {
	var out []string
	for _, n := range fs.names {
		if strings.EqualFold(path.Ext(n), ext) {
			out = append(out, n)
		}
	}
	return out
}

// readDir loads the regular files at the top level of dir. Sub-directories
// (e.g. data/ recording snapshots) and symlinks are skipped.
func readDir(dir string) (map[string][]byte, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		if len(files) >= maxFiles {
			return nil, fmt.Errorf("loadrunner: more than %d files in %s", maxFiles, dir)
		}
		info, err := e.Info()
		if err != nil || info.Size() > maxFileBytes {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		files[e.Name()] = b
	}
	return files, nil
}

// ImportDir imports the VuGen script in dir (the folder holding the .usr).
func ImportDir(dir string) (*model.Scenario, *model.CompatReport, error) {
	files, err := readDir(dir)
	if err != nil {
		return nil, nil, err
	}
	sc, rep, _, err := run(files, filepath.Base(filepath.Clean(dir)))
	return sc, rep, err
}

// ImportFiles imports a script from in-memory files (e.g. an upload). Keys
// are file names; directory components are ignored. name is used when the
// script has no .usr file.
func ImportFiles(files map[string][]byte, name string) (*model.Scenario, *model.CompatReport, error) {
	sc, rep, _, err := run(files, name)
	return sc, rep, err
}

// Analyze performs the standalone static portability assessment. It returns
// an Analysis even for non-web scripts (with Assessment "not-portable").
func Analyze(files map[string][]byte, name string) (*Analysis, error) {
	_, _, an, err := run(files, name)
	if an == nil {
		return nil, err
	}
	if errors.Is(err, ErrUnsupportedProtocol) {
		err = nil
	}
	return an, err
}

// AnalyzeDir is Analyze for a script directory.
func AnalyzeDir(dir string) (*Analysis, error) {
	files, err := readDir(dir)
	if err != nil {
		return nil, err
	}
	return Analyze(files, filepath.Base(filepath.Clean(dir)))
}

// --- protocols ---------------------------------------------------------------

var webProtocols = map[string]bool{
	"qtweb": true, "web": true, "webhttphtml": true, "http": true, "html": true, "httphtml": true,
	"webhttp": true, "webhtml": true, "webhtmlhttp": true,
}

// web-based protocols whose web_* calls import, but which carry extra APIs.
var webFamily = map[string]bool{
	"webservices": true, "soap": true, "sapweb": true, "siebelweb": true, "flex": true, "websocket": true,
	"oraclewebapplications11i": true, "peoplesoftenterprise": true,
}

func usrProtocols(usr *iniFile) []string {
	g := usr.section("General")
	var out []string
	seen := map[string]bool{}
	add := func(v string) {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if p == "" || strings.EqualFold(p, "multi") || seen[strings.ToLower(p)] {
				continue
			}
			seen[strings.ToLower(p)] = true
			out = append(out, p)
		}
	}
	for _, k := range []string{"Type", "AdditionalTypes", "ActiveTypes", "Protocol", "ProtocolName"} {
		add(g.val(k))
	}
	if s := usr.section("Protocols"); s != nil {
		for _, kv := range s.keys {
			add(kv.key)
		}
	}
	return out
}

// --- pipeline ----------------------------------------------------------------

type scriptFile struct {
	file  string // base file name
	entry string // entry function name
	role  string // init | action | end
}

func run(files map[string][]byte, name string) (*model.Scenario, *model.CompatReport, *Analysis, error) {
	fs := newFileSet(files)
	if len(fs.withExt(".usr")) == 0 && len(fs.withExt(".c")) == 0 {
		return nil, nil, nil, ErrNoScript
	}
	rep := &model.CompatReport{Source: name, Format: "loadrunner", Validated: "not-run"}
	an := &Analysis{Name: name, RuntimeFunctions: map[string]int{}, ProtocolFunctions: map[string]int{}}
	im := &importer{
		name: name, fs: fs, rep: rep, an: an,
		lb: "{", rb: "}",
		params: map[string]*paramDef{}, known: map[string]bool{}, arrays: map[string]bool{},
		funcs: map[string]string{}, userFuncs: map[string]string{}, once: map[string]bool{}, warnedRef: map[string]bool{},
	}

	// .usr: protocol, action order, parameter braces.
	var usr *iniFile
	usrs := fs.withExt(".usr")
	if len(usrs) > 0 {
		an.UsrFile = usrs[0]
		if len(usrs) > 1 {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("several .usr files found; using %s", usrs[0]))
		}
		usr = parseINI(fs.data[usrs[0]])
		an.file(usrs[0]).Role = "usr"
		rep.Source = usrs[0]
		if name == "" {
			name = strings.TrimSuffix(usrs[0], path.Ext(usrs[0]))
			im.name, an.Name = name, name
		}
		g := usr.section("General")
		if v := g.val("ParamLeftBrace"); v != "" {
			im.lb = v
		}
		if v := g.val("ParamRightBrace"); v != "" {
			im.rb = v
		}
	} else {
		rep.Warnings = append(rep.Warnings, "no .usr file: assuming Web (HTTP/HTML) and conventional file names")
	}
	if name == "" {
		name = "loadrunner-script"
		im.name, an.Name = name, name
	}
	im.sc = model.NewScenario(name)
	im.sc.Metadata = map[string]string{"importer": "loadrunner", "source": rep.Source}

	protocolBlocked := false
	if usr != nil {
		protos := usrProtocols(usr)
		an.Protocols = protos
		var web, other []string
		for _, p := range protos {
			n := norm(p)
			switch {
			case webProtocols[n]:
				web = append(web, p)
			case webFamily[n]:
				web = append(web, p)
				rep.Add("protocol", p, usrs[0], model.Partial, "web-based protocol: web_* calls are imported, protocol-specific APIs are not")
			default:
				other = append(other, p)
			}
		}
		an.WebProtocol = len(web) > 0
		if len(protos) == 0 {
			an.WebProtocol = true
			rep.Warnings = append(rep.Warnings, usrs[0]+": no protocol declared; assuming Web (HTTP/HTML)")
		}
		for _, p := range other {
			st, note := model.Unsupported, "non-web protocol; requires the licensed LoadRunner runtime"
			if an.WebProtocol {
				st, note = model.Partial, "additional non-web protocol in a multi-protocol script; only web_*/lr_* calls are imported"
			}
			rep.Add("protocol", p, usrs[0], st, note)
		}
		if !an.WebProtocol {
			protocolBlocked = true
		} else if len(web) > 0 {
			rep.Add("protocol", strings.Join(web, ","), usrs[0], model.Supported, "Web (HTTP/HTML)")
		}
		im.sc.Metadata["protocol"] = strings.Join(protos, ",")
	} else {
		an.Protocols = []string{"Web (HTTP/HTML) (assumed)"}
		an.WebProtocol = true
	}

	// Script files and their roles.
	scripts := im.scriptFiles(usr)
	parsed := map[string]*cFile{}
	for _, s := range scripts {
		data, fname, ok := fs.get(s.file)
		if !ok {
			rep.Add("action file", s.file, an.UsrFile, model.Unsupported, "listed in the .usr but not supplied")
			continue
		}
		f := parseC(fname, data)
		parsed[s.file] = f
		im.scanFile(f, s.role)
	}
	// Header files (globals.h, vdf.h, others) and unreferenced .c files.
	var headers []*cFile
	for _, n := range fs.names {
		ext := strings.ToLower(path.Ext(n))
		if ext != ".h" && ext != ".c" {
			continue
		}
		if ext == ".c" && parsed[n] != nil {
			continue
		}
		referenced := false
		for _, s := range scripts {
			if strings.EqualFold(s.file, n) {
				referenced = true
			}
		}
		if referenced {
			continue
		}
		f := parseC(n, fs.data[n])
		role := "globals"
		if ext == ".c" {
			role = "other"
		}
		im.scanFile(f, role)
		headers = append(headers, f)
	}
	// Custom function names across all files (for call classification).
	for _, s := range scripts {
		if f := parsed[s.file]; f != nil {
			for _, fn := range f.funcs {
				if fn.name != s.entry {
					im.userFuncs[fn.name] = f.name
				}
			}
		}
	}
	for _, f := range headers {
		for _, fn := range f.funcs {
			im.userFuncs[fn.name] = f.name
		}
	}

	if protocolBlocked {
		for _, sf := range scripts {
			if f := parsed[sf.file]; f != nil {
				im.reportProtocolCalls(f)
			}
		}
		im.finishAnalysis(true)
		rep.Manual = append(rep.Manual, "Re-record the business flow at the HTTP level (e.g. as a HAR) or use the protocol's own tooling; Load Meter replays HTTP only")
		return nil, rep, an, fmt.Errorf("%w: %s", ErrUnsupportedProtocol, strings.Join(an.Protocols, ", "))
	}

	// Parameters.
	prms := fs.withExt(".prm")
	if usr != nil {
		if p := usr.section("General").val("ParameterFile"); p != "" {
			if _, fname, ok := fs.get(p); ok {
				prms = append([]string{fname}, prms...)
			}
		}
	}
	if len(prms) > 0 {
		an.file(prms[0]).Role = "parameters"
		im.loadParams(prms[0], fs.data[prms[0]])
		for _, extra := range prms[1:] {
			if extra != prms[0] {
				rep.Add("parameter file", extra, extra, model.Ignored, "only one .prm per script is read ("+prms[0]+")")
			}
		}
	}
	// Names registered by the script itself.
	for _, f := range parsed {
		for _, fn := range f.funcs {
			walkCalls(fn.body, im.collectNames)
		}
	}

	// Run-time settings.
	for _, cfg := range fs.withExt(".cfg") {
		an.file(cfg).Role = "runtime-settings"
		im.loadCfg(cfg, fs.data[cfg])
	}
	for _, usp := range fs.withExt(".usp") {
		an.file(usp).Role = "runtime-settings"
		im.loadUsp(usp, fs.data[usp])
	}
	for _, dat := range fs.withExt(".dat") {
		an.file(dat).Role = "data"
	}

	// Globals / headers.
	for _, f := range headers {
		im.reportHeader(f)
	}

	// Convert: init -> Setup, actions -> Steps (one group per action), end -> Teardown.
	for _, s := range scripts {
		f := parsed[s.file]
		if f == nil {
			continue
		}
		im.role = s.role
		steps := im.convertFile(f, s.entry)
		switch s.role {
		case "init":
			im.sc.Setup = append(im.sc.Setup, steps...)
		case "end":
			im.sc.Teardown = append(im.sc.Teardown, steps...)
		default:
			if len(steps) == 0 {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("action %s (%s) produced no steps", s.entry, s.file))
				continue
			}
			im.sc.Steps = append(im.sc.Steps, model.Step{Type: model.StepGroup, Name: s.entry, Steps: steps, Comment: "action " + s.file})
		}
	}
	for _, l := range im.pendLoc {
		rep.Warnings = append(rep.Warnings, l+": registration (web_reg_*) is not followed by any request; dropped")
	}
	if len(im.next) > 0 {
		rep.Warnings = append(rep.Warnings, "web_add_header at the end of the script is not followed by any request; dropped")
	}

	im.applyRuntime()
	if len(im.sc.Variables) == 0 {
		im.sc.Variables = nil
	}
	for _, p := range im.sc.Validate() {
		rep.Warnings = append(rep.Warnings, "validation: "+p)
	}
	im.finishAnalysis(false)
	im.sc.Metadata["portability"] = an.Assessment
	im.sc.Metadata["requests"] = strconv.Itoa(an.Requests)
	if an.UsrFile != "" {
		im.sc.Metadata["usr"] = an.UsrFile
	}
	im.sc.Description = fmt.Sprintf("Imported from LoadRunner VuGen script %s (%s)", rep.Source, an.Assessment)
	return im.sc, rep, an, nil
}

// scriptFiles resolves the ordered init/action/end files.
func (im *importer) scriptFiles(usr *iniFile) []scriptFile {
	var out []scriptFile
	roleOf := func(key string) string {
		switch strings.ToLower(key) {
		case "vuser_init", "init":
			return "init"
		case "vuser_end", "end":
			return "end"
		}
		return "action"
	}
	if s := usr.section("Actions"); s != nil && len(s.keys) > 0 {
		var init, end, acts []scriptFile
		for _, kv := range s.keys {
			file := kv.val
			if file == "" {
				file = kv.key + ".c"
			}
			sf := scriptFile{file: baseName(file), entry: kv.key, role: roleOf(kv.key)}
			switch sf.role {
			case "init":
				init = append(init, sf)
			case "end":
				end = append(end, sf)
			default:
				acts = append(acts, sf)
			}
		}
		out = append(append(append(out, init...), acts...), end...)
		return out
	}
	// Conventional names: vuser_init.c / Action*.c / vuser_end.c, or the
	// init.c / run.c / end.c set from the official file list.
	fs := im.fs
	for _, n := range []string{"vuser_init.c", "init.c"} {
		if _, f, ok := fs.get(n); ok {
			out = append(out, scriptFile{file: f, entry: strings.TrimSuffix(f, path.Ext(f)), role: "init"})
			break
		}
	}
	var acts []string
	for _, n := range fs.withExt(".c") {
		l := strings.ToLower(n)
		if strings.HasPrefix(l, "action") || l == "run.c" {
			acts = append(acts, n)
		}
	}
	sort.SliceStable(acts, func(i, j int) bool { return naturalLess(acts[i], acts[j]) })
	for _, a := range acts {
		out = append(out, scriptFile{file: a, entry: strings.TrimSuffix(a, path.Ext(a)), role: "action"})
	}
	for _, n := range []string{"vuser_end.c", "end.c"} {
		if _, f, ok := fs.get(n); ok {
			out = append(out, scriptFile{file: f, entry: strings.TrimSuffix(f, path.Ext(f)), role: "end"})
			break
		}
	}
	if usr != nil {
		im.rep.Warnings = append(im.rep.Warnings, "the .usr has no [Actions] section; action files were found by name")
	}
	return out
}

// naturalLess orders Action.c < Action1.c < Action2.c < Action10.c.
func naturalLess(a, b string) bool {
	na, nb := numSuffix(a), numSuffix(b)
	pa := strings.TrimRight(strings.TrimSuffix(strings.ToLower(a), ".c"), "0123456789")
	pb := strings.TrimRight(strings.TrimSuffix(strings.ToLower(b), ".c"), "0123456789")
	if pa != pb {
		return pa < pb
	}
	return na < nb
}

func numSuffix(s string) int {
	s = strings.TrimSuffix(strings.ToLower(s), ".c")
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	n, err := strconv.Atoi(s[i:])
	if err != nil {
		return -1
	}
	return n
}

// collectNames pre-registers parameter names created by the script so that
// {Name} references anywhere (even before the registration in file order)
// are rewritten.
func (im *importer) collectNames(c *cCall) {
	switch c.name {
	case "web_reg_save_param":
		if n, _, ok := firstStr(c); ok {
			im.known[n] = true
		}
	case "web_reg_save_param_ex", "web_reg_save_param_json", "web_reg_save_param_regexp",
		"web_reg_save_param_xpath", "web_reg_save_param_attrib":
		a := parseLRArgs(c.args, false)
		if n := im.paramName(&a); n != "" {
			im.known[n] = true
		}
	case "lr_save_string", "lr_save_int", "lr_save_var":
		if len(c.args) >= 2 {
			if n, _, ok := strArg(c.args[len(c.args)-1]); ok {
				im.known[n] = true
			}
		}
	case "lr_param_sprintf", "web_save_timestamp_param", "lr_save_datetime", "lr_save_searched_string":
		// name is the first (or, for lr_save_datetime, last) string argument
		if c.name == "lr_save_datetime" && len(c.args) > 0 {
			if n, _, ok := strArg(c.args[len(c.args)-1]); ok {
				im.known[n] = true
			}
			return
		}
		if n, _, ok := firstStr(c); ok {
			im.known[n] = true
		}
	case "web_reg_find":
		a := parseLRArgs(c.args, false)
		if v := a.val("SaveCount"); v != "" {
			im.known[v] = true
		}
	}
}

// convertFile converts the entry function of one script file and reports
// everything else in the file (other functions, globals).
func (im *importer) convertFile(f *cFile, entry string) []model.Step {
	for _, d := range f.topLevel {
		im.rep.Add("global declaration", "", im.loc(f, d.line), model.Unsupported, "file-level C code not imported. Excerpt: "+d.excerpt)
		im.customCode(f, d.line, "global", d.excerpt)
	}
	fn := f.fn(entry)
	if fn == nil {
		for _, cand := range f.funcs {
			if strings.EqualFold(cand.name, entry) {
				fn = cand
				break
			}
		}
	}
	if fn == nil && len(f.funcs) > 0 {
		fn = f.funcs[0]
		im.rep.Warnings = append(im.rep.Warnings, fmt.Sprintf("%s: entry function %s() not found; using %s()", f.name, entry, fn.name))
	}
	for _, other := range f.funcs {
		if other == fn {
			continue
		}
		im.rep.Add("custom function", other.name, im.loc(f, other.line), model.Unsupported,
			fmt.Sprintf("C function %s() (lines %d-%d) is not imported; calls to it are reported where they occur", other.name, other.line, other.endLine))
		im.customCode(f, other.line, "custom-function", f.excerpt(other.line))
	}
	if fn == nil {
		im.rep.Add("action file", f.name, f.name, model.Unsupported, "no function definition found")
		return nil
	}
	b := im.newBuilder(f.name)
	im.stmts(f, fn.body, b)
	return b.finish()
}

func (im *importer) reportHeader(f *cFile) {
	ff := im.an.file(f.name)
	if ff.Role == "other" {
		im.rep.Add("source file", f.name, f.name, model.Unsupported, "C file not listed as an action in the .usr; not imported")
		return
	}
	for _, d := range f.topLevel {
		if d.kind == sDecl && strings.HasPrefix(d.keyword, "#") {
			continue
		}
		im.rep.Add("global declaration", "", im.loc(f, d.line), model.Partial, "global C declaration ignored (Load Meter variables are strings in ${...}). Excerpt: "+d.excerpt)
		im.customCode(f, d.line, "global", d.excerpt)
	}
	for _, fn := range f.funcs {
		im.rep.Add("custom function", fn.name, im.loc(f, fn.line), model.Unsupported, fmt.Sprintf("C function %s() defined in %s is not imported", fn.name, f.name))
		im.customCode(f, fn.line, "custom-function", f.excerpt(fn.line))
	}
	if len(f.topLevel) == 0 && len(f.funcs) == 0 {
		im.rep.Add("header", f.name, f.name, model.Ignored, "only preprocessor directives")
	}
}

// reportProtocolCalls lists non-web protocol calls of a blocked script.
func (im *importer) reportProtocolCalls(f *cFile) {
	for _, fn := range f.funcs {
		walkCalls(fn.body, func(c *cCall) {
			if p := protocolOf(c.name); p != "" {
				im.rep.Add(c.name, "", im.loc(f, c.line), model.Unsupported, p+" protocol function; requires the licensed LoadRunner runtime")
			}
		})
	}
}
