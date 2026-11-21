//go:build envtest

// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package controllers

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"

	api "github.com/pravega/zookeeper-operator/api/v1beta1"
	"github.com/pravega/zookeeper-operator/pkg/zk"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestEnvtestMutableApplyPreservesDifferentImmutableFields(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	sts := zk.MakeStatefulSet(z).DeepCopy()
	zk.EnsureLogbackProjection(z, nil, sts)
	sts.Spec.ServiceName = "existing-headless"
	sts.Spec.Selector.MatchLabels["kind"] = "ZookeeperMember"
	sts.Spec.PodManagementPolicy = appsv1.ParallelPodManagement
	storage := resource.MustParse("32Gi")
	sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = storage
	if err := controllerutil.SetControllerReference(z, sts, cl.Scheme()); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, sts, client.FieldOwner("zookeeper-operator")); err != nil {
		t.Fatal(err)
	}
	z.Spec.Image.Tag = "updated"
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	if err := convergenceReconciler(cl).reconcileStatefulSet(ctx, z); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(z), sts); err != nil {
		t.Fatal(err)
	}
	spec := sts.Spec
	if spec.ServiceName != "existing-headless" || spec.PodManagementPolicy != appsv1.ParallelPodManagement || spec.Selector.MatchLabels["kind"] != "ZookeeperMember" || !spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().Equal(storage) {
		t.Fatal("StatefulSet immutable fields were not preserved")
	}
	if spec.Template.Spec.Containers[0].Image != z.Spec.Image.ToString() {
		t.Fatal("mutable template change did not take effect")
	}
}

func TestEnvtestPreservesNamedContainerPortAliases(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	z.Spec.Ports = append(z.Spec.Ports, corev1.ContainerPort{Name: "client-alt", ContainerPort: 2181})
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
	hasAlias := func(name string) bool {
		return slices.ContainsFunc(sts.Spec.Template.Spec.Containers[0].Ports, func(port corev1.ContainerPort) bool {
			return port.Name == name && port.ContainerPort == 2181 && port.Protocol == corev1.ProtocolTCP
		})
	}
	if len(sts.Spec.Template.Spec.Containers[0].Ports) != len(z.Spec.Ports) || !hasAlias("client") || !hasAlias("client-alt") {
		t.Fatal("an accepted port alias was dropped")
	}
	z.Spec.Ports = z.Spec.Ports[:len(z.Spec.Ports)-1]
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	settleCluster(t, ctx, r, z)
	if err := cl.Get(ctx, client.ObjectKeyFromObject(z), sts); err != nil {
		t.Fatal(err)
	}
	if len(sts.Spec.Template.Spec.Containers[0].Ports) != len(z.Spec.Ports) || !hasAlias("client") || hasAlias("client-alt") {
		t.Fatal("expected only the canonical client port after retiring the alias")
	}
}

func TestEnvtestRepairsNamedPortKeyDrift(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	sts := &appsv1.StatefulSet{}
	key := client.ObjectKeyFromObject(z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	svc := &corev1.Service{}
	svcKey := client.ObjectKey{Namespace: z.Namespace, Name: z.GetClientServiceName()}
	if err := cl.Get(ctx, svcKey, svc); err != nil {
		t.Fatal(err)
	}
	sts.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort = 2199
	sts.Spec.Template.Spec.Containers[0].Ports = append(sts.Spec.Template.Spec.Containers[0].Ports, corev1.ContainerPort{Name: "extra", ContainerPort: 9090})
	svc.Spec.Ports[0].Port = 2199
	svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: "extra", Port: 9090})
	for _, obj := range []client.Object{sts, svc} {
		if err := cl.Update(ctx, obj, client.FieldOwner("user")); err != nil {
			t.Fatal(err)
		}
	}
	runReconcile(t, ctx, r, z)
	if err := cl.Get(ctx, svcKey, svc); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	ports := sts.Spec.Template.Spec.Containers[0].Ports
	for name, port := range map[string]int32{"client": 2181, "extra": 9090} {
		if !slices.ContainsFunc(ports, func(p corev1.ContainerPort) bool { return p.Name == name && p.ContainerPort == port }) {
			t.Fatalf("expected %s/%d in container ports: %+v", name, port, ports)
		}
	}
	for name, port := range map[string]int32{"tcp-client": 2181, "extra": 9090} {
		if !slices.ContainsFunc(svc.Spec.Ports, func(p corev1.ServicePort) bool { return p.Name == name && p.Port == port }) {
			t.Fatalf("expected %s/%d in Service ports: %+v", name, port, svc.Spec.Ports)
		}
	}
	previous := sts.DeepCopy()
	runReconcile(t, ctx, r, z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	if sts.Generation != previous.Generation {
		t.Fatalf("repair was followed by another rollout: ports before=%+v after=%+v", previous.Spec.Template.Spec.Containers[0].Ports, sts.Spec.Template.Spec.Containers[0].Ports)
	}
	assertEnvtestSettled(t, ctx, r, cl, z, 3)
}

