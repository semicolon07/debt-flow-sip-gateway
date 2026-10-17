//go:build windows

package service

import (
	"context"
	"errors"

	"golang.org/x/sys/windows/svc"
)

func Run(name string, run func(context.Context) error) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !isService {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		return run(ctx)
	}
	handler := &windowsHandler{run: run}
	if err := svc.Run(name, handler); err != nil {
		return err
	}
	return handler.result
}

type windowsHandler struct {
	run    func(context.Context) error
	result error
}

func (h *windowsHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	status <- svc.Status{State: svc.StartPending}
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				status <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				h.result = <-done
				return false, exitCode(h.result)
			}
		case err := <-done:
			h.result = err
			return false, exitCode(err)
		}
	}
}

func exitCode(err error) uint32 {
	if err == nil || errors.Is(err, context.Canceled) {
		return 0
	}
	return 1
}
