package gonicorn

// The Go translation of examples/config.ru, endpoint for endpoint, so that a
// run against Gonicorn and a run against Punicorn are asking the same question
// of two runtimes rather than two different questions.
//
// It is compiled in rather than loaded from a file. That is the one structural
// difference from the Ruby side and it is not a small one: Punicorn's benchmark
// app is a Ractor.shareable_lambda that has to be provably immutable before a
// connection may call it, and the whole discipline that requirement imposes --
// no gem with mutable module state, no Rails -- is the price Ruby charges for
// the shape. Go charges nothing for it, because a handler shared between
// goroutines is the normal case. Nothing here needed arranging to be callable
// from 64 connections at once, and that absence is a result, not an omission.
//
// Two endpoints from config.ru have no counterpart and are left out:
//
//   /lit    asks whether the app's own string literals are frozen. Go has no
//           chilled strings and no magic comment that can be silently lost, so
//           there is nothing to ask.
//   /trim's Fiddle path  is here as debug.FreeOSMemory, but the question it
//           answers -- is resident memory the Ruby heap's or the allocator's --
//           splits differently in Go, where the runtime is the allocator.
//           See the endpoint's comment.

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// BenchApp is examples/config.ru.
func BenchApp() http.Handler { return http.HandlerFunc(benchApp) }