func TestEnvtestReplacesMutuallyExclusiveSources(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	z.Spec.Pod.Env = []corev1.EnvVar{{Name: "SETTING", Value: "desired"}}
	z.Spec.Volumes = []corev1.Volume{{Name: "input", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	sts := &appsv1.StatefulSet{}
	key := client.ObjectKeyFromObject(z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	env := sts.Spec.Template.Spec.Containers[0].Env
	env[0] = corev1.EnvVar{Name: "ENVOY_SIDECAR_STATUS", Value: "different"}
	env[1] = corev1.EnvVar{Name: "SETTING", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}
	sts.Spec.Template.Spec.Volumes[0].VolumeSource = corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "another"}}}
	if err := cl.Update(ctx, sts, client.FieldOwner("user")); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, ctx, r, z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	env = sts.Spec.Template.Spec.Containers[0].Env
	vol := sts.Spec.Template.Spec.Volumes[0]
	if env[0].Value != "" || env[0].ValueFrom == nil || env[1].Value != "desired" || env[1].ValueFrom != nil || vol.EmptyDir == nil || vol.ConfigMap != nil {
		t.Fatal("operator did not replace the incompatible environment and volume sources")
	}
	settleCluster(t, ctx, r, z)
	assertEnvtestSettled(t, ctx, r, cl, z, 3)
}

func TestEnvtestRetainsVolumeUntilIndependentMountIsRemoved(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	z.Spec.Volumes = []corev1.Volume{{Name: "shared", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	sts := &appsv1.StatefulSet{}
	key := client.ObjectKeyFromObject(z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	sts.Spec.Template.Spec.Containers = append(sts.Spec.Template.Spec.Containers, corev1.Container{
		Name: "independent", Image: "busybox:1",
		VolumeMounts: []corev1.VolumeMount{{Name: "shared", MountPath: "/shared"}},
	})
	if err := cl.Update(ctx, sts, client.FieldOwner("user")); err != nil {
		t.Fatal(err)
	}
	z.Spec.Volumes = nil
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	for _, mounted := range []bool{true, false} {
		settleCluster(t, ctx, r, z)
		if err := cl.Get(ctx, key, sts); err != nil {
			t.Fatal(err)
		}
		pod := &sts.Spec.Template.Spec
		i := slices.IndexFunc(pod.Containers, func(c corev1.Container) bool { return c.Name == "independent" })
		if i < 0 || slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool { return v.Name == "shared" }) != mounted || (len(pod.Containers[i].VolumeMounts) != 0) != mounted {
			t.Fatalf("expected the volume to survive only while the independent container mounts it: mounted=%v", mounted)
		}
		assertEnvtestSettled(t, ctx, r, cl, z, 3)
		if mounted {
			pod.Containers[i].VolumeMounts = nil
			if err := cl.Update(ctx, sts, client.FieldOwner("user")); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestEnvtestRetainsCompleteSourceOfSharedVolume(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	z.Spec.Volumes = []corev1.Volume{{Name: "shared", VolumeSource: corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "shared-config"}},
	}}}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	sts := &appsv1.StatefulSet{}
	key := client.ObjectKeyFromObject(z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	// An independent applier owns the mount and defaultMode, but not the ConfigMap name.
	intent := map[string]any{
		"apiVersion": "apps/v1", "kind": "StatefulSet",
		"metadata": map[string]string{"name": z.Name, "namespace": z.Namespace},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []corev1.Container{{Name: "independent", Image: "busybox:1", VolumeMounts: []corev1.VolumeMount{{Name: "shared", MountPath: "/shared"}}}},
			"volumes":    []map[string]any{{"name": "shared", "configMap": map[string]int{"defaultMode": 0o640}}},
		}}},
	}
	payload, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Patch(ctx, sts, client.RawPatch(types.ApplyPatchType, payload), client.FieldOwner("independent"), client.ForceOwnership); err != nil {
		t.Fatal(err)
	}
	z.Spec.Volumes = nil
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	settleCluster(t, ctx, r, z)
	assertEnvtestSettled(t, ctx, r, cl, z, 3)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	pod := &sts.Spec.Template.Spec
	if !slices.ContainsFunc(pod.Volumes, func(v corev1.Volume) bool {
		return v.Name == "shared" && v.ConfigMap != nil && v.ConfigMap.Name == "shared-config" && v.ConfigMap.DefaultMode != nil && *v.ConfigMap.DefaultMode == 0o640
	}) {
		t.Fatalf("shared volume lost its ConfigMap name or independently owned mode: %+v", pod.Volumes)
	}
	i := slices.IndexFunc(pod.Containers, func(c corev1.Container) bool { return c.Name == "independent" })
	if i < 0 || !slices.ContainsFunc(pod.Containers[i].VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == "shared" }) {
		t.Fatal("independent mount was not retained")
	}
	pod.Containers[i].VolumeMounts = nil
	if err := cl.Update(ctx, sts, client.FieldOwner("independent")); err != nil {
		t.Fatal(err)
	}
	settleCluster(t, ctx, r, z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(sts.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == "shared" }) {
		t.Fatal("partially shared volume was not pruned after its last mount disappeared")
	}
	assertEnvtestSettled(t, ctx, r, cl, z, 3)
}

