// Command samuori is the Samuori CLI (docs/operators/cli.md).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"samuori.invalid/samuori/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
