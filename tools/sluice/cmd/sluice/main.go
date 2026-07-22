// Command sluice is a DNS allowlist gateway + eBPF egress meter for sparkbox
// VMs. It answers guest DNS queries only for allowlisted domains, records the
// resolved addresses, and runs a TC/eBPF program on each guest tap that meters
// per-domain bandwidth and (optionally) drops egress to any address the
// allowlist never produced.
//
//	sluice run   --allowlist FILE [flags]   run the gateway + meter
//	sluice check --allowlist FILE NAME...   test names against the allowlist
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/vanpelt/sparky/tools/sluice/internal/allowlist"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(runCmd(os.Args[2:]))
	case "check":
		os.Exit(checkCmd(os.Args[2:]))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "sluice: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `sluice — DNS allowlist gateway + eBPF egress meter for sandbox VMs

usage:
  sluice run   --allowlist FILE [flags]     run the gateway and tap meter
  sluice check --allowlist FILE NAME...     test names against the allowlist

run 'sluice run -h' for flags.
`)
}

// checkCmd resolves names against an allowlist and prints the verdict — handy
// for sanity-checking a policy file before deploying it.
func checkCmd(args []string) int {
	fs := newFlagSet("check")
	path := fs.String("allowlist", "", "allowlist file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" || fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: sluice check --allowlist FILE NAME...")
		return 2
	}
	list, err := allowlist.Load(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sluice: %v\n", err)
		return 1
	}
	rc := 0
	for _, name := range fs.Args() {
		ok, pat := list.Allowed(name)
		if ok {
			fmt.Printf("ALLOW  %-40s (matched %s)\n", name, pat)
		} else {
			fmt.Printf("DENY   %-40s\n", name)
			rc = 1
		}
	}
	return rc
}

func newLogger(format string) *slog.Logger {
	switch strings.ToLower(format) {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	default:
		return slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
}

// notifyContext returns a context cancelled on SIGINT/SIGTERM.
func notifyContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}
