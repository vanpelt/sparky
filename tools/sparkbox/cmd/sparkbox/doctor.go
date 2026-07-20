package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/hostsetup"
)

// doctor runs the preflight/health battery against this host and exits non-zero
// if any hard check fails. Its flags mirror the subset of `serve` flags that
// determine where artifacts and keys live, so it diagnoses the exact layout the
// service uses.
func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfg := hostsetup.DefaultConfig()
	root := fs.String("root", cfg.Root, "sparkbox home directory")
	stateDir := fs.String("state-dir", "", "state dir (default <root>/data/state)")
	keyDir := fs.String("key-dir", "", "fleet key dir (default: state dir)")
	kernel := fs.String("kernel", "", "guest vmlinux path (default <root>/vmlinux)")
	imageDir := fs.String("image-dir", "", "rootfs template dir (default <root>/data/images)")
	defaultImage := fs.String("default-image", cfg.DefaultImage, "rootfs template basename")
	users := fs.String("users", "", "users.conf path (default <root>/users.conf)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sparkbox doctor [flags]\n\nReports whether this host is ready to run sparkbox.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg = applyPaths(cfg, *root, *stateDir, *keyDir, *kernel, *imageDir, *defaultImage, *users)

	results := hostsetup.RunChecks(hostsetup.System(), cfg, hostsetup.DefaultChecks())
	fmt.Println("sparkbox doctor —", cfg.Root)
	hostsetup.PrintResults(os.Stdout, results)
	if hostsetup.AnyFail(results) {
		return errors.New("one or more checks FAILED (see above)")
	}
	return nil
}

// applyPaths layers explicit path flags over the root-derived defaults, so
// passing only --root shifts the whole layout while individual flags still win.
func applyPaths(cfg hostsetup.Config, root, stateDir, keyDir, kernel, imageDir, defaultImage, users string) hostsetup.Config {
	if root != "" && root != cfg.Root {
		cfg = hostsetup.DefaultConfig()
		cfg.Root = root
		d := hostsetup.DefaultConfig()
		// Re-derive the root-relative paths for the new root.
		cfg.StateDir = replaceRoot(d.StateDir, d.Root, root)
		cfg.ImageDir = replaceRoot(d.ImageDir, d.Root, root)
		cfg.KernelPath = replaceRoot(d.KernelPath, d.Root, root)
		cfg.UsersPath = replaceRoot(d.UsersPath, d.Root, root)
	}
	if stateDir != "" {
		cfg.StateDir = stateDir
	}
	if keyDir != "" {
		cfg.KeyDir = keyDir
	}
	if kernel != "" {
		cfg.KernelPath = kernel
	}
	if imageDir != "" {
		cfg.ImageDir = imageDir
	}
	if defaultImage != "" {
		cfg.DefaultImage = defaultImage
	}
	if users != "" {
		cfg.UsersPath = users
	}
	return cfg
}

func replaceRoot(path, oldRoot, newRoot string) string {
	if len(path) >= len(oldRoot) && path[:len(oldRoot)] == oldRoot {
		return newRoot + path[len(oldRoot):]
	}
	return path
}
