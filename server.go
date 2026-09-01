// Package gonicorn is Punicorn's shape written in Go: an accept loop that hands
// every connection to its own goroutine, which then serves that connection's
// requests one after another until the peer goes away.
//
// It exists as a control. Punicorn's claim is about a *shape* -- one unit of
// concurrency per connection, no pool and no reactor -- and Ruby is the only
// language it has been measured in, where a Ractor costs what a Ractor costs.
// Go runs the identical shape with a scheduler built for it, so the difference
// between the two numbers is what the shape costs in Ruby, not what the shape
// costs.
//
// The goroutine per connection is net/http's own arrangement: Server.Serve
// accepts, then `go c.serve(ctx)`, and that goroutine reads, dispatches and
// writes each request in turn. Nothing here re-implements it, which is the
// point -- the comparison is against Go's standard answer, not a hand-rolled
// one that would only be interesting if it beat the standard answer.
//
// What this package adds around net/http is the rest of Punicorn::Server: the
// connection cap, the live-connection count, per-connection state, graceful
// drain, and a banner that reports the same facts.
package gonicorn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const Version = "0.1.0"

// Options mirrors Punicorn::Server::DEFAULTS, minus the parts that have no Go
// counterpart. Field-by-field, and why the ones that are missing are missing:
//
//   - first_data_timeout      -> FirstDataTimeout (http.Server.ReadHeaderTimeout)
//   - persistent_timeout      -> PersistentTimeout (http.Server.IdleTimeout)
//   - shutdown_timeout        -> ShutdownTimeout
//   - max_connections         -> MaxConnections
//   - max_keep_alive          no equivalent: net/http does not count requests on
//     a connection, and adding a counter would put a branch on the hot path to
//     enforce a limit (999) that the benchmark never reaches.
//   - backlog                 not settable from the standard library: Go calls
//     listen(2) itself with /proc/sys/net/core/somaxconn. Punicorn asks for
//     1024. Worth knowing when reading a run with a queue in it; nothing in
//     the -c 64 benchmark comes near either number.
//   - enable_keep_alives      always on here; http.Server.SetKeepAlivesEnabled
//     exists if that ever needs to be a flag.
//   - cable                   the WebSocket layer is Ruby-side and is not ported.
type Options struct {
	Host string
	Port int

	// Ceiling on concurrent connections, or 0 for none. Punicorn's note applies
	// unchanged: what separates a design that degrades from one that collapses
	// is having a ceiling at all. The mechanism differs -- Punicorn stops
	// selecting on the listener, this blocks before accept(2) -- but both leave
	// the peer waiting in the kernel's accept queue, which is the backpressure.
	MaxConnections int

	FirstDataTimeout  time.Duration
	PersistentTimeout time.Duration
	ShutdownTimeout   time.Duration

	// Go sends a Date header on every response and Puma does not, so a response
	// to `/` is ~37 bytes larger here than it is from Punicorn for reasons that
	// have nothing to do with either server's design. Set this false to take
	// that difference out of a wire-bytes comparison; it is true by default
	// because sending Date is what Go's standard server does, and the point of
	// this program is to measure Go's standard server.
	SendDate bool

	Quiet bool
}

func DefaultOptions() Options {
	return Options{
		Host:              "0.0.0.0",
		Port:              9292,
		FirstDataTimeout:  30 * time.Second,
		PersistentTimeout: 65 * time.Second,
		ShutdownTimeout:   15 * time.Second,
		SendDate:          true,
	}
}

// Server accepts connections and gives each one a goroutine.
type Server struct {
	opts    Options
	handler http.Handler
	http    *http.Server

	mu sync.Mutex
	ln net.Listener

	live     atomic.Int64
	accepted atomic.Uint64

	stopOnce sync.Once
	stopped  chan struct{}
}

func New(handler http.Handler, opts Options) *Server {
	s := &Server{opts: opts, handler: handler, stopped: make(chan struct{})}

	errLog := log.New(os.Stderr, "gonicorn: ", 0)
	if opts.Quiet {
		errLog = log.New(io.Discard, "", 0)
	}

	s.http = &http.Server{
		Handler:           http.HandlerFunc(s.serveHTTP),
		ReadHeaderTimeout: opts.FirstDataTimeout,
		IdleTimeout:       opts.PersistentTimeout,
		ErrorLog:          errLog,
		// Every connection gets one of these, and the handler reaches it through
		// the request context. It is this package's answer to Ractor-local
		// storage: state that belongs to one connection, that the next
		// connection cannot see, and that goes away when the connection does.
		// A Ractor gets that isolation from the runtime; a goroutine has to be
		// handed it, because two goroutines share everything by default.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if tc, ok := c.(*trackedConn); ok {
				return context.WithValue(ctx, connStateKey{}, tc.state)
			}
			return ctx
		},
	}
	return s
}

// Listen binds the socket. Separate from Run for the same reason Punicorn has
// add_tcp_listener: a test wants the port before anything is served on it, and
// Port 0 then reports what the kernel chose.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", net.JoinHostPort(s.opts.Host, fmt.Sprint(s.opts.Port)))
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	return nil
}

