package accesslog

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var rangeRe = regexp.MustCompile(`^(\d{1,2}):(\d{2})-(\d{1,2}):(\d{2})$`)

func clock(key int64) string {
	return time.Unix(key*60, 0).UTC().Format("2006-01-02 15:04")
}

// rates fills Rate, Window and Curve from the per-minute tables.
func (a *analyzer) rates() error {
	p := a.p
	if len(a.allMin) > 0 {
		lo, hi := int64(1<<62), int64(-1<<62)
		var peakKey int64
		peak := -1
		for k, n := range a.allMin {
			lo, hi = min(lo, k), max(hi, k)
			if n > peak || n == peak && k < peakKey {
				peak, peakKey = n, k
			}
		}
		span := int(hi-lo) + 1
		total := 0
		for _, n := range a.allMin {
			total += n
		}
		p.Rate = RateSummary{Minutes: span, AvgRPS: round(float64(total)/float64(span)/60, 3),
			PeakRPS: round(float64(peak)/60, 3), PeakMinute: clock(peakKey)}
	}
	if p.Replayable == 0 {
		return nil // nothing to replay; Generate reports it
	}
	win := strings.ToLower(strings.TrimSpace(a.o.Window))
	lo, hi := int64(1<<62), int64(-1<<62)
	for k := range a.repMin {
		lo, hi = min(lo, k), max(hi, k)
	}
	if hi-lo+1 > maxSpanMinutes {
		return fmt.Errorf("log spans %d minutes (more than %d); split it or use --window HH:MM-HH:MM on a shorter file", hi-lo+1, maxSpanMinutes)
	}
	span := int(hi-lo) + 1
	dense := make([]float64, span)
	for k, n := range a.repMin {
		dense[k-lo] = float64(n)
	}
	switch {
	case win == "avg":
		total := 0.0
		for _, v := range dense {
			total += v
		}
		avg := total / float64(span) / 60
		p.Window = WindowInfo{Mode: "avg", Spec: "avg", Start: clock(lo), Minutes: span, AvgRPS: round(avg, 3), PeakRPS: round(avg, 3)}
	case win == "peak":
		n := min(a.o.PeakMinutes, span)
		sum, best, at := 0.0, -1.0, 0
		for i, v := range dense {
			sum += v
			if i >= n {
				sum -= dense[i-n]
			}
			if i >= n-1 && sum > best {
				best, at = sum, i-n+1
			}
		}
		a.setWindow("peak", "peak", dense[at:at+n], clock(lo+int64(at)), 0)
	case rangeRe.MatchString(win):
		m := rangeRe.FindStringSubmatch(win)
		h1, m1, h2, m2 := atoi(m[1]), atoi(m[2]), atoi(m[3]), atoi(m[4])
		if h1 > 23 || h2 > 24 || m1 > 59 || m2 > 59 || (h2 == 24 && m2 != 0) {
			return fmt.Errorf("invalid --window %q: use peak, avg or HH:MM-HH:MM (24h clock of the log)", a.o.Window)
		}
		start, end := h1*60+m1, h2*60+m2
		length := (end - start + 1440) % 1440
		if length == 0 {
			return fmt.Errorf("invalid --window %q: start and end are equal", a.o.Window)
		}
		sums := make([]float64, length)
		days := map[int64]bool{}
		for i, v := range dense {
			key := lo + int64(i)
			tod := int(((key % 1440) + 1440) % 1440)
			off := (tod - start + 1440) % 1440
			if off >= length {
				continue
			}
			sums[off] += v
			days[floorDiv(key-int64(off), 1440)] = true
		}
		if len(days) == 0 {
			return fmt.Errorf("--window %s matches no replayable requests (log covers %s to %s)", a.o.Window, clock(lo), clock(hi))
		}
		// Average over the days that fall (even partly) inside the window.
		out := make([]float64, length)
		for i := range out {
			out[i] = sums[i] / float64(len(days))
		}
		a.setWindow("range", win, out, fmt.Sprintf("%02d:%02d", start/60, start%60), len(days))
		p.Window.Start = fmt.Sprintf("%02d:%02d", start/60, start%60)
	default:
		return fmt.Errorf("invalid --window %q: use peak, avg or HH:MM-HH:MM (24h clock of the log)", a.o.Window)
	}
	return nil
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func (a *analyzer) setWindow(mode, spec string, counts []float64, start string, days int) {
	p := a.p
	p.window = counts
	total, peak := 0.0, 0.0
	for _, v := range counts {
		total += v
		peak = max(peak, v)
	}
	p.Window = WindowInfo{Mode: mode, Spec: spec, Start: start, Minutes: len(counts),
		AvgRPS: round(total/float64(len(counts))/60, 3), PeakRPS: round(peak/60, 3), Days: days}
	for i, v := range counts {
		label := ""
		if mode == "range" {
			var h, m int
			fmt.Sscanf(start, "%d:%d", &h, &m)
			t := (h*60 + m + i) % 1440
			label = fmt.Sprintf("%02d:%02d", t/60, t%60)
		} else if t, err := time.Parse("2006-01-02 15:04", start); err == nil {
			label = t.Add(time.Duration(i) * time.Minute).Format("2006-01-02 15:04")
		}
		p.Curve = append(p.Curve, RatePoint{Minute: label, Requests: round(v, 1)})
	}
}
