package engine

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lelanga/phoenix/internal/model"
)

var errDataExhausted = errors.New("data source exhausted")

type feed struct {
	ds     model.DataSource
	cols   []string
	rows   [][]string
	mu     sync.Mutex
	cursor int
}

// SafePath resolves rel inside base and rejects escapes (imported scenarios are untrusted).
func SafePath(base, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths are not allowed in scenarios: %s", rel)
	}
	p := filepath.Join(base, rel)
	r, err := filepath.Rel(base, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes the scenario directory: %s", rel)
	}
	return p, nil
}

func loadFeed(ds model.DataSource, baseDir string) (*feed, error) {
	f := &feed{ds: ds, cols: ds.Columns, rows: ds.Rows}
	if ds.File != "" {
		p, err := SafePath(baseDir, ds.File)
		if err != nil {
			return nil, fmt.Errorf("data source %q: %w", ds.Name, err)
		}
		fh, err := os.Open(p)
		if err != nil {
			return nil, fmt.Errorf("data source %q: %w", ds.Name, err)
		}
		defer fh.Close()
		r := csv.NewReader(io.LimitReader(fh, 512<<20))
		if ds.Delimiter != "" {
			r.Comma = []rune(ds.Delimiter)[0]
		}
		r.FieldsPerRecord = -1
		r.TrimLeadingSpace = true
		recs, err := r.ReadAll()
		if err != nil {
			return nil, fmt.Errorf("data source %q: %w", ds.Name, err)
		}
		if len(f.cols) == 0 && len(recs) > 0 {
			f.cols, recs = recs[0], recs[1:]
		}
		f.rows = append(f.rows, recs...)
	}
	if len(f.cols) == 0 {
		return nil, fmt.Errorf("data source %q has no columns", ds.Name)
	}
	if len(f.rows) == 0 {
		return nil, fmt.Errorf("data source %q has no rows", ds.Name)
	}
	return f, nil
}

// next returns the row for a VU. per-vu ignores the cursor.
func (f *feed) next(vu int, rnd *rand.Rand) ([]string, error) {
	switch f.ds.Mode {
	case "random":
		return f.rows[rnd.IntN(len(f.rows))], nil
	case "per-vu":
		i := vu - 1
		if i >= len(f.rows) {
			if f.ds.OnExhaust == "" || f.ds.OnExhaust == "recycle" {
				i %= len(f.rows)
			} else {
				return nil, errDataExhausted
			}
		}
		return f.rows[i], nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cursor >= len(f.rows) {
		onEx := f.ds.OnExhaust
		if onEx == "" {
			onEx = "recycle"
			if f.ds.Mode == "unique" {
				onEx = "stop"
			}
		}
		if onEx != "recycle" {
			return nil, errDataExhausted
		}
		f.cursor = 0
	}
	row := f.rows[f.cursor]
	f.cursor++
	return row, nil
}

func (f *feed) assign(vars map[string]string, row []string) {
	for i, c := range f.cols {
		v := ""
		if i < len(row) {
			v = row[i]
		}
		vars[c] = v
		vars[f.ds.Name+"."+c] = v
	}
}

// LoadRows reads a data source (file and/or inline rows) with the same rules
// the engine uses, returning its columns and rows. Distributed runs use it to
// ship CSV contents to agents as inline rows.
func LoadRows(ds model.DataSource, baseDir string) ([]string, [][]string, error) {
	f, err := loadFeed(ds, baseDir)
	if err != nil {
		return nil, nil, err
	}
	return f.cols, f.rows, nil
}
