package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/macaz-dev/macaz-cli/internal/app"
)

var version = "dev"

func main() {
	app.Version = version
	ctx, cancel := signal.NotifyContext(context.Background(), gracefulSignals()...)
	defer cancel()
	if err := app.Run(ctx, os.Args[1:], app.Streams{
		In:  os.Stdin,
		Out: os.Stdout,
		Err: os.Stderr,
	}); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "macaz:", err)
		os.Exit(1)
	}
}
