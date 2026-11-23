//go:build envtest

// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/pravega/zookeeper-operator/api/v1beta1"
	"github.com/pravega/zookeeper-operator/pkg/zk"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

var defaultEnvtest = envtest.Environment{
	UseExistingCluster:    new(bool),
	CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd", "bases")},
	ErrorIfCRDPathMissing: true,
}

var startDefaultEnvtest = sync.OnceValues(defaultEnvtest.Start)

func TestMain(m *testing.M) {
	code := m.Run()
	if defaultEnvtest.Config != nil {
		if err := defaultEnvtest.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop shared envtest API server: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}

func newEnvtestCluster(t *testing.T, crds ...*apiextensionsv1.CustomResourceDefinition) (context.Context, client.WithWatch, *api.ZookeeperCluster) {
	t.Helper()
	var cfg *rest.Config
	var err error
	if len(crds) == 0 {
		cfg, err = startDefaultEnvtest()
	} else {
		environment := &envtest.Environment{UseExistingCluster: new(bool), CRDs: crds}
		cfg, err = environment.Start()
		if err == nil {
			t.Cleanup(func() {
				if err := environment.Stop(); err != nil {
					t.Errorf("stop isolated API server: %v", err)
				}
			})
		}
	}
	if err != nil {
		t.Fatalf("start isolated API server (run make test-envtest to provision assets): %v", err)
	}
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
		for _, list := range []client.ObjectList{
			&appsv1.StatefulSetList{}, &corev1.ServiceList{}, &policyv1.PodDisruptionBudgetList{}, &corev1.PodList{},
			&corev1.ConfigMapList{}, &corev1.ServiceAccountList{}, &corev1.PersistentVolumeClaimList{},
		} {
			if err := c.List(ctx, list, client.InNamespace(namespace.Name)); err != nil {
				t.Error(err)
				continue
			}
			items, err := meta.ExtractList(list)
			if err != nil {
				t.Error(err)
				continue
			}
			for _, item := range items {
				if err := client.IgnoreNotFound(c.Delete(ctx, item.(client.Object), client.GracePeriodSeconds(0))); err != nil {
					t.Error(err)
				}
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

func TestEnvtestReconcileSettles(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	z.Spec.Replicas = 2
	z.Spec.Conf.AdditionalConfig = map[string]string{"custom.a": "one", "custom.b": "two", "custom.c": "three"}
	z.Spec.Pod.Annotations = map[string]string{"custom": "set"}
	z.Spec.ClientService.Annotations = map[string]string{"custom": "set"}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	assertEnvtestSettled(t, ctx, r, cl, z, 5)
}

func TestEnvtestReconcileConflictRetries(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	exerciseReconcileConflictRetries(t, ctx, cl, z, "StatefulSet", "Service", "PodDisruptionBudget", "status")
}

func TestEnvtestStatefulSetCacheMissDoesNotBypassScaleMetadata(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	r := convergenceReconciler(cl)
	if err := r.reconcileStatefulSet(ctx, z); err != nil {
		t.Fatal(err)
	}
	z.Spec.Replicas = 2
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(z)
	readStatefulSet := func() *appsv1.StatefulSet {
		t.Helper()
		sts := &appsv1.StatefulSet{}
		if err := cl.Get(ctx, key, sts); err != nil {
			t.Fatal(err)
		}
		return sts
	}
	before := readStatefulSet()
	metadataErr := errors.New("metadata unavailable")
	zkClient := &scaleMetadataClient{updateErr: metadataErr}
	metadataAttempts := 0
	r.NewZkClient = func() zk.ZookeeperClient {
		metadataAttempts++
		return zkClient
	}
	r.Client = interceptor.NewClient(cl, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, name client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*appsv1.StatefulSet); ok && name == key {
				return apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "statefulsets"}, name.Name)
			}
			return c.Get(ctx, name, obj, opts...)
		},
	})
	if err := r.reconcileStatefulSet(ctx, z); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("expected the create to fail with AlreadyExists, got %v", err)
	}
	if got := readStatefulSet(); got.ResourceVersion != before.ResourceVersion || metadataAttempts != 0 {
		t.Fatalf("cache miss changed the existing StatefulSet or attempted to update ZooKeeper: replicas=%d, metadata attempts=%d", *got.Spec.Replicas, metadataAttempts)
	}

	r.Client = cl
	if err := r.reconcileStatefulSet(ctx, z); !errors.Is(err, metadataErr) {
		t.Fatalf("retry should attempt to update ZooKeeper before scaling: %v", err)
	}
	if got := readStatefulSet(); got.ResourceVersion != before.ResourceVersion {
		t.Fatalf("StatefulSet changed despite the failed ZooKeeper metadata update: replicas=%d", *got.Spec.Replicas)
	}
	zkClient.updateErr = nil
	if err := r.reconcileStatefulSet(ctx, z); err != nil {
		t.Fatal(err)
	}
	if got := readStatefulSet(); *got.Spec.Replicas != z.Spec.Replicas || metadataAttempts != 2 {
		t.Fatalf("successful metadata retry did not scale as expected: replicas=%d, metadata attempts=%d", *got.Spec.Replicas, metadataAttempts)
	}
}

