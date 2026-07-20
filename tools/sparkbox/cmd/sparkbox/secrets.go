package main

import (
	"context"
	"flag"
	"os"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/bootsecrets"
)

// fetchSecrets implements `sparkbox fetch-secrets`: the boot-time step that
// pulls fleet secrets from Scaleway Secret Manager into tmpfs, replacing the
// plaintext copies that used to ride in cloud-init user-data. The real work is
// in internal/bootsecrets; this is the flag surface.
func fetchSecrets(args []string) error {
	fs := flag.NewFlagSet("fetch-secrets", flag.ExitOnError)
	var (
		region    = fs.String("region", envOr("SCW_DEFAULT_REGION", "fr-par"), "Scaleway region")
		projectID = fs.String("project-id", os.Getenv("SCW_DEFAULT_PROJECT_ID"), "Scaleway project ID")
		path      = fs.String("path", "/sparkbox/fleet", "Secret Manager folder holding the fleet secrets")
		keyDir    = fs.String("key-dir", "/run/sparkbox/keys", "directory to write fleet key PEMs (put it on tmpfs)")
		envOut    = fs.String("env-out", "/run/sparkbox/secrets.env", "EnvironmentFile to write secret env vars into (put it on tmpfs)")
		baseURL   = fs.String("base-url", "", "override the Scaleway API base URL (tests only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return bootsecrets.Fetch(context.Background(), bootsecrets.Config{
		BaseURL:   *baseURL,
		Region:    *region,
		ProjectID: *projectID,
		Token:     os.Getenv("SCW_SECRET_KEY"),
		Path:      *path,
		KeyDir:    *keyDir,
		EnvOut:    *envOut,
		Log:       os.Stderr,
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
