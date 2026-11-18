package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeEndpointsReadinessAndStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	ready := make(chan struct{})
	address := occupied.Addr().String()
	if err := serveEndpoints(ctx, func() { close(ready) }, []httpEndpoint{{&http.Server{}, "127.0.0.1:0"}, {&http.Server{}, address}}, nil); err == nil {
		t.Fatal("startup unexpectedly succeeded despite port conflict")
	}
	select {
	case <-ready:
		t.Fatal("partial startup signaled ready")
	default:
	}
	occupied.Close()
	finished := make(chan error, 1)
	workerDone := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(204) })}
	go func() {
		finished <- serveEndpoints(ctx, func() { close(ready) }, []httpEndpoint{{server, address}}, []func(context.Context){func(ctx context.Context) { <-ctx.Done(); close(workerDone) }})
	}()
	select {
	case <-ready:
	case err := <-finished:
		t.Fatalf("server stopped before ready: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("server did not become ready")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + address)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatalf("HTTP status %d", response.StatusCode)
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
	}
	select {
	case <-workerDone:
	default:
		t.Fatal("worker was not joined")
	}
	if response, err := client.Get("http://" + address); err == nil {
		response.Body.Close()
		t.Fatal("listener did not release port")
	}
}