func TestEnvtestStatefulSetRejectsStaleParentSpec(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	r := convergenceReconciler(cl)
	if err := r.reconcileStatefulSet(ctx, z); err != nil {
		t.Fatal(err)
	}
	z.Spec.Replicas = 2
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	fresh := z.DeepCopy()
	fresh.Spec.Replicas = 1
	if err := cl.Update(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	var writes []string
	r.Client = observeMutations(cl, &writes)
	r.NewZkClient = func() zk.ZookeeperClient {
		t.Fatal("stale parent must not start a resize")
		return nil
	}
	if err := r.reconcileStatefulSet(ctx, z); !apierrors.IsConflict(err) || len(writes) != 0 {
		t.Fatalf("stale parent must conflict without writing: err=%v writes=%v", err, writes)
	}
}

func TestEnvtestResizeRechecksParentWithinZooKeeperBudget(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	r := convergenceReconciler(cl)
	if err := r.reconcileStatefulSet(ctx, z); err != nil {
		t.Fatal(err)
	}
	z.Spec.Replicas = 2
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	var sessionContext context.Context
	r.NewZkClient = func() zk.ZookeeperClient {
		return &resizeMetadataProbe{exists: func(zkCtx context.Context) error {
			sessionContext = zkCtx
			newer := z.DeepCopy()
			newer.Spec.Replicas = 3
			return cl.Update(zkCtx, newer)
		}, update: func() { t.Error("stale intent must not update ZooKeeper metadata") }}
	}
	r.APIReader = interceptor.NewClient(cl, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*api.ZookeeperCluster); ok && sessionContext != nil && ctx != sessionContext {
				t.Error("post-NodeExists freshness check did not use the bounded ZooKeeper context")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	var writes []string
	r.Client = observeMutations(cl, &writes)
	if err := r.reconcileStatefulSet(ctx, z); sessionContext == nil || !apierrors.IsConflict(err) || len(writes) != 0 {
		t.Fatalf("stale resize must conflict after NodeExists without writing: err=%v writes=%v", err, writes)
	}
}

func TestEnvtestResizeRejectsChangedStatefulSetBeforeMetadataWrite(t *testing.T) {
	for _, change := range []string{"deleted", "terminating", "replaced", "updated"} {
		t.Run(change, func(t *testing.T) {
			ctx, cl, z := newEnvtestCluster(t)
			r := convergenceReconciler(cl)
			settleCluster(t, ctx, r, z)
			sts := &appsv1.StatefulSet{}
			key := client.ObjectKeyFromObject(z)
			if err := cl.Get(ctx, key, sts); err != nil {
				t.Fatal(err)
			}
			if change == "terminating" {
				sts.Finalizers = []string{"example.com/hold"}
				if err := cl.Update(ctx, sts); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := cl.Get(ctx, key, sts); err != nil {
						t.Error(err)
						return
					}
					sts.Finalizers = nil
					if err := cl.Update(ctx, sts); err != nil {
						t.Error(err)
					}
				})
			}
			z.Spec.Replicas = 2
			if err := cl.Update(ctx, z); err != nil {
				t.Fatal(err)
			}
			changed, metadataWritten := false, false
			r.NewZkClient = func() zk.ZookeeperClient {
				return &resizeMetadataProbe{exists: func(ctx context.Context) error {
					if change == "updated" {
						sts.Annotations = map[string]string{"example.com/changed": "true"}
						if err := cl.Update(ctx, sts); err != nil {
							return err
						}
					} else if err := cl.Delete(ctx, sts); err != nil {
						return err
					}
					if change == "replaced" {
						replacement := zk.MakeStatefulSet(z)
						if err := controllerutil.SetControllerReference(z, replacement, cl.Scheme()); err != nil {
							return err
						}
						if err := cl.Create(ctx, replacement); err != nil {
							return err
						}
					}
					changed = true
					return nil
				}, update: func() { metadataWritten = true }}
			}
			err := r.reconcileStatefulSet(ctx, z)
			if !changed || metadataWritten || !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
				t.Fatalf("changed StatefulSet must stop resize before writing metadata: changed=%v metadataWritten=%v err=%v", changed, metadataWritten, err)
			}
		})
	}
}

