package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/yetone/magpie/internal/provider"
)

type diagnosticTestLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *diagnosticTestLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *diagnosticTestLog) events() []transportDiagnosticEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	var events []transportDiagnosticEvent
	for _, line := range strings.Split(b.b.String(), "\n") {
		if i := strings.Index(line, "codex-transport-v2: "); i >= 0 {
			var e transportDiagnosticEvent
			if json.Unmarshal([]byte(line[i+len("codex-transport-v2: "):]), &e) == nil {
				events = append(events, e)
			}
		}
	}
	return events
}

func waitDiagnostic(t *testing.T, logs *diagnosticTestLog, match func([]transportDiagnosticEvent) bool) []transportDiagnosticEvent {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		events := logs.events()
		if match(events) {
			return events
		}
		select {
		case <-tick.C:
		case <-timer.C:
			t.Fatalf("diagnostic events did not arrive: %+v", events)
		}
	}
}

func captureDiagnostics(t *testing.T) *diagnosticTestLog {
	t.Helper()
	logs := new(diagnosticTestLog)
	old := log.Writer()
	log.SetOutput(logs)
	transportDiagnosticsEnabled.Store(true)
	t.Cleanup(func() { transportDiagnosticsEnabled.Store(false); log.SetOutput(old) })
	return logs
}

// Both turns must name the same physical connection and different routes,
// including the second turn that never passes through an HTTP handshake.
func TestTransportDiagnosticsKeptRequestBindings(t *testing.T) {
	socketOn(t)
	f := &fakeSocketBackend{keep: true}
	f.frames = keptReply(f)
	chatgpt(t, f.ServeHTTP)
	logs := captureDiagnostics(t)
	for _, body := range []string{keptTurn("PRIVATE_SESSION", "gpt-5.5", userPing), keptTurn("PRIVATE_SESSION", "gpt-5.5", userPing, asstPong, userAgain)} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", CodexPath+"/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer PRIVATE_AUTH")
		req.Header.Set("chatgpt-account-id", "test-account")
		req.Header.Set("Session-Id", "PRIVATE_SESSION")
		New().Handler().ServeHTTP(rec, req)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "event: response.completed") {
			t.Fatalf("turn failed: %d %s", rec.Code, rec.Body.String())
		}
	}
	events := waitDiagnostic(t, logs, func(events []transportDiagnosticEvent) bool {
		n := 0
		for _, e := range events {
			if e.Event == "ws_terminal" {
				n++
			}
		}
		return n >= 2
	})
	var bindings []transportDiagnosticEvent
	for _, e := range events {
		if e.Event == "ws_bind" {
			bindings = append(bindings, e)
		}
		encoded, _ := json.Marshal(e)
		if strings.Contains(string(encoded), "PRIVATE_") {
			t.Fatalf("diagnostics contain private input: %s", encoded)
		}
	}
	if len(bindings) != 2 {
		t.Fatalf("want two request bindings, got %+v", bindings)
	}
	a, b := bindings[0], bindings[1]
	if a.Connection == 0 || a.Connection != b.Connection || a.Route == 0 || a.Route == b.Route || a.Request == b.Request || a.Trace == b.Trace || !b.Reused {
		t.Fatalf("kept connection lost request identity: %+v", bindings)
	}
	if a.Local == "" || a.Peer == "" || a.Session == "" || a.Session != b.Session || f.opened() != 1 {
		t.Fatalf("connection endpoints/session were lost: %+v", bindings)
	}
}

