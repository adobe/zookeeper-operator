// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	api "github.com/pravega/zookeeper-operator/api/v1beta1"
	"github.com/pravega/zookeeper-operator/pkg/zk"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// workloadWrites are the StatefulSet and Service applies that every reconcile issues.
// The API server does not persist an apply that changes nothing; envtest checks this.
var workloadWrites = []string{"apply *v1.StatefulSet/", "apply *v1.Service/"}

func TestReconcileSettledClusterSkipsStatusAndConfigWrites(t *testing.T) {
	r, z, _ := newScaleMetadataTestReconciler(t, 1)
	cl := r.Client.(client.WithWatch)
	ctx := t.Context()
	z.Spec.Replicas = 4
	z.Spec.Pod.ServiceAccountName = "example-account"
	z.Spec.Conf.AdditionalConfig = map[string]string{"custom.a": "one", "custom.b": "two", "custom.c": "three"}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	r = convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	sts := &appsv1.StatefulSet{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(z), sts); err != nil {
		t.Fatal(err)
	}
	sts.Status.Replicas, sts.Status.ReadyReplicas = z.Spec.Replicas, 2
	sts.Status.CurrentRevision, sts.Status.UpdateRevision = "settled", "settled"
	if err := cl.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"member-a", "member-b", "member-c", "member-d"} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace, Labels: map[string]string{"app": z.Name}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "zookeeper", Image: "example:1"}}},
		}
		if err := cl.Create(ctx, pod); err != nil {
			t.Fatal(err)
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "zookeeper", Ready: i < 2}}
		if err := cl.Status().Update(ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	settleCluster(t, ctx, r, z)
	if !slices.Equal(z.Status.Members.Ready, []string{"member-a", "member-b"}) || !slices.Equal(z.Status.Members.Unready, []string{"member-c", "member-d"}) {
		t.Fatalf("expected both ready and unready members in canonical order: %+v", z.Status.Members)
	}
	var writes []string
	reverse := false
	r.Client = observeMutations(interceptor.NewClient(cl, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if pods, ok := list.(*corev1.PodList); ok && len(pods.Items) == 4 {
				reverse = !reverse
				if reverse {
					slices.Reverse(pods.Items)
				}
			}
			return nil
		},
	}), &writes)
	for range 5 {
		runReconcile(t, ctx, r, z)
	}
	if unexpected := writesExcept(writes, workloadWrites...); len(unexpected) != 0 {
		t.Fatalf("settled cluster must not rewrite status, config or metadata: %v", unexpected)
	}
}

func TestReconcileConflictRetries(t *testing.T) {
	r, z, _ := newScaleMetadataTestReconciler(t, 1)
	exerciseReconcileConflictRetries(t, t.Context(), r.Client.(client.WithWatch), z, "StatefulSet", "Service", "PodDisruptionBudget", "status")
}

func TestUnchangedStatusRejectsStaleCluster(t *testing.T) {
	r, z, _ := newScaleMetadataTestReconciler(t, 1)
	key := client.ObjectKeyFromObject(z)
	if err := r.Client.Get(t.Context(), key, z); err != nil {
		t.Fatal(err)
	}
	newer := z.DeepCopy()
	newer.Spec.AdminServerService.External = true
	if err := r.Client.Update(t.Context(), newer); err != nil {
		t.Fatal(err)
	}
	var writes []string
	r.Client = observeMutations(r.Client.(client.WithWatch), &writes)
	if err := r.patchClusterStatus(t.Context(), z); !apierrors.IsConflict(err) || len(writes) != 0 {
		t.Fatalf("stale snapshot with unchanged status must conflict without writing: err=%v writes=%v", err, writes)
	}
}

