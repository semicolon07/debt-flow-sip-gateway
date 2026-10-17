//go:build !windows

package service

import (
	"context"
	"os/signal"
	"syscall"
)

func Run(_ string, run func(context.Context) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return run(ctx)
}
