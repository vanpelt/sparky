package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/bootsecrets"
)

// fetchSecrets implements `sparkbox fetch-secrets`: the boot-time step that
// pulls fleet secrets into tmpfs, replacing the plaintext copies that used to
// ride in cloud-init user-data. The real work is in internal/bootsecrets; this
// is the flag surface and the choice of store.
func fetchSecrets(args []string) error {
	fs := flag.NewFlagSet("fetch-secrets", flag.ExitOnError)
	var (
		provider = fs.String("provider", envOr("SPARKBOX_SECRETS_PROVIDER", "scw"), "secret store: scw (Scaleway Secret Manager) | op (1Password)")
		keyDir   = fs.String("key-dir", "/run/sparkbox/keys", "directory to write fleet key PEMs (put it on tmpfs)")
		envOut   = fs.String("env-out", "/run/sparkbox/secrets.env", "EnvironmentFile to write secret env vars into (put it on tmpfs)")

		// Scaleway
		region    = fs.String("region", envOr("SCW_DEFAULT_REGION", "fr-par"), "Scaleway region (--provider scw)")
		projectID = fs.String("project-id", os.Getenv("SCW_DEFAULT_PROJECT_ID"), "Scaleway project ID (--provider scw)")
		path      = fs.String("path", "/sparkbox/fleet", "Secret Manager folder holding the fleet secrets (--provider scw)")
		baseURL   = fs.String("base-url", "", "override the Scaleway API base URL (tests only)")

		// 1Password
		opVault   = fs.String("op-vault", envOr("SPARKBOX_OP_VAULT", "Sparkbox"), "1Password vault holding the fleet secrets, one item per secret (--provider op)")
		opAccount = fs.String("op-account", os.Getenv("OP_ACCOUNT"), "1Password account for desktop-app auth (--provider op); leave empty on a host and set OP_SERVICE_ACCOUNT_TOKEN instead")
		opField   = fs.String("op-field", "", "item field holding each payload (--provider op; default \"password\")")
		opBin     = fs.String("op-bin", envOr("SPARKBOX_OP_BIN", "op"), "path to the 1Password CLI (--provider op)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	var (
		source bootsecrets.Source
		err    error
	)
	switch *provider {
	case "scw", "scaleway":
		source, err = bootsecrets.NewScalewaySource(bootsecrets.ScalewayConfig{
			BaseURL:   *baseURL,
			Region:    *region,
			ProjectID: *projectID,
			// Never an argv flag, so it can't leak into the process table.
			Token: os.Getenv("SCW_SECRET_KEY"),
			Path:  *path,
		})
	case "op", "1password", "onepassword":
		source, err = bootsecrets.NewOnePasswordSource(bootsecrets.OnePasswordConfig{
			Vault:   *opVault,
			Account: *opAccount,
			Field:   *opField,
			Bin:     *opBin,
			// Same reasoning as SCW_SECRET_KEY: environment, never argv.
			Token: os.Getenv("OP_SERVICE_ACCOUNT_TOKEN"),
		})
	default:
		return fmt.Errorf("unknown --provider %q (want scw or op)", *provider)
	}
	if err != nil {
		return err
	}

	return bootsecrets.Fetch(context.Background(), bootsecrets.Config{
		Source: source,
		KeyDir: *keyDir,
		EnvOut: *envOut,
		Log:    os.Stderr,
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
