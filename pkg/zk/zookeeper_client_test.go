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
	"errors"
	"reflect"
	"testing"

	"github.com/pravega/zookeeper-operator/api/v1beta1"
	"github.com/samuel/go-zookeeper/zk"
)

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
			err := client.CreateNode(z, "/operator/example")
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
}

func (c *metadataTestConnection) Create(path string, data []byte, flags int32, acl []zk.ACL) (string, error) {
	return c.create(path, data, flags, acl)
}

func (c *metadataTestConnection) Exists(path string) (bool, *zk.Stat, error) {
	return c.exists(path)
}
