package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestCodexLargeCold1011Routes(t *testing.T) {
	socketOn(t)
	f := &fakeSocketBackend{whole: true, closeWith: websocket.StatusInternalError, hold: 150 * time.Millisecond, frames: func(socketTurn) []string { return nil }}
	chatgpt(t, f.ServeHTTP)
	big := strings.Replace(userPing, "ping", strings.Repeat("x", 7<<20), 1)
	send := func(session, acct, item string) time.Duration {
		at := time.Now()
		keptPost(t, acct, keptTurn(session, "gpt-5.5", item))
		return time.Since(at)
	}
	first := send("session-a", "account-a", big)
	second := send("session-a", "account-a", big)
	posts, ws := f.counts()
	t.Logf("first=%v repeated=%v HTTP=%d WS=%d", first, second, posts, ws)
	if posts != 2 || ws != 1 {
		t.Fatalf("same conversation incurred %d WS attempts; want 1", ws)
	}
	send("session-b", "account-a", big)
	send("session-a", "account-b", big)
	send("session-a", "account-a", userPing)
	posts, ws = f.counts()
	if posts != 5 || ws != 4 {
		t.Fatalf("cooldown affected a small message or another identity: HTTP %d WS %d", posts, ws)
	}
}

func TestCodexLargePreludeFrameDoesNotStartCooldown(t *testing.T) {
	socketOn(t)
	f := &fakeSocketBackend{whole: true, closeWith: websocket.StatusInternalError, frames: func(socketTurn) []string { return []string{`{"type":"codex.rate_limits"}`} }}
	chatgpt(t, f.ServeHTTP)
	msg := keptTurn("prelude", "gpt-5.5", strings.Replace(userPing, "ping", strings.Repeat("x", 1100000), 1))
	keptPost(t, "acct", msg)
	keptPost(t, "acct", msg)
	if posts, ws := f.counts(); posts != 2 || ws != 2 {
		t.Fatalf("unexpected cooldown after a received frame: HTTP %d WS %d", posts, ws)
	}
}

func TestCodexColdCooldownExpiresAndIsBounded(t *testing.T) {
	var cache coldTurnCache
	start := time.Unix(1000, 0)
	for i := 0; i <= maxColdTurns; i++ {
		cache.remember(strconv.Itoa(i), start.Add(time.Duration(i)*time.Second))
	}
	if cache.blocked("0", start) || !cache.blocked("1", start.Add(coldTurnCooldown)) {
		t.Fatal("capacity should evict the oldest failure only")
	}
	if cache.blocked("1", start.Add(coldTurnCooldown+time.Second)) {
		t.Fatal("checking a cooldown must not extend it")
	}
}

func TestCodexColdCooldownRetainsHealthyDelta(t *testing.T) {
	socketOn(t)
	var recoverWS atomic.Bool
	fail := &fakeSocketBackend{whole: true, closeWith: websocket.StatusInternalError, frames: func(socketTurn) []string { return nil }}
	healthy := &fakeSocketBackend{keep: true}
	healthy.frames = keptReply(healthy)
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		if recoverWS.Load() {
			healthy.ServeHTTP(w, r)
		} else {
			fail.ServeHTTP(w, r)
		}
	})
	big := strings.Replace(userPing, "ping", strings.Repeat("x", 1100000), 1)
	keptPost(t, "account", keptTurn("recovered", "gpt-5.5", big))
	recoverWS.Store(true)
	keptPost(t, "account", keptTurn("recovered", "gpt-5.5", userPing))
	keptPost(t, "account", keptTurn("recovered", "gpt-5.5", userPing, asstPong, big))
	if posts, ws := healthy.counts(); posts != 0 || ws != 2 || healthy.opened() != 1 {
		t.Fatalf("healthy socket was not reused: posts=%d messages=%d handshakes=%d", posts, ws, healthy.opened())
	}
	if !strings.Contains(string(healthy.turn(1).msg), `"previous_response_id":"resp-1"`) {
		t.Fatal("turn did not use a delta")
	}
}
