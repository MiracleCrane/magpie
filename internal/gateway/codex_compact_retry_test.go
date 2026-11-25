package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/yetone/magpie/internal/provider"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func compactSwitchFixture(t *testing.T) string {
	t.Helper()
	q, items := compactItems([]byte(strings.Replace(bigCompaction(), "fake/m1", "gpt-6.1-sol", 1)))
	last := items[len(items)-1]
	items = items[:len(items)-1]
	for i := 0; i < recentCompactToolOutputs; i++ {
		id := fmt.Sprintf("recent-%d", i)
		output := "short recent result"
		if i == 0 {
			output = strings.Repeat("keep recent output ", 900)
		}
		items = append(items, map[string]any{"type": "function_call", "call_id": id, "name": "shell", "arguments": "{}"}, map[string]any{"type": "function_call_output", "call_id": id, "output": output})
	}
	items = append(items, last)
	q["input"], _ = json.Marshal(items)
	b, err := marshalJSON(q)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func withoutTestCompactTrigger(t *testing.T, s string) string {
	t.Helper()
	q, items := compactItems([]byte(s))
	q["input"], _ = json.Marshal(items[:len(items)-1])
	b, err := marshalJSON(q)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertTargetedRetry(t *testing.T, body string) {
	t.Helper()
	if !strings.Contains(body, "image/png") || !strings.Contains(body, "now add a test") ||
		strings.Count(body, "keep recent output ") != 900 || strings.Count(body, "log line") >= 1000 ||
		!strings.Contains(body, "Middle of an older tool result omitted") {
		t.Fatalf("bad targeted replay, size %d", len(body))
	}
}

func TestCodexNativeCompactRetriesOversizedHistory(t *testing.T) {
	for _, inline := range []bool{false, true} {
		label := "unary"
		if inline {
			label = "inline"
		}
		t.Run(label, func(t *testing.T) {
			var sent []string
			codexUpstreamAt(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				sent = append(sent, string(b))
				if strings.Count(string(b), "log line") > 1000 {
					w.WriteHeader(400)
					io.WriteString(w, `{"error":{"message":"Your input exceeds the context window of this model.","type":"invalid_request_error","param":"input","code":"context_length_exceeded"}}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"resp_compact_test","object":"response.compaction","output":[]}`)
			})
			b := compactSwitchFixture(t)
			rest := "/responses/compact"
			if inline {
				rest = "/responses"
			} else {
				b = withoutTestCompactTrigger(t, b)
			}
			req := httptest.NewRequest("POST", CodexPath+rest, strings.NewReader(b))
			w := httptest.NewRecorder()
			New().codexUpstream(w, req, rest, []byte(b))
			if w.Code != 200 || len(sent) != 2 {
				t.Fatalf("status %d, attempts %d, reply %.240s", w.Code, len(sent), w.Body.String())
			}
			assertTargetedRetry(t, sent[1])
			if strings.Contains(sent[1], "compaction_trigger") != inline {
				t.Fatal("compaction trigger changed")
			}
		})
	}
}

func TestRoutedCodexCompactRetriesWithoutChangingAccounts(t *testing.T) {
	var bodies, accounts []string
	codexUpstreamAt(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		accounts = append(accounts, r.Header.Get("chatgpt-account-id"))
		if strings.Count(string(b), "log line") > 1000 {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model."}}`)
			return
		}
		io.WriteString(w, `{"id":"small","object":"response","output":[]}`)
	})
	p, _, ok := provider.Resolve("codex/gpt-6.1-sol")
	if !ok || p.Account == nil {
		t.Fatal("test Codex account not resolved")
	}
	b := compactSwitchFixture(t)
	b = withoutTestCompactTrigger(t, b)
	hdr := http.Header{}
	ctx := context.WithValue(context.Background(), codexCompactWireKey{}, true)
	res, err := New().forward(ctx, p, provider.Responses, "/responses", []byte(b), hdr)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || len(bodies) != 2 || accounts[0] == "" || accounts[0] != accounts[1] {
		t.Fatalf("status %d tries %d accounts %v", res.StatusCode, len(bodies), accounts)
	}
	assertTargetedRetry(t, bodies[1])
}

func TestCodexCompactPreflightKeepsMediaAndRecentResults(t *testing.T) {
	body := []byte(compactSwitchFixture(t))
	estimate := promptOf(provider.Responses, body)
	next, ok := planCodexCompact(body, estimate.Tokens/2)
	if !ok {
		t.Fatal("oversized full snapshot should be reduced")
	}
	assertTargetedRetry(t, string(next))
	if _, ok := planCodexCompact(body, estimate.Tokens*2); ok {
		t.Fatal("a fitting snapshot should be unchanged")
	}
}
