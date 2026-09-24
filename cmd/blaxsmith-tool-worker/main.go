package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "expected one public task configuration")
		os.Exit(2)
	}
	output, err := tooladapter.Execute(context.Background(), os.Args[1], "/workspace", tooladapter.CredentialFile)
	if len(output) != 0 {
		_, _ = os.Stdout.Write(output)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