func TestReconcilePreservesUnownedContainerFields(t *testing.T) {
	r, z, sts := newScaleMetadataTestReconciler(t, 1)
	settleCluster(t, t.Context(), r, z)
	if err := r.Client.Get(t.Context(), client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	sts.Spec.Template.Spec.Containers[0].Args = []string{"external-argument"}
	if err := r.Client.Update(t.Context(), sts); err != nil {
		t.Fatal(err)
	}
	z.Spec.Pod.NodeSelector = map[string]string{"pool": "new"}
	if err := r.Client.Update(t.Context(), z); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, t.Context(), r, z)
	if err := r.Client.Get(t.Context(), client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.Template.Spec.NodeSelector["pool"] != "new" {
		t.Fatal("desired node selector was not applied")
	}
	if args := sts.Spec.Template.Spec.Containers[0].Args; len(args) != 1 || args[0] != "external-argument" {
		t.Fatal("Pod-template apply replaced unowned container arguments")
	}
}

func TestReconcileAppliesOnlyRenderedFields(t *testing.T) {
	r, z, sts := newScaleMetadataTestReconciler(t, 1)
	settleCluster(t, t.Context(), r, z)
	if err := r.Client.Get(t.Context(), client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	sts.Spec.Template.Spec.Containers[0].Args = []string{"external-argument"}
	if err := r.Client.Update(t.Context(), sts); err != nil {
		t.Fatal(err)
	}
	z.Spec.Image.Tag = "next"
	if err := r.Client.Update(t.Context(), z); err != nil {
		t.Fatal(err)
	}
	applied := false
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*appsv1.StatefulSet); ok {
				if patch.Type() != types.ApplyPatchType {
					t.Errorf("StatefulSet must be written with server-side apply, got %s", patch.Type())
				}
				data, err := patch.Data(obj)
				if err != nil {
					return err
				}
				applied = true
				var body map[string]any
				if err := json.Unmarshal(data, &body); err != nil {
					return err
				}
				if _, ok := body["status"]; ok || strings.Contains(string(data), `"args"`) {
					t.Errorf("apply included status or unowned container arguments: %s", data)
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	runReconcile(t, t.Context(), r, z)
	if !applied {
		t.Fatal("image change did not apply the StatefulSet")
	}
}

func TestReconcileRepairsOwnedFieldDrift(t *testing.T) {
	r, z, sts := newScaleMetadataTestReconciler(t, 1)
	settleCluster(t, t.Context(), r, z)
	if err := r.Client.Get(t.Context(), client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	sts.Spec.Template.Spec.Containers[0].Image = "external:drift"
	if err := r.Client.Update(t.Context(), sts); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, t.Context(), r, z)
	if err := r.Client.Get(t.Context(), client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.Template.Spec.Containers[0].Image != z.Spec.Image.ToString() {
		t.Fatal("owned-field drift was not corrected")
	}
}

func TestReconcileRemovesOnlyPreviouslyAppliedMetadata(t *testing.T) {
	r, z, sts := newScaleMetadataTestReconciler(t, 1)
	cl := r.Client.(client.WithWatch)
	ctx := t.Context()
	z.Spec.Pod.Annotations = map[string]string{"requested": "old"}
	z.Spec.Pod.Labels["custom"] = "old"
	z.Spec.ClientService.Annotations = map[string]string{"requested": "old"}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	settleCluster(t, ctx, r, z)
	svc := &corev1.Service{}
	if err := cl.Get(ctx, client.ObjectKey{Name: z.GetClientServiceName(), Namespace: z.Namespace}, svc); err != nil {
		t.Fatal(err)
	}
	svc.Annotations["external"] = "keep"
	if err := cl.Update(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	sts.Spec.Template.Annotations["external"] = "keep"
	sts.Spec.Template.Labels["external"] = "keep"
	if err := cl.Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	z.Spec.Pod.Annotations = nil
	delete(z.Spec.Pod.Labels, "custom")
	z.Spec.ClientService.Annotations = nil
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, ctx, r, z)
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(svc), svc); err != nil {
		t.Fatal(err)
	}
	if _, exists := sts.Spec.Template.Annotations["requested"]; exists {
		t.Error("removed CR annotation remains on Pod template")
	}
	if _, exists := sts.Spec.Template.Labels["custom"]; exists {
		t.Error("removed CR label remains on Pod template")
	}
	if _, exists := svc.Annotations["requested"]; exists {
		t.Error("removed CR annotation remains on Service")
	}
	if sts.Spec.Template.Annotations["external"] != "keep" ||
		sts.Spec.Template.Labels["external"] != "keep" || svc.Annotations["external"] != "keep" {
		t.Fatal("removing applied keys erased unrelated metadata")
	}
}

// exerciseReconcileConflictRetries injects a concurrent write before the reconciler's write to each kind
// and checks that the stale write conflicts and the retry keeps both the concurrent and the desired change.
func exerciseReconcileConflictRetries(t *testing.T, ctx context.Context, cl client.WithWatch, z *api.ZookeeperCluster, kinds ...string) {
	t.Helper()
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			if err := cl.Get(ctx, client.ObjectKeyFromObject(z), z); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "StatefulSet":
				z.Spec.Pod.NodeSelector = map[string]string{"pool": "new"}
			case "Service":
				z.Spec.ClientService.Annotations = map[string]string{"requested": "new"}
			case "PodDisruptionBudget":
				z.Spec.MaxUnavailableReplicas = 2
			case "status":
				z.Status.InternalClientEndpoint = "stale"
				if err := cl.Status().Update(ctx, z); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "status" {
				if err := cl.Update(ctx, z); err != nil {
					t.Fatal(err)
				}
			}
			injected := false
			concurrent := func(ctx context.Context, c client.Client, obj client.Object, status bool) error {
				matches := false
				switch obj.(type) {
				case *appsv1.StatefulSet:
					matches = kind == "StatefulSet"
				case *corev1.Service:
					matches = kind == "Service"
				case *policyv1.PodDisruptionBudget:
					matches = kind == "PodDisruptionBudget"
				case *api.ZookeeperCluster:
					matches = kind == "status" && status
				}
				if !matches || injected {
					return nil
				}
				injected = true
				fresh := obj.DeepCopyObject().(client.Object)
				if err := c.Get(ctx, client.ObjectKeyFromObject(obj), fresh); err != nil {
					return err
				}
				annotations := fresh.GetAnnotations()
				if annotations == nil {
					annotations = map[string]string{}
				}
				annotations["external"] = "concurrent"
				fresh.SetAnnotations(annotations)
				return c.Update(ctx, fresh)
			}
			r.Client = interceptor.NewClient(cl, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if err := concurrent(ctx, c, obj, false); err != nil {
						return err
					}
					return c.Update(ctx, obj, opts...)
				},
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if err := concurrent(ctx, c, obj, false); err != nil {
						return err
					}
					return c.Patch(ctx, obj, patch, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if err := concurrent(ctx, c, obj, true); err != nil {
						return err
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
				SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if err := concurrent(ctx, c, obj, true); err != nil {
						return err
					}
					return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
				},
			})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(z)})
			if !injected || !apierrors.IsConflict(err) {
				t.Fatalf("stale %s write must conflict: injected=%v err=%v", kind, injected, err)
			}
			r.Client = cl
			runReconcile(t, ctx, r, z)
			var obj client.Object
			key := client.ObjectKeyFromObject(z)
			switch kind {
			case "StatefulSet":
				obj = &appsv1.StatefulSet{}
			case "Service":
				obj = &corev1.Service{}
				key.Name = z.GetClientServiceName()
			case "PodDisruptionBudget":
				obj = &policyv1.PodDisruptionBudget{}
			default:
				obj = &api.ZookeeperCluster{}
			}
			if err := cl.Get(ctx, key, obj); err != nil {
				t.Fatal(err)
			}
			if obj.GetAnnotations()["external"] != "concurrent" {
				t.Fatal("retry overwrote concurrent metadata")
			}
			switch got := obj.(type) {
			case *appsv1.StatefulSet:
				if got.Spec.Template.Spec.NodeSelector["pool"] != "new" {
					t.Fatal("retry lost desired node selector")
				}
			case *corev1.Service:
				if got.Annotations["requested"] != "new" {
					t.Fatal("retry lost desired Service annotation")
				}
			case *policyv1.PodDisruptionBudget:
				if got.Spec.MaxUnavailable.IntValue() != 2 {
					t.Fatal("retry lost desired disruption budget")
				}
			case *api.ZookeeperCluster:
				if got.Status.InternalClientEndpoint == "stale" {
					t.Fatal("retry did not persist observed endpoint")
				}
			}
		})
	}
}

