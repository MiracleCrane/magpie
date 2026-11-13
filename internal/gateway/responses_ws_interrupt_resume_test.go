package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestResponsesSocketInterruptedResponseContinues(t *testing.T) {
	for _, prior := range []bool{false, true} {
		name := "fresh"
		if prior {
			name = "with_completed_history"
		}
		t.Run(name, func(t *testing.T) {
			fresh(t)
			u := &respUp{}
			sockUp(t, u)
			_, ts := sockServer(t)
			c, _, err := dialSock(t, ts, CodexPath+"/responses", http.Header{"User-Agent": {"codex_cli_rs/0.166.0"}})
			if err != nil {
				t.Fatal(err)
			}
			previous, initial := "", userHi
			want := []string{"hi", "continue"}
			if prior {
				got := c.turn(create("", userHi))
				if last(got).Get("type").String() != "response.completed" {
					t.Fatal(got)
				}
				previous, initial = last(got).Get("response.id").String(), userMore
				want = []string{"hi", "answer 1", "and more", "continue"}
			}
			gone := make(chan struct{}, 1)
			u.mu.Lock()
			u.reply = func(w http.ResponseWriter, r *http.Request, n int) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, sse(
					`data: {"type":"response.created","response":{"id":"resp_interrupted","object":"response","status":"in_progress","output":[]}}`,
					`data: {"type":"response.output_text.delta","delta":"discard-partial-output"}`))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				gone <- struct{}{}
			}
			u.mu.Unlock()
			c.send(create(previous, initial))
			if _, err := c.until("response.output_text.delta"); err != nil {
				t.Fatal(err)
			}
			c.send(`{"type":"response.interrupt","response_id":"resp_interrupted","mode":"discard_partial_items"}`)
			got, err := c.until("response.incomplete")
			if err != nil {
				t.Fatal(err)
			}
			accepted := false
			for _, e := range got {
				accepted = accepted || e.Get("type").String() == "response.interrupt.accepted"
			}
			if !accepted || len(last(got).Get("response.output").Array()) != 0 {
				t.Fatalf("interrupt: %v", got)
			}
			select {
			case <-gone:
			case <-time.After(30 * time.Second):
				t.Fatal("upstream did not cancel")
			}
			u.mu.Lock()
			u.reply = nil
			u.mu.Unlock()
			calls := u.calls()
			next := `{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}`
			got = c.turn(create("resp_interrupted", next))
			if last(got).Get("type").String() != "response.completed" {
				t.Fatalf("interrupted response could not continue: %v", got)
			}
			if u.calls() != calls+1 {
				t.Fatalf("upstream calls=%d, want %d", u.calls(), calls+1)
			}
			body := u.body(calls)
			input := gjson.GetBytes(body, "input").Array()
			if len(input) != len(want) {
				t.Fatalf("input count=%d, want %d", len(input), len(want))
			}
			for i, text := range want {
				if input[i].Get("content.0.text").String() != text {
					t.Fatalf("input %d changed: %s", i, body)
				}
			}
			if strings.Contains(string(body), "discard-partial-output") || gjson.GetBytes(body, "previous_response_id").Exists() {
				t.Fatal("discarded output or local response ID went upstream")
			}
			id := last(got).Get("response.id").String()
			got = c.turn(create(id, userMore))
			if last(got).Get("type").String() != "response.completed" || len(gjson.GetBytes(u.body(calls+1), "input").Array()) != len(want)+2 {
				t.Fatal("normal continuation after interrupt lost or duplicated history")
			}
		})
	}
}