type resizeMetadataProbe struct {
	MockZookeeperClient
	exists func(context.Context) error
	update func()
}

func (c *resizeMetadataProbe) NodeExists(ctx context.Context, _ string) (int32, error) {
	return 0, c.exists(ctx)
}

func (c *resizeMetadataProbe) UpdateNode(context.Context, string, string, int32) error {
	c.update()
	return nil
}

func TestEnvtestApplyDoesNotRecreateDeletedStatefulSet(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	key := client.ObjectKeyFromObject(z)
	deleted := false
	r.Client = interceptor.NewClient(cl, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*appsv1.StatefulSet); ok && patch.Type() == types.ApplyPatchType && !deleted {
				if err := c.Delete(ctx, obj); err != nil {
					return err
				}
				deleted = true
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	if err := r.reconcileStatefulSet(ctx, z); err == nil || !deleted {
		t.Fatalf("apply must not create after deletion: deleted=%v err=%v", deleted, err)
	}
	if err := cl.Get(ctx, key, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("StatefulSet should stay deleted until the next reconcile: %v", err)
	}
	r.Client = cl
	if err := r.reconcileStatefulSet(ctx, z); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, &appsv1.StatefulSet{}); err != nil {
		t.Fatalf("create path did not recreate the StatefulSet: %v", err)
	}
}

func TestEnvtestReconcileRejectsStatefulSetOwnershipConflict(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	sts := zk.MakeStatefulSet(z)
	if err := cl.Create(ctx, sts, client.FieldOwner("zookeeper-operator")); err != nil {
		t.Fatal(err)
	}
	z.Spec.Replicas = 2
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	r := convergenceReconciler(cl)
	var metadataAttempts int
	r.NewZkClient = func() zk.ZookeeperClient {
		metadataAttempts++
		return &MockZookeeperClient{}
	}
	for _, scenario := range []string{"no controller", "previous cluster UID", "failed upgrade"} {
		t.Run(scenario, func(t *testing.T) {
			switch scenario {
			case "previous cluster UID":
				previous := z.DeepCopy()
				previous.UID = "previous-cluster"
				if err := controllerutil.SetControllerReference(previous, sts, cl.Scheme()); err != nil {
					t.Fatal(err)
				}
				if err := cl.Update(ctx, sts); err != nil {
					t.Fatal(err)
				}
			case "failed upgrade":
				z.Status.SetErrorConditionTrue("UpgradeFailed", "upgrade timed out")
				if err := cl.Status().Update(ctx, z); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.reconcileStatefulSet(ctx, z); err == nil || !strings.Contains(err.Error(), "is not controlled by ZookeeperCluster") {
				t.Fatalf("expected an ownership error, got %v", err)
			}
			got := &appsv1.StatefulSet{}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(sts), got); err != nil {
				t.Fatal(err)
			}
			if got.ResourceVersion != sts.ResourceVersion || metadataAttempts != 0 {
				t.Fatalf("ownership conflict caused a StatefulSet write or ZooKeeper metadata update: metadata attempts=%d", metadataAttempts)
			}
			if scenario == "failed upgrade" && !z.Status.IsClusterInUpgradeFailedState() {
				t.Fatal("an unrelated StatefulSet cleared the failed upgrade state")
			}
		})
	}
}