func TestLoggingAndUpgradeStatusRemainCorrect(t *testing.T) {
	r, z, sts := newScaleMetadataTestReconciler(t, 1)
	ctx := t.Context()
	cl := r.Client.(client.WithWatch)
	r.APIReader = cl
	settleCluster(t, ctx, r, z)
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	sts.Status.Replicas, sts.Status.ReadyReplicas = 1, 1
	sts.Status.CurrentRevision, sts.Status.UpdateRevision = "old", "logging"
	if err := cl.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	initialImage := z.Spec.Image.Tag
	z.Status.SetPodsReadyConditionTrue()
	z.Status.CurrentVersion = initialImage
	if err := cl.Status().Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	for _, logging := range []api.LoggingConfig{{Level: "WARN", Format: "json"}, {Level: "INFO", Format: "text"}} {
		z.Spec.Logging = logging
		if err := cl.Update(ctx, z); err != nil {
			t.Fatal(err)
		}
		runReconcile(t, ctx, r, z)
		if err := cl.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
			t.Fatal(err)
		}
		config := sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-config"]
		if !strings.Contains(config, `<root level="`+logging.Level+`">`) {
			t.Fatalf("logging change did not reach rollout annotation: %q", config)
		}
		if logging.Level == "INFO" && sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-hash"] != "" {
			t.Fatal("logging rollback retained the non-default hash")
		}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(z), z); err != nil {
			t.Fatal(err)
		}
		if z.Status.IsClusterInUpgradingState() || z.Status.CurrentVersion != initialImage || z.Status.TargetVersion != "" {
			t.Fatalf("logging-only rollout changed image upgrade state: %+v", z.Status)
		}
	}
	z.Spec.Image.Tag = "next"
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	sts.Status.CurrentRevision, sts.Status.UpdateRevision = "old", "new"
	if err := cl.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	// A later failure must not discard the intentional intermediate upgrade status.
	r.Client = interceptor.NewClient(cl, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Service); ok {
				return errors.New("later Service read failed")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(z)}); err == nil {
		t.Fatal("expected later read failure")
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(z), z); err != nil {
		t.Fatal(err)
	}
	if !z.Status.IsClusterInUpgradingState() || z.Status.TargetVersion != "next" || z.Status.IsClusterInReadyState() {
		t.Fatalf("intermediate upgrade transition was lost: %+v", z.Status)
	}
	r.Client = cl
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	sts.Status.CurrentRevision, sts.Status.UpdateRevision = "new", "new"
	sts.Status.Replicas, sts.Status.ReadyReplicas = 1, 1
	if err := cl.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, ctx, r, z)
	if err := cl.Get(ctx, client.ObjectKeyFromObject(z), z); err != nil {
		t.Fatal(err)
	}
	if z.Status.IsClusterInUpgradingState() || z.Status.CurrentVersion != "next" || z.Status.TargetVersion != "" {
		t.Fatalf("upgrade completion was lost: %+v", z.Status)
	}
	var writes []string
	r.Client = observeMutations(cl, &writes)
	runReconcile(t, ctx, r, z)
	if unexpected := writesExcept(writes, workloadWrites...); len(unexpected) != 0 {
		t.Fatalf("completed upgrade must settle: %v", unexpected)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(z), z); err != nil {
		t.Fatal(err)
	}
	z.Status.CurrentVersion, z.Status.TargetVersion = "old", "next"
	z.Status.SetUpgradingConditionTrue(api.UpdatingZookeeperReason, "0")
	z.Status.SetErrorConditionTrue("UpgradeFailed", "deadline exceeded")
	z.Status.SetPodsReadyConditionFalse()
	if err := cl.Status().Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	sts.Status.ReadyReplicas = 0
	if err := cl.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	writes = nil
	runReconcile(t, ctx, r, z)
	if unexpected := writesExcept(writes, workloadWrites...); len(unexpected) != 0 {
		t.Fatalf("unchanged failed upgrade must not write: %v", unexpected)
	}
	sts.Status.ReadyReplicas = 1
	if err := cl.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	r.Client = cl
	settleCluster(t, ctx, r, z)
	if z.Status.IsClusterInUpgradeFailedState() || z.Status.IsClusterInUpgradingState() ||
		z.Status.CurrentVersion != "next" || z.Status.TargetVersion != "" {
		t.Fatalf("failed upgrade recovery was lost: %+v", z.Status)
	}
}

