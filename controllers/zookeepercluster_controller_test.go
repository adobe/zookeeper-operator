/**
 * Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ghodss/yaml"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/pravega/zookeeper-operator/api/v1beta1"
	"github.com/pravega/zookeeper-operator/pkg/controller/config"
	"github.com/pravega/zookeeper-operator/pkg/utils"
	"github.com/pravega/zookeeper-operator/pkg/zk"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestZookeepercluster(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ZookeeperCluster Controller Spec")
}

func TestReconcileEventPredicate(t *testing.T) {
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	ready := sts.DeepCopy()
	ready.Status.ReadyReplicas = 1
	rollout := sts.DeepCopy()
	rollout.Status.UpdateRevision = "new-revision"
	stsSpec := sts.DeepCopy()
	stsSpec.Generation++
	metadata := sts.DeepCopy()
	metadata.ResourceVersion = "2"
	metadata.Labels = map[string]string{"external": "value"}
	cluster := &v1beta1.ZookeeperCluster{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	clusterSpec := cluster.DeepCopy()
	clusterSpec.Generation++
	clusterStatus := cluster.DeepCopy()
	clusterStatus.Status.ReadyReplicas = 1

	p := reconcileEventPredicate()
	for _, tt := range []struct {
		name     string
		old, new client.Object
		want     bool
	}{
		{"StatefulSet readiness", sts, ready, true},
		{"StatefulSet rollout revision", sts, rollout, true},
		{"StatefulSet generation", sts, stsSpec, true},
		{"StatefulSet metadata only", sts, metadata, false},
		{"cluster generation", cluster, clusterSpec, true},
		{"cluster status only", cluster, clusterStatus, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new}); got != tt.want {
				t.Fatalf("Update() = %v, want %v", got, tt.want)
			}
		})
	}
	if !p.Create(event.CreateEvent{Object: sts}) ||
		!p.Delete(event.DeleteEvent{Object: sts}) ||
		!p.Generic(event.GenericEvent{Object: sts}) {
		t.Fatal("create, delete, and generic events must remain accepted")
	}
}

func TestGenerateYAMLRejectsInvalidLogging(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		logging v1beta1.LoggingConfig
		field   string
	}{
		{v1beta1.LoggingConfig{Level: "invalid"}, "spec.logging.level"},
		{v1beta1.LoggingConfig{Format: "invalid"}, "spec.logging.format"},
	} {
		z := &v1beta1.ZookeeperCluster{}
		z.Spec.Logging = tc.logging
		r := &ZookeeperClusterReconciler{}
		if err := r.GenerateYAML(z); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Fatalf("expected validation error for %s, got %v", tc.field, err)
		}
	}
	files, err := os.ReadDir(".")
	if err != nil || len(files) != 0 {
		t.Fatalf("invalid logging created output files: %v, error: %v", files, err)
	}
}

func TestGenerateYAMLProjectsDefaultLogging(t *testing.T) {
	t.Chdir(t.TempDir())
	z := &v1beta1.ZookeeperCluster{ObjectMeta: metav1.ObjectMeta{Name: "example"}}
	if err := (&ZookeeperClusterReconciler{}).GenerateYAML(z); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("ZookeeperCluster/StatefulSet.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var sts appsv1.StatefulSet
	if err := yaml.Unmarshal(data, &sts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-config"], `<root level="INFO">`) {
		t.Fatal("exported StatefulSet does not contain its default Logback config")
	}
	for _, volume := range sts.Spec.Template.Spec.Volumes {
		if volume.Name == "conf" && volume.Projected != nil {
			return
		}
	}
	t.Fatal("exported StatefulSet does not project the default Logback config")
}

func TestPDBReconcilesMaxUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		previous int
		desired  int32
		drifted  bool
	}{
		{name: "relax", previous: 1, desired: 2},
		{name: "tighten", previous: 2, desired: 1},
		{name: "repair spec drift", previous: 1, desired: 1, drifted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, z, pdb := newPDBTestReconciler(t, tc.previous)
			z.Spec.MaxUnavailableReplicas = tc.desired
			if tc.drifted {
				pdb.Spec.MinAvailable = pdb.Spec.MaxUnavailable
				pdb.Spec.MaxUnavailable = nil
				pdb.Spec.Selector.MatchLabels["app"] = "other"
				if err := r.Client.Update(context.Background(), pdb); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.reconcilePodDisruptionBudget(context.Background(), z); err != nil {
				t.Fatal(err)
			}
			if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(pdb), pdb); err != nil {
				t.Fatal(err)
			}
			if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != int(tc.desired) {
				t.Fatalf("maxUnavailable = %v, want %d", pdb.Spec.MaxUnavailable, tc.desired)
			}
			if pdb.Spec.MinAvailable != nil || pdb.Spec.Selector.MatchLabels["app"] != "example" {
				t.Fatalf("PDB retained drifted owned fields: %+v", pdb.Spec)
			}
			if pdb.Annotations["external"] != "keep" || pdb.Status.DisruptionsAllowed != 1 ||
				pdb.Spec.UnhealthyPodEvictionPolicy == nil || *pdb.Spec.UnhealthyPodEvictionPolicy != policyv1.AlwaysAllow {
				t.Fatalf("reconciliation changed unrelated fields: %+v", pdb)
			}
		})
	}
}

func TestPDBNoChangeSkipsWrite(t *testing.T) {
	r, z, _ := newPDBTestReconciler(t, 1)
	writes := 0
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			writes++
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			writes++
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	for i := 0; i < 2; i++ {
		if err := r.reconcilePodDisruptionBudget(context.Background(), z); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 0 {
		t.Fatalf("unchanged PDB received %d writes", writes)
	}
}

func TestPDBPatchConflictRetries(t *testing.T) {
	r, z, pdb := newPDBTestReconciler(t, 1)
	z.Spec.MaxUnavailableReplicas = 2
	cl := r.Client
	r.Client = interceptor.NewClient(cl.(client.WithWatch), interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return apierrors.NewConflict(schema.GroupResource{Group: "policy", Resource: "poddisruptionbudgets"}, pdb.Name, errors.New("stale writer"))
		},
	})
	if err := r.reconcilePodDisruptionBudget(context.Background(), z); !apierrors.IsConflict(err) {
		t.Fatalf("expected patch conflict, got %v", err)
	}
	r.Client = cl
	if err := r.reconcilePodDisruptionBudget(context.Background(), z); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(pdb), pdb); err != nil {
		t.Fatal(err)
	}
	if pdb.Spec.MaxUnavailable.IntValue() != 2 {
		t.Fatalf("retry left maxUnavailable = %v, want 2", pdb.Spec.MaxUnavailable)
	}
}

func newPDBTestReconciler(t *testing.T, maxUnavailable int) (*ZookeeperClusterReconciler, *v1beta1.ZookeeperCluster, *policyv1.PodDisruptionBudget) {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1beta1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	z := &v1beta1.ZookeeperCluster{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default", UID: "ensemble-uid"}}
	z.Spec.MaxUnavailableReplicas = 1
	value := intstr.FromInt(maxUnavailable)
	evictionPolicy := policyv1.AlwaysAllow
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: z.Name, Namespace: z.Namespace, Annotations: map[string]string{"external": "keep"}},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable:             &value,
			Selector:                   &metav1.LabelSelector{MatchLabels: map[string]string{"app": "example"}},
			UnhealthyPodEvictionPolicy: &evictionPolicy,
		},
		Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 1},
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(pdb).WithStatusSubresource(pdb).Build()
	return &ZookeeperClusterReconciler{Client: cl, Scheme: s}, z, pdb
}

func TestFinalizerRetainPreservesPVCs(t *testing.T) {
	for _, foreignFinalizer := range []bool{false, true} {
		t.Run(strconv.FormatBool(foreignFinalizer), func(t *testing.T) {
			r, z, pvc := newFinalizerTestReconciler(t)
			ctx := context.Background()
			if foreignFinalizer {
				z.Finalizers = append(z.Finalizers, "other.example/hold")
			}
			z.Spec.Persistence.VolumeReclaimPolicy = v1beta1.VolumeReclaimPolicyRetain
			if err := r.Client.Update(ctx, z); err != nil {
				t.Fatal(err)
			}
			if err := r.Client.Delete(ctx, z); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(z)}); err != nil {
				t.Fatal(err)
			}
			err := r.Client.Get(ctx, client.ObjectKeyFromObject(z), z)
			if foreignFinalizer {
				if err != nil || len(z.Finalizers) != 1 || z.Finalizers[0] != "other.example/hold" {
					t.Fatalf("unrelated finalizer must survive: %v, %v", z.Finalizers, err)
				}
			} else if !apierrors.IsNotFound(err) {
				t.Fatalf("retained cluster still has finalizers %v: %v", z.Finalizers, err)
			}
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil || !pvc.DeletionTimestamp.IsZero() {
				t.Fatalf("Retain must leave the PVC untouched: %v", err)
			}
		})
	}
}

func TestFinalizerDeleteFailureRetainsFinalizer(t *testing.T) {
	for _, deleteErr := range []error{
		apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "data-example-0", errors.New("denied")),
		errors.New("temporary API failure"),
	} {
		t.Run(deleteErr.Error(), func(t *testing.T) {
			r, z, pvc := newFinalizerTestReconciler(t)
			ctx := context.Background()
			if err := r.Client.Delete(ctx, z); err != nil {
				t.Fatal(err)
			}
			cl := r.Client
			r.Client = interceptor.NewClient(cl.(client.WithWatch), interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
						return deleteErr
					}
					return c.Delete(ctx, obj, opts...)
				},
			})
			req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(z)}
			if _, err := r.Reconcile(ctx, req); !errors.Is(err, deleteErr) {
				t.Fatalf("cleanup error = %v, want %v", err, deleteErr)
			}
			if err := cl.Get(ctx, req.NamespacedName, z); err != nil || !utils.ContainsString(z.Finalizers, utils.ZkFinalizer) {
				t.Fatalf("failed cleanup lost its finalizer: %v, %v", z.Finalizers, err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil {
				t.Fatal(err)
			}
			r.Client = cl
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(ctx, req.NamespacedName, z); !apierrors.IsNotFound(err) {
				t.Fatalf("successful retry did not release cluster finalizer: %v", err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); !apierrors.IsNotFound(err) {
				t.Fatalf("successful retry did not delete PVC: %v", err)
			}
		})
	}
}

func TestFinalizerDeletionSkipsNormalReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name              string
		operatorFinalizer bool
		defaulted         bool
	}{
		{name: "no operator finalizer"},
		{name: "operator finalizer", operatorFinalizer: true},
		{name: "defaulted without operator finalizer", defaulted: true},
		{name: "defaulted with operator finalizer", operatorFinalizer: true, defaulted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, z, _ := newFinalizerTestReconciler(t)
			ctx := context.Background()
			if !tc.defaulted {
				z.Spec = v1beta1.ZookeeperClusterSpec{}
			}
			z.Spec.TriggerRollingRestart = !tc.defaulted
			originalSpec := z.DeepCopy().Spec
			z.Finalizers = []string{"other.example/hold"}
			if tc.operatorFinalizer {
				z.Finalizers = append(z.Finalizers, utils.ZkFinalizer)
			}
			if err := r.Client.Update(ctx, z); err != nil {
				t.Fatal(err)
			}
			if err := r.Client.Delete(ctx, z); err != nil {
				t.Fatal(err)
			}
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					return errors.New("terminating clusters must not create resources")
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					cluster, ok := obj.(*v1beta1.ZookeeperCluster)
					if !ok || !reflect.DeepEqual(cluster.Spec, originalSpec) || utils.ContainsString(cluster.Finalizers, utils.ZkFinalizer) {
						return errors.New("termination must only remove the operator finalizer")
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
			})
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(z)})
			if err != nil || result != (reconcile.Result{}) {
				t.Fatalf("terminating reconcile = %+v, %v", result, err)
			}
		})
	}
}

func TestFinalizerDeleteMissingOrTerminatingPVC(t *testing.T) {
	for _, tc := range []string{"already missing", "disappears after list", "PVC protection"} {
		t.Run(tc, func(t *testing.T) {
			r, z, pvc := newFinalizerTestReconciler(t)
			ctx := context.Background()
			switch tc {
			case "already missing":
				if err := r.Client.Delete(ctx, pvc); err != nil {
					t.Fatal(err)
				}
			case "disappears after list":
				r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
					Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
						if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
							if err := c.Delete(ctx, obj, opts...); err != nil {
								return err
							}
						}
						return c.Delete(ctx, obj, opts...)
					},
				})
			case "PVC protection":
				pvc.Finalizers = []string{"kubernetes.io/pvc-protection"}
				if err := r.Client.Update(ctx, pvc); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Client.Delete(ctx, z); err != nil {
				t.Fatal(err)
			}
			req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(z)}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			if err := r.Client.Get(ctx, req.NamespacedName, z); !apierrors.IsNotFound(err) {
				t.Fatalf("successful cleanup did not release cluster finalizer: %v", err)
			}
			if tc == "PVC protection" {
				if err := r.Client.Get(ctx, client.ObjectKeyFromObject(pvc), pvc); err != nil || pvc.DeletionTimestamp.IsZero() {
					t.Fatalf("accepted deletion must leave a protected PVC terminating: %v", err)
				}
			}
		})
	}
}

func TestFinalizerOrphanCleanupPropagatesDeleteFailure(t *testing.T) {
	r, z, pvc := newFinalizerTestReconciler(t)
	z.Spec.Replicas, z.Status.ReadyReplicas = 1, 1
	orphan := pvc.DeepCopy()
	orphan.Name, orphan.ResourceVersion = "data-example-1", ""
	if err := r.Client.Create(context.Background(), orphan); err != nil {
		t.Fatal(err)
	}
	deleteErr := errors.New("temporary API failure")
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return deleteErr
		},
	})
	if err := r.cleanupOrphanPVCs(context.Background(), z); !errors.Is(err, deleteErr) {
		t.Fatalf("orphan cleanup error = %v, want %v", err, deleteErr)
	}
}

func newFinalizerTestReconciler(t *testing.T) (*ZookeeperClusterReconciler, *v1beta1.ZookeeperCluster, *corev1.PersistentVolumeClaim) {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1beta1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	z := &v1beta1.ZookeeperCluster{ObjectMeta: metav1.ObjectMeta{
		Name: "example", Namespace: "default", UID: "ensemble-uid", Finalizers: []string{utils.ZkFinalizer},
	}}
	z.WithDefaults()
	z.Spec.Persistence.VolumeReclaimPolicy = v1beta1.VolumeReclaimPolicyDelete
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-example-0", Namespace: z.Namespace,
		Labels: map[string]string{"app": z.Name, "uid": string(z.UID)},
	}}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(z, pvc).WithStatusSubresource(z).Build()
	return &ZookeeperClusterReconciler{Client: cl, Scheme: s, Log: log}, z, pvc
}

func TestScaleMetadataFailureDoesNotChangeReplicas(t *testing.T) {
	for _, tc := range []struct {
		name             string
		current, desired int32
	}{
		{name: "up", current: 3, desired: 5},
		{name: "down", current: 5, desired: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, z, sts := newScaleMetadataTestReconciler(t, tc.current)
			z.Spec.Replicas = tc.desired
			metadataErr := errors.New("metadata unavailable")
			fault := &scaleMetadataClient{updateErr: metadataErr}
			r.NewZkClient = func() zk.ZookeeperClient { return fault }
			if err := r.reconcileStatefulSet(context.Background(), z); err == nil {
				t.Error("metadata failure did not stop reconciliation")
			}
			if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(sts), sts); err != nil {
				t.Fatal(err)
			}
			if *sts.Spec.Replicas != tc.current {
				t.Fatalf("replicas after metadata failure = %d, want %d", *sts.Spec.Replicas, tc.current)
			}
			fault.updateErr = nil
			if err := r.reconcileStatefulSet(context.Background(), z); err != nil {
				t.Fatal(err)
			}
			if err := r.Client.Get(context.Background(), client.ObjectKeyFromObject(sts), sts); err != nil {
				t.Fatal(err)
			}
			if *sts.Spec.Replicas != tc.desired {
				t.Fatalf("replicas after retry = %d, want %d", *sts.Spec.Replicas, tc.desired)
			}
		})
	}
}

func TestCanceledReconcileStopsClientWork(t *testing.T) {
	t.Run("pre-canceled makes no mutations", func(t *testing.T) {
		r, z, _ := newScaleMetadataTestReconciler(t, 3)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		mutations := 0
		r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				mutations++
				return c.Update(ctx, obj, opts...)
			},
		})
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(z)})
		if !errors.Is(err, context.Canceled) || mutations != 0 {
			t.Fatalf("canceled reconcile: error=%v mutations=%d, want context.Canceled and no mutations", err, mutations)
		}
	})

	t.Run("blocked helper receives cancellation", func(t *testing.T) {
		r, z, _ := newScaleMetadataTestReconciler(t, 3)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started, release := make(chan struct{}), make(chan struct{})
		defer close(release)
		r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					close(started)
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-release:
						return errors.New("test released blocked request")
					}
				}
				return c.Get(ctx, key, obj, opts...)
			},
		})
		done := make(chan error, 1)
		go func() {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(z)})
			done <- err
		}()
		select {
		case <-started:
		case err := <-done:
			t.Fatalf("did not reach blocked request: %v", err)
		case <-time.After(time.Second):
			t.Fatal("did not reach blocked request")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("reconcile error=%v, want context.Canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("cancellation did not interrupt blocked Kubernetes request")
		}
	})

	t.Run("different keys own independent sessions", func(t *testing.T) {
		r, z, _ := newScaleMetadataTestReconciler(t, 3)
		z.Spec.Replicas = 5
		if err := r.Client.Update(context.Background(), z); err != nil {
			t.Fatal(err)
		}
		other := z.DeepCopy()
		other.Name = "other"
		other.UID = "other-metadata-test"
		other.ResourceVersion = ""
		if err := r.Client.Create(context.Background(), other); err != nil {
			t.Fatal(err)
		}
		otherSts := zk.MakeStatefulSet(other)
		if err := controllerutil.SetControllerReference(other, otherSts, r.Scheme); err != nil {
			t.Fatal(err)
		}
		replicas := int32(3)
		otherSts.Spec.Replicas = &replicas
		if err := r.Client.Create(context.Background(), otherSts); err != nil {
			t.Fatal(err)
		}
		sessions := make(chan *cancellationSessionClient, 2)
		r.NewZkClient = func() zk.ZookeeperClient {
			session := &cancellationSessionClient{started: make(chan context.Context, 1), closed: make(chan struct{})}
			sessions <- session
			return session
		}
		start := func(ctx context.Context, cluster *v1beta1.ZookeeperCluster) (<-chan error, *cancellationSessionClient) {
			t.Helper()
			done := make(chan error, 1)
			go func() {
				_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cluster)})
				done <- err
			}()
			select {
			case session := <-sessions:
				select {
				case operationCtx := <-session.started:
					deadline, ok := operationCtx.Deadline()
					if !ok || time.Until(deadline) > 30*time.Second {
						t.Error("ZooKeeper operation has no 30-second bound")
					}
					if parentDeadline, ok := ctx.Deadline(); ok && !deadline.Equal(parentDeadline) {
						t.Error("ZooKeeper operation did not preserve the earlier parent deadline")
					}
				case <-time.After(time.Second):
					t.Fatal("session did not start")
				}
				return done, session
			case err := <-done:
				t.Fatalf("reconcile did not start session: %v", err)
			case <-time.After(time.Second):
				t.Fatal("reconcile did not start session")
			}
			return nil, nil
		}
		firstCtx, firstCancel := context.WithCancel(context.Background())
		defer firstCancel()
		secondCtx, secondCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer secondCancel()
		firstDone, first := start(firstCtx, z)
		secondDone, second := start(secondCtx, other)
		firstCancel()
		select {
		case err := <-firstDone:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("first reconcile error=%v, want context.Canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("first reconcile did not stop")
		}
		select {
		case <-first.closed:
		default:
			t.Fatal("first reconcile did not close its session")
		}
		select {
		case <-second.closed:
			t.Fatal("canceling first reconcile closed second session")
		case err := <-secondDone:
			t.Fatalf("canceling first reconcile stopped second: %v", err)
		default:
		}
		secondCancel()
		select {
		case err := <-secondDone:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("second reconcile error=%v, want context.Canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("second reconcile did not stop")
		}
		select {
		case <-second.closed:
		default:
			t.Fatal("second reconcile did not close its session")
		}
	})
}

type cancellationSessionClient struct {
	MockZookeeperClient
	started chan context.Context
	closed  chan struct{}
}

func (c *cancellationSessionClient) Connect(ctx context.Context, _ string) error {
	c.started <- ctx
	<-ctx.Done()
	return ctx.Err()
}

func (c *cancellationSessionClient) Close() { close(c.closed) }

func TestResourceWriteGuards(t *testing.T) {
	r, z, _ := newScaleMetadataTestReconciler(t, 1)
	exerciseResourceWriteGuards(t, r.Client.(client.WithWatch), z)
}

func exerciseResourceWriteGuards(t *testing.T, cl client.WithWatch, z *v1beta1.ZookeeperCluster) {
	t.Helper()
	ctx := t.Context()
	z.Spec.Pod.ServiceAccountName = "example-account"
	z.Spec.Pod.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "old-secret"}}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		resource  client.Object
		reconcile func(*ZookeeperClusterReconciler, context.Context, *v1beta1.ZookeeperCluster) error
	}{
		{"ConfigMap", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: z.ConfigMapName(), Namespace: z.Namespace}}, (*ZookeeperClusterReconciler).reconcileConfigMap},
		{"ServiceAccount", &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: z.Spec.Pod.ServiceAccountName, Namespace: z.Namespace}}, (*ZookeeperClusterReconciler).reconcileStatefulSet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &ZookeeperClusterReconciler{Client: cl, APIReader: cl, Scheme: cl.Scheme(), NewZkClient: func() zk.ZookeeperClient { return &MockZookeeperClient{} }, Log: log}
			if err := tc.reconcile(r, ctx, z); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(tc.resource), tc.resource); err != nil {
				t.Fatal(err)
			}
			tc.resource.SetAnnotations(map[string]string{"external": "keep"})
			if err := cl.Update(ctx, tc.resource); err != nil {
				t.Fatal(err)
			}
			t.Run("unchanged", func(t *testing.T) {
				r.Client = rejectResourceWrites(cl, tc.resource)
				defer func() { r.Client = cl }()
				for range 2 {
					if err := tc.reconcile(r, ctx, z); err != nil {
						t.Fatal(err)
					}
				}
			})
			t.Run("change and conflict retry", func(t *testing.T) {
				switch tc.resource.(type) {
				case *corev1.ConfigMap:
					z.Spec.Conf.TickTime = 4000
				case *corev1.ServiceAccount:
					z.Spec.Pod.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "new-secret"}}
				}
				if err := cl.Update(ctx, z); err != nil {
					t.Fatal(err)
				}
				concurrentWrite := func(ctx context.Context, c client.WithWatch, obj client.Object) error {
					if reflect.TypeOf(obj) == reflect.TypeOf(tc.resource) {
						if err := c.Get(ctx, client.ObjectKeyFromObject(tc.resource), tc.resource); err != nil {
							return err
						}
						tc.resource.GetAnnotations()["external"] = "concurrent"
						return c.Update(ctx, tc.resource)
					}
					return nil
				}
				r.Client = interceptor.NewClient(cl, interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						if err := concurrentWrite(ctx, c, obj); err != nil {
							return err
						}
						return c.Update(ctx, obj, opts...)
					},
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if err := concurrentWrite(ctx, c, obj); err != nil {
							return err
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				})
				defer func() { r.Client = cl }()
				if err := tc.reconcile(r, ctx, z); !apierrors.IsConflict(err) {
					t.Fatalf("stale write must conflict, got %v", err)
				}
				r.Client = cl
				if err := tc.reconcile(r, ctx, z); err != nil {
					t.Fatal(err)
				}
				if err := cl.Get(ctx, client.ObjectKeyFromObject(tc.resource), tc.resource); err != nil {
					t.Fatal(err)
				}
				if tc.resource.GetAnnotations()["external"] != "concurrent" {
					t.Fatal("retry overwrote the concurrent annotation")
				}
				switch resource := tc.resource.(type) {
				case *corev1.ConfigMap:
					if !strings.Contains(resource.Data["zoo.cfg"], "tickTime=4000\n") {
						t.Fatal("changed configuration did not reach the ConfigMap")
					}
				case *corev1.ServiceAccount:
					if len(resource.ImagePullSecrets) != 1 || resource.ImagePullSecrets[0].Name != "new-secret" {
						t.Fatalf("imagePullSecrets were not updated: %v", resource.ImagePullSecrets)
					}
				}
			})
			t.Run("owned field removal", func(t *testing.T) {
				switch resource := tc.resource.(type) {
				case *corev1.ConfigMap:
					resource.BinaryData = map[string][]byte{"obsolete": []byte("old")}
					if err := cl.Update(ctx, resource); err != nil {
						t.Fatal(err)
					}
				case *corev1.ServiceAccount:
					z.Spec.Pod.ImagePullSecrets = nil
					if err := cl.Update(ctx, z); err != nil {
						t.Fatal(err)
					}
				}
				if err := tc.reconcile(r, ctx, z); err != nil {
					t.Fatal(err)
				}
				if err := cl.Get(ctx, client.ObjectKeyFromObject(tc.resource), tc.resource); err != nil {
					t.Fatal(err)
				}
				switch resource := tc.resource.(type) {
				case *corev1.ConfigMap:
					if len(resource.BinaryData) != 0 {
						t.Fatal("obsolete binary data was retained")
					}
				case *corev1.ServiceAccount:
					if len(resource.ImagePullSecrets) != 0 {
						t.Fatal("removed imagePullSecrets were retained")
					}
				}
				r.Client = rejectResourceWrites(cl, tc.resource)
				defer func() { r.Client = cl }()
				if err := tc.reconcile(r, ctx, z); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestServiceAccountNoChangeStillScalesStatefulSet(t *testing.T) {
	r, z, sts := newScaleMetadataTestReconciler(t, 3)
	z.Spec.Pod.ServiceAccountName = "example-account"
	if err := r.Client.Update(t.Context(), z); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileStatefulSet(t.Context(), z); err != nil {
		t.Fatal(err)
	}
	z.Spec.Replicas = 5
	if err := r.Client.Update(t.Context(), z); err != nil {
		t.Fatal(err)
	}
	r.Client = rejectResourceWrites(r.Client.(client.WithWatch), &corev1.ServiceAccount{})
	if err := r.reconcileStatefulSet(t.Context(), z); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Get(t.Context(), client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	if *sts.Spec.Replicas != 5 {
		t.Fatalf("unchanged ServiceAccount stopped StatefulSet scaling: replicas=%d", *sts.Spec.Replicas)
	}
}

func rejectResourceWrites(cl client.WithWatch, resource client.Object) client.WithWatch {
	reject := func(obj client.Object) error {
		if reflect.TypeOf(obj) == reflect.TypeOf(resource) {
			return fmt.Errorf("unchanged %T/%s received a write", obj, obj.GetName())
		}
		return nil
	}
	return interceptor.NewClient(cl, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := reject(obj); err != nil {
				return err
			}
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := reject(obj); err != nil {
				return err
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
}

func TestScaleMetadataStatefulSetConflictRetries(t *testing.T) {
	r, z, sts := newScaleMetadataTestReconciler(t, 3)
	z.Spec.Replicas = 5
	cl := r.Client
	r.Client = interceptor.NewClient(cl.(client.WithWatch), interceptor.Funcs{
		Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "statefulsets"}, sts.Name, errors.New("stale writer"))
		},
	})
	if err := r.reconcileStatefulSet(context.Background(), z); !apierrors.IsConflict(err) {
		t.Fatalf("expected StatefulSet conflict, got %v", err)
	}
	r.Client = cl
	if err := r.reconcileStatefulSet(context.Background(), z); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	if *sts.Spec.Replicas != 5 {
		t.Fatalf("replicas after retry = %d, want 5", *sts.Spec.Replicas)
	}
}

func newScaleMetadataTestReconciler(t *testing.T, replicas int32) (*ZookeeperClusterReconciler, *v1beta1.ZookeeperCluster, *appsv1.StatefulSet) {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1beta1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	z := &v1beta1.ZookeeperCluster{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default", UID: "metadata-test"}}
	z.WithDefaults()
	z.Spec.Replicas = replicas
	sts := zk.MakeStatefulSet(z)
	if err := controllerutil.SetControllerReference(z, sts, s); err != nil {
		t.Fatal(err)
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(z, sts).WithStatusSubresource(z, sts).WithReturnManagedFields().Build()
	return &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return &MockZookeeperClient{} }, Log: log}, z, sts
}

type scaleMetadataClient struct {
	MockZookeeperClient
	updateErr error
}

func (c *scaleMetadataClient) UpdateNode(context.Context, string, string, int32) error {
	return c.updateErr
}

type MockZookeeperClient struct {
	// dummy struct
}

func (client *MockZookeeperClient) Connect(ctx context.Context, zkUri string) (err error) {
	// do nothing
	return nil
}

func (client *MockZookeeperClient) CreateNode(ctx context.Context, zoo *v1beta1.ZookeeperCluster, zNodePath string) (err error) {
	return nil
}

func (client *MockZookeeperClient) UpdateNode(ctx context.Context, path string, data string, version int32) (err error) {
	return nil
}

func (client *MockZookeeperClient) NodeExists(ctx context.Context, zNodePath string) (version int32, err error) {
	return 0, nil
}

func (client *MockZookeeperClient) Close() {
	return
}

var _ = Describe("ZookeeperCluster Controller", func() {
	const (
		Name      = "example"
		Namespace = "default"
	)

	var (
		s            = scheme.Scheme
		mockZkClient = new(MockZookeeperClient)
		r            *ZookeeperClusterReconciler
	)
	makeOwnedStatefulSet := func(z *v1beta1.ZookeeperCluster) *appsv1.StatefulSet {
		sts := zk.MakeStatefulSet(z)
		Ω(controllerutil.SetControllerReference(z, sts, s)).To(Succeed())
		return sts
	}

	Context("Reconcile", func() {
		var (
			res reconcile.Result
			req reconcile.Request
			z   *v1beta1.ZookeeperCluster
		)

		BeforeEach(func() {
			req = reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      Name,
					Namespace: Namespace,
				},
			}
			z = &v1beta1.ZookeeperCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      Name,
					Namespace: Namespace,
					UID:       "test-zookeeper-uid",
				},
			}
			s.AddKnownTypes(v1beta1.GroupVersion, z)
		})

		Context("Logging", func() {
			var cl client.WithWatch

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				z.Spec.Pod.Annotations = map[string]string{"custom": "keep"}
				cl = fake.NewClientBuilder().WithScheme(s).
					WithRuntimeObjects(z, zk.MakeConfigMap(z), makeOwnedStatefulSet(z)).
					WithStatusSubresource(z).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
			})

			It("should not roll an existing StatefulSet with unchanged default logging", func() {
				sts := &appsv1.StatefulSet{}
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				original := sts.Spec.Template.DeepCopy()

				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).NotTo(HaveOccurred())
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				Ω(sts.Spec.Template).To(Equal(*original))
			})

			It("should apply logging updates and repairs without repeated template changes", func() {
				for _, logging := range []v1beta1.LoggingConfig{
					{Level: "WARN", Format: "text"},
					{Level: "WARN", Format: "json"},
					{Level: "INFO", Format: "text"},
				} {
					Ω(cl.Get(context.TODO(), req.NamespacedName, z)).To(Succeed())
					z.Spec.Logging = logging
					Ω(cl.Update(context.TODO(), z)).To(Succeed())
					_, err := r.Reconcile(context.TODO(), req)
					Ω(err).NotTo(HaveOccurred())
					cm := &corev1.ConfigMap{}
					cmKey := types.NamespacedName{Name: z.ConfigMapName(), Namespace: Namespace}
					Ω(cl.Get(context.TODO(), cmKey, cm)).To(Succeed())
					Ω(cm.Data["logback.xml"]).To(ContainSubstring(`<root level="` + logging.Level + `">`))
					sts := &appsv1.StatefulSet{}
					Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
					Ω(sts.Spec.Template.Annotations).To(HaveKeyWithValue("custom", "keep"))
					Ω(sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-config"]).To(Equal(cm.Data["logback.xml"]))
					var conf corev1.Volume
					for _, volume := range sts.Spec.Template.Spec.Volumes {
						if volume.Name == "conf" {
							conf = volume
						}
					}
					Ω(conf.Projected).NotTo(BeNil())
					if logging.Level == "INFO" && logging.Format == "text" {
						Ω(sts.Spec.Template.Annotations).NotTo(HaveKey("zookeeper.pravega.io/logback-hash"))
					} else {
						Ω(sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-hash"]).NotTo(BeEmpty())
					}
					template := sts.Spec.Template.DeepCopy()
					expectedLogback := cm.Data["logback.xml"]
					cm.Data["logback.xml"] = "manual edit"
					Ω(cl.Update(context.TODO(), cm)).To(Succeed())
					_, err = r.Reconcile(context.TODO(), req)
					Ω(err).NotTo(HaveOccurred())
					Ω(cl.Get(context.TODO(), cmKey, cm)).To(Succeed())
					Ω(cm.Data["logback.xml"]).To(Equal(expectedLogback))
					Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
					Ω(sts.Spec.Template).To(Equal(*template))
					Ω(cl.Get(context.TODO(), req.NamespacedName, z)).To(Succeed())
					Ω(z.Spec.TriggerRollingRestart).To(BeFalse())
					Ω(z.Spec.Pod.Annotations).To(Equal(map[string]string{"custom": "keep"}))
				}
			})

			It("should not update the pod template when the ConfigMap update fails", func() {
				sts := &appsv1.StatefulSet{}
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				original := sts.Spec.Template.DeepCopy()
				z.Spec.Logging.Level = "WARN"
				Ω(cl.Update(context.TODO(), z)).To(Succeed())
				r.Client = interceptor.NewClient(cl, interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if _, ok := obj.(*corev1.ConfigMap); ok {
							return errors.New("config update failed")
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				})
				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).To(MatchError("config update failed"))
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				Ω(sts.Spec.Template).To(Equal(*original))
			})

			It("should reject invalid logging before changing managed resources", func() {
				z.Spec.Logging.Level = "invalid"
				Ω(cl.Update(context.TODO(), z)).To(Succeed())
				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).To(MatchError(ContainSubstring("spec.logging.level")))
			})

			It("should reject logging changes for the previous default image before rolling pods", func() {
				sts := &appsv1.StatefulSet{}
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				original := sts.Spec.Template.DeepCopy()
				z.Spec.Image.Tag = "3.8.4-0.2.15-adobe-20250923"
				z.Spec.Logging.Level = "WARN"
				Ω(cl.Update(context.TODO(), z)).To(Succeed())

				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).To(MatchError(ContainSubstring("upgrade")))
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				Ω(sts.Spec.Template).To(Equal(*original))
			})

			It("should validate fresh logging settings when the informer has an old CRD schema", func() {
				sts := &appsv1.StatefulSet{}
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				original := sts.Spec.Template.DeepCopy()
				z.Spec.Image.Tag = "3.8.4-0.2.15-adobe-20250923"
				Ω(cl.Update(context.TODO(), z)).To(Succeed())
				fresh := z.DeepCopy()
				fresh.Spec.Logging.Level = "WARN"
				r.APIReader = fake.NewClientBuilder().WithScheme(s).WithObjects(fresh).Build()

				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).To(MatchError(ContainSubstring("upgrade")))
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				Ω(sts.Spec.Template).To(Equal(*original))
			})

			It("should finalize a cluster with incompatible logging when it is deleted", func() {
				z.Spec.Image.Tag = "3.8.4-0.2.15-adobe-20250923"
				z.Spec.Logging.Level = "WARN"
				z.Spec.Persistence.VolumeReclaimPolicy = v1beta1.VolumeReclaimPolicyDelete
				z.Finalizers = []string{utils.ZkFinalizer}
				Ω(cl.Update(context.TODO(), z)).To(Succeed())
				Ω(cl.Delete(context.TODO(), z)).To(Succeed())

				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).NotTo(HaveOccurred())
				Ω(cl.Get(context.TODO(), req.NamespacedName, &v1beta1.ZookeeperCluster{})).NotTo(Succeed())
			})

			It("should project default logging when reverting an older customized StatefulSet", func() {
				sts := &appsv1.StatefulSet{}
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-hash"] = "previous"
				Ω(cl.Update(context.TODO(), sts)).To(Succeed())

				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).NotTo(HaveOccurred())
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				Ω(sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-config"]).To(ContainSubstring(`<root level="INFO">`))
				for _, volume := range sts.Spec.Template.Spec.Volumes {
					if volume.Name == "conf" {
						Ω(volume.Projected).NotTo(BeNil())
					}
				}
			})

			It("should project logging when recreating a missing StatefulSet", func() {
				sts := &appsv1.StatefulSet{}
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				Ω(cl.Delete(context.TODO(), sts)).To(Succeed())

				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).NotTo(HaveOccurred())
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				Ω(sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-config"]).To(ContainSubstring(`<root level="INFO">`))
			})
		})

		Context("Before defaults are applied", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(z).WithStatusSubresource(z).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("shouldn't error", func() {
				Ω(err).To(BeNil())
			})

			It("should set the default zk spec options", func() {
				foundZk := &v1beta1.ZookeeperCluster{}
				err = cl.Get(context.TODO(), req.NamespacedName, foundZk)
				Ω(err).To(BeNil())
				Ω(foundZk.Spec.Replicas).To(BeEquivalentTo(3))
			})

			It("should requeue the request", func() {
				Ω(res.Requeue).To(BeTrue())
			})
		})

		Context("After defaults are applied", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(z).WithStatusSubresource(z).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not error", func() {
				Ω(err).To(BeNil())
			})

			It("should requeue after ReconcileTime delay", func() {
				Ω(res.RequeueAfter).To(Equal(ReconcileTime))
			})

			It("should create a config-map", func() {
				foundCm := &corev1.ConfigMap{}
				nn := types.NamespacedName{
					Name:      Name + "-configmap",
					Namespace: Namespace,
				}
				err = cl.Get(context.TODO(), nn, foundCm)
				Ω(err).To(BeNil())
			})

			It("should create a stateful-set", func() {
				foundSts := &appsv1.StatefulSet{}
				err = cl.Get(context.TODO(), req.NamespacedName, foundSts)
				Ω(err).To(BeNil())
				Ω(*foundSts.Spec.Replicas).To(BeEquivalentTo(3))
			})

			It("should create a client-service", func() {
				foundSvc := &corev1.Service{}
				nn := types.NamespacedName{
					Name:      Name + "-client",
					Namespace: Namespace,
				}
				err = cl.Get(context.TODO(), nn, foundSvc)
				Ω(err).To(BeNil())
			})

			It("should create a headless-service", func() {
				foundSvc := &corev1.Service{}
				nn := types.NamespacedName{
					Name:      Name + "-headless",
					Namespace: Namespace,
				}
				err = cl.Get(context.TODO(), nn, foundSvc)
				Ω(err).To(BeNil())
			})

			It("should create a pdb", func() {
				foundPdb := &policyv1.PodDisruptionBudget{}
				err = cl.Get(context.TODO(), req.NamespacedName, foundPdb)
				Ω(err).To(BeNil())
			})

		})

		Context("With update to sts", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Spec.Pod.ServiceAccountName = "zookeeper"
				z.Status.Init()
				next := z.DeepCopy()
				st := makeOwnedStatefulSet(z)
				next.Spec.Replicas = 6
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, st).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})

			It("should update the sts", func() {
				foundSts := &appsv1.StatefulSet{}
				err = cl.Get(context.TODO(), req.NamespacedName, foundSts)
				Ω(err).To(BeNil())
				Ω(*foundSts.Spec.Replicas).To(BeEquivalentTo(6))
			})
		})

		Context("With no update to sts", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				next := z.DeepCopy()
				st := makeOwnedStatefulSet(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, st).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})

			It("should update the sts", func() {
				foundSts := &appsv1.StatefulSet{}
				err = cl.Get(context.TODO(), req.NamespacedName, foundSts)
				Ω(err).To(BeNil())
			})

		})

		Context("With update to ImagePullSecrets", func() {
			var (
				cl   client.Client
				err  error
				next *v1beta1.ZookeeperCluster
				sa   *corev1.ServiceAccount
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Spec.Pod.ServiceAccountName = "zookeeper"
				z.Status.Init()
				next = z.DeepCopy()
				sa = zk.MakeServiceAccount(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, sa).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})

			It("should create the service account", func() {
				foundSA := &corev1.ServiceAccount{}
				err = cl.Get(context.TODO(), types.NamespacedName{Name: "zookeeper", Namespace: Namespace}, foundSA)
				Ω(err).To(BeNil())
				Ω(foundSA.ImagePullSecrets).To(HaveLen(0))
			})
			It("should update the service account", func() {
				next.Spec.Pod.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "test-pull-secret"}}
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, sa).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).To(BeNil())

				foundSA := &corev1.ServiceAccount{}
				err = cl.Get(context.TODO(), types.NamespacedName{Name: "zookeeper", Namespace: Namespace}, foundSA)
				Ω(err).To(BeNil())
				Ω(foundSA.ImagePullSecrets).To(HaveLen(1))
			})
		})

		Context("upgrading the image for zookeepercluster", func() {
			var (
				cl  client.Client
				err error
			)
			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				next := z.DeepCopy()
				next.Spec.Image.Tag = "0.2.7"
				next.Status.CurrentVersion = "0.2.6"
				next.Status.SetPodsReadyConditionTrue()
				st := makeOwnedStatefulSet(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, st).WithStatusSubresource(next).Build()
				st = &appsv1.StatefulSet{}
				err = cl.Get(context.TODO(), req.NamespacedName, st)
				// changing the Revision value to simulate the upgrade scenario
				st.Status.CurrentRevision = "CurrentRevision"
				st.Status.UpdateRevision = "UpdateRevision"
				cl.Status().Update(context.TODO(), st)
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})

			It("should set the upgrade condition to true", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				Ω(err).To(BeNil())
				Ω(foundZookeeper.Status.IsClusterInUpgradingState()).To(BeTrue())
			})

			It("should set the target version", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				Ω(err).To(BeNil())
				Ω(foundZookeeper.Status.TargetVersion).To(BeEquivalentTo("0.2.7"))
			})

			It("should set the target version", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)

				Ω(err).To(BeNil())
				Ω(foundZookeeper.Status.TargetVersion).To(BeEquivalentTo("0.2.7"))
			})

			It("should check if the cluster is in upgrade failed state", func() {
				z.Status.SetErrorConditionTrue("UpgradeFailed", " ")
				cl.Status().Update(context.TODO(), z)
				res, err = r.Reconcile(context.TODO(), req)
				Ω(err).To(BeNil())
			})
		})

		Context("Checking for upgrade completion for zookeepercluster", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				next := z.DeepCopy()
				next.Spec.Image.Tag = "0.2.7"
				next.Status.CurrentVersion = "0.2.6"
				next.Status.TargetVersion = "0.2.7"
				next.Status.SetUpgradingConditionTrue(" ", " ")
				st := makeOwnedStatefulSet(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, st).WithStatusSubresource(next).Build()
				st = &appsv1.StatefulSet{}
				err = cl.Get(context.TODO(), req.NamespacedName, st)
				// changing the Revision value to simulate the upgrade scenario completion
				st.Status.CurrentRevision = "complete"
				st.Status.UpdateRevision = "complete"
				cl.Status().Update(context.TODO(), st)
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				res, err = r.Reconcile(context.TODO(), req)
				res, err = r.Reconcile(context.TODO(), req)
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})

			It("should set the currentversion to Image.tag", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				Ω(err).To(BeNil())
				Ω(foundZookeeper.Status.CurrentVersion).To(BeEquivalentTo("0.2.7"))
			})

			It("should set the target version to empty", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				Ω(err).To(BeNil())
				Ω(foundZookeeper.Status.TargetVersion).To(BeEquivalentTo(""))
			})
		})

		Context("Checking for upgrade failed for zookeepercluster", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				next := z.DeepCopy()
				next.Status.SetUpgradingConditionTrue(" ", "1")
				next.Status.TargetVersion = "0.2.7"
				st := makeOwnedStatefulSet(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, st).WithStatusSubresource(next).Build()
				st = &appsv1.StatefulSet{}
				err = cl.Get(context.TODO(), req.NamespacedName, st)
				// changing the Revision value to simulate the upgrade scenario
				st.Status.CurrentRevision = "currentRevision"
				st.Status.UpdateRevision = "updateRevision"
				st.Status.UpdatedReplicas = 2
				cl.Status().Update(context.TODO(), st)
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
				// sleeping for 3 seconds
				time.Sleep(3 * time.Second)
				// checking if more than 2 secs have passed from the last update time
				err = checkSyncTimeout(next, " ", 1, 2*time.Second)

			})

			It("checking update replicas", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				_, condition := foundZookeeper.Status.GetClusterCondition(v1beta1.ClusterConditionUpgrading)
				Ω(condition.Message).To(Equal("2"))
			})

			It("should raise an error", func() {
				Ω(err.Error()).To(Equal("progress deadline exceeded"))
			})
		})

		Context("Checking for healing of upgrade failed for zookeepercluster", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				next := z.DeepCopy()
				next.Status.SetErrorConditionTrue("UpgradeFailed", " ")
				next.Status.TargetVersion = "0.2.7"
				next.Status.CurrentVersion = "0.2.6"
				next.Status.ReadyReplicas = 3
				next.Spec.Replicas = 3
				next.Spec.Image.Tag = "0.2.7"
				st := makeOwnedStatefulSet(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, st).WithStatusSubresource(next).Build()
				st = &appsv1.StatefulSet{}
				err = cl.Get(context.TODO(), req.NamespacedName, st)
				// changing the Revision value to simulate the upgrade scenario
				st.Status.CurrentRevision = "updateRevision"
				st.Status.UpdateRevision = "updateRevision"
				st.Status.UpdatedReplicas = 2
				cl.Status().Update(context.TODO(), st)
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
				// sleeping for 3 seconds
				time.Sleep(3 * time.Second)
				// checking if more than 2 secs have passed from the last update time
				err = checkSyncTimeout(next, " ", 1, 2*time.Second)

			})

			It("checking update replicas", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				condition := foundZookeeper.Status.CurrentVersion
				Ω(condition).To(Equal("0.2.7"))
			})

			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})
		})

		Context("Upgrading with Targetversion empty", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				next := z.DeepCopy()
				next.Spec.Image.Tag = "0.2.7"
				next.Status.CurrentVersion = "0.2.6"
				next.Status.TargetVersion = ""
				next.Status.IsClusterInUpgradingState()
				st := makeOwnedStatefulSet(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, st).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})
			It("should set the upgrade condition to false", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				Ω(foundZookeeper.Status.IsClusterInUpgradingState()).To(Equal(false))
			})
		})

		Context("Checking result when request namespace does not contains zookeeper cluster", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(z).WithStatusSubresource(z).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				req.NamespacedName.Namespace = "temp"
				res, err = r.Reconcile(context.TODO(), req)
			})
			It("should have false in reconcile result", func() {
				Ω(res.Requeue).To(Equal(false))
				Ω(err).To(BeNil())
			})
		})

		Context("Checking client", func() {
			var (
				cl    client.Client
				err   error
				count int
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(z).WithStatusSubresource(z).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				err = mockZkClient.Connect(context.Background(), "127.0.0.0:2181")
				Ω(err).To(BeNil())
			})
			It("should not raise an error", func() {
				err = r.GenerateYAML(z)
				Ω(err).To(BeNil())
			})
			It("should not raise an error", func() {
				err = r.cleanupOrphanPVCs(context.Background(), z)
				Ω(err).To(BeNil())
			})
			It("should not raise an error", func() {
				z.Status.ReadyReplicas = -1
				z.Spec.Replicas = -1
				err = cl.Update(context.TODO(), z)
				err = r.cleanupOrphanPVCs(context.Background(), z)
				Ω(err).To(BeNil())
			})
			It("should not raise an error", func() {
				count, err = r.getPVCCount(context.Background(), z)
				_, err = r.getPVCList(context.Background(), z)
				Ω(err).To(BeNil())
				Ω(count).To(Equal(0))
			})
			It("should not raise an error", func() {
				_ = cl.Get(context.TODO(), req.NamespacedName, z)
				z.Spec.Persistence.VolumeReclaimPolicy = v1beta1.VolumeReclaimPolicyDelete
				cl.Update(context.TODO(), z)
				err = r.reconcileFinalizers(context.Background(), z)
				Ω(err).To(BeNil())
			})

			It("should delete pvc", func() {
				pvcDelete := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "Name",
						Namespace: "Namespace",
					},
				}
				Ω(r.Client.Create(context.TODO(), pvcDelete)).To(Succeed())
				Ω(r.deletePVC(context.Background(), *pvcDelete)).To(Succeed())
				Ω(r.deletePVC(context.Background(), *pvcDelete)).To(Succeed())
			})

			It("should not raise an error", func() {
				err = r.cleanUpAllPVCs(context.Background(), z)
				_ = os.RemoveAll("ZookeeperCluster")
				Ω(err).To(BeNil())
			})

			It("calling YamlExporterReconciler", func() {
				recon := YAMLExporterReconciler(z)
				Ω(recon).NotTo(BeNil())
			})
		})

		Context("With an update to the client svc", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				next := z.DeepCopy()
				next.Spec.Ports[0].ContainerPort = 2182
				svc := zk.MakeClientService(z)
				Ω(controllerutil.SetControllerReference(next, svc, s)).To(Succeed())
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, svc).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				Ω(err).ToNot(HaveOccurred())
			})
		})

		Context("reconcileFinalizers", func() {
			var (
				cl  client.Client
				err error
			)
			BeforeEach(func() {
				z.WithDefaults()
				z.Spec.Persistence = nil
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(z).WithStatusSubresource(z).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
				err = r.reconcileFinalizers(context.Background(), z)
				// update deletion timestamp
				_ = cl.Get(context.TODO(), req.NamespacedName, z)
				now := metav1.Now()
				z.SetDeletionTimestamp(&now)
				cl.Update(context.TODO(), z)
				err = r.reconcileFinalizers(context.Background(), z)
			})
			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})
		})

		Context("reconcileFinalizers", func() {
			var (
				cl  client.Client
				err error
			)
			BeforeEach(func() {
				z.WithDefaults()
				z.Spec.Persistence = nil
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(z).WithStatusSubresource(z).Build()
			})
			It("should have 1 finalizer, should not raise an error", func() {
				config.DisableFinalizer = false
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				err = r.reconcileFinalizers(context.Background(), z)
				Expect(z.ObjectMeta.Finalizers).To(HaveLen(1))
				Ω(err).To(BeNil())
			})
			It("should have 0 finalizer, should not raise an error", func() {
				config.DisableFinalizer = true
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				err = r.reconcileFinalizers(context.Background(), z)
				Expect(z.ObjectMeta.Finalizers).To(HaveLen(0))
				Ω(err).To(BeNil())
			})
		})

		Context("trigger rolling restart", func() {
			var (
				cl      client.Client
				err     error
				foundZk = &v1beta1.ZookeeperCluster{}
				next    *v1beta1.ZookeeperCluster
			)
			BeforeEach(func() {
				z.WithDefaults()
				next = z.DeepCopy()
				next.Spec.TriggerRollingRestart = true
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
				err = cl.Get(context.TODO(), req.NamespacedName, foundZk)
			})

			It("should update restartTime annotation and reset triggerRollingRestart to false when triggerRollingRestart is set to true", func() {
				Ω(res.Requeue).To(Equal(true))
				Ω(err).To(BeNil())
				Ω(foundZk.Spec.TriggerRollingRestart).To(Equal(false))
				_, restartTimeExists := foundZk.Spec.Pod.Annotations["restartTime"]
				Ω(restartTimeExists).To(Equal(true))
			})

			It("should not update restartTime annotation when set triggerRollingRestart to false", func() {
				_, restartTimeExists := foundZk.Spec.Pod.Annotations["restartTime"]
				Ω(restartTimeExists).To(Equal(true))

				next.Spec.TriggerRollingRestart = false
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)

				Ω(res.Requeue).To(Equal(false))
				Ω(err).To(BeNil())
				Ω(foundZk.Spec.TriggerRollingRestart).To(Equal(false))
				_, restartTimeExists = foundZk.Spec.Pod.Annotations["restartTime"]
				Ω(restartTimeExists).To(Equal(true))
			})

			It("should update restartTime annotation to new value when rolling restart is triggered multiple times", func() {
				oldRestartValue, restartTimeExists := foundZk.Spec.Pod.Annotations["restartTime"]
				Ω(restartTimeExists).To(Equal(true))

				// wait 1 second to ensure that restartTime, if set, will have a different value
				time.Sleep(1 * time.Second)

				// update the crd instance
				next.Spec.TriggerRollingRestart = false
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
				err = cl.Get(context.TODO(), req.NamespacedName, foundZk)

				// check that restartTime was not updated
				Ω(res.Requeue).To(Equal(false))
				Ω(err).To(BeNil())
				Ω(foundZk.Spec.TriggerRollingRestart).To(Equal(false))
				_, restartTimeExists = foundZk.Spec.Pod.Annotations["restartTime"]
				Ω(restartTimeExists).To(Equal(true))

				// wait 1 second to ensure that restartTime, if set, will have a different value
				time.Sleep(1 * time.Second)

				// update the crd instance to trigger rolling restart
				next.Spec.TriggerRollingRestart = true
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, NewZkClient: func() zk.ZookeeperClient { return mockZkClient }}
				res, err = r.Reconcile(context.TODO(), req)
				err = cl.Get(context.TODO(), req.NamespacedName, foundZk)

				// check that restartTime was updated
				Ω(res.Requeue).To(Equal(true))
				Ω(err).To(BeNil())
				Ω(foundZk.Spec.TriggerRollingRestart).To(Equal(false))
				newRestartValue, restartTimeExists := foundZk.Spec.Pod.Annotations["restartTime"]
				Ω(restartTimeExists).To(Equal(true))
				Ω(oldRestartValue).NotTo(Equal(newRestartValue))
			})
		})
	})
})
