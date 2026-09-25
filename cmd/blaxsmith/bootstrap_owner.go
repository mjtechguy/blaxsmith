package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/mjtechguy/blaxsmith/db"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"golang.org/x/term"
)

func bootstrapOwner(args []string) error {
	flags := flag.NewFlagSet("bootstrap-owner", flag.ContinueOnError)
	email := flags.String("email", "", "first owner's sign-in email")
	slug := flags.String("organization-slug", "", "organization URL slug")
	name := flags.String("organization-name", "", "organization display name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *email == "" || *slug == "" || *name == "" {
		return errors.New("bootstrap-owner requires --email, --organization-slug, and --organization-name")
	}
	dsn := os.Getenv("BLAXSMITH_DATABASE_URL")
	if dsn == "" {
		return errors.New("set BLAXSMITH_DATABASE_URL")
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return errors.New("bootstrap-owner requires an interactive terminal")
	}
	defer tty.Close()
	read := func(prompt string) ([]byte, error) {
		if _, err := fmt.Fprint(tty, prompt); err != nil {
			return nil, err
		}
		password, err := term.ReadPassword(int(tty.Fd()))
		fmt.Fprintln(tty)
		return password, err
	}
	password, err := read("New owner password: ")
	if err != nil {
		return err
	}
	defer clear(password)
	confirmation, err := read("Confirm password: ")
	if err != nil {
		return err
	}
	defer clear(confirmation)
	if !bytes.Equal(password, confirmation) {
		return errors.New("passwords do not match")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	pool, err := tenant.NewPool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("configure database: %w", err)
	}
	defer pool.Close()
	if _, err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	owner, err := identity.BootstrapOwner(ctx, pool, *email, *slug, *name, password)
	if err != nil {
		return err
	}
	fmt.Printf("Created first owner %s in organization %s\n", owner.PrincipalID, owner.OrganizationID)
	return nil
}

// adminCommand holds restricted operator repairs that act directly on the
// database (BLAXSMITH_DATABASE_URL), outside any browser session.
func adminCommand(args []string) error {
	if len(args) == 1 && args[0] == "upgrade-secrets" {
		return adminUpgradeSecrets()
	}
	if len(args) > 0 && args[0] == "reset-link" {
		return adminResetLink(args[1:])
	}
	if len(args) == 0 || args[0] != "set-email" {
		return errors.New("usage: blaxsmith admin set-email --login <current email or handle> --email <new email>\n" +
			"       blaxsmith admin reset-link --login <email or handle> --origin https://<host> [--organization <slug>]\n" +
			"       blaxsmith admin upgrade-secrets")
	}
	flags := flag.NewFlagSet("admin set-email", flag.ContinueOnError)
	login := flags.String("login", "", "the account's current email or internal handle")
	email := flags.String("email", "", "the new sign-in email")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *login == "" || *email == "" {
		return errors.New("admin set-email requires --login and --email")
	}
	dsn := os.Getenv("BLAXSMITH_DATABASE_URL")
	if dsn == "" {
		return errors.New("set BLAXSMITH_DATABASE_URL")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	pool, err := tenant.NewPool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("configure database: %w", err)
	}
	defer pool.Close()
	if err := db.Verify(ctx, pool); err != nil {
		return fmt.Errorf("verify database migrations: %w", err)
	}
	principal, err := identity.OperatorSetEmail(ctx, pool, *login, *email)
	if err != nil {
		return err
	}
	fmt.Printf("Set the email of %s; its sessions were signed out\n", principal)
	return nil
}

// adminUpgradeSecrets moves access secrets onto per-organization data keys:
// it re-wraps data keys still under an older master key and re-encrypts
// legacy master-key rows. Idempotent; serve-app also runs it at startup.
func adminUpgradeSecrets() error {
	dsn := os.Getenv("BLAXSMITH_DATABASE_URL")
	if dsn == "" {
		return errors.New("set BLAXSMITH_DATABASE_URL")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	pool, err := tenant.NewPool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("configure database: %w", err)
	}
	defer pool.Close()
	if err := db.Verify(ctx, pool); err != nil {
		return fmt.Errorf("verify database migrations: %w", err)
	}
	secrets, err := appSecretStore(pool)
	if err != nil {
		return err
	}
	if secrets == nil {
		return errors.New("set BLAXSMITH_ACCESS_KEY_FILE")
	}
	summary, err := upgradeSecrets(ctx, secrets)
	if err != nil {
		return err
	}
	fmt.Println(summary)
	return nil
}

func upgradeSecrets(ctx context.Context, secrets *access.SecretStore) (string, error) {
	keys, err := secrets.RewrapKeys(ctx)
	if err != nil {
		return "", err
	}
	rows, err := secrets.UpgradeLegacy(ctx, 100)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Access secrets upgraded: %d data keys under a non-current master key, %d legacy rows remaining", keys, rows), nil
}

// adminResetLink prints a single-use link for the account to choose its own
// password; no password ever passes through the operator.
func adminResetLink(args []string) error {
	flags := flag.NewFlagSet("admin reset-link", flag.ContinueOnError)
	login := flags.String("login", "", "the account's email or internal handle")
	origin := flags.String("origin", "", "the app's public origin, e.g. https://blaxsmith.example.com")
	org := flags.String("organization", "", "organization slug, if the account belongs to several")
	if err := flags.Parse(args); err != nil {
		return err
	}
	u, err := url.Parse(*origin)
	if flags.NArg() != 0 || *login == "" || err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" && u.Path != "/" {
		return errors.New("admin reset-link requires --login and an https --origin with no path")
	}
	dsn := os.Getenv("BLAXSMITH_DATABASE_URL")
	if dsn == "" {
		return errors.New("set BLAXSMITH_DATABASE_URL")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	pool, err := tenant.NewPool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("configure database: %w", err)
	}
	defer pool.Close()
	if err := db.Verify(ctx, pool); err != nil {
		return fmt.Errorf("verify database migrations: %w", err)
	}
	link, err := identity.OperatorResetLink(ctx, pool, *login, *org)
	if err != nil {
		return err
	}
	fmt.Printf("Single-use %s link, valid until %s:\n%s/setup/%s\n", link.Purpose,
		link.ExpiresAt.UTC().Format(time.RFC3339), strings.TrimSuffix(u.String(), "/"), url.PathEscape(link.Token))
	return nil
}
