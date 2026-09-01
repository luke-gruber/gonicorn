package gonicorn

// Facts about this process that the runtime does not expose, read the way
// examples/config.ru reads them so that the two servers' /stats and /trim
// answers are the same measurement and not two similar ones.

import (
	"net/http"
	"os"
	"strconv"
	"strings"
)

// nativeThreadCount is the Threads: line of /proc/self/status -- OS threads
// alive now. runtime/pprof's threadcreate profile counts threads ever created,
// which is a different and less useful number here.
func nativeThreadCount() int {
	return statusField("Threads")
}

// rssMB is VmRSS in MB, matching config.ru's rss lambda.
func rssMB() int { return statusFieldMB("VmRSS") }

func statusFieldMB(name string) int { return statusField(name) / 1024 }

// statusField returns the field's value in the units /proc/self/status states
// (kB for the Vm* fields, a plain count for Threads), or -1 if it cannot be
// read -- the same -1 config.ru's rescue produces, so a broken reading is
// visible rather than reported as zero.
func statusField(name string) int {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return -1
	}
	prefix := name + ":"
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		f := strings.Fields(line[len(prefix):])
		if len(f) == 0 {
			return -1
		}
		v, err := strconv.Atoi(f[0])
		if err != nil {
			return -1
		}
		return v
	}
	return -1
}

// The app reaches the server's counters through the connection it was asked
// on, which is the only handle a handler has. Both return -1 when there is no
// server behind the request (an httptest handler, say), matching the -1 the
// Ruby app reports when a statistic is unavailable.
func liveConnections(r *http.Request) int64 {
	if st := ConnStateFrom(r.Context()); st != nil && st.srv != nil {
		return st.srv.LiveConnections()
	}
	return -1
}

func acceptedConnections(r *http.Request) int64 {
	if st := ConnStateFrom(r.Context()); st != nil && st.srv != nil {
		return int64(st.srv.AcceptedConnections())
	}
	return -1
}