func TestEnvtestReconcileRejectsServiceNameCollision(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	r := convergenceReconciler(cl)
	for _, test := range []struct {
		name      string
		svc       *corev1.Service
		reconcile reconcileFun
	}{
		{"unowned client Service", zk.MakeClientService(z), r.reconcileClientService},
		{"headless Service from previous cluster UID", zk.MakeHeadlessService(z), r.reconcileHeadlessService},
		{"concurrently created admin Service", zk.MakeAdminServerService(z), r.reconcileAdminServerService},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc := test.svc
			svc.Spec.Selector["app"] = "another-app"
			key := client.ObjectKeyFromObject(svc)
			if svc.Spec.ClusterIP == corev1.ClusterIPNone {
				previous := z.DeepCopy()
				previous.UID = "previous-cluster"
				if err := controllerutil.SetControllerReference(previous, svc, cl.Scheme()); err != nil {
					t.Fatal(err)
				}
			}
			concurrent := svc.Name == z.GetAdminServerServiceName()
			if concurrent {
				r.Client = interceptor.NewClient(cl, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, name client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if name != key {
							return c.Get(ctx, name, obj, opts...)
						}
						if err := c.Create(ctx, svc, client.FieldOwner("zookeeper-operator")); err != nil {
							return err
						}
						return apierrors.NewNotFound(schema.GroupResource{Resource: "services"}, name.Name)
					},
				})
				defer func() { r.Client = cl }()
			} else if err := cl.Create(ctx, svc, client.FieldOwner("zookeeper-operator")); err != nil {
				t.Fatal(err)
			}
			err := test.reconcile(ctx, z)
			if concurrent {
				if !apierrors.IsAlreadyExists(err) || svc.ResourceVersion == "" {
					t.Fatalf("concurrent create must return AlreadyExists, got %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "is not controlled by ZookeeperCluster") {
				t.Fatalf("expected an ownership error, got %v", err)
			}
			got := &corev1.Service{}
			if err := cl.Get(ctx, key, got); err != nil {
				t.Fatal(err)
			}
			if got.ResourceVersion != svc.ResourceVersion {
				t.Fatalf("Service changed despite the name collision: selector %v, owners %v", got.Spec.Selector, got.OwnerReferences)
			}
		})
	}
}

func TestEnvtestReconcileZookeeperEnvOverride(t *testing.T) {
	for _, mode := range []string{"new", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cl, z := newEnvtestCluster(t)
			optional := true
			env := []corev1.EnvVar{
				{Name: "ORIGINAL_STATUS", Value: "$(ENVOY_SIDECAR_STATUS)"},
				{Name: "BASE", Value: "/srv"},
				{Name: "BASE", Value: "$(BASE)/data"},
				{Name: "ENVOY_SIDECAR_STATUS", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "optional-config"}, Key: "status", Optional: &optional,
				}}},
			}
			if mode == "legacy" {
				sts := zk.MakeStatefulSet(z)
				container := &sts.Spec.Template.Spec.Containers[0]
				// The previous operator appended user variables to the built-in variable.
				container.Env = append(container.Env, env...)
				if err := controllerutil.SetControllerReference(z, sts, cl.Scheme()); err != nil {
					t.Fatal(err)
				}
				if err := cl.Create(ctx, sts, client.FieldOwner("zookeeper-operator")); err != nil {
					t.Fatal(err)
				}
			}
			z.Spec.Pod.Env = env
			if err := cl.Update(ctx, z); err != nil {
				t.Fatal(err)
			}
			r := convergenceReconciler(cl)
			settleCluster(t, ctx, r, z)
			assertEnvtestSettled(t, ctx, r, cl, z, 3)
			sts := &appsv1.StatefulSet{}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(z), sts); err != nil {
				t.Fatal(err)
			}
			got := sts.Spec.Template.Spec.Containers[0].Env
			if len(got) != len(env)+1 || got[0].Name != "ENVOY_SIDECAR_STATUS" || got[0].ValueFrom == nil || got[0].ValueFrom.FieldRef == nil || !reflect.DeepEqual(got[1:], env) {
				t.Fatalf("expected the generated fallback and the exact ordered env overrides: got %+v", got)
			}
			fallback := got[0]
			z.Spec.Pod.Env = nil
			if err := cl.Update(ctx, z); err != nil {
				t.Fatal(err)
			}
			runReconcile(t, ctx, r, z)
			if err := cl.Get(ctx, client.ObjectKeyFromObject(z), sts); err != nil {
				t.Fatal(err)
			}
			if got := sts.Spec.Template.Spec.Containers[0].Env; !reflect.DeepEqual(got, []corev1.EnvVar{fallback}) {
				t.Fatalf("removing duplicate overrides should leave only the generated fallback: got %+v", got)
			}
			settleCluster(t, ctx, r, z)
			assertEnvtestSettled(t, ctx, r, cl, z, 3)
		})
	}
}

