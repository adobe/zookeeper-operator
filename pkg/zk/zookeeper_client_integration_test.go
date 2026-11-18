//go:build zkintegration

// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package zk_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pravega/zookeeper-operator/api/v1beta1"
	"github.com/pravega/zookeeper-operator/controllers"
	"github.com/pravega/zookeeper-operator/pkg/utils"
	operatorzk "github.com/pravega/zookeeper-operator/pkg/zk"
	"github.com/samuel/go-zookeeper/zk"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestMetadataCreateRetryPreservesExistingData(t *testing.T) {
	address, conn, suffix := startMetadataZooKeeper(t)
	zookeeper := &operatorzk.DefaultZookeeperClient{}
	if err := zookeeper.Connect(context.Background(), address); err != nil {
		t.Fatal(err)
	}
	closeClient := sync.OnceFunc(zookeeper.Close)
	deadline := time.AfterFunc(time.Minute, closeClient)
	t.Cleanup(func() {
		deadline.Stop()
		closeClient()
	})
	if _, err := zookeeper.NodeExists(context.Background(), "/"); err != nil {
		t.Fatalf("wait for production client session: %v", err)
	}
	z := &v1beta1.ZookeeperCluster{}
	z.Spec.Replicas = 3
	root := "/metadata-" + suffix

	t.Run("existing child without parents", func(t *testing.T) {
		path := root + "-child"
		if _, err := conn.Create(path, []byte("CLUSTER_SIZE=5"), 0, zk.WorldACL(zk.PermAll)); err != nil {
			t.Fatal(err)
		}
		if err := zookeeper.CreateNode(context.Background(), z, path); err != nil {
			t.Fatal(err)
		}
		assertMetadataData(t, conn, path, "CLUSTER_SIZE=5", 0)
	})

	t.Run("new metadata and repeated create", func(t *testing.T) {
		path := root + "/new/child"
		if err := zookeeper.CreateNode(context.Background(), z, path); err != nil {
			t.Fatal(err)
		}
		assertMetadataData(t, conn, path, "CLUSTER_SIZE=3", 0)
		if _, err := conn.Set(path, []byte("CLUSTER_SIZE=5"), 0); err != nil {
			t.Fatal(err)
		}
		if err := zookeeper.CreateNode(context.Background(), z, path); err != nil {
			t.Fatal(err)
		}
		assertMetadataData(t, conn, path, "CLUSTER_SIZE=5", 1)
	})

	t.Run("existing parent missing child", func(t *testing.T) {
		parent := root + "/existing"
		if _, err := conn.Create(parent, []byte("parent-data"), 0, zk.WorldACL(zk.PermAll)); err != nil {
			t.Fatal(err)
		}
		if err := zookeeper.CreateNode(context.Background(), z, parent+"/child"); err != nil {
			t.Fatal(err)
		}
		assertMetadataData(t, conn, parent, "parent-data", 0)
		assertMetadataData(t, conn, parent+"/child", "CLUSTER_SIZE=3", 0)
	})

	t.Run("real authorization errors", func(t *testing.T) {
		parent := root + "/restricted"
		if _, err := conn.Create(parent, nil, 0, zk.DigestACL(zk.PermRead, "metadata-test", "test-only")); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{parent + "/child", parent + "/missing/child"} {
			if err := zookeeper.CreateNode(context.Background(), z, path); !errors.Is(err, zk.ErrNoAuth) {
				t.Fatalf("CreateNode(%s) = %v, want ErrNoAuth", path, err)
			}
		}
	})

	t.Run("metadata success then status conflict", func(t *testing.T) {
		s := runtime.NewScheme()
		if err := scheme.AddToScheme(s); err != nil {
			t.Fatal(err)
		}
		if err := v1beta1.AddToScheme(s); err != nil {
			t.Fatal(err)
		}
		cluster := &v1beta1.ZookeeperCluster{ObjectMeta: metav1.ObjectMeta{
			Name: "metadata-" + suffix, Namespace: "default", UID: "metadata-test",
		}}
		cluster.WithDefaults()
		cluster.Spec.Replicas = 1
		sts := operatorzk.MakeStatefulSet(cluster)
		sts.Status.Replicas = 1
		sts.Status.ReadyReplicas = 1
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster, sts).WithStatusSubresource(cluster, sts).Build()
		conflict := true
		faultClient := interceptor.NewClient(cl, interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if subresource == "status" && conflict {
					conflict = false
					return apierrors.NewConflict(schema.GroupResource{Group: v1beta1.GroupVersion.Group, Resource: "zookeeperclusters"}, obj.GetName(), errors.New("stale writer"))
				}
				return c.SubResource(subresource).Update(ctx, obj, opts...)
			},
		})
		r := &controllers.ZookeeperClusterReconciler{
			Client: faultClient, Scheme: s, NewZkClient: func() operatorzk.ZookeeperClient {
				client := &operatorzk.DefaultZookeeperClient{}
				return &metadataSessionClient{DefaultZookeeperClient: client, address: address}
			},
		}
		ctx := context.Background()
		request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}
		if _, err := r.Reconcile(ctx, request); !apierrors.IsConflict(err) {
			t.Fatalf("first reconcile = %v, want status conflict after metadata creation", err)
		}
		path := utils.GetMetaPath(cluster)
		assertMetadataData(t, conn, path, "CLUSTER_SIZE=1", 0)
		if err := cl.Get(ctx, request.NamespacedName, cluster); err != nil {
			t.Fatal(err)
		}
		if cluster.Status.MetaRootCreated {
			t.Fatal("failed status update persisted MetaRootCreated")
		}
		if _, err := conn.Set(path, []byte("CLUSTER_SIZE=5"), 0); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Reconcile(ctx, request); err != nil {
			t.Fatalf("retry after status conflict: %v", err)
		}
		if err := cl.Get(ctx, request.NamespacedName, cluster); err != nil {
			t.Fatal(err)
		}
		if !cluster.Status.MetaRootCreated {
			t.Fatal("retry did not persist MetaRootCreated")
		}
		assertMetadataData(t, conn, path, "CLUSTER_SIZE=5", 1)
	})
}

