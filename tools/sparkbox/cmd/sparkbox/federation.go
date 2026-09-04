package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/federation"
)

// federationCmd is `sparkbox federation check <file>`: the same loader `serve`
// runs on --federation-config, as a command an operator can run BEFORE a
// deploy applies the file. The binary is the validator — a list it refuses is
// a gateway that will not start — so deploy.sh calls this first and refuses
// with the same words, while somebody is still watching.
//
// It prints the list as the guest will see it, which is also the quickest way
// to learn what a fleet actually federates with.
func federationCmd(args []string) error {
	fs := flag.NewFlagSet("federation", flag.ExitOnError)
	hivemindAudience := fs.String("hivemind-audience", defaultAudience,
		"audience of the built-in HiveMind entry, when no file is given")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: sparkbox federation check [FILE]")
		fmt.Fprintln(fs.Output(), "  Validate a --federation-config file and print the list guests will be served.")
		fmt.Fprintln(fs.Output(), "  With no FILE, print the built-in default.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 || rest[0] != "check" || len(rest) > 2 {
		fs.Usage()
		return errors.New("federation: expected `check [FILE]`")
	}
	path := ""
	if len(rest) == 2 {
		path = rest[1]
	}
	cfg, err := federation.Load(path, federation.Default(*hivemindAudience))
	if err != nil {
		return err
	}
	if len(cfg.Federators) == 0 {
		fmt.Fprintln(os.Stderr, "warning: the list names nobody; sandboxes will mint no assertion at all")
	}
	fmt.Printf("federators: %s\n", strings.Join(cfg.Names(), " "))
	fmt.Printf("audiences:  %s\n", strings.Join(cfg.Audiences(), " "))
	fmt.Print(cfg.Guest())
	return nil
}
