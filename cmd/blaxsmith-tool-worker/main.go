package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
)

func main() {
	// `bx` is a symlink to this binary inside the runner image.
	if filepath.Base(os.Args[0]) == "bx" {
		os.Exit(tooladapter.Bx(os.Args[1:], os.Stdout, os.Stderr))
	}
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "pane":
			os.Exit(tooladapter.Pane(os.Args[2:]))
		case "bx":
			os.Exit(tooladapter.Bx(os.Args[2:], os.Stdout, os.Stderr))
		case "resume-argv":
			argv, err := tooladapter.ResumeArgv()
			exitOn(err)
			exitOn(json.NewEncoder(os.Stdout).Encode(argv))
			return
		}
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "expected one public task configuration")
		os.Exit(2)
	}
	output, err := tooladapter.Execute(context.Background(), os.Args[1], "/workspace", tooladapter.CredentialFile)
	if len(output) != 0 {
		_, _ = os.Stdout.Write(output)
	}
	exitOn(err)
}

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
