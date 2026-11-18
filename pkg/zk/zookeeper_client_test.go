/**
 * Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package zk

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pravega/zookeeper-operator/api/v1beta1"
	"github.com/samuel/go-zookeeper/zk"
)

func TestZookeeperOperationDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{
		{name: "earlier parent deadline", timeout: 100 * time.Millisecond},
		{name: "default operation bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testNeverReadySession(t, tc.timeout)
		})
	}
}

func testNeverReadySession(t *testing.T, parentTimeout time.Duration) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	client := &DefaultZookeeperClient{}
	ctx, cancel := context.WithCancel(context.Background())
	if parentTimeout > 0 {
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), parentTimeout)
	}
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		if err := client.Connect(ctx, listener.Addr().String()); err != nil {
			done <- err
			return
		}
		_, err := client.NodeExists(ctx, "/")
		done <- err
	}()
	var socket net.Conn
	select {
	case socket = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("client did not connect to never-ready server")
	}
	defer func() { _ = socket.Close() }()
	defer client.Close()
	disconnected := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, socket)
		close(disconnected)
	}()
	wait := parentTimeout
	if wait == 0 {
		wait = 30 * time.Second
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("never-ready session error=%v, want context.DeadlineExceeded", err)
		}
		if time.Since(start) < wait {
			t.Fatalf("operation stopped before its %s deadline", wait)
		}
	case <-time.After(wait + 2*time.Second):
		t.Fatal("deadline did not interrupt never-ready ZooKeeper session")
	}
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("canceled session retained its socket")
	}
}

func TestZookeeperPreCanceledConnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &DefaultZookeeperClient{}
	defer c.Close()
	if err := c.Connect(ctx, "invalid-address"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect error=%v, want cancellation before any connection work", err)
	}
}

func TestZookeeperCancellationStopsBlockedOperations(t *testing.T) {
	for _, operation := range []string{"create", "exists", "update"} {
		t.Run(operation, func(t *testing.T) {
			started, released := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			block := func() {
				calls.Add(1)
				close(started)
				<-released
			}
			conn := &metadataTestConnection{
				create: func(string, []byte, int32, []zk.ACL) (string, error) {
					block()
					return "", zk.ErrClosing
				},
				exists: func(string) (bool, *zk.Stat, error) {
					block()
					return false, nil, zk.ErrClosing
				},
				set: func(string, []byte, int32) (*zk.Stat, error) {
					block()
					return nil, zk.ErrClosing
				},
				close: sync.OnceFunc(func() { close(released) }),
			}
			c := &DefaultZookeeperClient{conn: conn}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			defer c.Close()
			invoke := func() error {
				switch operation {
				case "create":
					return c.CreateNode(ctx, &v1beta1.ZookeeperCluster{}, "/parent/child")
				case "exists":
					_, err := c.NodeExists(ctx, "/parent")
					return err
				default:
					return c.UpdateNode(ctx, "/parent", "data", 0)
				}
			}
			done := make(chan error, 1)
			go func() { done <- invoke() }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("operation did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("operation error=%v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				t.Fatal("blocked operation did not stop")
			}
			if err := invoke(); !errors.Is(err, context.Canceled) {
				t.Fatalf("pre-canceled operation error=%v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("made %d requests, want no requests after cancellation", calls.Load())
			}
		})
	}
}

func TestZookeeperCanceledCreateDoesNotCreateChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var paths []string
	c := &DefaultZookeeperClient{conn: &metadataTestConnection{
		create: func(path string, _ []byte, _ int32, _ []zk.ACL) (string, error) {
			paths = append(paths, path)
			cancel()
			return path, nil
		},
	}}
	err := c.CreateNode(ctx, &v1beta1.ZookeeperCluster{}, "/parent/child")
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(paths, []string{"/parent"}) {
		t.Fatalf("CreateNode error=%v paths=%v, want cancellation without child creation", err, paths)
	}
}

func TestMetadataCreateResponses(t *testing.T) {
	for _, tc := range []struct {
		name        string
		parentErr   error
		childErr    error
		verifyPath  string
		verifyErr   error
		disappeared bool
		wantErr     error
	}{
		{name: "new parent and child"},
		{name: "existing parent missing child", parentErr: zk.ErrNodeExists},
		{name: "existing child", parentErr: zk.ErrNodeExists, childErr: zk.ErrNodeExists},
		{name: "child created concurrently", childErr: zk.ErrNodeExists},
		{name: "parent creation denied", parentErr: zk.ErrNoAuth, wantErr: zk.ErrNoAuth},
		{name: "child creation denied", childErr: zk.ErrNoAuth, wantErr: zk.ErrNoAuth},
		{name: "parent verification failed", parentErr: zk.ErrNodeExists, verifyPath: "/operator", verifyErr: zk.ErrConnectionClosed, wantErr: zk.ErrConnectionClosed},
		{name: "child verification failed", childErr: zk.ErrNodeExists, verifyPath: "/operator/example", verifyErr: zk.ErrConnectionClosed, wantErr: zk.ErrConnectionClosed},
		{name: "parent disappeared", parentErr: zk.ErrNodeExists, verifyPath: "/operator", disappeared: true, wantErr: zk.ErrNoNode},
		{name: "child disappeared", childErr: zk.ErrNodeExists, verifyPath: "/operator/example", disappeared: true, wantErr: zk.ErrNoNode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			conn := &metadataTestConnection{
				create: func(path string, data []byte, flags int32, acl []zk.ACL) (string, error) {
					paths = append(paths, path)
					if flags != 0 || !reflect.DeepEqual(acl, zk.WorldACL(zk.PermAll)) {
						t.Fatalf("unexpected creation flags/ACL: %d, %v", flags, acl)
					}
					switch path {
					case "/operator":
						if data != nil {
							t.Fatalf("parent data = %q, want nil", data)
						}
						return path, tc.parentErr
					case "/operator/example":
						if string(data) != "CLUSTER_SIZE=3" {
							t.Fatalf("child data = %q, want CLUSTER_SIZE=3", data)
						}
						return path, tc.childErr
					default:
						t.Fatalf("unexpected creation path: %s", path)
						return "", nil
					}
				},
				exists: func(path string) (bool, *zk.Stat, error) {
					if path == tc.verifyPath {
						return !tc.disappeared, &zk.Stat{}, tc.verifyErr
					}
					return true, &zk.Stat{}, nil
				},
			}
			z := &v1beta1.ZookeeperCluster{}
			z.Spec.Replicas = 3
			client := &DefaultZookeeperClient{conn: conn}
			err := client.CreateNode(context.Background(), z, "/operator/example")
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("CreateNode() = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && !reflect.DeepEqual(paths, []string{"/operator", "/operator/example"}) {
				t.Fatalf("creation paths = %v, want parent and child", paths)
			}
		})
	}
}

type metadataTestConnection struct {
	*zk.Conn
	create func(string, []byte, int32, []zk.ACL) (string, error)
	exists func(string) (bool, *zk.Stat, error)
	set    func(string, []byte, int32) (*zk.Stat, error)
	close  func()
}

func (c *metadataTestConnection) Create(path string, data []byte, flags int32, acl []zk.ACL) (string, error) {
	return c.create(path, data, flags, acl)
}

func (c *metadataTestConnection) Exists(path string) (bool, *zk.Stat, error) {
	return c.exists(path)
}

func (c *metadataTestConnection) Set(path string, data []byte, version int32) (*zk.Stat, error) {
	return c.set(path, data, version)
}

func (c *metadataTestConnection) Close() {
	if c.close != nil {
		c.close()
	}
}
