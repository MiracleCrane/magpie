package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/yetone/magpie/internal/provider"
)

// Retry a rejected compaction on the same account, keeping its original
// items, images, checkpoints and recent tool results. The old transcript on
// disk is unaffected. An input delta can't be repaired without its prefix.
const oldCompactToolBytes = 8192
const recentCompactToolOutputs = 24

type codexCompactWireKey struct{}

func isCodexCompactWire(ctx context.Context) bool {
	yes, _ := ctx.Value(codexCompactWireKey{}).(bool)
	return yes
}

// The plan uses the complete outbound body and the selected account's
// advertised window, not the previous provider's usage report.
func planCodexCompact(body []byte, budget int) ([]byte, bool) {
	if len(body) < 256<<10 || getIn(body, "previous_response_id").Str != "" {
		return nil, false
	}
	limit := budget * 90 / 100
	if estimate := promptOf(provider.Responses, body); estimate == nil || estimate.Tokens <= limit {
		return nil, false
	}
	var best []byte
	for _, cap := range []int{oldCompactToolBytes, 4096, 2048} {
		next, ok := smallerNativeCompactAt(body, cap)
		if !ok {
			continue
		}
		best = next
		if estimate := promptOf(provider.Responses, next); estimate != nil && estimate.Tokens <= limit {
			break
		}
	}
	return best, best != nil
}

func smallerNativeCompact(body []byte) ([]byte, bool) {
	return smallerNativeCompactAt(body, oldCompactToolBytes)
}

func smallerNativeCompactAt(body []byte, cap int) ([]byte, bool) {
	if getIn(body, "previous_response_id").Str != "" {
		return nil, false
	}
	q, items := compactItems(body)
	if q == nil || len(items) == 0 {
		return nil, false
	}
	recent, changed := 0, false
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		switch it["type"] {
		case "function_call_output", "custom_tool_call_output":
		default:
			continue
		}
		if recent < recentCompactToolOutputs {
			recent++
			continue
		}
		switch out := it["output"].(type) {
		case string:
			if v, ok := trimOldCompactTool(out, cap); ok {
				it["output"], changed = v, true
			}
		case []any:
			for _, part := range out {
				if m, ok := part.(map[string]any); ok {
					if text, ok := m["text"].(string); ok {
						if v, ok := trimOldCompactTool(text, cap); ok {
							m["text"], changed = v, true
						}
					}
				}
			}
		}
	}
	if !changed {
		return nil, false
	}
	var err error
	q["input"], err = json.Marshal(items)
	if err != nil {
		return nil, false
	}
	next, err := marshalJSON(q)
	return next, err == nil && len(next) < len(body)
}

func trimOldCompactTool(s string, cap int) (string, bool) {
	if len(s) <= cap {
		return s, false
	}
	cut := cap / 2
	return strings.ToValidUTF8(s[:cut], "\ufffd") +
		"\n[Middle of an older tool result omitted from this compaction retry; full result remains in the original conversation.]\n" +
		strings.ToValidUTF8(s[len(s)-cut:], "\ufffd"), true
}

func compactOverflowStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge || status == http.StatusUnprocessableEntity
}

type compactRefusalBody struct {
	io.Reader
	io.Closer
}

func retryNativeCompact(res *http.Response, body []byte) ([]byte, bool) {
	prefix, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	res.Body = compactRefusalBody{io.MultiReader(bytes.NewReader(prefix), res.Body), res.Body}
	if err != nil || res.StatusCode != http.StatusRequestEntityTooLarge && !overflowed(res.StatusCode, prefix) {
		return nil, false
	}
	next, ok := smallerNativeCompact(body)
	if ok {
		res.Body.Close()
	}
	return next, ok
}
