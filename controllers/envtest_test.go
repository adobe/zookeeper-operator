//go:build envtest

// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package controllers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	api "github.com/pravega/zookeeper-operator/api/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

func newEnvtestCluster(t *testing.T, crds ...*apiextensionsv1.CustomResourceDefinition) (context.Context, client.WithWatch, *api.ZookeeperCluster) {
	t.Helper()
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if len(crds) > 0 {
		environment.CRDDirectoryPaths = nil
		environment.CRDs = crds
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
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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

func storageObject(namespace, name string, storage map[string]interface{}) *unstructured.Unstructured {
	spec := map[string]interface{}{"replicas": int64(1)}
	for key, value := range storage {
		spec[key] = value
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "zookeeper.pravega.io/v1beta1",
		"kind":       "ZookeeperCluster",
		"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
		"spec":       spec,
	}}
}

func TestEnvtestStorageValidation(t *testing.T) {
	ctx, c, base := newEnvtestCluster(t)
	tests := []struct {
		name    string
		storage map[string]interface{}
		invalid bool
	}{
		{name: "omitted"},
		{name: "empty-mode", storage: map[string]interface{}{"storageType": ""}},
		{name: "persistent-default", storage: map[string]interface{}{"persistence": map[string]interface{}{}}},
		{name: "persistent-case", storage: map[string]interface{}{"storageType": "PeRsIsTeNcE", "persistence": map[string]interface{}{}}},
		{name: "persistent-unicode-case", storage: map[string]interface{}{"storageType": "per\u017fi\u017ftence"}},
		{name: "ephemeral-case", storage: map[string]interface{}{"storageType": "EpHeMeRaL", "ephemeral": map[string]interface{}{}}},
		{name: "ephemeral-default", storage: map[string]interface{}{"storageType": "ephemeral"}},
		{name: "persistent-delete", storage: map[string]interface{}{"persistence": map[string]interface{}{"reclaimPolicy": "Delete"}}},
		{name: "typo", storage: map[string]interface{}{"storageType": "persistense"}, invalid: true},
		{name: "whitespace", storage: map[string]interface{}{"storageType": " ephemeral"}, invalid: true},
		{name: "both", storage: map[string]interface{}{"persistence": map[string]interface{}{}, "ephemeral": map[string]interface{}{}}, invalid: true},
		{name: "ephemeral-with-persistence", storage: map[string]interface{}{"storageType": "ephemeral", "persistence": map[string]interface{}{}}, invalid: true},
		{name: "persistent-with-ephemeral", storage: map[string]interface{}{"storageType": "persistence", "ephemeral": map[string]interface{}{}}, invalid: true},
		{name: "omitted-with-ephemeral", storage: map[string]interface{}{"ephemeral": map[string]interface{}{}}, invalid: true},
		{name: "empty-with-ephemeral", storage: map[string]interface{}{"storageType": "", "ephemeral": map[string]interface{}{}}, invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := storageObject(base.Namespace, tt.name, tt.storage)
			err := c.Create(ctx, cluster)
			if tt.invalid {
				if !apierrors.IsInvalid(err) {
					t.Fatalf("invalid new storage must be rejected, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			policy, present, err := unstructured.NestedString(cluster.Object, "spec", "persistence", "reclaimPolicy")
			if err != nil {
				t.Fatal(err)
			}
			if _, persistent := tt.storage["persistence"]; persistent {
				want := "Retain"
				if tt.name == "persistent-delete" {
					want = "Delete"
				}
				if !present || policy != want {
					t.Errorf("admission reclaim policy = %q, want %q", policy, want)
				}
			} else if present {
				t.Error("admission must not create a persistence block")
			}
			for _, mutation := range []map[string]interface{}{
				{"storageType": "persistense"},
				{"storageType": "ephemeral", "persistence": map[string]interface{}{}},
				{"storageType": "persistence", "ephemeral": map[string]interface{}{}},
			} {
				updated := cluster.DeepCopy()
				updated.Object["spec"] = storageObject("", "", mutation).Object["spec"]
				if err := c.Update(ctx, updated); !apierrors.IsInvalid(err) {
					t.Fatalf("invalid storage mutation must be rejected, got %v", err)
				}
			}
		})
	}
}

func TestEnvtestLegacyStorageObjectRemainsUpdatable(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "config", "crd", "bases", "zookeeper.pravega.io_zookeeperclusters.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	current := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, current); err != nil {
		t.Fatal(err)
	}
	old := current.DeepCopy()
	// Reconstruct the pre-storage-admission schema without keeping a duplicate CRD fixture.
	spec := old.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	spec.XValidations = nil
	persistence := spec.Properties["persistence"]
	policy := persistence.Properties["reclaimPolicy"]
	policy.Default = nil
	persistence.Properties["reclaimPolicy"] = policy
	spec.Properties["persistence"] = persistence
	old.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec
	ctx, c, base := newEnvtestCluster(t, old)
	legacy := []map[string]interface{}{
		{"storageType": "persistense"},
		{"storageType": "EpHeMeRaL", "persistence": map[string]interface{}{}, "ephemeral": map[string]interface{}{}},
		{"storageType": "persistence", "ephemeral": map[string]interface{}{}},
		{"ephemeral": map[string]interface{}{}},
		{"storageType": "ephemeral", "persistence": map[string]interface{}{}},
		{"storageType": "", "ephemeral": map[string]interface{}{}},
	}
	clusters := make([]*unstructured.Unstructured, len(legacy))
	for i, storage := range legacy {
		cluster := storageObject(base.Namespace, "legacy-"+string(rune('a'+i)), storage)
		if err := c.Create(ctx, cluster); err != nil {
			t.Fatalf("seed old-schema invalid storage: %v", err)
		}
		clusters[i] = cluster
	}
	install := func(schema *apiextensionsv1.CustomResourceDefinition) {
		t.Helper()
		live := &apiextensionsv1.CustomResourceDefinition{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(schema), live); err != nil {
			t.Fatal(err)
		}
		live.Spec = *schema.Spec.DeepCopy()
		if err := c.Update(ctx, live); err != nil {
			t.Fatalf("install storage schema: %v", err)
		}
	}
	install(current)
	// CRD updates reach the admission handler asynchronously.
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := storageObject(base.Namespace, "schema-probe", map[string]interface{}{"storageType": "persistense"})
		err := c.Create(ctx, probe)
		if apierrors.IsInvalid(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, c.Delete(ctx, probe)
	}); err != nil {
		t.Fatalf("new schema must reject invalid creation: %v", err)
	}
	for i, cluster := range clusters {
		t.Run(cluster.GetName(), func(t *testing.T) {
			if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
				t.Fatal(err)
			}
			cluster.SetLabels(map[string]string{"unrelated": "metadata"})
			if err := c.Update(ctx, cluster); err != nil {
				t.Fatalf("unchanged invalid storage must allow metadata update: %v", err)
			}
			if err := unstructured.SetNestedField(cluster.Object, int64(1), "status", "readyReplicas"); err != nil {
				t.Fatal(err)
			}
			if err := c.Status().Update(ctx, cluster); err != nil {
				t.Fatalf("unchanged invalid storage must allow status update: %v", err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
				t.Fatal(err)
			}
			ready, _, err := unstructured.NestedInt64(cluster.Object, "status", "readyReplicas")
			if err != nil || ready != 1 || cluster.GetLabels()["unrelated"] != "metadata" {
				t.Fatalf("metadata/status updates were not persisted: %v", cluster.Object)
			}
			unrelated := cluster.DeepCopy()
			if err := unstructured.SetNestedField(unrelated.Object, int64(3), "spec", "replicas"); err != nil {
				t.Fatal(err)
			}
			if err := c.Update(ctx, unrelated); err != nil {
				t.Fatalf("unchanged invalid storage must allow unrelated spec update: %v", err)
			}
			cluster = unrelated
			for _, mutation := range []struct {
				field string
				value interface{}
			}{
				{"storageType", "another-typo"},
				{"persistence", map[string]interface{}{"annotations": map[string]interface{}{"changed": "true"}}},
				{"ephemeral", map[string]interface{}{"emptydirvolumesource": map[string]interface{}{"medium": "Memory"}}},
			} {
				updated := cluster.DeepCopy()
				if err := unstructured.SetNestedField(updated.Object, mutation.value, "spec", mutation.field); err != nil {
					t.Fatal(err)
				}
				if err := c.Update(ctx, updated); !apierrors.IsInvalid(err) {
					t.Errorf("changing %s while storage stays invalid must be rejected, got %v", mutation.field, err)
				}
			}
			if i == len(clusters)-1 {
				// Keep one invalid object unchanged through schema rollback.
				return
			}
			corrected := cluster.DeepCopy()
			corrected.Object["spec"] = map[string]interface{}{"replicas": int64(1), "storageType": "ephemeral"}
			if err := c.Update(ctx, corrected); err != nil {
				t.Fatalf("correction to valid storage must succeed: %v", err)
			}
			regressed := corrected.DeepCopy()
			regressed.Object["spec"] = cluster.Object["spec"]
			if err := c.Update(ctx, regressed); !apierrors.IsInvalid(err) {
				t.Errorf("corrected storage must not regress to invalid, got %v", err)
			}
		})
	}
	install(old)
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := storageObject(base.Namespace, "rollback-probe", map[string]interface{}{"storageType": "persistense"})
		err := c.Create(ctx, probe)
		if apierrors.IsInvalid(err) {
			return false, nil
		}
		return err == nil, err
	}); err != nil {
		t.Fatalf("schema rollback must restore old admission behavior: %v", err)
	}
	for _, cluster := range clusters {
		if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
			t.Fatal(err)
		}
		cluster.SetAnnotations(map[string]string{"schema": "rolled-back"})
		if err := c.Update(ctx, cluster); err != nil {
			t.Fatalf("object must remain updatable after schema rollback: %v", err)
		}
	}
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
