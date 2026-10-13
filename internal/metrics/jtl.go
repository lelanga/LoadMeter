package metrics

import (
	"strconv"
	"sync/atomic"
)

// TransactionMessagePrefix starts the responseMessage of a transaction row in
// the raw CSV. JMeter's Transaction Controller writes exactly this text and
// JMeter's HTML report generator uses it to tell transaction (controller) rows
// from sampler rows.
const TransactionMessagePrefix = "Number of samples in transaction : "

// transactionCodeMessage renders the JMeter responseCode/responseMessage of a
// transaction row.
func transactionCodeMessage(x *Sample) (string, string) {
	code := ""
	if x.OK {
		code = "200"
	}
	return code, TransactionMessagePrefix + strconv.Itoa(x.SubSamples) +
		", number of failing samples : " + strconv.Itoa(x.SubFailures)
}

// jtlThreads supplies the grpThreads/allThreads columns.
type jtlThreads struct {
	fn    atomic.Pointer[func() int]
	gauge atomic.Int64 // last SetGauge value, used when no function is set
}

// SetThreads sets the function that reports the number of active VUs for the
// grpThreads/allThreads raw CSV columns (it is called once per sample, so it
// must be cheap and safe for concurrent use). Without it the last gauge value
// is used.
func (c *Collector) SetThreads(fn func() int) {
	if fn == nil {
		c.threads.fn.Store(nil)
		return
	}
	c.threads.fn.Store(&fn)
}

func (c *Collector) threadCount() int {
	if f := c.threads.fn.Load(); f != nil {
		return (*f)()
	}
	return int(c.threads.gauge.Load())
}
