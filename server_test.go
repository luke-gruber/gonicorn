package gonicorn

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startServer boots a real Server on a real port rather than using httptest,
// because everything worth testing here -- the connection cap, the live count,
// the per-connection state, the drain -- lives in the accept path that
// httptest.Server would replace.
func startServer(t *testing.T, h http.Handler, opts Options) (*Server, string) {
	t.Helper()
	opts.Host = "127.0.0.1"
	opts.Port = 0
	opts.Quiet = true
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = 5 * time.Second
	}
	srv := New(h, opts)
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Run() }()

	t.Cleanup(func() {
		srv.Stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after Stop")
		}
	})
	return srv, fmt.Sprintf("http://127.0.0.1:%d", srv.Port())
}

func get(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(b)
}

func TestBenchAppEndpoints(t *testing.T) {
	_, base := startServer(t, BenchApp(), DefaultOptions())
	c := &http.Client{Timeout: 10 * time.Second}

	for _, tc := range []struct {
		path   string
		status int
		body   string // exact match unless want is a prefix check below
	}{
		{"/", 200, "Hello, world!"},
		{"/cpu?n=10", 200, "285"}, // 0+1+4+...+81
		{"/bytes?n=8", 200, "xxxxxxxx"},
		{"/alloc?n=7", 200, "7"},
		{"/stream?n=3", 200, "chunk 0\nchunk 1\nchunk 2\n"},
		{"/gc?off", 200, "disabled"},
		{"/gc", 200, "enabled"},
		{"/nope", 404, "not found"},
	} {
		status, body := get(t, c, base+tc.path)
		if status != tc.status || body != tc.body {
			t.Errorf("GET %s = %d %q, want %d %q", tc.path, status, body, tc.status, tc.body)
		}
	}

	// Defaults come from the query being absent, exactly as they do in
	// config.ru: /cpu with no n is n=20000.
	if _, body := get(t, c, base+"/cpu"); body != "2666466670000" {
		t.Errorf("GET /cpu = %q, want the n=20000 sum", body)
	}
	if _, body := get(t, c, base+"/bytes"); len(body) != 1024 {
		t.Errorf("GET /bytes returned %d bytes, want 1024", len(body))
	}
}

func TestEchoHashesTheRequestBody(t *testing.T) {
	_, base := startServer(t, BenchApp(), DefaultOptions())
	c := &http.Client{Timeout: 10 * time.Second}

	resp, err := c.Post(base+"/echo", "application/octet-stream", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("POST /echo: %v", err)
	}
	defer resp.Body.Close()
	var got struct {
		Bytes int    `json:"bytes"`
		SHA1  string `json:"sha1"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Bytes != 5 || got.SHA1 != "aaf4c61ddcc5e8a2dabede0f3b482cd9aea9434d" {
		t.Errorf("/echo = %+v, want 5 bytes and sha1(hello)", got)
	}
}

// A chunked request body has no Content-Length, which is the path config.ru's
// /echo exercises against Puma's decoder.
func TestEchoAcceptsAChunkedBody(t *testing.T) {
	_, base := startServer(t, BenchApp(), DefaultOptions())
	req, err := http.NewRequest("POST", base+"/echo", io.NopCloser(strings.NewReader("hello")))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1 // forces Transfer-Encoding: chunked
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST /echo chunked: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"bytes":5`) {
		t.Errorf("chunked /echo = %s", b)
	}
}

func TestBoomAnswers500OnTheSameConnection(t *testing.T) {
	_, base := startServer(t, BenchApp(), DefaultOptions())
	c := &http.Client{Timeout: 10 * time.Second}

	// net/http on its own would log the panic and drop the connection with no
	// response at all; Server.serveHTTP is what turns it into a 500, and the
	// request after it proves the connection survived.
	if status, _ := get(t, c, base+"/boom"); status != 500 {
		t.Errorf("GET /boom = %d, want 500", status)
	}
	if status, body := get(t, c, base+"/"); status != 200 || body != "Hello, world!" {
		t.Errorf("after /boom: GET / = %d %q", status, body)
	}
}

