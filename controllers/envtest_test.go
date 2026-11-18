//go:build envtest

// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package controllers

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	api "github.com/pravega/zookeeper-operator/api/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func newEnvtestCluster(t *testing.T) (context.Context, client.WithWatch, *api.ZookeeperCluster) {
	t.Helper()
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatalf("start isolated API server (run make test-envtest to provision assets): %v", err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop isolated API server: %v", err)
		}
	})
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := policyv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "zookeeper-envtest-"}}
	if err := c.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// envtest has no namespace controller or garbage collector.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		clusters := &api.ZookeeperClusterList{}
		if err := c.List(ctx, clusters, client.InNamespace(namespace.Name)); err != nil {
			t.Error(err)
			return
		}
		for i := range clusters.Items {
			cluster := &clusters.Items[i]
			cluster.Finalizers = nil
			if err := c.Update(ctx, cluster); err != nil {
				t.Error(err)
				continue
			}
			if err := client.IgnoreNotFound(c.Delete(ctx, cluster)); err != nil {
				t.Error(err)
			}
		}
		if err := c.Delete(ctx, namespace); err != nil {
			t.Error(err)
			return
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(namespace), namespace); err != nil {
			t.Error(err)
			return
		}
		namespace.Spec.Finalizers = nil
		if err := c.SubResource("finalize").Update(ctx, namespace); err != nil {
			t.Error(err)
			return
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(namespace), &corev1.Namespace{}); !apierrors.IsNotFound(err) {
			t.Errorf("namespace cleanup must complete without a namespace controller, got %v", err)
		}
	})
	cluster := &api.ZookeeperCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: namespace.Name},
		Spec:       api.ZookeeperClusterSpec{Replicas: 1},
	}
	cluster.WithDefaults()
	if err := c.Create(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	return ctx, c, cluster
}

func TestEnvtestResourceWriteGuards(t *testing.T) {
	_, c, cluster := newEnvtestCluster(t)
	exerciseResourceWriteGuards(t, c, cluster)
}

func TestEnvtestStatusSubresource(t *testing.T) {
	ctx, c, cluster := newEnvtestCluster(t)
	cluster.Status.ReadyReplicas = 1
	if err := c.Status().Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.Replicas = 3
	cluster.Status.ReadyReplicas = 99
	if err := c.Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	got := &api.ZookeeperCluster{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Replicas != 3 || got.Status.ReadyReplicas != 1 {
		t.Fatalf("main-resource update must persist spec but preserve status: spec=%d ready=%d", got.Spec.Replicas, got.Status.ReadyReplicas)
	}
}

func TestEnvtestUpdateConflict(t *testing.T) {
	ctx, c, cluster := newEnvtestCluster(t)
	stale := cluster.DeepCopy()
	cluster.Labels = map[string]string{"writer": "first"}
	if err := c.Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	stale.Labels = map[string]string{"writer": "stale"}
	if err := c.Update(ctx, stale); !apierrors.IsConflict(err) {
		t.Fatalf("stale writer must receive Conflict, got %v", err)
	}
	got := &api.ZookeeperCluster{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), got); err != nil {
		t.Fatal(err)
	}
	if got.Labels["writer"] != "first" {
		t.Fatalf("stale writer replaced persisted data: %v", got.Labels)
	}
}

func TestEnvtestDeletionFinalizer(t *testing.T) {
	ctx, c, cluster := newEnvtestCluster(t)
	cluster.Finalizers = []string{"test.zookeeper.pravega.io/hold"}
	if err := c.Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}
	if err := c.Get(ctx, key, cluster); err != nil {
		t.Fatalf("finalizer must keep deleting resource readable: %v", err)
	}
	if cluster.DeletionTimestamp.IsZero() || len(cluster.Finalizers) != 1 {
		t.Fatalf("expected deletion timestamp and installed finalizer, got %v", cluster.ObjectMeta)
	}
	cluster.Finalizers = nil
	if err := c.Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 10*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, key, &api.ZookeeperCluster{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}); err != nil {
		t.Fatalf("resource must disappear after finalizer removal: %v", err)
	}
}
