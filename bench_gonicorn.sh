#!/bin/sh
# The Go control for punions' bench.sh: same load, same cores, same endpoint,
# one goroutine per connection where punicorn puts a Ractor. Run the Ruby side
# of the pair with $PUNIONS/bench.sh, minutes apart on an otherwise idle box --
# the two numbers are only comparable if the box is.
cd "$(dirname "$0")" || exit 1

# Where the punions checkout lives: punicorn, the Ractor-per-connection server
# this is a control for, and examples/config.ru, the app app.go translates.
# Nothing here needs it to exist; it is passed to gonicorn for command-line
# parity with punicorn, which takes the rackup file as a positional argument.
PUNIONS=${PUNIONS:-/home/luke/workspace/punions}

PORT=${PORT:-9333}
# bench.sh hits `/` -- the endpoint with no application in it, where what is
# measured is the server. Override to compare another one:
#   ENDPOINT='/cpu?n=20000' ./bench_gonicorn.sh
ENDPOINT=${ENDPOINT:-/}

# Build first -- bin/ is gitignored, and a stale binary would be measured
# without saying so.
go build -o bin/gonicorn ./cmd/gonicorn || exit 1

# `-date=false` takes Go's Date header off the wire, which Puma does not send;
# add it when comparing bytes rather than requests.
#GONICORN_FLAGS=-date=false

RACKUP=
[ -f "$PUNIONS/examples/config.ru" ] && RACKUP="$PUNIONS/examples/config.ru"

( # bench.sh sleeps 6 because that is roughly what Ruby takes to boot punicorn.
  # A Go binary is listening in milliseconds, so waiting for the listener rather
  # than for the clock keeps the two runs' measured windows the same length
  # instead of handing this one five extra idle seconds.
  i=0
  while [ $i -lt 100 ]; do
    ss -ltn "sport = :$PORT" | grep -q ":$PORT" && break
    sleep 0.1
    i=$((i + 1))
  done
  taskset -c 8-15 h2load --h1 --duration 10 -c 64 -t 8 http://127.0.0.1:$PORT$ENDPOINT
  kill "$(ss -lptn "sport = :$PORT" | grep -oP 'pid=\K[0-9]+' | head -1)"
) &

# GOMAXPROCS follows the affinity mask, so taskset -c 0-7 gives Go the same
# eight cores punicorn gets and the load generator keeps 8-15 to itself.
taskset -c 0-7 bin/gonicorn $GONICORN_FLAGS -q -b 127.0.0.1 -p $PORT $RACKUP