func benchApp(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		// No work at all: what remains is the server -- accept, parse, build the
		// request, write the response. This is the row Punicorn wins in Ruby.
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "Hello, world!")

	case "/sleep":
		// Nearly all I/O wait. The interesting number is how many of these can
		// be in flight at once, which is a question about what one unit of
		// concurrency costs while it is doing nothing.
		time.Sleep(time.Duration(queryFloat(r, "s", 0.1) * float64(time.Second)))
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "slept")

	case "/cpu":
		// The GVL probe on the Ruby side: 20 000 integer multiplies and about
		// three objects per hundred requests, so nothing reaches the collector
		// and what is measured is the interpreter and how many of it can run.
		// Go has no interpreter and no global lock, so this endpoint measures
		// how well eight cores are kept fed and little else -- which is exactly
		// what makes it the right thing to put next to Punicorn's number.
		//
		// The accumulator is written into the response so the loop is live. Go
		// does not turn a sum of squares into a closed form, and if some later
		// compiler does, this endpoint stops measuring anything -- check the
		// req/s against /bytes?n=1 before believing a sudden win here.
		n := queryInt(r, "n", 20000)
		acc := 0
		for i := 0; i < n; i++ {
			acc += i * i
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, strconv.Itoa(acc))

	case "/bytes":
		// The response write path.
		n := queryInt(r, "n", 1024)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte(strings.Repeat("x", n)))

	case "/stream":
		// Chunked responses. Ruby returns an Enumerator body and Puma drives it;
		// here the handler writes and flushes, which is the same shape from the
		// connection's point of view -- the goroutine stays on this connection
		// for the whole body rather than handing it to anyone.
		n := queryInt(r, "n", 5)
		w.Header().Set("Content-Type", "text/plain")
		flusher, _ := w.(http.Flusher)
		for i := 0; i < n; i++ {
			fmt.Fprintf(w, "chunk %d\n", i)
			if flusher != nil {
				flusher.Flush()
			}
		}

	case "/echo":
		// Request bodies. Punicorn's version exercises StringIO under Puma's
		// MAX_BODY, a Tempfile above it, and the chunked decoder; net/http
		// streams the body instead of buffering it to either, so what this
		// tests here is the read path and nothing about spill-to-disk.
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sum := sha1.Sum(data)
		writeJSON(w, map[string]any{"bytes": len(data), "sha1": hex.EncodeToString(sum[:])})

	case "/alloc":
		// The counterpart to /cpu: the same order of CPU time spent building
		// objects instead of in arithmetic, so it reaches the collector. On the
		// Ruby side this is the endpoint where a heap per Ractor differs from a
		// shared heap. Go has one heap for the process however many goroutines
		// there are, so the pair /cpu and /alloc measures a different thing here
		// -- not "does isolation cost GC" but "what does the same allocation
		// volume cost a collector that was designed for it".
		//
		// The row is a pointer so each one is its own heap object: Ruby's Hash,
		// String and Array per row are three allocations, and a value struct in
		// a slice would have been one.
		if q := r.URL.Query(); q.Get("ballast") == "1" {
			// Ractor[:ballast] on the Ruby side: a large live set belonging to
			// this connection alone, the way a real application has one after
			// booting. Here it hangs off the connection's own state, so it is
			// per connection exactly as the Ractor-local is -- see connState.
			if st := ConnStateFrom(r.Context()); st != nil {
				st.mu.Lock()
				if st.ballast == nil {
					b := make([]string, 400000)
					for i := range b {
						b[i] = "ballast " + strconv.Itoa(i)
					}
					st.ballast = b
				}
				st.mu.Unlock()
			}
		}
		n := queryInt(r, "n", 2000)
		rows := make([]*allocRow, n)
		for i := range rows {
			rows[i] = &allocRow{ID: i, Name: "row " + strconv.Itoa(i), Tags: []string{"a", "b", "c"}}
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, strconv.Itoa(len(rows)))

	case "/ensure":
		// A handler with cleanup, which is where an application releases a
		// database connection or a lock.
		//
		// The Ruby endpoint exists to ask whether a Ractor that outlasts
		// shutdown runs its ensure block. It does not: there is no Ractor#kill,
		// so at shutdown_timeout the process leaves and the cleanup is lost.
		// Go is in the same position for the same reason -- a goroutine cannot
		// be killed from outside either, and closing the connection under a
		// handler does not unwind it -- so the marker file this writes is
		// absent here for exactly the reason it is absent there. Puma is the
		// odd one out: a Thread can be raised into and killed, and both run
		// ensure.
		q := r.URL.Query()
		marker := q.Get("file")
		if marker == "" {
			marker = "/tmp/claude-1000/ensure-marker"
		}
		secs := queryFloat(r, "s", 60)
		defer func() {
			os.WriteFile(marker, []byte(fmt.Sprintf("defer ran at %d\n", time.Now().UnixNano())), 0o644)
		}()
		if q.Get("spin") == "1" {
			// Burn CPU rather than sleep: a sleeping goroutine is parked and
			// says little, and the question is whether one that is not waiting
			// on anything can be stopped from outside. It cannot.
			deadline := time.Now().Add(time.Duration(secs * float64(time.Second)))
			acc := 0
			for time.Now().Before(deadline) {
				acc++
			}
			_ = acc
		} else {
			time.Sleep(time.Duration(secs * float64(time.Second)))
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "finished normally")

	case "/boom":
		// Raises on purpose. The 500 comes from Server.serveHTTP; net/http on
		// its own would log the panic and drop the connection with no response.
		panic("boom from the app")

	case "/trim":
		// RSS after a collection, and again after handing free pages back to the
		// OS. On the Ruby side the gap between the two is memory the Ruby heap
		// has released that glibc is still holding, and telling those apart is
		// the whole question when a peak-RSS column looks bad.
		//
		// Go splits the same question differently: the runtime is the allocator,
		// so there is no second party holding pages, and the gap here is only
		// the runtime's own scavenging delay -- it returns memory on its own
		// schedule and FreeOSMemory makes it do so now. A large gap means the
		// scavenger had not got to it yet, not that anything leaked.
		runtime.GC()
		before := rssMB()
		debug.FreeOSMemory()
		writeJSON(w, map[string]any{
			"hwm_mb":            statusFieldMB("VmHWM"),
			"rss_after_gc_mb":   before,
			"rss_after_trim_mb": rssMB(),
		})

	case "/gc":
		// Turn the collector off for a measurement window, as config.ru's /gc
		// does, for the same reason: running the same work with no collector at
		// all is the only way to see what remains when every collection cost is
		// removed at once.
		off := strings.Contains(r.URL.RawQuery, "off")
		if off {
			debug.SetGCPercent(-1)
		} else {
			debug.SetGCPercent(initialGCPercent)
		}
		w.Header().Set("Content-Type", "text/plain")
		if off {
			io.WriteString(w, "disabled")
		} else {
			io.WriteString(w, "enabled")
		}

	case "/gcstat":
		// The Ruby endpoint answers for the calling connection's own Ractor
		// heap, which is the whole reason it is asked over a keep-alive
		// connection that has just done work. There is no such thing here:
		// every goroutine allocates from one heap, so this reports the process
		// and the connection it was asked on is irrelevant. That difference is
		// the memory half of the Ractor question -- N heaps that each pay a
		// page floor, against one that does not.
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		writeJSON(w, map[string]any{
			"num_gc":            m.NumGC,
			"gc_pause_total_ms": float64(m.PauseTotalNs) / 1e6,
			"gc_cpu_fraction":   m.GCCPUFraction,
			"heap_alloc_mb":     m.HeapAlloc / 1024 / 1024,
			"heap_sys_mb":       m.HeapSys / 1024 / 1024,
			"heap_idle_mb":      m.HeapIdle / 1024 / 1024,
			"heap_released_mb":  m.HeapReleased / 1024 / 1024,
			"heap_objects":      m.HeapObjects,
			"mallocs":           m.Mallocs,
			"frees":             m.Frees,
			"next_gc_mb":        m.NextGC / 1024 / 1024,
			"rss_mb":            rssMB(),
		})

	case "/stats":
		// Punicorn reports Ractors, native threads and Ruby threads. The
		// counterparts: goroutines is what one-per-connection produces,
		// native threads is what the runtime multiplexes them onto, and the
		// ratio between those two columns is the difference between the M:N
		// scheduler PuMaNy bets on and the one Go ships.
		writeJSON(w, map[string]any{
			"goroutines":       runtime.NumGoroutine(),
			"native_threads":   nativeThreadCount(),
			"gomaxprocs":       runtime.GOMAXPROCS(0),
			"live_connections": liveConnections(r),
			"accepted":         acceptedConnections(r),
		})

	default:
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "not found")
	}
}

// The GOGC the process started with, so that /gc without `off` restores what
// was there rather than asserting 100 -- a run under GOGC=400 would otherwise
// come back from a measurement window quietly retuned.
var initialGCPercent = func() int {
	prev := debug.SetGCPercent(-1)
	debug.SetGCPercent(prev)
	return prev
}()

type allocRow struct {
	ID   int
	Name string
	Tags []string
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// Query values are read per branch, as config.ru's regexes are, so that `/`
// never pays for parsing a query string it does not have.
func queryInt(r *http.Request, key string, def int) int {
	if r.URL.RawQuery == "" {
		return def
	}
	if v, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil {
		return v
	}
	return def
}

func queryFloat(r *http.Request, key string, def float64) float64 {
	if r.URL.RawQuery == "" {
		return def
	}
	if v, err := strconv.ParseFloat(r.URL.Query().Get(key), 64); err == nil {
		return v
	}
	return def
}
