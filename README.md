# Background

This is to compare Ruby 4.1.0dev Ractor speeds with Go goroutines in a networked context on a Linux machine.
First, git clone [punions](https://github.com/ko1/punions). Run this benchmark in the punions repository:

```sh
PORT=9333
( sleep 6
  taskset -c 8-15 h2load --h1 --duration 10 -c 64 -t 8 http://127.0.0.1:$PORT/
  kill "$(ss -lptn "sport = :$PORT" | grep -oP 'pid=\K[0-9]+' | head -1)"
) &

env RUBY_MN_THREADS=1 \
    GEM_HOME=$HOME/.gem/ruby/4.1.0 \
    GEM_PATH=$HOME/.gem/ruby/4.1.0:$HOME/.rubies/ruby_master/lib/ruby/gems/4.1.0+4 \
    taskset -c 0-7 $HOME/.rubies/ruby_master/bin/ruby \
    punicorn/bin/punicorn -q -b 127.0.0.1 -p $PORT examples/config.ru
```

Replace `GEM_HOME` and `GEM_PATH` with your correct paths.

NOTE: you will need `h2load` for this.

# Gonicorn

Punicorn's shape in Go: **every connection gets its own goroutine**.

```text
Punicorn                                Gonicorn

accept ──▶ Ractor.new per connection    accept ──▶ go c.serve() per connection
             │                                       │
        one GVL each                          no GVL to have
             │                                       │
   all cores, one process                  all cores, one process
```

It does not run Rack, and it is not a server to deploy. It is a **control** for
[punicorn](https://github.com/ko1/punions), the Ractor-per-connection Rack
server it copies the shape of — that repo is `$PUNIONS` throughout this one.

Punicorn's claim is about a shape — one unit of concurrency per connection, no
thread pool and no reactor — and the only numbers for that shape are Ruby
numbers, where a Ractor costs what a Ractor costs. Running the identical shape
on a runtime that was built for it separates the two questions: what the shape
is worth, and what Ruby charges to express it.

The goroutine per connection is not written here. `net/http`'s `Server.Serve`
accepts and does `go c.serve(ctx)`, and that goroutine reads, dispatches and
writes each request on the connection in turn — Punicorn's arrangement exactly,
arrived at independently and shipped in the standard library. Re-implementing it
by hand would only be interesting if the hand-rolled one won, and the comparison
worth having is against Go's standard answer.

## The numbers

Same box, same method, minutes apart, 2026-09-01. Server pinned to cores 0–7,
`h2load --h1 --duration 10 -c 64 -t 8` pinned to 8–15, endpoint `/`, which is
the endpoint with no application in it. Three paired runs; each row is that
server's median run whole, and the req/s spread across its three runs was 1.1%
for Gonicorn and 0.6% for Punicorn.

| `/` | req/s | TTFB p50 | p95 | p99 | server CPU | peak RSS |
|---|---|---|---|---|---|---|
| **Gonicorn** (goroutine/conn) | **942 518** | 287 µs | 443 µs | 547 µs | 6.78 of 8 cores | **24 MB** |
| Punicorn (Ractor/conn) | 334 215 | 505 µs | 1.69 ms | 3.46 ms | 7.13 of 8 cores | 2 324 MB |

**2.8× the requests for slightly less CPU, and a hundredth of the memory.**

Four things that table is not:

- **It is not a language benchmark.** `/` is the one endpoint whose application
  is a constant string, chosen so that what is left is accept, parse, build the
  request, write the response. On `/cpu` or `/alloc` the gap would be Go against
  the Ruby interpreter, which is a different and much less interesting question;
  `ENDPOINT=` in `bench_gonicorn.sh` will run those if you want them, but they
  do not say anything about the shape.
- **It is not like-for-like on implementation maturity.** `net/http` has had
  well over a decade of people making it faster. Punicorn is 1 810 lines on top
  of Puma's parser. Some unknown share of 2.8× is that and not the runtime.
- **Neither server saturated its eight cores** (6.8 and 7.1). Loopback softirq
  work is charged to the kernel rather than to either process, so the missing
  core-and-a-bit is mostly there. h2load itself used about 3 of its 8 cores, so
  the load generator was not the limit for either row.
- **The RSS column is not measuring the same thing twice.** Punicorn's resident
  memory rises with cumulative requests and settles at a plateau far above its
  live set — the shape is diagnosed at length in punions' `punicorn/README.md`
  and it is a heap per Ractor plus a glibc allocator that has not reached steady
  state. Go has one heap for the process however many goroutines exist, so there
  is no per-unit page floor to pay 64 times. That is the memory half of the
  Ractor question, and it is the largest number in the table.

Wire bytes: Go sends a `Date` header and Puma does not, so a response to `/` is
114 bytes here against Punicorn's 78. `-date=false` removes it and makes the two
identical at 78 bytes. It was worth about 2% in a single run (964 260 req/s),
against a run-to-run spread of 1.1% — small, and not nothing.

## Running it

```sh
./bench_gonicorn.sh                         # build, load for 10s, stop
ENDPOINT='/cpu?n=20000' ./bench_gonicorn.sh
PUNIONS=~/src/punions ./bench_gonicorn.sh   # where the punions checkout lives

go build -o bin/gonicorn ./cmd/gonicorn
bin/gonicorn -b 127.0.0.1 -p 9333           # same flags as punicorn/bin/punicorn
go test -race ./...
```

`bench_gonicorn.sh` is punions' `bench.sh` with the server swapped, and the
other half of the pair is still `$PUNIONS/bench.sh` — run the two minutes apart
on an otherwise idle box, because that is the only thing that makes the numbers
comparable. It differs from `bench.sh` in two places and both are noted in the
file: it builds first, and it waits for the listener instead of sleeping six
seconds, because six seconds is how long Ruby takes to boot and Go is listening
before the shell has finished the line.

`PUNIONS` defaults to `/home/luke/workspace/punions` and is only used to hand
gonicorn the rackup path for command-line parity; nothing here needs the
checkout to exist.

The flags are punicorn's — `-b`, `-p`, `-c`, `-q` — so the two command lines are
interchangeable. A trailing `config.ru` is accepted and ignored with a warning,
because the app is compiled in and a silent difference between what the command
line names and what is served would be worse than a line of noise.

`GOMAXPROCS` follows the affinity mask, so `taskset -c 0-7` gives Go the same
eight cores punicorn gets, with no environment variable to remember.

## What is here

| | |
|---|---|
| `server.go` | the accept path: connection cap, live count, per-connection state, drain. |
| `app.go` | punions' `examples/config.ru`, endpoint for endpoint. |
| `proc.go` | `/proc/self/status` readers, so `/stats` and `/trim` answer with the same measurement config.ru uses. |
| `cmd/gonicorn/main.go` | flags, banner, signals. |
| `server_test.go` | the accept path against a real socket — `httptest` would replace the part worth testing. |
| `bench_gonicorn.sh` | the run that produced the table above. |

Standard library only. No `go.sum`, nothing to vendor, `go build` works offline.

## The app

`app.go` is punions' `examples/config.ru` translated, so that a run against either server
is the same question asked of two runtimes.

| path | same | different |
|---|---|---|
| `/` `/sleep` `/bytes` `/stream` | — | |
| `/cpu` | same loop, same n | no interpreter to measure; the accumulator is written to the response so the loop stays live |
| `/alloc` | same row count, one heap object per Ruby allocation | Go has one heap for the process, so this is not asking about a heap per unit of concurrency |
| `/alloc?ballast=1` | per **connection**, as `Ractor[:ballast]` is | hangs off `connState` rather than the runtime |
| `/echo` | same body, same SHA-1 | net/http streams the body; there is no `MAX_BODY` and no Tempfile spill to exercise |
| `/boom` | 500 on the same connection | net/http alone would log the panic and drop the connection; `Server.serveHTTP` is what answers |
| `/ensure` | cleanup on the normal path | and, like Punicorn, **no** cleanup when shutdown outlasts the handler — see below |
| `/gc` `/gcstat` `/trim` | same questions | process-wide, not per unit of concurrency |
| `/stats` | goroutines / native threads | Punicorn reports Ractors / native threads / Ruby threads |
| `/lit` | — | left out: Go has no chilled string literals and no magic comment to lose |

## What the shape costs in each language

The three places the port was not a transcription are the three places the two
runtimes actually differ.

**Sharing the application.** Punicorn's app must be deeply immutable before a
connection may call it — `Ractor.shareable_lambda`, no gem with mutable module
state, no Rails — and that requirement is most of what makes Punicorn an
experiment rather than a server you can point at an application. In Go a handler
shared by 64 connections is the ordinary case and nothing needed arranging. The
absence of work here is the result.

**Per-connection state.** A Ractor gets isolation from the runtime: state
reached as `Ractor[:key]` cannot be seen by another connection, by construction.
Go shares everything by default, so the equivalent has to be handed to the
handler — `ConnContext` attaches a `connState` to each connection and the
handler reaches it through the request context. Same guarantee, opt-in rather
than enforced, and a `sync.Mutex` where the Ractor needed none.

**Stopping.** Punicorn's drain needs 60 lines and an inverted ownership rule for
the socket — the server keeps every accepted descriptor and the connection Ractor
borrows it — because there is no `Ractor#kill` and no way to signal one, so the
only way to wake a parked connection is `shutdown(2)` on its socket. `net/http`
tracks its own idle connections and `Shutdown(ctx)` is the whole story.

But **past the deadline the two are in exactly the same position**: Go has no
goroutine kill either, closing the connection under a running handler does not
unwind it, and a `defer` is lost the same way an `ensure` is. That is the
finding `/ensure` exists to record, and Go does not improve on it. Puma is the
odd one out — a Thread can be raised into and killed, and both run `ensure`.

## Known gaps

- The WebSocket and Action Cable layer is not ported. The axis where the
  one-per-connection shape wins in Ruby is *held* connections, and that half of
  the comparison is missing.
- No `max_keep_alive`: `net/http` does not count requests on a connection, and
  a counter on the hot path to enforce a limit the benchmark never reaches would
  be measured rather than the server.
- The listen backlog is whatever Go asks for (`/proc/sys/net/core/somaxconn`);
  Punicorn asks for 1024. Nothing at `-c 64` comes near either.
- Only `/` has been measured. The other endpoints work and are tested; no run of
  them is recorded.
