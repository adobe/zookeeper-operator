/**
 * Copyright (c) 2020 Dell Inc., or its subsidiaries. All Rights Reserved.
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
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pravega/zookeeper-operator/api/v1beta1"
	"github.com/samuel/go-zookeeper/zk"
)

type ZookeeperClient interface {
	Connect(context.Context, string) error
	CreateNode(context.Context, *v1beta1.ZookeeperCluster, string) error
	NodeExists(context.Context, string) (int32, error)
	UpdateNode(context.Context, string, string, int32) error
	Close()
}

// OperationTimeout bounds ZooKeeper work, including session readiness, independently
// of the five-second session timeout used to preserve a disconnected session.
const OperationTimeout = 30 * time.Second

const sessionTimeout = 5 * time.Second

type zookeeperConnection interface {
	Create(string, []byte, int32, []zk.ACL) (string, error)
	Exists(string) (bool, *zk.Stat, error)
	Set(string, []byte, int32) (*zk.Stat, error)
	Close()
}

// DefaultZookeeperClient owns one session; use a new client for each operation scope.
type DefaultZookeeperClient struct {
	conn      zookeeperConnection
	events    <-chan zk.Event
	cancel    context.CancelFunc
	closeOnce sync.Once
	socketMu  sync.Mutex
	socket    net.Conn
}

func (client *DefaultZookeeperClient) Connect(ctx context.Context, zkUri string) error {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !strings.Contains(zkUri, ":") {
		zkUri = net.JoinHostPort(zkUri, strconv.Itoa(zk.DefaultPort))
	}
	host, port, err := net.SplitHostPort(zkUri)
	if err != nil {
		return err
	}
	// Resolve with the operation context; the upstream host provider uses
	// context-free DNS even when a context-aware dialer is supplied.
	addresses, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return err
	}
	servers := make([]string, len(addresses))
	for i, address := range addresses {
		servers[i] = net.JoinHostPort(address, port)
	}
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	client.cancel = sessionCancel
	conn, events, err := zk.Connect(servers, sessionTimeout, zk.WithDialer(func(network, address string, timeout time.Duration) (net.Conn, error) {
		dialer := net.Dialer{Timeout: timeout}
		socket, err := dialer.DialContext(sessionCtx, network, address)
		if err != nil {
			return nil, err
		}
		client.socketMu.Lock()
		defer client.socketMu.Unlock()
		if err := sessionCtx.Err(); err != nil {
			_ = socket.Close()
			return nil, err
		}
		client.socket = socket
		return socket, nil
	}))
	if err != nil {
		sessionCancel()
		return fmt.Errorf("failed to connect to zookeeper %s: %w", zkUri, err)
	}
	client.conn = conn
	client.events = events
	err = client.operation(ctx, func(ctx context.Context) error {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case event, ok := <-events:
				if !ok {
					return zk.ErrConnectionClosed
				}
				if event.State == zk.StateHasSession {
					return nil
				}
			}
		}
	})
	if err != nil {
		client.Close()
	}
	return err
}

func (client *DefaultZookeeperClient) operation(ctx context.Context, work func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, OperationTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(closed)
		client.Close()
	})
	err := work(ctx)
	if !stop() {
		<-closed
	}
	if ctx.Err() != nil {
		client.Close()
		return ctx.Err()
	}
	return err
}

func (client *DefaultZookeeperClient) CreateNode(ctx context.Context, zoo *v1beta1.ZookeeperCluster, zNodePath string) error {
	return client.operation(ctx, func(ctx context.Context) error {
		return client.createNode(ctx, zoo, zNodePath)
	})
}

func (client *DefaultZookeeperClient) createNode(ctx context.Context, zoo *v1beta1.ZookeeperCluster, zNodePath string) error {
	paths := strings.Split(zNodePath, "/")
	pathLength := len(paths)
	var parentPath string
	for i := 1; i < pathLength-1; i++ {
		parentPath += "/" + paths[i]
		if err := client.createNodeIfAbsent(ctx, parentPath, nil); err != nil {
			return fmt.Errorf("error creating parent zkNode: %s: %w", parentPath, err)
		}
	}
	data := "CLUSTER_SIZE=" + strconv.Itoa(int(zoo.Spec.Replicas))
	childNode := parentPath + "/" + paths[pathLength-1]
	if err := client.createNodeIfAbsent(ctx, childNode, []byte(data)); err != nil {
		return fmt.Errorf("error creating sub zkNode: %s: %w", childNode, err)
	}
	return nil
}

func (client *DefaultZookeeperClient) createNodeIfAbsent(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := client.conn.Create(path, data, 0, zk.WorldACL(zk.PermAll))
	if err != zk.ErrNodeExists {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	exists, _, err := client.conn.Exists(path)
	if err != nil {
		return err
	}
	if !exists {
		return zk.ErrNoNode
	}
	return nil
}

func (client *DefaultZookeeperClient) UpdateNode(ctx context.Context, path string, data string, version int32) error {
	return client.operation(ctx, func(context.Context) error {
		if _, err := client.conn.Set(path, []byte(data), version); err != nil {
			return fmt.Errorf("error updating zkNode: %w", err)
		}
		return nil
	})
}

func (client *DefaultZookeeperClient) NodeExists(ctx context.Context, zNodePath string) (version int32, err error) {
	version = -1
	err = client.operation(ctx, func(context.Context) error {
		exists, zNodeStat, err := client.conn.Exists(zNodePath)
		if err != nil {
			return fmt.Errorf("znode exists check failed for path %s: %w", zNodePath, err)
		}
		if !exists {
			return fmt.Errorf("znode exists check failed for path %s: %w", zNodePath, zk.ErrNoNode)
		}
		version = zNodeStat.Version
		return nil
	})
	return version, err
}

func (client *DefaultZookeeperClient) Close() {
	client.closeOnce.Do(func() {
		if client.cancel != nil {
			client.cancel()
		}
		// Upstream Close alone does not interrupt an incomplete handshake.
		client.socketMu.Lock()
		if client.socket != nil {
			_ = client.socket.Close()
		}
		client.socketMu.Unlock()
		if client.conn != nil {
			client.conn.Close()
		}
		if client.events != nil {
			for range client.events {
			}
		}
	})
}
