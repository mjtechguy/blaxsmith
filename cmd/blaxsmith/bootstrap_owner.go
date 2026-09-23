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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"golang.org/x/term"
)

func bootstrapOwner(args []string) error {
	flags := flag.NewFlagSet("bootstrap-owner", flag.ContinueOnError)
	username := flags.String("username", "", "first owner's login name")
	slug := flags.String("organization-slug", "", "organization URL slug")
	name := flags.String("organization-name", "", "organization display name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *username == "" || *slug == "" || *name == "" {
		return errors.New("bootstrap-owner requires --username, --organization-slug, and --organization-name")
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
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("configure database: %w", err)
	}
	defer pool.Close()
	if _, err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	owner, err := identity.BootstrapOwner(ctx, pool, *username, *slug, *name, password)
	if err != nil {
		return err
	}
	fmt.Printf("Created first owner %s in organization %s\n", owner.PrincipalID, owner.OrganizationID)
	return nil
}
