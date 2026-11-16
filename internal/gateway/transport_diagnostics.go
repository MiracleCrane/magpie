package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/yetone/magpie/internal/netproxy"
)

// Connection diagnostics keep transport metadata only. A connection has its
// own identity; pooled HTTP requests and kept WebSocket turns bind separately.
type transportDiagnosticEvent struct {
	writer         io.Writer
	At             int64  `json:"at_ns,string"`
	PID            int    `json:"pid"`
	Boot           int64  `json:"boot,string"`
	Event          string `json:"event"`
	Route          int64  `json:"route,omitempty"`
	Request        uint64 `json:"request,omitempty"`
	Session        string `json:"session,omitempty"`
	Downstream     string `json:"downstream,omitempty"`
	Connection     uint64 `json:"connection,omitempty"`
	Local          string `json:"local,omitempty"`
	Peer           string `json:"peer,omitempty"`
	CreatedAt      int64  `json:"created_ns,omitempty,string"`
	Trace          uint64 `json:"trace,omitempty"`
	Reused         bool   `json:"reused,omitempty"`
	ReadBytes      int64  `json:"read_bytes,omitempty"`
	WriteBytes     int64  `json:"write_bytes,omitempty"`
	LastRead       int64  `json:"last_read_ns,omitempty,string"`
	LastWrite      int64  `json:"last_write_ns,omitempty,string"`
	CloseStarted   int64  `json:"close_started_ns,omitempty,string"`
	CloseFinished  int64  `json:"close_finished_ns,omitempty,string"`
	Error          string `json:"error,omitempty"`
	Errno          uint64 `json:"errno,omitempty"`
	CloseStatus    int    `json:"close_status,omitempty"`
	Cancelled      bool   `json:"cancelled,omitempty"`
	Frames         uint64 `json:"frames,omitempty"`
	FrameBytes     uint64 `json:"frame_bytes,omitempty"`
	LastFrame      int64  `json:"last_frame_ns,omitempty,string"`
	DeliveredBytes int64  `json:"delivered_bytes,omitempty"`
	LastDelivered  int64  `json:"last_delivered_ns,omitempty,string"`
	Terminal       bool   `json:"terminal,omitempty"`
	Dropped        uint64 `json:"dropped,omitempty"`
	Pings          uint64 `json:"pings,omitempty"`
	Pongs          uint64 `json:"pongs,omitempty"`
	LastPing       int64  `json:"last_ping_ns,omitempty,string"`
	LastPong       int64  `json:"last_pong_ns,omitempty,string"`
	ReadError      string `json:"read_error,omitempty"`
	ReadErrorAt    int64  `json:"read_error_ns,omitempty,string"`
	ReadErrno      uint64 `json:"read_errno,omitempty"`
	WriteError     string `json:"write_error,omitempty"`
	WriteErrorAt   int64  `json:"write_error_ns,omitempty,string"`
	WriteErrno     uint64 `json:"write_errno,omitempty"`
}

var transportDiagnostics = struct {
	once    sync.Once
	queue   chan transportDiagnosticEvent
	dropped atomic.Uint64
	boot    int64
	mu      sync.Mutex
	active  map[uint64]*diagnosticConn
}{queue: make(chan transportDiagnosticEvent, 1024), boot: time.Now().UnixNano(), active: make(map[uint64]*diagnosticConn)}

var diagnosticIDs atomic.Uint64
var transportDiagnosticsEnabled atomic.Bool

func init() { transportDiagnosticsEnabled.Store(true) }