func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Port is the bound port, which is only the requested one when that was not 0.
func (s *Server) Port() int {
	if a, ok := s.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

// LiveConnections counts connections still open. Drops to zero when the last
// one closes.
func (s *Server) LiveConnections() int64 { return s.live.Load() }

// AcceptedConnections counts connections accepted since boot.
func (s *Server) AcceptedConnections() uint64 { return s.accepted.Load() }

// Run serves until Stop. It returns nil on a clean stop, so that a caller can
// treat a returned error as a real failure.
func (s *Server) Run() error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		return errors.New("gonicorn: no listener; call Listen first")
	}

	err := s.http.Serve(s.limited(ln))
	if errors.Is(err, http.ErrServerClosed) {
		<-s.stopped // Stop's Shutdown is still draining; do not return before it is done
		return nil
	}
	return err
}

// Stop closes the listener and gives connections that are mid-request
// ShutdownTimeout to finish, which is Punicorn#drain: an in-flight response is
// answered rather than cut off, and an idle keep-alive connection is closed as
// soon as it is noticed to be idle.
//
// Where Punicorn has to call shutdown(2) on each parked socket to wake a Ractor
// that is blocked reading -- it has no Ractor#kill and no way to signal one --
// net/http tracks its own idle connections and closes them itself. The Ruby
// server needs 60 lines and an inverted ownership rule for the socket to get
// what this gets from one call, which is the shape of most of the difference
// between the two programs.
func (s *Server) Stop() error {
	var err error
	s.stopOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
		defer cancel()
		err = s.http.Shutdown(ctx)
		if errors.Is(err, context.DeadlineExceeded) {
			// Past the deadline the remaining connections are cut, which is
			// where Punicorn simply leaves: it cannot unwind a Ractor, so its
			// handlers' ensure blocks do not run. Go is in the same position --
			// there is no goroutine kill, and Close on the connection does not
			// unwind the handler either. See the /ensure endpoint.
			err = s.http.Close()
		}
		close(s.stopped)
	})
	return err
}

// Banner is what Punicorn prints at boot, answering the same questions.
func (s *Server) Banner() string {
	host := s.opts.Host
	connCap := "none"
	if s.opts.MaxConnections > 0 {
		connCap = fmt.Sprint(s.opts.MaxConnections)
	}
	return fmt.Sprintf(""+
		"Gonicorn %s — one goroutine per connection\n"+
		"* Go:        %s %s/%s\n"+
		"* Listening: http://%s:%d\n"+
		"* Scheduler: GOMAXPROCS=%d, %d goroutines at boot, native threads: %d\n"+
		"* Max conns: %s\n"+
		"Use Ctrl-C to stop",
		Version, runtime.Version(), runtime.GOOS, runtime.GOARCH,
		host, s.Port(), runtime.GOMAXPROCS(0), runtime.NumGoroutine(), nativeThreadCount(), connCap)
}

// serveHTTP is the whole middleware stack: suppress Date if asked, and turn a
// panic into a 500.
//
// Punicorn routes an application exception through Puma's lowlevel_error and
// answers 500 on the same connection. net/http would recover the panic, log it,
// and close the connection without a response, so the equivalent has to be
// written here. It is deliberately the only wrapper on the hot path, and it
// does not wrap the ResponseWriter: a wrapper would allocate on every request
// to learn something (were headers already sent?) that only matters on a path
// that never runs during a benchmark. If a handler panics after writing, the
// WriteHeader below is ignored and net/http logs "superfluous WriteHeader",
// which is the correct amount of noise for that case.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.opts.SendDate {
		w.Header()["Date"] = nil
	}
	defer func() {
		e := recover()
		if e == nil {
			return
		}
		if e == http.ErrAbortHandler { // net/http's own "drop this quietly"
			panic(e)
		}
		s.http.ErrorLog.Printf("app error: %v", e)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "Internal Server Error\n")
	}()
	s.handler.ServeHTTP(w, r)
}

// --- connection accounting -------------------------------------------------

// limitListener is netutil.LimitListener plus the live count, written out
// rather than imported to keep this a standard-library-only program.
//
// Acquiring the slot *before* accept(2) is what leaves the excess peer in the
// kernel's accept queue rather than accepting it and then holding an idle
// socket open with nobody serving it.
type limitListener struct {
	net.Listener
	srv *Server
	sem chan struct{}
}

func (s *Server) limited(ln net.Listener) net.Listener {
	l := &limitListener{Listener: ln, srv: s}
	if s.opts.MaxConnections > 0 {
		l.sem = make(chan struct{}, s.opts.MaxConnections)
	}
	return l
}

func (l *limitListener) Accept() (net.Conn, error) {
	if l.sem != nil {
		l.sem <- struct{}{}
	}
	c, err := l.Listener.Accept()
	if err != nil {
		if l.sem != nil {
			<-l.sem
		}
		return nil, err
	}
	l.srv.live.Add(1)
	l.srv.accepted.Add(1)
	return &trackedConn{Conn: c, lis: l, state: &connState{srv: l.srv}}, nil
}

// trackedConn releases the slot and the count when the connection closes, and
// carries the connection's own state to ConnContext.
type trackedConn struct {
	net.Conn
	lis   *limitListener
	state *connState
	once  sync.Once
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.lis.srv.live.Add(-1)
		if c.lis.sem != nil {
			<-c.lis.sem
		}
	})
	return err
}

// connState is per-connection storage, the goroutine-world stand-in for
// Ractor[:key]. Reached from a handler with ConnStateFrom(r.Context()).
type connState struct {
	srv     *Server
	mu      sync.Mutex
	ballast []string
}

type connStateKey struct{}

// ConnStateFrom returns the calling connection's state, or nil if the handler
// is running somewhere that has none (httptest, say).
func ConnStateFrom(ctx context.Context) *connState {
	st, _ := ctx.Value(connStateKey{}).(*connState)
	return st
}