func TestEnvtestReconcileRestoresInternalAdminService(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	admin := &corev1.Service{}
	key := client.ObjectKey{Namespace: z.Namespace, Name: z.GetAdminServerServiceName()}
	if err := cl.Get(ctx, key, admin); err != nil {
		t.Fatal(err)
	}
	admin.Spec.Type = corev1.ServiceTypeLoadBalancer
	admin.Spec.LoadBalancerSourceRanges = []string{"192.0.2.0/24"}
	if err := cl.Update(ctx, admin, client.FieldOwner("user")); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, ctx, r, z)
	if err := cl.Get(ctx, key, admin); err != nil {
		t.Fatal(err)
	}
	if admin.Spec.Type != corev1.ServiceTypeClusterIP || admin.Spec.Ports[0].NodePort != 0 || len(admin.Spec.LoadBalancerSourceRanges) != 0 {
		t.Fatalf("expected an internal admin Service without a NodePort or load balancer source ranges, got %+v", admin.Spec)
	}
}

func TestEnvtestReconcileDefaultsConverge(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	z.Spec.Containers = []corev1.Container{{
		Name: "sidecar", Image: "busybox:1",
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("1.1m"),
		}},
		StartupProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			GRPC: &corev1.GRPCAction{Port: 9090},
		}},
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8080)},
		}},
	}}
	z.Spec.Volumes = []corev1.Volume{{
		Name: "secret", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: "example"},
		},
	}, {
		Name: "empty",
	}}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	patches := map[types.PatchType]int{}
	r.Client = interceptor.NewClient(cl, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*appsv1.StatefulSet); ok {
				patches[patch.Type()]++
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	const passes = 3
	assertEnvtestSettled(t, ctx, r, cl, z, passes)
	if compatibility, apply := patches[types.MergePatchType], patches[types.ApplyPatchType]; compatibility != 0 || apply != passes {
		t.Fatalf("settled reconciles sent %d StatefulSet compatibility patches and %d applies, want 0 and %d", compatibility, apply, passes)
	}
	r.Client = cl
	sts := &appsv1.StatefulSet{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(z), sts); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.Template.Spec.DNSPolicy != corev1.DNSClusterFirst ||
		sts.Spec.Template.Spec.Containers[1].TerminationMessagePath != "/dev/termination-log" {
		t.Fatal("test requires real API-defaulted Pod-template fields")
	}
	if !slices.ContainsFunc(sts.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
		return v.Name == "empty" && v.EmptyDir != nil
	}) {
		t.Fatal("test requires the API to default the sourceless volume to EmptyDir")
	}
	svc := &corev1.Service{}
	svcKey := types.NamespacedName{Name: z.GetClientServiceName(), Namespace: z.Namespace}
	if err := cl.Get(ctx, svcKey, svc); err != nil {
		t.Fatal(err)
	}
	if svc.Spec.Ports[0].Protocol != corev1.ProtocolTCP || svc.Spec.Ports[0].TargetPort.IntVal != 2181 {
		t.Fatal("test requires real API-defaulted Service ports")
	}
	for _, external := range []bool{true, false} {
		if err := cl.Get(ctx, client.ObjectKeyFromObject(z), z); err != nil {
			t.Fatal(err)
		}
		z.Spec.AdminServerService.External = external
		z.Spec.DomainName = ""
		if external {
			z.Spec.DomainName = "example.test"
		}
		if err := cl.Update(ctx, z); err != nil {
			t.Fatal(err)
		}
		runReconcile(t, ctx, r, z)
		admin := &corev1.Service{}
		if err := cl.Get(ctx, client.ObjectKey{Name: z.GetAdminServerServiceName(), Namespace: z.Namespace}, admin); err != nil {
			t.Fatal(err)
		}
		if external {
			if admin.Spec.Type != corev1.ServiceTypeLoadBalancer || admin.Spec.Ports[0].NodePort == 0 {
				t.Fatal("external admin service did not get an allocated NodePort")
			}
		} else if admin.Spec.Type != corev1.ServiceTypeClusterIP || admin.Spec.Ports[0].NodePort != 0 {
			t.Fatal("removing the external service did not clear its NodePort")
		}
		headless := &corev1.Service{}
		if err := cl.Get(ctx, client.ObjectKey{Name: z.Name + "-headless", Namespace: z.Namespace}, headless); err != nil {
			t.Fatal(err)
		}
		if _, hasDNS := headless.Annotations["external-dns.alpha.kubernetes.io/hostname"]; hasDNS != external {
			t.Fatal("external-DNS annotation change or removal did not converge")
		}
		settleCluster(t, ctx, r, z)
		assertEnvtestSettled(t, ctx, r, cl, z, 3)
	}
}

