package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 || (os.Args[1] != "check" && os.Args[1] != "freeze") {
		return fmt.Errorf("usage: blaxsmith <check|freeze> --recipe PATH --spec PATH --transcript PATH [--repo .] [--ref HEAD] [--scope .]")
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
