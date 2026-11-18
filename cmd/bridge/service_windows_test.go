package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

func TestWindowsServiceLifecycle(t *testing.T) {
	for _, stop := range []svc.Cmd{svc.Stop, svc.Shutdown} {
		t.Run(map[svc.Cmd]string{svc.Stop: "stop", svc.Shutdown: "shutdown"}[stop], func(t *testing.T) {
			requests := make(chan svc.ChangeRequest, 1)
			statuses := make(chan svc.Status, 10)
			completed := make(chan uint32, 1)
			runtime := serviceRuntime{func(ctx context.Context, ready func()) error {
				ready()
				<-ctx.Done()
				return nil
			}}
			go func() { _, code := runtime.Execute(nil, requests, statuses); completed <- code }()
			expect := func(state svc.State) svc.Status {
				t.Helper()
				select {
				case status := <-statuses:
					if status.State != state {
						t.Fatalf("service reported %v, wanted %v", status.State, state)
					}
					return status
				case <-time.After(2 * time.Second):
					t.Fatalf("no status %d", state)
					return svc.Status{}
				}
			}
			expect(svc.StartPending)
			if running := expect(svc.Running); running.Accepts != svc.AcceptStop|svc.AcceptShutdown {
				t.Fatal("service did not advertise stop and shutdown")
			}
			requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
			expect(svc.Running)
			requests <- svc.ChangeRequest{Cmd: stop}
			expect(svc.StopPending)
			select {
			case code := <-completed:
				if code != 0 {
					t.Fatalf("graceful stop exit %d", code)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stop did not finish")
			}
		})
	}
	statuses := make(chan svc.Status, 4)
	runtime := serviceRuntime{func(context.Context, func()) error { return errors.New("listener could not bind") }}
	specific, code := runtime.Execute(nil, make(chan svc.ChangeRequest), statuses)
	if !specific || code == 0 || len(statuses) != 1 || (<-statuses).State != svc.StartPending {
		t.Fatal("startup failure was reported as a running or successful service")
	}
}