// No disk write or backpressure is allowed on the network goroutines.
func emitTransportDiagnostic(e transportDiagnosticEvent) {
	if !transportDiagnosticsEnabled.Load() {
		return
	}
	if e.At == 0 {
		e.At = time.Now().UnixNano()
	}
	e.PID, e.Boot = os.Getpid(), transportDiagnostics.boot
	e.writer = log.Writer()
	transportDiagnostics.once.Do(func() {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			write := func(e transportDiagnosticEvent) {
				e.Dropped = transportDiagnostics.dropped.Swap(0)
				if b, err := json.Marshal(e); err == nil {
					log.New(e.writer, log.Prefix(), log.Flags()).Printf("codex-transport-v2: %s", b)
				}
			}
			for {
				select {
				case e := <-transportDiagnostics.queue:
					write(e)
				case <-ticker.C:
					if !transportDiagnosticsEnabled.Load() {
						continue
					}
					transportDiagnostics.mu.Lock()
					connections := make([]*diagnosticConn, 0, len(transportDiagnostics.active))
					for _, c := range transportDiagnostics.active {
						connections = append(connections, c)
					}
					transportDiagnostics.mu.Unlock()
					for _, c := range connections {
						e := c.snapshot("wire_progress")
						e.At, e.PID, e.Boot = time.Now().UnixNano(), os.Getpid(), transportDiagnostics.boot
						e.writer = log.Writer()
						write(e)
					}
				}
			}
		}()
	})
	select {
	case transportDiagnostics.queue <- e:
	default:
		transportDiagnostics.dropped.Add(1)
	}
}

type diagnosticRequestKey struct{}

type diagnosticRequest struct {
	id         uint64
	route      int64
	session    string
	downstream string
}

func diagnosticRequestOf(ctx context.Context) *diagnosticRequest {
	d, _ := ctx.Value(diagnosticRequestKey{}).(*diagnosticRequest)
	return d
}

func withTransportDiagnostic(ctx context.Context, route int64, session, downstream string) context.Context {
	if !transportDiagnosticsEnabled.Load() {
		return ctx
	}
	sum := sha256.Sum256([]byte(session))
	d := &diagnosticRequest{id: diagnosticIDs.Add(1), route: route, session: hex.EncodeToString(sum[:8]), downstream: downstream}
	ctx = context.WithValue(ctx, diagnosticRequestKey{}, d)
	d.emit("request_start", nil, nil)
	context.AfterFunc(ctx, func() { d.emit("request_context_done", nil, ctx.Err()) })
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) {
			// socketTrace also emits a synthetic GotConn with no Conn.
			if c := diagnosticConnOf(i.Conn); c != nil {
				e := c.snapshot("http_bind")
				e.Reused = i.Reused
				d.fill(&e)
				emitTransportDiagnostic(e)
			}
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			d.emit("tls_handshake", nil, err)
		},
	})
}

func (d *diagnosticRequest) fill(e *transportDiagnosticEvent) {
	if d != nil {
		e.Route, e.Request, e.Session, e.Downstream = d.route, d.id, d.session, d.downstream
	}
}

func (d *diagnosticRequest) emit(kind string, c *diagnosticConn, err error) {
	if d == nil {
		return
	}
	e := transportDiagnosticEvent{Event: kind}
	if c != nil {
		e = c.snapshot(kind)
	}
	d.fill(&e)
	e.Error, e.Errno = transportErrorClass(err)
	emitTransportDiagnostic(e)
}

// diagnosticDispatch installs the observer after netproxy's LAN and IPv4
// wrappers, before any request or transport clone can use the dialer.
func diagnosticDispatch(base *http.Transport) http.RoundTripper {
	dispatch := netproxy.Dispatch(base)
	dial := base.DialContext
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		d := diagnosticRequestOf(ctx)
		if d == nil {
			return c, err
		}
		if err != nil {
			d.emit("dial_error", nil, err)
			return c, err
		}
		observed := &diagnosticConn{Conn: c, id: diagnosticIDs.Add(1), created: time.Now().UnixNano()}
		transportDiagnostics.mu.Lock()
		transportDiagnostics.active[observed.id] = observed
		transportDiagnostics.mu.Unlock()
		d.emit("wire_open", observed, nil)
		return observed, nil
	}
	return dispatch
}

type diagnosticConn struct {
	net.Conn
	id                          uint64
	created                     int64
	read, written               atomic.Int64
	lastRead, lastWrite         atomic.Int64
	closeStarted, closeFinished atomic.Int64
	readFault, writeFault       atomic.Pointer[diagnosticFault]
	pings, pongs                atomic.Uint64
	lastPing, lastPong          atomic.Int64
}