func TestEnvtestRestoresDuplicateEnvWithoutMovingIndependentVariables(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	z.Spec.Pod.Env = []corev1.EnvVar{
		{Name: "DATA_DIR", Value: "$(EXTERNAL_BASE)/data"},
		{Name: "WRAPPED", Value: "first"},
		{Name: "WRAPPED", Value: "$(WRAPPED)/second"},
	}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	sts := &appsv1.StatefulSet{}
	key := client.ObjectKeyFromObject(z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	c := &sts.Spec.Template.Spec.Containers[0]
	c.Env = slices.Insert(c.Env, 1, corev1.EnvVar{Name: "EXTERNAL_BASE", Value: "/srv"})
	c.Env = append(c.Env, corev1.EnvVar{Name: "EXTERNAL_TRAILING", Value: "$(WRAPPED)"})
	if err := cl.Update(ctx, sts, client.FieldOwner("user")); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, ctx, r, z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, env := range sts.Spec.Template.Spec.Containers[0].Env {
		names = append(names, env.Name)
	}
	want := []string{"ENVOY_SIDECAR_STATUS", "EXTERNAL_BASE", "DATA_DIR", "WRAPPED", "WRAPPED", "EXTERNAL_TRAILING"}
	if !slices.Equal(names, want) {
		t.Fatalf("expected the injected variables to keep their positions: got %v, want %v", names, want)
	}
	assertEnvtestSettled(t, ctx, r, cl, z, 3)
}

func TestEnvtestClearsRenderedEmptyEnvironmentLiteral(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	z.Spec.Pod.Env = []corev1.EnvVar{{Name: "SETTING"}}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	sts := &appsv1.StatefulSet{}
	key := client.ObjectKeyFromObject(z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	c := &sts.Spec.Template.Spec.Containers[0]
	c.Env[1].Value = "out-of-date"
	c.Env = append(c.Env, corev1.EnvVar{Name: "UNRELATED", Value: "keep"})
	if err := cl.Update(ctx, sts, client.FieldOwner("user")); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, ctx, r, z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	env := sts.Spec.Template.Spec.Containers[0].Env
	if !slices.ContainsFunc(env, func(e corev1.EnvVar) bool { return e.Name == "SETTING" && e.Value == "" && e.ValueFrom == nil }) || !slices.ContainsFunc(env, func(e corev1.EnvVar) bool { return e.Name == "UNRELATED" && e.Value == "keep" }) {
		t.Fatalf("expected the rendered empty value and preserved unrelated value: %+v", env)
	}
}

func TestEnvtestRepairsRenderedProbeAndLifecycleActions(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	sts := &appsv1.StatefulSet{}
	key := client.ObjectKeyFromObject(z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	container := &sts.Spec.Template.Spec.Containers[0]
	container.ReadinessProbe.ProbeHandler = corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(8080)}}
	container.Lifecycle.PreStop = &corev1.LifecycleHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/stop", Port: intstr.FromInt32(8080)}}
	container.Lifecycle.PostStart = &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}
	if err := cl.Update(ctx, sts, client.FieldOwner("user")); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, ctx, r, z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	container = &sts.Spec.Template.Spec.Containers[0]
	if container.ReadinessProbe.Exec == nil || container.ReadinessProbe.HTTPGet != nil || container.Lifecycle.PreStop.Exec == nil || container.Lifecycle.PreStop.HTTPGet != nil || container.Lifecycle.PostStart == nil {
		t.Fatal("requested actions did not replace incompatible variants while preserving the unrelated hook")
	}
}

