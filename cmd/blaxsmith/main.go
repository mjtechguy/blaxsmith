package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
	"github.com/mjtechguy/blaxsmith/internal/catalog"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: blaxsmith <check|freeze|tools|serve|migrate|bootstrap-owner> [flags]")
	}
	if os.Args[1] == "tools" {
		return listTools(os.Args[2:])
	}
	if os.Args[1] == "serve" {
		return serve(os.Args[2:])
	}
	if os.Args[1] == "migrate" {
		return migrateDatabase(os.Args[2:])
	}
	if os.Args[1] == "bootstrap-owner" {
		return bootstrapOwner(os.Args[2:])
	}
	if os.Args[1] != "check" && os.Args[1] != "freeze" {
		return fmt.Errorf("usage: blaxsmith <check|freeze|tools|serve|migrate|bootstrap-owner> [flags]")
	}
	var in recipe.Input
	flags := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	flags.StringVar(&in.Repo, "repo", ".", "local Git repository")
	flags.StringVar(&in.Ref, "ref", "HEAD", "commit or ref to resolve once; working tree changes are ignored")
	flags.StringVar(&in.Recipe, "recipe", "", "committed recipe JSON path, relative to repository root")
	flags.StringVar(&in.Spec, "spec", "", "committed Forge specification path")
	flags.StringVar(&in.Transcript, "transcript", "", "committed Forge interview transcript path")
	flags.StringVar(&in.Scope, "scope", ".", "source directory for AGENTS.md inheritance")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || in.Recipe == "" || in.Spec == "" || in.Transcript == "" {
		return fmt.Errorf("provide --recipe, --spec, and --transcript; positional arguments are not supported")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	bundle, err := recipe.Freeze(ctx, in)
	if err != nil {
		return err
	}
	if os.Args[1] == "check" {
		fmt.Printf("PASS %s: %d stages, %d frozen artifacts\ncommit: %s\nbundle: %s\n%s\n", bundle.Recipe.Name, len(bundle.StageOrder), len(bundle.Artifacts), bundle.Source.Commit, bundle.Digest, bundle.Guild.Report)
		return nil
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(bundle)
}

func migrateDatabase(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("migrate accepts no arguments; set BLAXSMITH_DATABASE_URL")
	}
	dsn := os.Getenv("BLAXSMITH_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("set BLAXSMITH_DATABASE_URL")
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
	count, err := db.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	fmt.Printf("Applied %d database migrations\n", count)
	return nil
}

func listTools(args []string) error {
	flags := flag.NewFlagSet("tools", flag.ContinueOnError)
	tool := flags.String("tool", "all", "codex, claude-code, opencode, or all")
	limit := flags.Int("limit", 20, "number of recent stable versions, 1–100")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("tools accepts flags only")
	}
	names := []string{"codex", "claude-code", "opencode"}
	if *tool != "all" {
		names = []string{*tool}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Second}
	results := make([]catalog.Result, 0, len(names))
	for _, name := range names {
		result, err := catalog.Fetch(ctx, client, name, *limit)
		if err != nil {
			return err
		}
		results = append(results, result)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(results)
}