func TestStatsReportsTheServersOwnCounters(t *testing.T) {
	srv, base := startServer(t, BenchApp(), DefaultOptions())
	c := &http.Client{Timeout: 10 * time.Second}

	_, body := get(t, c, base+"/stats")
	var got struct {
		Goroutines      int   `json:"goroutines"`
		NativeThreads   int   `json:"native_threads"`
		GOMAXPROCS      int   `json:"gomaxprocs"`
		LiveConnections int64 `json:"live_connections"`
		Accepted        int64 `json:"accepted"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if got.LiveConnections < 1 {
		t.Errorf("live_connections = %d while a request is being served", got.LiveConnections)
	}
	if got.Accepted < 1 || uint64(got.Accepted) != srv.AcceptedConnections() {
		t.Errorf("accepted = %d, server says %d", got.Accepted, srv.AcceptedConnections())
	}
	if got.Goroutines < 1 || got.NativeThreads < 1 || got.GOMAXPROCS < 1 {
		t.Errorf("implausible /stats: %+v", got)
	}
}

func TestGCStatAndTrimReportTheProcess(t *testing.T) {
	_, base := startServer(t, BenchApp(), DefaultOptions())
	c := &http.Client{Timeout: 10 * time.Second}

	for _, path := range []string{"/gcstat", "/trim"} {
		_, body := get(t, c, base+path)
		var got map[string]any
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("%s: decode %q: %v", path, body, err)
		}
		if len(got) == 0 {
			t.Errorf("%s returned no fields", path)
		}
	}

	_, body := get(t, c, base+"/trim")
	var trim struct {
		RSSAfterGC   int `json:"rss_after_gc_mb"`
		RSSAfterTrim int `json:"rss_after_trim_mb"`
	}
	json.Unmarshal([]byte(body), &trim)
	if trim.RSSAfterGC <= 0 || trim.RSSAfterTrim <= 0 {
		t.Errorf("/trim reported no RSS: %s (is /proc readable?)", body)
	}
}

// One goroutine per connection means a keep-alive connection is accepted once
// however many requests go down it.
func TestKeepAliveReusesOneConnection(t *testing.T) {
	srv, base := startServer(t, BenchApp(), DefaultOptions())
	c := &http.Client{Timeout: 10 * time.Second}
	defer c.CloseIdleConnections()

	for i := 0; i < 5; i++ {
		if status, _ := get(t, c, base+"/"); status != 200 {
			t.Fatalf("request %d: status %d", i, status)
		}
	}
	if n := srv.AcceptedConnections(); n != 1 {
		t.Errorf("accepted %d connections for 5 keep-alive requests, want 1", n)
	}
	if n := srv.LiveConnections(); n != 1 {
		t.Errorf("live = %d after 5 requests on one connection, want 1", n)
	}
}

func TestLiveConnectionsFallsToZeroWhenPeersLeave(t *testing.T) {
	srv, base := startServer(t, BenchApp(), DefaultOptions())
	c := &http.Client{Timeout: 10 * time.Second}
	get(t, c, base+"/")
	c.CloseIdleConnections()

	deadline := time.Now().Add(3 * time.Second)
	for srv.LiveConnections() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := srv.LiveConnections(); n != 0 {
		t.Errorf("live = %d after the client went away, want 0", n)
	}
}

// The cap leaves the excess peer in the kernel's accept queue: its connect(2)
// completes, and nothing is read from it until a slot frees. That is the
// backpressure Punicorn gets by not selecting on the listener.
func TestMaxConnectionsQueuesRatherThanRefuses(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxConnections = 1
	srv, base := startServer(t, BenchApp(), opts)
	addr := base[len("http://"):]

	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer first.Close()
	if got := rawGet(t, first, "/"); !strings.Contains(got, "Hello, world!") {
		t.Fatalf("first connection: %q", got)
	}
	if n := srv.LiveConnections(); n != 1 {
		t.Fatalf("live = %d, want 1", n)
	}

	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial second: %v", err)
	}
	defer second.Close()
	fmt.Fprintf(second, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	second.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := bufio.NewReader(second).ReadString('\n'); err == nil {
		t.Fatal("second connection was served while the cap was reached")
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("second connection failed with %v, want a read timeout", err)
	}

	// Free the slot; the queued peer is picked up and answered.
	first.Close()
	second.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(second).ReadString('\n')
	if err != nil {
		t.Fatalf("queued connection never served: %v", err)
	}
	if !strings.HasPrefix(line, "HTTP/1.1 200") {
		t.Errorf("queued connection got %q", line)
	}
}

func rawGet(t *testing.T, c net.Conn, path string) string {
	t.Helper()
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(buf[:n])
}

// Per-connection state is what a handler gets instead of Ractor[:key]: two
// requests on one connection see the same store, a second connection sees its
// own.
func TestConnStateIsPerConnection(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := ConnStateFrom(r.Context())
		if st == nil {
			http.Error(w, "no conn state", 500)
			return
		}
		st.mu.Lock()
		st.ballast = append(st.ballast, "x")
		n := len(st.ballast)
		st.mu.Unlock()
		fmt.Fprint(w, n)
	})
	_, base := startServer(t, h, DefaultOptions())

	c := &http.Client{Timeout: 5 * time.Second}
	for i := 1; i <= 3; i++ {
		if _, body := get(t, c, base+"/"); body != fmt.Sprint(i) {
			t.Fatalf("request %d on the same connection saw %q", i, body)
		}
	}
	// A second client is a second connection, and starts from nothing.
	other := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
	if _, body := get(t, other, base+"/"); body != "1" {
		t.Errorf("a fresh connection saw %q, want 1", body)
	}
	other.CloseIdleConnections()
}

// Shutdown finishes an in-flight response rather than cutting it off, and stops
// accepting immediately -- Punicorn#drain's contract.
func TestStopFinishesInFlightRequests(t *testing.T) {
	released := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-released
		io.WriteString(w, "finished")
	})
	srv, base := startServer(t, h, DefaultOptions())

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(base + "/")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		done <- result{body: string(b), err: err}
	}()

	<-started
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Stop() }()

	// Stop must not return while the handler is still running.
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned (%v) with a request in flight", err)
	case <-time.After(200 * time.Millisecond):
	}

	// The listener is closed by now, so a new peer is refused.
	if c, err := net.DialTimeout("tcp", base[len("http://"):], time.Second); err == nil {
		c.Close()
		t.Error("connected after Stop closed the listener")
	}

	close(released)
	if err := <-stopped; err != nil {
		t.Errorf("Stop: %v", err)
	}
	res := <-done
	if res.err != nil || res.body != "finished" {
		t.Errorf("in-flight request = %q, %v; want it to complete", res.body, res.err)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	srv, _ := startServer(t, BenchApp(), DefaultOptions())
	for i := 0; i < 3; i++ {
		if err := srv.Stop(); err != nil {
			t.Fatalf("Stop %d: %v", i, err)
		}
	}
}

// The counterpart to config.ru's /ensure: a handler that returns normally runs
// its cleanup. That a handler killed by shutdown does *not* is the finding the
// endpoint exists for, and is not asserted here -- it is the same absence on
// both servers and there is nothing to assert it against.
func TestEnsureRunsDeferredCleanup(t *testing.T) {
	_, base := startServer(t, BenchApp(), DefaultOptions())
	marker := filepath.Join(t.TempDir(), "ensure-marker")

	c := &http.Client{Timeout: 5 * time.Second}
	if status, body := get(t, c, base+"/ensure?s=0.01&file="+marker); status != 200 || body != "finished normally" {
		t.Fatalf("/ensure = %d %q", status, body)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if !strings.HasPrefix(string(b), "defer ran at ") {
		t.Errorf("marker = %q", b)
	}
}

// -date=false takes Go's Date header off the wire so that a response to `/` is
// the same size as Puma's, which is the only reason the flag exists.
func TestDateHeaderCanBeSuppressed(t *testing.T) {
	for _, sendDate := range []bool{true, false} {
		opts := DefaultOptions()
		opts.SendDate = sendDate
		_, base := startServer(t, BenchApp(), opts)

		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(base + "/")
		if err != nil {
			t.Fatalf("SendDate=%v: %v", sendDate, err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Date") != ""; got != sendDate {
			t.Errorf("SendDate=%v: Date header present = %v", sendDate, got)
		}
	}
}

func TestRunWithoutListenIsAnError(t *testing.T) {
	srv := New(BenchApp(), DefaultOptions())
	if err := srv.Run(); err == nil {
		t.Error("Run with no listener returned nil")
	}
}

// A sanity check that concurrent connections really are served concurrently:
// N requests that each sleep take about one sleep, not N of them.
func TestConcurrentConnectionsDoNotQueue(t *testing.T) {
	const n = 16
	_, base := startServer(t, BenchApp(), DefaultOptions())

	var wg sync.WaitGroup
	start := time.Now()
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A Transport each, so each request is its own connection.
			c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{}}
			defer c.CloseIdleConnections()
			resp, err := c.Get(base + "/sleep?s=0.2")
			if err != nil {
				errs <- err
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent request: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("%d concurrent 200ms sleeps took %v; they are not concurrent", n, elapsed)
	}
}
