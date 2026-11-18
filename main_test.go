//go:build envtest

// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pravega/zookeeper-operator/controllers"
	zkClient "github.com/pravega/zookeeper-operator/pkg/zk"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestOperatorHealthLifecycle(t *testing.T) {
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	initialize := make(chan struct{})
	var initializeOnce sync.Once
	finishInitialization := func() { initializeOnce.Do(func() { close(initialize) }) }
	cacheStarting := make(chan struct{})
	var startingOnce sync.Once
	cfg.Wrap(func(transport http.RoundTripper) http.RoundTripper {
		return roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/zookeeperclusters") {
				startingOnce.Do(func() { close(cacheStarting) })
				select {
				case <-initialize:
				case <-request.Context().Done():
					return nil, request.Context().Err()
				}
			}
			return transport.RoundTrip(request)
		})
	})
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                server.Options{BindAddress: "0"},
		HealthProbeBindAddress: address,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := configureHealthChecks(ctx, mgr); err != nil {
		t.Fatal(err)
	}
	if err := (&controllers.ZookeeperClusterReconciler{
		Client: mgr.GetClient(), Scheme: scheme, Log: ctrl.Log,
		NewZkClient: func() zkClient.ZookeeperClient { return new(zkClient.DefaultZookeeperClient) },
	}).SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	stopping, finishShutdown := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	releaseShutdown := func() { finishOnce.Do(func() { close(finishShutdown) }) }
	defer releaseShutdown()
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		close(stopping)
		<-finishShutdown
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	defer func() {
		finishInitialization()
		cancel()
		releaseShutdown()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("manager did not shut down")
		}
	}()
	awaitSignal(t, cacheStarting, "cache initialization")
	baseURL := "http://" + address
	awaitProbe(t, baseURL+"/healthz", http.StatusOK)
	t.Run("not ready during cache initialization", func(t *testing.T) {
		awaitProbe(t, baseURL+"/readyz", http.StatusInternalServerError)
	})

	finishInitialization()
	t.Run("ready without ensembles", func(t *testing.T) {
		awaitProbe(t, baseURL+"/readyz", http.StatusOK)
	})

	cancel()
	awaitSignal(t, stopping, "manager shutdown")
	t.Run("not ready while manager work drains", func(t *testing.T) {
		awaitProbe(t, baseURL+"/readyz", http.StatusInternalServerError)
	})
	// HTTP probes remain available while manager work drains.
	awaitProbe(t, baseURL+"/healthz", http.StatusOK)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func awaitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(15 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func awaitProbe(t *testing.T, url string, want int) {
	t.Helper()
	httpClient := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	var status int
	var err error
	for time.Now().Before(deadline) {
		var response *http.Response
		response, err = httpClient.Get(url)
		if err == nil {
			status = response.StatusCode
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if status == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s: status=%d err=%v, want %d", url, status, err, want)
}
