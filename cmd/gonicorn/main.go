// Command gonicorn serves examples/config.ru's endpoints with one goroutine per
// connection, taking the same flags as punicorn/bin/punicorn so that the two
// can be swapped in a benchmark script without editing anything else.
package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/luke-gruber/gonicorn"
)

func main() {
	opts := gonicorn.DefaultOptions()
	var showHelp bool

	fs := flag.NewFlagSet("gonicorn", flag.ContinueOnError)
	fs.StringVar(&opts.Host, "b", opts.Host, "address to bind")
	fs.StringVar(&opts.Host, "bind", opts.Host, "address to bind")
	fs.IntVar(&opts.Port, "p", opts.Port, "port to bind")
	fs.IntVar(&opts.Port, "port", opts.Port, "port to bind")
	fs.IntVar(&opts.MaxConnections, "c", 0,
		"cap concurrent connections; beyond it peers wait in the kernel backlog (default: no cap)")
	fs.IntVar(&opts.MaxConnections, "max-connections", 0, "same as -c")
	fs.BoolVar(&opts.Quiet, "q", false, "do not print the banner")
	fs.BoolVar(&opts.Quiet, "quiet", false, "do not print the banner")
	fs.BoolVar(&opts.SendDate, "date", true,
		"send a Date header (Puma does not; -date=false makes the two servers' responses the same size)")
	fs.BoolVar(&showHelp, "h", false, "show this help")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gonicorn [options] [config.ru]")
		fs.PrintDefaults()
	}

	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(2)
	}
	if showHelp {
		fs.Usage()
		return
	}

	// punicorn takes the rackup file as a positional argument and bench.sh
	// passes it. Accepting and ignoring it keeps the two command lines
	// interchangeable; the app is compiled in, and saying so once is better
	// than a silent difference between what the command line claims to serve
	// and what is served.
	if rackup := fs.Arg(0); rackup != "" && !opts.Quiet {
		fmt.Fprintf(os.Stderr, "gonicorn: ignoring %s; the benchmark app is compiled in (gonicorn/app.go)\n", rackup)
	}

	srv := gonicorn.New(gonicorn.BenchApp(), opts)
	if err := srv.Listen(); err != nil {
		fmt.Fprintln(os.Stderr, "gonicorn:", err)
		os.Exit(1)
	}
	if !opts.Quiet {
		fmt.Println(srv.Banner())
	}

	// SIGTERM is how the benchmark script stops the server, so a clean drain on
	// it is not a nicety: a run that ends with connections cut mid-response
	// reports errors that belong to the shutdown and not to the server.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		if err := srv.Stop(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "gonicorn: shutdown:", err)
		}
	}()

	if err := srv.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "gonicorn:", err)
		os.Exit(1)
	}
}
