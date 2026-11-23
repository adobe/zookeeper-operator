// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package controllers

import (
	"context"
	"encoding/json"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/managedfields"
	applyconfigs "k8s.io/client-go/applyconfigurations"
	"k8s.io/client-go/util/csaupgrade"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type portIdentity struct {
	number   int32
	protocol corev1.Protocol
}

func portKey(number int32, protocol corev1.Protocol) portIdentity {
	if protocol == "" {
		protocol = corev1.ProtocolTCP
	}
	return portIdentity{number, protocol}
}

func containerPortKey(port corev1.ContainerPort) portIdentity {
	return portKey(port.ContainerPort, port.Protocol)
}

func removeStaleContainerPorts(live, desired *corev1.PodSpec) bool {
	changed := false
	forRenderedContainers(live, desired, func(current, wanted *corev1.Container) {
		current.Ports = slices.DeleteFunc(current.Ports, func(port corev1.ContainerPort) bool {
			stale := port.Name != "" && slices.ContainsFunc(wanted.Ports, func(next corev1.ContainerPort) bool {
				return next.Name == port.Name && containerPortKey(next) != containerPortKey(port)
			})
			changed = changed || stale
			return stale
		})
	})
	return changed
}

func normalizeContainerLists(spec *corev1.PodSpec) bool {
	changed := false
	for _, containers := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
		for i := range containers {
			c := &containers[i]
			ports, env := len(c.Ports), len(c.Env)
			c.Ports = uniqueBy(c.Ports, containerPortKey)
			c.Env = uniqueBy(c.Env, envKey)
			changed = changed || len(c.Ports) != ports || len(c.Env) != env
		}
	}
	return changed
}

func envKey(env corev1.EnvVar) string { return env.Name }

func uniqueBy[T any, K comparable](values []T, key func(T) K) []T {
	seen := make(map[K]bool, len(values))
	return slices.DeleteFunc(values, func(value T) bool {
		id := key(value)
		previous := seen[id]
		seen[id] = true
		return previous
	})
}

// Apply cannot express duplicate list keys. Merge an SSA-compatible intent locally,
// then restore the exact ordered lists and let the API server validate one guarded patch.
func (r *ZookeeperClusterReconciler) applyCompatibleStatefulSet(ctx context.Context, current, prepared, intent, desired *appsv1.StatefulSet, retiringVolumes []corev1.Volume) error {
	next := &appsv1.StatefulSet{}
	if err := r.mergeWorkloadLocally(prepared, intent, next); err != nil {
		return err
	}
	if resolveRetiringVolumes(prepared, intent, next, retiringVolumes) {
		// Keep complete sources while referenced; don't persist orphaned fragments.
		next = &appsv1.StatefulSet{}
		if err := r.mergeWorkloadLocally(prepared, intent, next); err != nil {
			return err
		}
	}
	restoreContainerLists(current.Spec.Template.Spec.Containers, next.Spec.Template.Spec.Containers, desired.Spec.Template.Spec.Containers, intent.Spec.Template.Spec.Containers)
	restoreContainerLists(current.Spec.Template.Spec.InitContainers, next.Spec.Template.Spec.InitContainers, desired.Spec.Template.Spec.InitContainers, intent.Spec.Template.Spec.InitContainers)
	next.Status = current.Status
	if err := r.patchMergedWorkload(ctx, current, next); err != nil {
		return err
	}
	*current = *next
	return nil
}

func (r *ZookeeperClusterReconciler) mergeWorkloadLocally(prepared, intent, next client.Object) error {
	gvk := intent.GetObjectKind().GroupVersionKind()
	prepared.GetObjectKind().SetGroupVersionKind(gvk)
	if err := csaupgrade.UpgradeManagedFields(prepared, legacyFieldManagers, applyFieldManager); err != nil {
		return err
	}
	manager, err := managedfields.NewDefaultFieldManager(applyconfigs.NewTypeConverter(r.Scheme), r.Scheme, r.Scheme, r.Scheme, gvk, gvk.GroupVersion(), "", nil)
	if err != nil {
		return err
	}
	payload, err := runtime.DefaultUnstructuredConverter.ToUnstructured(intent)
	if err != nil {
		return err
	}
	delete(payload, "status")
	merged, err := manager.Apply(prepared, &unstructured.Unstructured{Object: payload}, applyFieldManager, true)
	if err != nil {
		return err
	}
	data, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, next)
}

func (r *ZookeeperClusterReconciler) patchMergedWorkload(ctx context.Context, current, next client.Object) error {
	next.GetObjectKind().SetGroupVersionKind(current.GetObjectKind().GroupVersionKind())
	next.SetResourceVersion(current.GetResourceVersion())
	if equality.Semantic.DeepEqual(current, next) {
		return nil
	}
	// Changes made by this non-apply PATCH must migrate back to the apply manager.
	return r.Client.Patch(ctx, next, client.MergeFromWithOptions(current, client.MergeFromWithOptimisticLock{}), client.FieldOwner(createFieldManager))
}

func restoreContainerLists(live, merged, desired, normalized []corev1.Container) {
	for i, container := range desired {
		restorePorts := len(container.Ports) != len(normalized[i].Ports)
		restoreEnv := len(container.Env) != len(normalized[i].Env)
		if !restorePorts && !restoreEnv {
			continue
		}
		for j := range merged {
			if merged[j].Name != container.Name {
				continue
			}
			var previous corev1.Container
			if at := slices.IndexFunc(live, func(c corev1.Container) bool { return c.Name == container.Name }); at >= 0 {
				previous = live[at]
			}
			if restorePorts {
				merged[j].Ports = restoreList(previous.Ports, merged[j].Ports, container.Ports, containerPortKey)
			}
			if restoreEnv {
				merged[j].Env = restoreList(previous.Env, merged[j].Env, container.Env, envKey)
			}
			break
		}
	}
}

func restoreList[T any, K comparable](live, merged, desired []T, key func(T) K) []T {
	positions := make(map[K][]int, len(desired))
	for i, value := range desired {
		id := key(value)
		positions[id] = append(positions[id], i)
	}
	resolved := slices.Clone(desired)
	extra := make(map[K][]int)
	for i, value := range merged {
		id := key(value)
		switch len(positions[id]) {
		case 0:
			extra[id] = append(extra[id], i)
		case 1:
			resolved[positions[id][0]] = value
		}
	}
	// Anchor independent entries before the next rendered occurrence in the
	// live list. Preserve their order even when the rendered order changes.
	gaps := make(map[int][]T)
	used := make([]bool, len(merged))
	anchor := len(resolved)
	for i := len(live) - 1; i >= 0; i-- {
		id := key(live[i])
		if indexes := positions[id]; len(indexes) != 0 {
			anchor = min(anchor, indexes[len(indexes)-1])
			if len(indexes) > 1 {
				positions[id] = indexes[:len(indexes)-1]
			}
		} else if indexes := extra[id]; len(indexes) != 0 {
			index := indexes[len(indexes)-1]
			gaps[anchor] = append(gaps[anchor], merged[index])
			used[index] = true
			extra[id] = indexes[:len(indexes)-1]
		}
	}
	result := make([]T, 0, len(resolved))
	for i, value := range resolved {
		gap := gaps[i]
		slices.Reverse(gap)
		result = append(result, gap...)
		result = append(result, value)
	}
	tail := gaps[len(resolved)]
	slices.Reverse(tail)
	result = append(result, tail...)
	for i, value := range merged {
		if !used[i] && len(positions[key(value)]) == 0 {
			result = append(result, value)
		}
	}
	return result
}