func startMetadataZooKeeper(t *testing.T) (string, *zk.Conn, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, "../../scripts/zookeeper-version.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("read project ZooKeeper version: %v\n%s", err, output)
	}
	image := "zookeeper:" + strings.TrimSpace(string(output))
	t.Logf("using ZooKeeper image %s", image)
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%x", id)
	name := "zk-metadata-" + suffix
	t.Cleanup(func() {
		if t.Failed() {
			for _, args := range [][]string{{"inspect", "--format", "{{json .State}}", name}, {"logs", "--tail", "100", name}} {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
				cancel()
				t.Logf("docker %s: %v\n%s", strings.Join(args, " "), err, output)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, "docker", "rm", "--force", "--volumes", name).CombinedOutput()
		if err != nil && !strings.Contains(string(output), "No such container:") {
			t.Errorf("remove owned ZooKeeper container: %v\n%s", err, output)
		}
	})
	output, err = exec.CommandContext(ctx, "docker", "run", "--detach", "--name", name,
		"--publish", "127.0.0.1::2181", "--memory", "512m", "--cpus", "1",
		"--env", "JVMFLAGS=-Xms32m -Xmx128m", image).CombinedOutput()
	if err != nil {
		t.Fatalf("start disposable ZooKeeper (Docker required): %v\n%s", err, output)
	}
	output, err = exec.CommandContext(ctx, "docker", "port", name, "2181/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("discover ZooKeeper port: %v\n%s", err, output)
	}
	address := strings.TrimSpace(string(output))
	conn, events, err := zk.Connect([]string{address}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	closeConn := sync.OnceFunc(conn.Close)
	deadline := time.AfterFunc(time.Minute, closeConn)
	t.Cleanup(func() {
		deadline.Stop()
		closeConn()
	})
	ready := time.NewTimer(20 * time.Second)
	defer ready.Stop()
	for {
		select {
		case event, open := <-events:
			if !open {
				t.Fatal("ZooKeeper session closed before readiness")
			}
			if event.State == zk.StateHasSession {
				return address, conn, suffix
			}
		case <-ready.C:
			t.Fatal("timed out waiting for ZooKeeper session readiness")
		}
	}
}

func assertMetadataData(t *testing.T, conn *zk.Conn, path, want string, version int32) {
	t.Helper()
	data, stat, err := conn.Get(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want || stat.Version != version {
		t.Fatalf("%s: data=%q version=%d, want %q version=%d", path, data, stat.Version, want, version)
	}
}

// Route each reconciliation-owned session to the disposable test server.
type metadataSessionClient struct {
	*operatorzk.DefaultZookeeperClient
	address string
}

func (c *metadataSessionClient) Connect(ctx context.Context, _ string) error {
	return c.DefaultZookeeperClient.Connect(ctx, c.address)
}
