package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"matrix-cli/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := cli.NewDefault()
	if err == nil {
		err = app.Root().ExecuteContext(ctx)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "matrix:", err)
		os.Exit(1)
	}
}