type diagnosticFault struct {
	at    int64
	code  string
	errno uint64
}

func (c *diagnosticConn) snapshot(kind string) transportDiagnosticEvent {
	e := transportDiagnosticEvent{Event: kind, Connection: c.id, Local: c.LocalAddr().String(), Peer: c.RemoteAddr().String(), CreatedAt: c.created,
		ReadBytes: c.read.Load(), WriteBytes: c.written.Load(), LastRead: c.lastRead.Load(), LastWrite: c.lastWrite.Load(), CloseStarted: c.closeStarted.Load(), CloseFinished: c.closeFinished.Load(),
		Pings: c.pings.Load(), Pongs: c.pongs.Load(), LastPing: c.lastPing.Load(), LastPong: c.lastPong.Load()}
	if f := c.readFault.Load(); f != nil {
		e.ReadError, e.ReadErrorAt, e.ReadErrno = f.code, f.at, f.errno
	}
	if f := c.writeFault.Load(); f != nil {
		e.WriteError, e.WriteErrorAt, e.WriteErrno = f.code, f.at, f.errno
	}
	return e
}

func (c *diagnosticConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	at := time.Now().UnixNano()
	if n > 0 {
		c.read.Add(int64(n))
		c.lastRead.Store(time.Now().UnixNano())
	}
	if err != nil {
		code, errno := transportErrorClass(err)
		if c.readFault.CompareAndSwap(nil, &diagnosticFault{at: at, code: code, errno: errno}) {
			e := c.snapshot("wire_read_error")
			e.At = at
			e.Error, e.Errno = code, errno
			emitTransportDiagnostic(e)
		}
	}
	return n, err
}

func (c *diagnosticConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	at := time.Now().UnixNano()
	if n > 0 {
		c.written.Add(int64(n))
		c.lastWrite.Store(time.Now().UnixNano())
	}
	if err != nil {
		code, errno := transportErrorClass(err)
		if c.writeFault.CompareAndSwap(nil, &diagnosticFault{at: at, code: code, errno: errno}) {
			e := c.snapshot("wire_write_error")
			e.At = at
			e.Error, e.Errno = code, errno
			emitTransportDiagnostic(e)
		}
	}
	return n, err
}

func (c *diagnosticConn) Close() error {
	if c.closeStarted.CompareAndSwap(0, time.Now().UnixNano()) {
		emitTransportDiagnostic(c.snapshot("wire_close_start"))
	}
	err := c.Conn.Close()
	if c.closeFinished.CompareAndSwap(0, time.Now().UnixNano()) {
		e := c.snapshot("wire_close_done")
		e.Error, e.Errno = transportErrorClass(err)
		emitTransportDiagnostic(e)
		transportDiagnostics.mu.Lock()
		delete(transportDiagnostics.active, c.id)
		transportDiagnostics.mu.Unlock()
	}
	return err
}

func diagnosticConnOf(c net.Conn) *diagnosticConn {
	for range 8 {
		switch v := c.(type) {
		case *diagnosticConn:
			return v
		case interface{ NetConn() net.Conn }:
			c = v.NetConn()
		default:
			return nil
		}
	}
	return nil
}

// Only fixed error classes and numeric OS error codes can leave this function.
func transportErrorClass(err error) (string, uint64) {
	if err == nil {
		return "", 0
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return "os_error", uint64(errno)
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled", 0
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline", 0
	case errors.Is(err, net.ErrClosed):
		return "closed", 0
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected_eof", 0
	case errors.Is(err, io.EOF):
		return "eof", 0
	}
	var timed net.Error
	if errors.As(err, &timed) && timed.Timeout() {
		return "timeout", 0
	}
	if websocket.CloseStatus(err) >= 0 {
		return "websocket_close", 0
	}
	var record tls.RecordHeaderError
	if errors.As(err, &record) {
		return "tls_record", 0
	}
	return "other", 0
}