// A raw transport EOF must be recorded before the cleanup close, and the
// stream must still fail. Diagnostics cannot fabricate completion.
func TestTransportDiagnosticsRawEOFPrecedesCleanup(t *testing.T) {
	socketOn(t)
	f := &fakeSocketBackend{drop: true, frames: func(socketTurn) []string { return wholeReply(socketTurn{})[:5] }}
	chatgpt(t, f.ServeHTTP)
	logs := captureDiagnostics(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = withTransportDiagnostic(ctx, 701, "PRIVATE_SESSION", "127.0.0.1:10000")
	req := httptest.NewRequest("POST", provider.CodexBase+"/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"PRIVATE_PROMPT"}`)).WithContext(ctx)
	// NewRequest on the server side has no GetBody; the transport requires it.
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"PRIVATE_PROMPT"}`)), nil
	}
	res, err := New().client.Transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(res.Body)
	res.Body.Close()
	if readErr == nil {
		t.Fatal("an interrupted upstream became a successful response")
	}
	events := waitDiagnostic(t, logs, func(events []transportDiagnosticEvent) bool {
		for _, e := range events {
			if e.Event == "wire_close_done" {
				return true
			}
		}
		return false
	})
	var raw, ws, close transportDiagnosticEvent
	for _, e := range events {
		switch e.Event {
		case "wire_read_error":
			raw = e
		case "ws_read_error":
			ws = e
		case "wire_close_start":
			close = e
		}
	}
	if raw.Connection == 0 || raw.Connection != ws.Connection || ws.Connection != close.Connection || raw.At > close.At || raw.Error != "eof" || ws.Route != 701 {
		t.Fatalf("cannot distinguish upstream EOF from cleanup: raw=%+v ws=%+v close=%+v", raw, ws, close)
	}
}

func TestTransportDiagnosticsFixedErrorClasses(t *testing.T) {
	for _, tc := range []struct {
		err    error
		code   string
		number uint64
	}{
		{nil, "", 0}, {io.EOF, "eof", 0}, {io.ErrUnexpectedEOF, "unexpected_eof", 0},
		{context.Canceled, "cancelled", 0}, {context.DeadlineExceeded, "deadline", 0},
		{net.ErrClosed, "closed", 0}, {syscall.Errno(10054), "os_error", 10054},
		{errors.New("PRIVATE_ERROR authorization=PRIVATE_AUTH"), "other", 0},
	} {
		code, number := transportErrorClass(tc.err)
		if code != tc.code || number != tc.number {
			t.Errorf("got %q/%d, want %q/%d", code, number, tc.code, tc.number)
		}
	}
}

// TLS must not hide the TCP endpoints used by the CONNECT proxy. Those
// endpoints are the join key for the independently captured Clash records.
func TestTransportDiagnosticsHTTPSConnectBinding(t *testing.T) {
	socketOn(t)
	f := &fakeSocketBackend{frames: wholeReply}
	up := httptest.NewTLSServer(f)
	t.Cleanup(up.Close)
	was := provider.CodexBase
	provider.CodexBase = up.URL + "/backend-api/codex"
	t.Cleanup(func() { provider.CodexBase = was })
	wantHost := strings.TrimPrefix(up.URL, "https://")
	seen := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != wantHost {
			http.Error(w, "fixture only", 400)
			return
		}
		to, err := net.Dial("tcp", wantHost)
		if err != nil {
			http.Error(w, "fixture unavailable", 502)
			return
		}
		from, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			to.Close()
			return
		}
		defer to.Close()
		defer from.Close()
		seen <- r.RemoteAddr
		io.WriteString(from, "HTTP/1.1 200 Connection Established\r\n\r\n")
		finished := make(chan struct{})
		go func() { io.Copy(to, from); to.Close(); close(finished) }()
		io.Copy(from, to)
		from.Close()
		<-finished
	}))
	t.Cleanup(proxy.Close)
	proxyURL, _ := url.Parse(proxy.URL)
	base := plainVendorTransport(0)
	base.Proxy = http.ProxyURL(proxyURL)
	base.TLSClientConfig = up.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	base.TLSClientConfig.MinVersion = tls.VersionTLS12
	client := &http.Client{Transport: codexSocket{next: diagnosticDispatch(base)}}
	t.Cleanup(base.CloseIdleConnections)
	logs := captureDiagnostics(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = withTransportDiagnostic(ctx, 702, "private-session", "127.0.0.1:10000")
	req, _ := http.NewRequestWithContext(ctx, "POST", provider.CodexBase+"/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"ping"}`))
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	events := waitDiagnostic(t, logs, func(events []transportDiagnosticEvent) bool {
		for _, e := range events {
			if e.Route == 702 && e.Event == "ws_terminal" {
				return true
			}
		}
		return false
	})
	var source string
	select {
	case source = <-seen:
	default:
		t.Fatal("request bypassed the CONNECT proxy")
	}
	for _, e := range events {
		if e.Route == 702 && e.Event == "ws_bind" {
			if e.Connection == 0 || e.Local != source || e.Peer != proxy.Listener.Addr().String() {
				t.Fatalf("TLS/CONNECT endpoint association lost: %+v, proxy source %s", e, source)
			}
			return
		}
	}
	t.Fatal("no WebSocket-to-CONNECT connection binding")
}

func TestTransportDiagnosticsPassivePingKeepsAutomaticPong(t *testing.T) {
	socketOn(t)
	pinged := make(chan error, 1)
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		if _, _, err = c.Read(r.Context()); err != nil {
			return
		}
		c.CloseRead(r.Context())
		err = c.Ping(r.Context())
		pinged <- err
		if err != nil {
			return
		}
		for _, frame := range wholeReply(socketTurn{}) {
			if c.Write(r.Context(), websocket.MessageText, []byte(frame)) != nil {
				return
			}
		}
		c.Close(websocket.StatusNormalClosure, "")
	})
	logs := captureDiagnostics(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = withTransportDiagnostic(ctx, 703, "private-session", "127.0.0.1:10000")
	req, _ := http.NewRequestWithContext(ctx, "POST", provider.CodexBase+"/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"ping"}`))
	res, err := New().client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = <-pinged; err != nil {
		t.Fatalf("diagnostic callback disabled automatic Pong: %v", err)
	}
	waitDiagnostic(t, logs, func(events []transportDiagnosticEvent) bool {
		for _, e := range events {
			if e.Route == 703 && e.Event == "ws_ping_received" && e.Pings == 1 && e.LastPing != 0 {
				return true
			}
		}
		return false
	})
}
