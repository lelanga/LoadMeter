package triage

import (
	"fmt"
	"regexp"
	"strings"
)

type classifyCtx struct {
	extractFirst int64  // ms offset of the earliest extraction failure cluster, -1 if none
	busy         []bool // generator CPU saturated, per second
}

type verdict struct{ side, conf, reason, explain, next string }

// busyFraction is the share of seconds in [first,last] (ms offsets) in which
// the generator was saturated; 0 when there is no data.
func (x *classifyCtx) busyFraction(first, last int64) float64 {
	if len(x.busy) == 0 {
		return 0
	}
	a, b := int(first/1000), int(last/1000)
	if a >= len(x.busy) {
		return 0
	}
	b = min(b, len(x.busy)-1)
	n, hit := 0, 0
	for i := a; i <= b; i++ {
		n++
		if x.busy[i] {
			hit++
		}
	}
	if n == 0 {
		return 0
	}
	return float64(hit) / float64(n)
}

var reExtractName = regexp.MustCompile(`extraction "([^"]+)"`)

func has(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

// classify applies the ordered rule list. The first matching rule wins.
// Rules are deliberately explicit so every verdict can be explained.
func classify(c *Cluster, x *classifyCtx) verdict {
	msg := c.rawLower
	url := strings.ToLower(c.Example.URL)
	after := c.AfterExtractionPct >= 50
	switch {
	case c.ErrKind == "blocked_by_policy":
		return verdict{SidePolicy, "high", "policy_block",
			"Load Meter's own safety allowlist refused these requests; nothing was sent to the target.",
			"Fix the URL or baseUrl, or add the host to the allowlist only if you are authorised to test it."}

	case has(url, "${", "{{", "%7b%7b", "%24%7b") || has(msg, "unresolved", "undefined variable", "${", "{{"):
		return verdict{SideScript, "high", "unresolved_variable",
			"The request still contains an unresolved variable, so the script never filled in a value it needs.",
			"Run a single-user debug iteration and check where the variable should have been set (data file column or extractor)."}

	case c.ErrKind == "invalid_request":
		return verdict{SideScript, "high", "invalid_request",
			"The request could not be built (bad URL, header or body), so it never reached the target.",
			"Validate the scenario and check the URL, headers and body of this request."}

	case c.ErrKind == "extraction":
		name := ""
		if m := reExtractName.FindStringSubmatch(c.rawLower); m != nil {
			name = " (" + m[1] + ")"
		}
		return verdict{SideScript, "high", "extraction_missing",
			fmt.Sprintf("A value the script needs from this response%s was not found %d times: the response changed, an earlier step failed, or the extractor expression is wrong.", name, c.Count),
			"Debug one iteration and compare this step's real response with the extractor's expression."}

	case c.ErrKind == "assertion":
		switch {
		case c.Status >= 500 || c.Status == 429:
			return verdict{SideTarget, "medium", "assertion_on_error_response",
				fmt.Sprintf("The assertion failed because the server answered HTTP %d, not because the script is wrong.", c.Status),
				"Look at the server's logs/metrics for the time this cluster started."}
		case after:
			return verdict{SideScript, "high", "assertion_after_extraction",
				fmt.Sprintf("%.0f%% of these assertion failures came within 10 s of an extraction failure on the same user: the script is missing a value from an earlier step.", c.AfterExtractionPct),
				"Fix the failing extractor first, then re-run; these should disappear."}
		case strings.HasPrefix(msg, "latency "):
			return verdict{SideTarget, "medium", "assertion_latency",
				"The response was slower than the latency limit set in the assertion.",
				"Compare with the p95/p99 of this request and decide if the limit or the server needs to change."}
		}
		return verdict{SideScript, "medium", "assertion_value",
			fmt.Sprintf("The server answered (HTTP %d) but the response did not contain what the assertion expects, such as a value that is dynamic or never appeared.", c.Status),
			"Open the response in a debug iteration and check whether the expected text or JSON path really exists."}

	case c.ErrKind == "http_4xx" || (c.Status >= 400 && c.Status < 500):
		switch {
		case c.Status == 429:
			return verdict{SideTarget, "high", "rate_limited",
				"The target is rate limiting (HTTP 429): the load exceeds what it is configured to accept.",
				"Check the target's rate-limit settings; lower the rate or ask for a test exemption."}
		case c.Status == 408:
			return verdict{SideTarget, "medium", "request_timeout_408",
				"The server reported a request timeout (HTTP 408).",
				"Check server timeouts and slow upstream calls."}
		case after && c.Status != 0:
			return verdict{SideScript, "high", "4xx_after_extraction",
				fmt.Sprintf("%.0f%% of these HTTP %d answers came within 10 s of an extraction failure on the same user: a missing token or id is being sent.", c.AfterExtractionPct, c.Status),
				"Fix the failing extractor (see the extraction cluster) and re-run."}
		case x.extractFirst >= 0 && x.extractFirst <= c.first+1000 && (c.Status == 400 || c.Status == 401 || c.Status == 403 || c.Status == 404 || c.Status == 422):
			return verdict{SideScript, "medium", "4xx_after_extractor_failure",
				fmt.Sprintf("HTTP %d began after an extraction failure elsewhere in the run, so a missing correlated value is likely.", c.Status),
				"Fix the extraction cluster first and re-run."}
		case c.Status == 401 || c.Status == 403:
			return verdict{SideScript, "medium", "auth_rejected",
				fmt.Sprintf("The server rejected the credentials or token (HTTP %d): expired or missing token, wrong test user, or a missing correlation.", c.Status),
				"Check login/token handling in a debug iteration and that test users are valid."}
		}
		return verdict{SideScript, "medium", "client_error",
			fmt.Sprintf("The server rejected the request as invalid (HTTP %d): the script is sending something the target does not accept.", c.Status),
			"Compare this request with a working one from the browser or recording."}

	case c.ErrKind == "http_5xx" || c.Status >= 500:
		e := "The target answered with a server error"
		if c.Status == 503 || c.Status == 502 || c.Status == 504 {
			e = "The target (or a gateway in front of it) was unavailable or overloaded"
		}
		return verdict{SideTarget, "high", "server_error",
			fmt.Sprintf("%s (HTTP %d). The script received a response, so it is not a script fault.", e, c.Status),
			"Check the target's logs and resource usage around the time this cluster started."}

	case c.ErrKind == "connect" || c.ErrKind == "connect_timeout":
		switch {
		case has(msg, "too many open files", "cannot assign requested address", "can't assign requested address", "address already in use", "no buffer space", "ephemeral"):
			return verdict{SideGenerator, "high", "local_port_exhaustion",
				"The load generator ran out of sockets, file descriptors or local ports, so connections failed on this machine.",
				"Raise the open-files limit (ulimit -n), widen the local port range, enable keep-alive, or spread load over more generators."}
		case has(msg, "no such host"):
			return verdict{SideScript, "medium", "dns_failure",
				"The host name could not be resolved: the URL or baseUrl in the script is probably wrong.",
				"Check the host name in the scenario variables."}
		case x.busyFraction(c.first, c.last) >= 0.5:
			return verdict{SideGenerator, "medium", "connect_generator_busy",
				fmt.Sprintf("Connections failed while the load generator CPU was saturated (%.0f%% of the time these errors occurred).", 100*x.busyFraction(c.first, c.last)),
				"Re-run with fewer users or on a bigger or second generator and see if the errors go away."}
		case has(msg, "connection refused"):
			return verdict{SideTarget, "medium", "connection_refused",
				"Nothing was listening at the target address, or its accept queue was full: the target was down, restarting or overloaded.",
				"Check that the target is running and listening on that port, and its connection backlog."}
		}
		return verdict{SideTarget, "low", "connect_failed",
			"Connections to the target could not be established.",
			"Check network path and target availability."}

	case c.ErrKind == "timeout" || c.ErrKind == "ttfb_timeout" || c.ErrKind == "read_timeout":
		if f := x.busyFraction(c.first, c.last); f >= 0.5 {
			return verdict{SideGenerator, "medium", "timeout_generator_busy",
				fmt.Sprintf("Requests timed out while the load generator CPU was saturated (%.0f%% of the time these errors occurred), so the delay may be on the generator.", 100*f),
				"Re-run with fewer users or on a bigger or second generator."}
		}
		return verdict{SideTarget, "medium", "timeout",
			"The target did not answer within the request timeout (the script's timeout could also be set too low).",
			"Compare the timeout with the latency of successful requests, and check server load around this time."}
	}
	if has(msg, "connection reset", "eof", "broken pipe", "tls handshake", "i/o timeout", "unexpected end") {
		return verdict{SideTarget, "medium", "connection_dropped",
			"The connection was dropped or reset by the target (or something in front of it) while the request was in flight.",
			"Check server and load balancer logs for resets, restarts or connection limits."}
	}
	return verdict{SideUnknown, "low", "unclassified",
		"The failure does not match a known pattern.", "Look at the example message and the request in a debug iteration."}
}
