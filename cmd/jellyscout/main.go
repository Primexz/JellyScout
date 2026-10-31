package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Primexz/jellyscout/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}