func TestEnvtestReconcileAdoptsLegacyResources(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	createLegacyResources(t, ctx, cl, z)
	sts := &appsv1.StatefulSet{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(z), sts); err != nil {
		t.Fatal(err)
	}
	sts.Spec.Template.Spec.Containers[0].Args = []string{"external-argument"}
	if err := cl.Update(ctx, sts, client.FieldOwner("external-injector")); err != nil {
		t.Fatal(err)
	}
	clientSvc := &corev1.Service{}
	clientKey := client.ObjectKey{Name: z.GetClientServiceName(), Namespace: z.Namespace}
	if err := cl.Get(ctx, clientKey, clientSvc); err != nil {
		t.Fatal(err)
	}
	clientSvc.Annotations = map[string]string{"external-dns.alpha.kubernetes.io/hostname": "zk.user.example"}
	clientSvc.Spec.Ports = append(clientSvc.Spec.Ports, corev1.ServicePort{Name: "extra", Port: 8081})
	if err := cl.Update(ctx, clientSvc, client.FieldOwner("user")); err != nil {
		t.Fatal(err)
	}
	generation, clusterIP := sts.Generation, clientSvc.Spec.ClusterIP
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	if sts.Generation != generation {
		t.Fatalf("adopting a legacy StatefulSet changed its spec: generation %d -> %d", generation, sts.Generation)
	}
	if _, ok := sts.Labels["owner-rv"]; ok {
		t.Error("legacy owner-rv label was not removed")
	}
	if args := sts.Spec.Template.Spec.Containers[0].Args; len(args) != 1 || args[0] != "external-argument" {
		t.Error("adoption removed a field owned by another manager")
	}
	for _, entry := range sts.ManagedFields {
		if legacyFieldManagers.Has(entry.Manager) {
			t.Errorf("legacy field manager %q was not migrated", entry.Manager)
		}
	}
	if err := cl.Get(ctx, clientKey, clientSvc); err != nil {
		t.Fatal(err)
	}
	if clientSvc.Spec.ClusterIP != clusterIP {
		t.Error("adoption changed the allocated cluster IP")
	}
	if clientSvc.Annotations["external-dns.alpha.kubernetes.io/hostname"] != "zk.user.example" {
		t.Error("adoption removed a user-owned external-DNS annotation")
	}
	if !slices.ContainsFunc(clientSvc.Spec.Ports, func(port corev1.ServicePort) bool {
		return port.Name == "extra" && port.Port == 8081
	}) {
		t.Error("adoption removed a user-owned Service port")
	}
	assertEnvtestSettled(t, ctx, r, cl, z, 3)
}