func convergenceReconciler(cl client.WithWatch) *ZookeeperClusterReconciler {
	return &ZookeeperClusterReconciler{
		Client: cl, APIReader: cl, Scheme: cl.Scheme(), Log: log,
		NewZkClient: func() zk.ZookeeperClient { return &MockZookeeperClient{} },
	}
}

func runReconcile(t *testing.T, ctx context.Context, r *ZookeeperClusterReconciler, z *api.ZookeeperCluster) {
	t.Helper()
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(z)}); err != nil {
		t.Fatal(err)
	}
}

func settleCluster(t *testing.T, ctx context.Context, r *ZookeeperClusterReconciler, z *api.ZookeeperCluster) {
	t.Helper()
	for range 4 {
		runReconcile(t, ctx, r, z)
	}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(z), z); err != nil {
		t.Fatal(err)
	}
}

func writesExcept(writes []string, ignored ...string) []string {
	var remaining []string
	for _, write := range writes {
		skip := false
		for _, prefix := range ignored {
			skip = skip || strings.HasPrefix(write, prefix)
		}
		if !skip {
			remaining = append(remaining, write)
		}
	}
	return remaining
}

func observeMutations(cl client.WithWatch, writes *[]string) client.WithWatch {
	record := func(verb string, obj client.Object) {
		*writes = append(*writes, fmt.Sprintf("%s %T/%s", verb, obj, obj.GetName()))
	}
	return interceptor.NewClient(cl, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			record("create", obj)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			record("update", obj)
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			verb := "patch"
			if patch.Type() == types.ApplyPatchType {
				verb = "apply"
			}
			record(verb, obj)
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			record("delete", obj)
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			record(sub+" update", obj)
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			record(sub+" patch", obj)
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
}
