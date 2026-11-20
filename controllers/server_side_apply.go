// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/pravega/zookeeper-operator/api/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/csaupgrade"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

// applyFieldManager owns the StatefulSet and Service fields that the operator renders.
const applyFieldManager = "zookeeper-operator-apply"

// StatefulSets and Services use create-only requests so cache misses cannot overwrite live objects.
const createFieldManager = "zookeeper-operator-create"

// Fields owned by Create and Update managers move to applyFieldManager so that an apply
// can remove fields the operator no longer renders.
var legacyFieldManagers = sets.New("zookeeper-operator", createFieldManager)

func legacyManagersFor(current client.Object) sets.Set[string] {
	// make run-local wrote as "main". Only migrate that generic manager when
	// it owns the legacy operator's StatefulSet bookkeeping label.
	if _, ok := current.(*appsv1.StatefulSet); ok && current.GetLabels()["owner-rv"] != "" {
		marker := fieldpath.NewSet(fieldpath.MakePathOrDie("metadata", "labels", "owner-rv"))
		for _, owner := range csaupgrade.FindFieldsOwners(current.GetManagedFields(), metav1.ManagedFieldsOperationUpdate, marker) {
			if owner.Manager == "main" && owner.Subresource == "" {
				return legacyFieldManagers.Union(sets.New("main"))
			}
		}
	}
	return legacyFieldManagers
}

// applyStatefulSet updates an existing StatefulSet without changing its immutable fields.
func (r *ZookeeperClusterReconciler) applyStatefulSet(ctx context.Context, current, desired *appsv1.StatefulSet) error {
	intent := desired.DeepCopy()
	intent.Spec.Selector = current.Spec.Selector
	intent.Spec.ServiceName = current.Spec.ServiceName
	intent.Spec.PodManagementPolicy = current.Spec.PodManagementPolicy
	intent.Spec.VolumeClaimTemplates = current.Spec.VolumeClaimTemplates
	operatorFields, externalFields := statefulSetFields(current)
	defaultRenderedPullPolicies(&intent.Spec.Template.Spec, externalFields)
	prepared := current.DeepCopy()
	retired := removeSharedRetiredContainers(&prepared.Spec.Template.Spec, &desired.Spec.Template.Spec, operatorFields, externalFields)
	portDrift := removeStaleContainerPorts(&prepared.Spec.Template.Spec, &desired.Spec.Template.Spec)
	orderedLists := normalizeContainerLists(&intent.Spec.Template.Spec)
	sourceDrift := clearIncompatibleSources(&prepared.Spec.Template.Spec, &intent.Spec.Template.Spec)
	securityDrift := clearInheritedSecurityOverrides(&prepared.Spec.Template.Spec, &intent.Spec.Template.Spec)
	securityDrift = clearConflictingSeccompAnnotations(&prepared.Spec.Template, &intent.Spec.Template) || securityDrift
	retiringVolumes := retiringVolumesForMerge(&prepared.Spec.Template.Spec, &intent.Spec.Template.Spec, operatorFields, externalFields)
	if orderedLists || portDrift || sourceDrift || retired || securityDrift || len(retiringVolumes) != 0 {
		return r.applyCompatibleStatefulSet(ctx, current, prepared, intent, desired, retiringVolumes)
	}
	return r.apply(ctx, current, intent)
}

// applyService updates an existing Service only if this cluster controls it.
func (r *ZookeeperClusterReconciler) applyService(ctx context.Context, cluster *v1beta1.ZookeeperCluster, current, desired *corev1.Service) error {
	if err := requireClusterOwner(cluster, current, "service"); err != nil {
		return err
	}
	intent := desired.DeepCopy()
	prepared := current.DeepCopy()
	external := intent.Spec.Type == corev1.ServiceTypeLoadBalancer
	if external && (current.Spec.Type == corev1.ServiceTypeLoadBalancer || current.Spec.Type == corev1.ServiceTypeNodePort) {
		// Carry the allocation by name even when the numeric port key changes.
		// Keep it in later applies so ownership migration cannot prune it.
		for i := range intent.Spec.Ports {
			port := &intent.Spec.Ports[i]
			if j := slices.IndexFunc(current.Spec.Ports, func(live corev1.ServicePort) bool { return live.Name == port.Name }); port.NodePort == 0 && j >= 0 {
				port.NodePort = current.Spec.Ports[j].NodePort
			}
		}
	}
	prepared.Spec.Ports = slices.DeleteFunc(prepared.Spec.Ports, func(live corev1.ServicePort) bool {
		if slices.ContainsFunc(intent.Spec.Ports, func(wanted corev1.ServicePort) bool {
			return servicePortKey(live) == servicePortKey(wanted) && live.Name == wanted.Name
		}) {
			return false
		}
		return external || (live.Name != "" && slices.ContainsFunc(intent.Spec.Ports, func(wanted corev1.ServicePort) bool { return live.Name == wanted.Name }))
	})
	const sourceRanges = "service.beta.kubernetes.io/load-balancer-source-ranges"
	incompatibleRanges := false
	if !external {
		_, hasAnnotation := prepared.Annotations[sourceRanges]
		incompatibleRanges = hasAnnotation || len(prepared.Spec.LoadBalancerSourceRanges) != 0
		delete(prepared.Annotations, sourceRanges)
		delete(intent.Annotations, sourceRanges)
		prepared.Spec.LoadBalancerSourceRanges = nil
	}
	if len(current.Spec.Ports) == len(prepared.Spec.Ports) && !incompatibleRanges {
		return r.apply(ctx, current, intent)
	}
	// Publish the type, ports, and annotations together or not at all.
	next := &corev1.Service{}
	if err := r.mergeWorkloadLocally(current, prepared, intent, next); err != nil {
		return err
	}
	next.Status = current.Status
	if err := r.patchMergedWorkload(ctx, current, next); err != nil {
		return err
	}
	*current = *next
	return nil
}

func servicePortKey(port corev1.ServicePort) portIdentity { return portKey(port.Port, port.Protocol) }

func requireClusterOwner(cluster *v1beta1.ZookeeperCluster, obj client.Object, kind string) error {
	if !metav1.IsControlledBy(obj, cluster) {
		return fmt.Errorf("%s %s already exists and is not controlled by ZookeeperCluster %s", kind, client.ObjectKeyFromObject(obj), client.ObjectKeyFromObject(cluster))
	}
	return nil
}

// apply updates the observed object; its UID prevents apply from recreating a deleted object.
func (r *ZookeeperClusterReconciler) apply(ctx context.Context, current, intent client.Object) error {
	if current.GetResourceVersion() == "" {
		return fmt.Errorf("cannot apply %T %s without a resource version", current, client.ObjectKeyFromObject(current))
	}
	migration, err := csaupgrade.UpgradeManagedFieldsPatch(current, legacyManagersFor(current), applyFieldManager)
	if err != nil {
		return err
	}
	if migration != nil {
		if err := r.Client.Patch(ctx, current, client.RawPatch(types.JSONPatchType, migration)); err != nil {
			return err
		}
	}
	intent.SetResourceVersion(current.GetResourceVersion())
	intent.SetUID(current.GetUID())
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(data, &body); err != nil {
		return err
	}
	delete(body, "status")
	if data, err = json.Marshal(body); err != nil {
		return err
	}
	return r.Client.Patch(ctx, current, client.RawPatch(types.ApplyPatchType, data),
		client.FieldOwner(applyFieldManager), client.ForceOwnership)
}