func TestEnvtestReconcileLegacyOwnedFieldRemoval(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	z.Spec.Pod.NodeSelector = map[string]string{"pool": "old"}
	z.Spec.Pod.Env = []corev1.EnvVar{{Name: "OWNED", Value: "remove"}}
	z.Spec.Pod.Annotations = map[string]string{"custom": "remove"}
	z.Spec.Containers = []corev1.Container{
		{Name: "sidecar", Image: "busybox:1"},
		{Name: "retired", Image: "busybox:1"},
	}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	createLegacyResources(t, ctx, cl, z)
	sts := &appsv1.StatefulSet{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(z), sts); err != nil {
		t.Fatal(err)
	}
	for i := range sts.Spec.Template.Spec.Containers {
		container := &sts.Spec.Template.Spec.Containers[i]
		if container.Name == "sidecar" {
			container.Args = []string{"external-argument"}
		}
		if container.Name == "zookeeper" {
			container.Env = append(container.Env, corev1.EnvVar{Name: "EXTERNAL", Value: "keep"})
		}
	}
	if err := cl.Update(ctx, sts, client.FieldOwner("external-injector")); err != nil {
		t.Fatal(err)
	}
	// The CR changes before the new operator first reconciles the legacy StatefulSet.
	z.Spec.Pod.NodeSelector = nil
	z.Spec.Pod.Env = nil
	z.Spec.Pod.Annotations = nil
	z.Spec.Containers = []corev1.Container{{Name: "sidecar", Image: "busybox:2"}}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	if len(sts.Spec.Template.Spec.NodeSelector) != 0 || len(sts.Spec.Template.Spec.Containers) != 2 {
		t.Fatal("fields the legacy operator rendered were not removed")
	}
	if _, ok := sts.Spec.Template.Annotations["custom"]; ok {
		t.Error("Pod annotation the legacy operator rendered was not removed")
	}
	externalEnv := false
	for _, container := range sts.Spec.Template.Spec.Containers {
		if container.Name == "sidecar" &&
			(container.Image != "busybox:2" || len(container.Args) != 1 || container.Args[0] != "external-argument") {
			t.Error("sidecar image change erased an unowned container field")
		}
		for _, env := range container.Env {
			if env.Name == "OWNED" {
				t.Error("environment entry the legacy operator rendered was not removed")
			}
			externalEnv = externalEnv || env.Name == "EXTERNAL"
		}
	}
	if !externalEnv {
		t.Error("removing an owned environment entry erased an unowned entry")
	}
	for _, logging := range []api.LoggingConfig{{Level: "WARN", Format: "json"}, {Level: "INFO", Format: "text"}} {
		if err := cl.Get(ctx, client.ObjectKeyFromObject(z), z); err != nil {
			t.Fatal(err)
		}
		z.Spec.Logging = logging
		if err := cl.Update(ctx, z); err != nil {
			t.Fatal(err)
		}
		runReconcile(t, ctx, r, z)
	}
	settleCluster(t, ctx, r, z)
	assertEnvtestSettled(t, ctx, r, cl, z, 3)
}

// createLegacyResources creates the StatefulSet and Services as earlier operator versions did:
// with Create requests from the legacy field manager and the owner-rv label.
func createLegacyResources(t *testing.T, ctx context.Context, cl client.Client, z *api.ZookeeperCluster) {
	t.Helper()
	sts := zk.MakeStatefulSet(z)
	zk.EnsureLogbackProjection(z, nil, sts)
	sts.Labels = map[string]string{"owner-rv": z.ResourceVersion}
	for key, value := range z.Spec.Labels {
		sts.Labels[key] = value
	}
	objects := []client.Object{sts, zk.MakeClientService(z), zk.MakeHeadlessService(z), zk.MakeAdminServerService(z)}
	for _, obj := range objects {
		if err := controllerutil.SetControllerReference(z, obj, cl.Scheme()); err != nil {
			t.Fatal(err)
		}
		if err := cl.Create(ctx, obj, client.FieldOwner("zookeeper-operator")); err != nil {
			t.Fatal(err)
		}
	}
}

// assertEnvtestSettled checks that further reconciles do not change any persisted object.
func assertEnvtestSettled(t *testing.T, ctx context.Context, r *ZookeeperClusterReconciler, cl client.Client, z *api.ZookeeperCluster, passes int) {
	t.Helper()
	settled := envtestResourceVersions(t, ctx, cl, z)
	for range passes {
		runReconcile(t, ctx, r, z)
	}
	if got := envtestResourceVersions(t, ctx, cl, z); !reflect.DeepEqual(got, settled) {
		t.Fatalf("settled reconciliation changed persisted objects:\nbefore %v\nafter  %v", settled, got)
	}
}

// envtestResourceVersions returns the resourceVersion of every object the reconciler writes.
func envtestResourceVersions(t *testing.T, ctx context.Context, cl client.Client, z *api.ZookeeperCluster) map[string]string {
	t.Helper()
	objects := map[string]client.Object{
		"cluster":     &api.ZookeeperCluster{ObjectMeta: metav1.ObjectMeta{Name: z.Name}},
		"statefulset": &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: z.Name}},
		"configmap":   &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: z.ConfigMapName()}},
		"pdb":         &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: z.Name}},
		"client":      &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: z.GetClientServiceName()}},
		"headless":    &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: z.Name + "-headless"}},
		"admin":       &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: z.GetAdminServerServiceName()}},
	}
	versions := map[string]string{}
	for name, obj := range objects {
		if err := cl.Get(ctx, client.ObjectKey{Name: obj.GetName(), Namespace: z.Namespace}, obj); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		versions[name] = obj.GetResourceVersion()
	}
	return versions
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
