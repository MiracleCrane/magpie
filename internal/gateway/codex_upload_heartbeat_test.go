package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

// The long write still goes to a local server. Give it a finite rate until
// the server receives its Pong so loopback cannot absorb the whole upload
// before the server has had a chance to ask.
type heartbeatUploadConn struct {
	net.Conn
	pong <-chan struct{}
}

func (c *heartbeatUploadConn) Write(data []byte) (int, error) {
	written := 0
	for len(data) > 0 {
		select {
		case <-c.pong:
			n, err := c.Conn.Write(data)
			return written + n, err
		default:
		}
		n, err := c.Conn.Write(data[:min(len(data), 16<<10)])
		written += n
		if err != nil {
			return written, err
		}
		data = data[n:]
		time.Sleep(4 * time.Millisecond)
	}
	return written, nil
}

func limitedUploadServer(t *testing.T, release <-chan struct{}) *Server {
	t.Helper()
	s := New()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		raw, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &heartbeatUploadConn{Conn: raw, pong: release}, nil
	}}
	t.Cleanup(transport.CloseIdleConnections)
	s.client.Transport = codexSocket{next: transport}
	return s
}

func TestCodexUploadAnswersHeartbeatBeforeMessageEnds(t *testing.T) {
	socketOn(t)
	pong := make(chan struct{})
	var upgrades, posts atomic.Int64
	payload := strings.Repeat("0123456789abcdef", 1<<19)
	httpFallback := &fakeSocketBackend{whole: true}
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			httpFallback.ServeHTTP(w, r)
			return
		}
		upgrades.Add(1)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(-1)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		kind, reader, err := conn.Reader(ctx)
		if err != nil || kind != websocket.MessageText {
			t.Errorf("reading first frame: kind=%v err=%v", kind, err)
			return
		}
		// An application frame can arrive before the Ping. It must not
		// get lost or stop the control-frame reader until the upload ends.
		metadata := `{"type":"codex.response.metadata","headers":{"x-codex-turn-state":"early-meta"}}`
		if err := conn.Write(ctx, websocket.MessageText, []byte(metadata)); err != nil {
			t.Error(err)
			return
		}
		probed := make(chan error, 1)
		go func() {
			deadline, stop := context.WithTimeout(ctx, 700*time.Millisecond)
			defer stop()
			err := conn.Ping(deadline)
			probed <- err
			if err != nil {
				conn.Close(websocket.StatusInternalError, "missed upload heartbeat")
				return
			}
			close(pong)
		}()
		msg, readErr := io.ReadAll(reader)
		if err := <-probed; err != nil {
			t.Logf("upload missed the server heartbeat: %v", err)
			return
		}
		if readErr != nil || gjson.GetBytes(msg, "type").String() != "response.create" ||
			gjson.GetBytes(msg, "input.0.content.0.text").String() != payload {
			t.Errorf("corrupt upload: read %d bytes, error=%v", len(msg), readErr)
			return
		}
		for _, frame := range keptReply(&fakeSocketBackend{})(socketTurn{}) {
			if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
				t.Error(err)
				return
			}
		}
	})
	s := limitedUploadServer(t, pong)
	body := keptTurn("upload-heartbeat", "gpt-5.5", strings.Replace(userPing, "ping", payload, 1))
	req := httptest.NewRequest(http.MethodPost, CodexPath+"/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer chatgpt-token")
	req.Header.Set("ChatGPT-Account-ID", "heartbeat-account")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if got := rec.Body.String(); rec.Code != http.StatusOK ||
		!strings.Contains(got, `"delta":"pong"`) || !strings.Contains(got, "event: response.completed\n") {
		t.Fatalf("did not receive text and completion: status=%d response=%.200s", rec.Code, got)
	}
	if upgrades.Load() != 1 || posts.Load() != 0 || rec.Header().Get("X-Codex-Turn-State") != "early-meta" {
		t.Errorf("upgrades=%d HTTP=%d turn state=%q, want one socket, no replay and the early metadata",
			upgrades.Load(), posts.Load(), rec.Header().Get("X-Codex-Turn-State"))
	}
}

func TestCodexUploadCancelDuringSlowWrite(t *testing.T) {
	socketOn(t)
	started := make(chan struct{}, 1)
	disconnected := make(chan int, 1)
	var posts atomic.Int64
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			http.Error(w, "unexpected HTTP replay", http.StatusBadRequest)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(-1)
		ctx, stop := context.WithTimeout(r.Context(), 10*time.Second)
		defer stop()
		_, reader, err := conn.Reader(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		first, err := io.CopyN(io.Discard, reader, 16<<10)
		if err != nil {
			t.Error(err)
			return
		}
		started <- struct{}{}
		remaining, err := io.Copy(io.Discard, reader)
		if err == nil {
			// At a continuation-frame boundary the socket library can
			// return the bytes preceding EOF. Verify the connection ends
			// and the accumulated data is shorter than a complete create.
			_, _, err = conn.Read(ctx)
		}
		if err == nil {
			t.Error("unexpected second upstream message")
		}
		disconnected <- int(first + remaining)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	big := strings.Replace(userPing, "ping", strings.Repeat("x", 2<<20), 1)
	req := httptest.NewRequest(http.MethodPost, CodexPath+"/responses", strings.NewReader(keptTurn("cancel-upload", "gpt-5.5", big))).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer chatgpt-token")
	req.Header.Set("ChatGPT-Account-ID", "cancel-account")
	s := limitedUploadServer(t, nil) // every write stays rate-limited
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		s.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not receive the start of the upload")
	}
	start := time.Now()
	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not release the gateway request")
	}
	select {
	case n := <-disconnected:
		if n >= len(big) {
			t.Errorf("upstream received %d bytes after cancellation, expected a partial upload", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream socket still open after cancellation")
	}
	if n := posts.Load(); n != 0 {
		t.Errorf("canceled upload replayed over HTTP %d times", n)
	}
	t.Logf("gateway and upstream released in %s", time.Since(start))
}

func TestCodexUploadKeepsSocketForNextDelta(t *testing.T) {
	socketOn(t)
	f := &fakeSocketBackend{whole: true, keep: true}
	f.frames = keptReply(f)
	chatgpt(t, f.ServeHTTP)
	big := strings.Replace(userPing, "ping", strings.Repeat("x", 2<<20), 1)
	keptPost(t, "account", keptTurn("large-then-delta", "gpt-5.5", big))
	keptPost(t, "account", keptTurn("large-then-delta", "gpt-5.5", big, asstPong, userAgain))
	if p, n := f.counts(); p != 0 || n != 2 || f.opened() != 1 {
		t.Fatalf("HTTP=%d, messages=%d, handshakes=%d, want two messages on one socket", p, n, f.opened())
	}
	last := f.turn(1).msg
	if gjson.GetBytes(last, "previous_response_id").String() != "resp-1" ||
		gjson.GetBytes(last, "input.#").Int() != 1 || gjson.GetBytes(last, "input.0.content.0.text").String() != "again" {
		t.Fatalf("second message (%d bytes) did not continue with just the new input", len(last))
	}
}
