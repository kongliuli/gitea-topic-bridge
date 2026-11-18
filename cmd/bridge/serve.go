package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

type httpEndpoint struct {
	server  *http.Server
	address string
}

func serveEndpoints(parent context.Context, ready func(), endpoints []httpEndpoint, workers []func(context.Context)) error {
	var listeners []net.Listener
	defer func() {
		for _, listener := range listeners {
			listener.Close()
		}
	}()
	for _, endpoint := range endpoints {
		listener, err := (&net.ListenConfig{}).Listen(parent, "tcp", endpoint.address)
		if err != nil {
			return err
		}
		listeners = append(listeners, listener)
	}
	if parent.Err() != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var workerGroup sync.WaitGroup
	for _, worker := range workers {
		workerGroup.Go(func() { worker(ctx) })
	}
	result := make(chan error, len(endpoints))
	for index, endpoint := range endpoints {
		listener := listeners[index]
		go func() { result <- endpoint.server.Serve(listener) }()
		log.Printf("listening on %s", listener.Addr())
	}
	ready()
	var serveErr error
	select {
	case serveErr = <-result:
		if errors.Is(serveErr, http.ErrServerClosed) && parent.Err() != nil {
			serveErr = nil
		}
	case <-parent.Done():
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 12*time.Second)
	defer stop()
	for _, endpoint := range endpoints {
		if err := endpoint.server.Shutdown(shutdown); err != nil {
			endpoint.server.Close()
			serveErr = errors.Join(serveErr, err)
		}
	}
	workerGroup.Wait()
	return serveErr
}