func TestEnvtestRetiresSharedContainerAsAUnit(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	z.Spec.Containers = []corev1.Container{{Name: "retired", Image: "busybox:1"}}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	sts := &appsv1.StatefulSet{}
	key := client.ObjectKeyFromObject(z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	intent := map[string]any{
		"apiVersion": "apps/v1", "kind": "StatefulSet",
		"metadata": map[string]string{"name": z.Name, "namespace": z.Namespace},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []corev1.Container{
			{Name: "retired", Args: []string{"extra"}},
			{Name: "independent", Image: "busybox:1"},
		}}}},
	}
	data, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Patch(ctx, sts, client.RawPatch(types.ApplyPatchType, data), client.FieldOwner("injector")); err != nil {
		t.Fatal(err)
	}
	z.Spec.Containers = nil
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, ctx, r, z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	containers := sts.Spec.Template.Spec.Containers
	if len(containers) != 2 || !slices.ContainsFunc(containers, func(c corev1.Container) bool { return c.Name == "independent" }) || slices.ContainsFunc(containers, func(c corev1.Container) bool { return c.Name == "retired" }) {
		t.Fatalf("expected the retired container removed and the independent one retained: %+v", containers)
	}
}

func TestEnvtestPullPolicyFollowsImageButPreservesExplicitOwners(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	z.Spec.Containers = []corev1.Container{
		{Name: "defaulted", Image: "busybox:1"},
		{Name: "external-policy", Image: "busybox:1"},
		{Name: "explicit-policy", Image: "busybox:1", ImagePullPolicy: corev1.PullIfNotPresent},
	}
	z.Spec.InitContainers = []corev1.Container{{Name: "defaulted-init", Image: "busybox:1"}}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	sts := &appsv1.StatefulSet{}
	key := client.ObjectKeyFromObject(z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	sts.Spec.Template.Spec.Containers[1].ImagePullPolicy = corev1.PullNever
	if err := cl.Update(ctx, sts, client.FieldOwner("user")); err != nil {
		t.Fatal(err)
	}
	for i := range z.Spec.Containers {
		z.Spec.Containers[i].Image = "busybox:latest"
	}
	z.Spec.InitContainers[0].Image = "busybox"
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, ctx, r, z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	for i, want := range []corev1.PullPolicy{corev1.PullAlways, corev1.PullNever, corev1.PullIfNotPresent} {
		if got := sts.Spec.Template.Spec.Containers[i].ImagePullPolicy; got != want {
			t.Errorf("container %d pull policy = %s, want %s", i, got, want)
		}
	}
	if sts.Spec.Template.Spec.InitContainers[0].ImagePullPolicy != corev1.PullAlways {
		t.Error("an untagged init image should default to Always")
	}
}

func TestEnvtestAdminServiceTransitionsAreAtomic(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	key := client.ObjectKey{Namespace: z.Namespace, Name: z.GetAdminServerServiceName()}
	svc := &corev1.Service{}
	if err := cl.Get(ctx, key, svc); err != nil {
		t.Fatal(err)
	}
	svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: "internal-extra", Port: 9090})
	svc.Annotations = map[string]string{"external": "preserve"}
	if err := cl.Update(ctx, svc, client.FieldOwner("user")); err != nil {
		t.Fatal(err)
	}
	z.Spec.AdminServerService.External = true
	const frontend = "networking.gke.io/load-balancer-type"
	const ranges = "service.beta.kubernetes.io/load-balancer-source-ranges"
	z.Spec.AdminServerService.Annotations = map[string]string{frontend: "Internal"}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	for _, external := range []bool{true, false} {
		writes := 0
		r.Client = observeServicePatches(cl, key, func(live *corev1.Service) {
			writes++
			wantType := corev1.ServiceTypeClusterIP
			if external {
				wantType = corev1.ServiceTypeLoadBalancer
			}
			if live.Spec.Type != wantType || len(live.Spec.Ports) != 1 || live.Spec.Ports[0].Name != "tcp-admin-server" || live.Annotations["external"] != "preserve" {
				t.Fatalf("unexpected persisted admin Service state: type=%s ports=%+v", live.Spec.Type, live.Spec.Ports)
			}
			if external && live.Annotations[frontend] != "Internal" || !external && (live.Annotations[ranges] != "" || len(live.Spec.LoadBalancerSourceRanges) != 0) {
				t.Fatal("Service type and exposure annotations must transition together")
			}
		})
		if err := r.reconcileAdminServerService(ctx, z); err != nil {
			t.Fatal(err)
		}
		if writes != 1 {
			t.Fatalf("transition issued %d Service patches, want 1", writes)
		}
		if external {
			if err := cl.Get(ctx, key, svc); err != nil {
				t.Fatal(err)
			}
			svc.Annotations[ranges] = "192.0.2.0/24"
			svc.Spec.LoadBalancerSourceRanges = []string{"198.51.100.0/24"}
			if err := cl.Update(ctx, svc, client.FieldOwner("user")); err != nil {
				t.Fatal(err)
			}
			z.Spec.AdminServerService.External = false
			z.Spec.AdminServerService.Annotations = nil
			if err := cl.Update(ctx, z); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestEnvtestAdminPortChangeRetainsManuallyAllocatedNodePort(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	z.Spec.AdminServerService.External = true
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	key := client.ObjectKey{Namespace: z.Namespace, Name: z.GetAdminServerServiceName()}
	svc := &corev1.Service{}
	if err := cl.Get(ctx, key, svc); err != nil {
		t.Fatal(err)
	}
	nodePort := svc.Spec.Ports[0].NodePort
	if nodePort == 0 {
		t.Fatal("expected the test Service to have an allocated NodePort")
	}
	allocate := false
	svc.Spec.AllocateLoadBalancerNodePorts = &allocate
	svc.Spec.LoadBalancerSourceRanges = []string{"192.0.2.0/24"}
	if err := cl.Update(ctx, svc, client.FieldOwner("network-config")); err != nil {
		t.Fatal(err)
	}
	const newPort = 8081
	for i := range z.Spec.Ports {
		if z.Spec.Ports[i].Name == "admin-server" {
			z.Spec.Ports[i].ContainerPort = newPort
		}
	}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := r.reconcileAdminServerService(ctx, z); err != nil {
			t.Fatal(err)
		}
		if err := cl.Get(ctx, key, svc); err != nil {
			t.Fatal(err)
		}
		if ports := svc.Spec.Ports; len(ports) != 1 || ports[0].Port != newPort || ports[0].NodePort != nodePort {
			t.Fatalf("reconcile %d: expected port %d to retain NodePort %d, got %+v", i+1, newPort, nodePort, ports)
		}
		if svc.Spec.AllocateLoadBalancerNodePorts == nil || *svc.Spec.AllocateLoadBalancerNodePorts || !slices.Equal(svc.Spec.LoadBalancerSourceRanges, []string{"192.0.2.0/24"}) {
			t.Fatal("external access settings changed during the port update")
		}
	}
}

func TestEnvtestHeadlessServicePortSwapIsAtomic(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	key := client.ObjectKeyFromObject(zk.MakeHeadlessService(z))
	z.Spec.Ports[0].ContainerPort, z.Spec.Ports[1].ContainerPort = z.Spec.Ports[1].ContainerPort, z.Spec.Ports[0].ContainerPort
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	desired := zk.MakeHeadlessService(z)
	writes := 0
	r.Client = observeServicePatches(cl, key, func(live *corev1.Service) {
		writes++
		for _, wanted := range desired.Spec.Ports {
			if !slices.ContainsFunc(live.Spec.Ports, func(p corev1.ServicePort) bool { return p.Name == wanted.Name && p.Port == wanted.Port }) {
				t.Fatalf("persisted headless Service lacks %s/%d", wanted.Name, wanted.Port)
			}
		}
	})
	if err := r.reconcileHeadlessService(ctx, z); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("port swap issued %d Service patches, want 1", writes)
	}
}

