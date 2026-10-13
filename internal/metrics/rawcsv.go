package metrics

import (
	"bytes"
	"encoding/csv"
	"strconv"
	"sync"
)

// rawEnc formats one raw CSV row outside the collector lock.
type rawEnc struct {
	buf bytes.Buffer
	w   *csv.Writer
	rec []string
}

var rawEncPool = sync.Pool{New: func() any {
	e := &rawEnc{rec: make([]string, 0, 21)}
	e.w = csv.NewWriter(&e.buf)
	return e
}}

func ms3(d int64) string { return strconv.FormatInt(d, 10) }

// encodeRaw renders x as a CSV record (JTL-compatible leading columns, then
// Load Meter extras). The caller returns the encoder to rawEncPool.
func encodeRaw(x *Sample, threads int) *rawEnc {
	e := rawEncPool.Get().(*rawEnc)
	e.buf.Reset()
	reused := ""
	if x.Kind == KindRequest {
		reused = strconv.FormatBool(x.ConnReused)
	}
	code, msg := strconv.Itoa(x.Status), x.ErrKind
	if x.Kind == KindTransaction {
		code, msg = transactionCodeMessage(x)
	}
	th := strconv.Itoa(threads)
	e.rec = append(e.rec[:0],
		strconv.FormatInt(x.Start.UnixMilli(), 10), strconv.FormatInt(x.Latency.Milliseconds(), 10), x.Name,
		code, msg, "vu-"+strconv.Itoa(x.VU), strconv.FormatBool(x.OK), x.ErrMsg,
		strconv.FormatInt(x.BytesIn, 10), strconv.FormatInt(x.BytesOut, 10),
		strconv.FormatInt(x.TTFB.Milliseconds(), 10), x.URL, x.Kind,
		ms3(x.Connect.Milliseconds()), ms3(x.DNS.Milliseconds()), ms3(x.TLS.Milliseconds()), reused,
		strconv.Itoa(x.Retries), strconv.FormatInt(x.BodyBytes, 10), th, th)
	_ = e.w.Write(e.rec)
	e.w.Flush()
	return e
}
