package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/mjtechguy/blaxsmith/db"
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
	if len(args) == 0 || args[0] != "set-email" {
		return errors.New("usage: blaxsmith admin set-email --login <current email or handle> --email <new email>")
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