func observeServicePatches(cl client.WithWatch, key client.ObjectKey, observe func(*corev1.Service)) client.WithWatch {
	return interceptor.NewClient(cl, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := c.Patch(ctx, obj, patch, opts...); err != nil {
				return err
			}
			if svc, ok := obj.(*corev1.Service); ok && client.ObjectKeyFromObject(svc) == key {
				observe(svc)
			}
			return nil
		},
	})
}

func TestEnvtestEnforcesRequestedZooKeeperIdentity(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	key := client.ObjectKeyFromObject(z)
	sts := &appsv1.StatefulSet{}
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	oldUser, requestedUser, readOnly := int64(2000), int64(1000), true
	sts.Spec.Template.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: &oldUser, ReadOnlyRootFilesystem: &readOnly}
	if err := cl.Update(ctx, sts, client.FieldOwner("user")); err != nil {
		t.Fatal(err)
	}
	z.Spec.Pod.SecurityContext = &corev1.PodSecurityContext{RunAsUser: &requestedUser}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	runReconcile(t, ctx, r, z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	pod, container := sts.Spec.Template.Spec.SecurityContext, sts.Spec.Template.Spec.Containers[0].SecurityContext
	if pod == nil || pod.RunAsUser == nil || *pod.RunAsUser != requestedUser || container == nil || container.RunAsUser != nil || container.ReadOnlyRootFilesystem == nil || !*container.ReadOnlyRootFilesystem {
		t.Fatal("operator must enforce the requested identity without dropping other security settings")
	}
}

