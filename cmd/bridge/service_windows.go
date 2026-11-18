package main

import (
	"context"
	"errors"
	"log"
	"time"

	"golang.org/x/sys/windows/svc"
)

type serviceRuntime struct {
	run func(context.Context, func()) error
}

func runWindowsService(name string, run func(context.Context, func()) error) error {
	active, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !active {
		return errors.New("-service may only be used when launched by Windows Service Control Manager")
	}
	return svc.Run(name, serviceRuntime{run})
}

func (runtime serviceRuntime) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	result := make(chan error, 1)
	status := svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: 15000}
	statuses <- status
	go func() { result <- runtime.run(ctx, func() { close(ready) }) }()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ready:
			ready = nil
			if status.State == svc.StartPending {
				status = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				statuses <- status
			}
		case change, ok := <-requests:
			if !ok {
				requests = nil
				cancel()
				continue
			}
			switch change.Cmd {
			case svc.Interrogate:
				statuses <- status
			case svc.Stop, svc.Shutdown:
				if status.State == svc.StopPending {
					continue
				}
				status = svc.Status{State: svc.StopPending, CheckPoint: 1, WaitHint: 15000}
				statuses <- status
				cancel()
			}
		case <-tick.C:
			if status.State == svc.StartPending || status.State == svc.StopPending {
				status.CheckPoint++
				statuses <- status
			}
		case err := <-result:
			if err != nil {
				log.Printf("Bridge service stopped: %v", err)
				return true, 1
			}
			if ctx.Err() == nil {
				return true, 1
			}
			return false, 0
		}
	}
}
