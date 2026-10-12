package summary

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lelanga/loadmeter/internal/ai"
	"github.com/lelanga/loadmeter/internal/redact"
)

// Source says who wrote the summary text.
const (
	SourceTemplate = "template"
	SourceAI       = "ai"
)

// Result is a rendered summary.
type Result struct {
	Text     string      `json:"text"`
	Lines    []string    `json:"lines"`
	Source   string      `json:"source"`             // template | ai
	Verified bool        `json:"numbersVerified"`    // true when every number was checked against the facts
	Fallback string      `json:"fallback,omitempty"` // why the template was used although AI was requested
	Rejected []Violation `json:"rejected,omitempty"` // numbers the verifier refused
	Provider string      `json:"provider,omitempty"`
	Model    string      `json:"model,omitempty"`
	Facts    Facts       `json:"facts"`
}

const aiSystem = `You rewrite load-test results as a short plain-English summary for a non-programmer.
Rules:
- Use ONLY the facts in the JSON. Do not add numbers, causes, names or advice that are not in it.
- Every number you write must appear in the facts (copy it exactly or round it only slightly). Do not calculate new numbers.
- Say "likely" or "may" for causes; the "side" fields are rule-based hints, not proof.
- At most 8 short sentences, plain text, no markdown, no bullet characters.
- If failures exist, name the biggest group, say which side it points to, and give the suggested next check.
` + ai.GuardInstruction

// maxAIChars bounds accepted model output.
const maxAIChars = 1800

// Summarize renders the template summary and, when wantAI is true, tries to
// have the model reword it. The AI client enforces enablement, cloud consent,
// local-only mode, call limits and the monthly budget exactly as for the other
// AI features (ai.Client.Complete); this function adds no policy of its own.
// Only the facts JSON is sent, wrapped as untrusted data. If the AI is not
// available, errors, or writes a number that is not in the facts, the template
// summary is returned and Fallback says why.
func Summarize(ctx context.Context, client *ai.Client, f Facts, wantAI bool) Result {
	lines := Render(f)
	res := Result{Text: Text(lines), Lines: lines, Source: SourceTemplate, Verified: true, Facts: f}
	if !wantAI {
		return res
	}
	if client == nil || !client.Cfg.Enabled {
		res.Fallback = "AI is not enabled, so the built-in summary is shown. Enable a provider with 'loadmeter ai enable <light-id> <strong-id>' (add --cloud-consent for a cloud provider) or in Settings → AI."
		return res
	}
	b, _ := json.Marshal(f)
	resp, err := client.Complete(ctx, ai.Request{Task: "summary", MaxTokens: 600, System: aiSystem,
		Messages: []ai.Message{{Role: "user", Content: "Write the summary from these facts.\n" + ai.Untrusted("run-facts", redact.Text(string(b)))}}})
	if err != nil {
		res.Fallback = "The AI could not be used (" + err.Error() + "), so the built-in summary is shown."
		return res
	}
	res.Provider, res.Model = resp.Provider, resp.Model
	text := cleanAI(resp.Text)
	switch {
	case text == "":
		res.Fallback = "The AI returned no text, so the built-in summary is shown."
	case len(text) > maxAIChars:
		res.Fallback = "The AI text was too long to trust, so the built-in summary is shown."
	default:
		if bad := Verify(text, f); len(bad) > 0 {
			nums := make([]string, 0, len(bad))
			for _, v := range bad {
				nums = append(nums, v.Number)
			}
			res.Rejected = bad
			res.Fallback = fmt.Sprintf("The AI wording contained numbers that are not in the run's facts (%s), so the built-in summary is shown instead.", strings.Join(nums, ", "))
			return res
		}
		res.Text, res.Lines, res.Source = text, strings.Split(text, "\n"), SourceAI
	}
	return res
}

func cleanAI(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```text")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