func TestEnvtestSeccompProfileReplacesConflictingLegacyAnnotation(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	r := convergenceReconciler(cl)
	settleCluster(t, ctx, r, z)
	key := client.ObjectKeyFromObject(z)
	sts := &appsv1.StatefulSet{}
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	sts.Spec.Template.Annotations[corev1.SeccompPodAnnotationKey] = "localhost/previous.json"
	sts.Spec.Template.Annotations[corev1.SeccompContainerAnnotationKeyPrefix+"zookeeper"] = "localhost/previous.json"
	sts.Spec.Template.Annotations["example.com/unrelated"] = "keep"
	if err := cl.Update(ctx, sts, client.FieldOwner("profile-config")); err != nil {
		t.Fatal(err)
	}
	z.Spec.Pod.SecurityContext = &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if err := cl.Update(ctx, z); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileStatefulSet(ctx, z); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	template := sts.Spec.Template
	templateGeneration := sts.Generation
	if template.Spec.SecurityContext == nil || template.Spec.SecurityContext.SeccompProfile == nil || template.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("requested seccomp profile was not applied")
	}
	for _, key := range []string{corev1.SeccompPodAnnotationKey, corev1.SeccompContainerAnnotationKeyPrefix + "zookeeper"} {
		if value, ok := template.Annotations[key]; ok && value != corev1.SeccompProfileRuntimeDefault && value != corev1.DeprecatedSeccompProfileDockerDefault {
			t.Fatalf("conflicting legacy annotation %s retained: %s", key, value)
		}
	}
	if template.Annotations["example.com/unrelated"] != "keep" {
		t.Fatal("unrelated annotation was removed")
	}
	settleCluster(t, ctx, r, z)
	if err := cl.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	if sts.Generation != templateGeneration {
		t.Fatal("completing the ownership migration triggered a second template change")
	}
	assertEnvtestSettled(t, ctx, r, cl, z, 3)
}

func TestEnvtestMigratesLegacyLocalRunner(t *testing.T) {
	for _, initialLogging := range []string{"INFO", "WARN"} {
		t.Run(initialLogging, func(t *testing.T) {
			ctx, cl, z := newEnvtestCluster(t)
			z.Spec.Logging = api.LoggingConfig{Level: initialLogging}
			z.Spec.Pod.NodeSelector = map[string]string{"pool": "old"}
			if err := cl.Update(ctx, z); err != nil {
				t.Fatal(err)
			}
			sts := zk.MakeStatefulSet(z)
			sts.Labels = maps.Clone(sts.Labels)
			sts.Labels["owner-rv"] = z.ResourceVersion
			if err := controllerutil.SetControllerReference(z, sts, cl.Scheme()); err != nil {
				t.Fatal(err)
			}
			if err := cl.Create(ctx, sts, client.FieldOwner("main")); err != nil {
				t.Fatal(err)
			}
			z.Spec.Pod.NodeSelector = nil
			if initialLogging == "INFO" {
				z.Spec.Logging.Level = "WARN"
			} else {
				z.Spec.Logging.Level = "INFO"
			}
			if err := cl.Update(ctx, z); err != nil {
				t.Fatal(err)
			}
			if err := convergenceReconciler(cl).reconcileStatefulSet(ctx, z); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
				t.Fatal(err)
			}
			template := sts.Spec.Template
			if len(template.Spec.NodeSelector) != 0 || template.Spec.Volumes[0].Projected == nil {
				t.Fatal("local runner did not converge scheduling and logging fields")
			}
			if _, present := template.Annotations["zookeeper.pravega.io/logback-hash"]; present != (z.Spec.Logging.Level == "WARN") {
				t.Fatal("logging hash did not follow the requested configuration")
			}
		})
	}
}
