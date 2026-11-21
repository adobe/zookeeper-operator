// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package controllers

import (
	"reflect"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

func clearInheritedSecurityOverrides(live, intent *corev1.PodSpec) bool {
	pod := intent.SecurityContext
	if pod == nil {
		pod = &corev1.PodSecurityContext{}
	}
	changed := false
	forRenderedContainers(live, intent, func(current, desired *corev1.Container) {
		container := current.SecurityContext
		if container == nil {
			return
		}
		requested := desired.SecurityContext
		if requested == nil {
			requested = &corev1.SecurityContext{}
		}
		changed = clearOverride(&container.RunAsUser, pod.RunAsUser, requested.RunAsUser) || changed
		changed = clearOverride(&container.RunAsGroup, pod.RunAsGroup, requested.RunAsGroup) || changed
		changed = clearOverride(&container.RunAsNonRoot, pod.RunAsNonRoot, requested.RunAsNonRoot) || changed
		changed = clearOverride(&container.SELinuxOptions, pod.SELinuxOptions, requested.SELinuxOptions) || changed
		changed = clearOverride(&container.WindowsOptions, pod.WindowsOptions, requested.WindowsOptions) || changed
		changed = clearOverride(&container.SeccompProfile, pod.SeccompProfile, requested.SeccompProfile) || changed
		changed = clearOverride(&container.AppArmorProfile, pod.AppArmorProfile, requested.AppArmorProfile) || changed
		if nonRoot := preferExplicit(requested.RunAsNonRoot, pod.RunAsNonRoot); nonRoot != nil && *nonRoot && requested.RunAsUser == nil && container.RunAsUser != nil && *container.RunAsUser == 0 {
			container.RunAsUser = nil
			changed = true
		}
		seccomp := preferExplicit(requested.SeccompProfile, pod.SeccompProfile)
		apparmor := preferExplicit(requested.AppArmorProfile, pod.AppArmorProfile)
		confinement := seccomp != nil && seccomp.Type != corev1.SeccompProfileTypeUnconfined || apparmor != nil && apparmor.Type != corev1.AppArmorProfileTypeUnconfined
		if confinement && requested.Privileged == nil && container.Privileged != nil && *container.Privileged {
			container.Privileged = nil
			changed = true
		}
	})
	return changed
}

func preferExplicit[T any](explicit, inherited *T) *T {
	if explicit != nil {
		return explicit
	}
	return inherited
}

func clearOverride[T any](live **T, inherited, explicit *T) bool {
	if *live == nil || inherited == nil || explicit != nil || equality.Semantic.DeepEqual(*live, inherited) {
		return false
	}
	*live = nil
	return true
}

func clearConflictingSeccompAnnotations(live, intent *corev1.PodTemplateSpec) bool {
	if len(live.Annotations) == 0 {
		return false
	}
	changed := false
	clear := func(key string, profile *corev1.SeccompProfile) {
		annotation, exists := live.Annotations[key]
		if _, explicit := intent.Annotations[key]; exists && !explicit && profile != nil && !matchesSeccompProfile(annotation, profile) {
			delete(live.Annotations, key)
			changed = true
		}
	}
	var podProfile *corev1.SeccompProfile
	if intent.Spec.SecurityContext != nil {
		podProfile = intent.Spec.SecurityContext.SeccompProfile
	}
	clear(corev1.SeccompPodAnnotationKey, podProfile)
	// A legacy container annotation can override the inherited pod profile.
	for _, list := range [][]corev1.Container{intent.Spec.Containers, intent.Spec.InitContainers} {
		for _, container := range list {
			profile := podProfile
			if container.SecurityContext != nil {
				profile = preferExplicit(container.SecurityContext.SeccompProfile, podProfile)
			}
			clear(corev1.SeccompContainerAnnotationKeyPrefix+container.Name, profile)
		}
	}
	return changed
}

func matchesSeccompProfile(annotation string, profile *corev1.SeccompProfile) bool {
	switch profile.Type {
	case corev1.SeccompProfileTypeRuntimeDefault:
		return annotation == corev1.SeccompProfileRuntimeDefault || annotation == corev1.DeprecatedSeccompProfileDockerDefault
	case corev1.SeccompProfileTypeUnconfined:
		return annotation == corev1.SeccompProfileNameUnconfined
	case corev1.SeccompProfileTypeLocalhost:
		return profile.LocalhostProfile != nil && annotation == corev1.SeccompLocalhostProfileNamePrefix+*profile.LocalhostProfile
	default:
		return false
	}
}

func statefulSetFields(current *appsv1.StatefulSet) (operator, external *fieldpath.Set) {
	operator, external = fieldpath.NewSet(), fieldpath.NewSet()
	legacy := legacyManagersFor(current)
	for _, entry := range current.ManagedFields {
		if entry.Subresource != "" || entry.FieldsV1 == nil || entry.APIVersion != "apps/v1" {
			continue
		}
		fields := &fieldpath.Set{}
		if fields.FromJSON(entry.FieldsV1.GetRawReader()) != nil {
			continue
		}
		if entry.Manager == applyFieldManager && entry.Operation == metav1.ManagedFieldsOperationApply || legacy.Has(entry.Manager) && entry.Operation == metav1.ManagedFieldsOperationUpdate {
			operator = operator.Union(fields)
		} else {
			external = external.Union(fields)
		}
	}
	return operator, external
}

func containerPath(list, name string) fieldpath.Path {
	return fieldpath.MakePathOrDie("spec", "template", "spec", list, fieldpath.KeyByFields("name", name))
}

func referencedVolumes(spec *corev1.PodSpec) sets.Set[string] {
	names := sets.New[string]()
	for _, list := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
		for _, container := range list {
			for _, mount := range container.VolumeMounts {
				names.Insert(mount.Name)
			}
			for _, device := range container.VolumeDevices {
				names.Insert(device.Name)
			}
		}
	}
	return names
}

func retiringVolumesForMerge(live, desired *corev1.PodSpec, operator, external *fieldpath.Set) []corev1.Volume {
	requested := sets.New[string]()
	for _, volume := range desired.Volumes {
		requested.Insert(volume.Name)
	}
	referenced := referencedVolumes(live)
	var retiring []corev1.Volume
	for _, volume := range live.Volumes {
		if requested.Has(volume.Name) {
			continue
		}
		path := fieldpath.MakePathOrDie("spec", "template", "spec", "volumes", fieldpath.KeyByFields("name", volume.Name))
		if ownsFieldOrChild(operator, path) && (referenced.Has(volume.Name) || ownsFieldOrChild(external, path)) {
			retiring = append(retiring, volume)
		}
	}
	return retiring
}

func ownsFieldOrChild(fields *fieldpath.Set, path fieldpath.Path) bool {
	if fields.Has(path) {
		return true
	}
	for _, element := range path {
		fields = fields.WithPrefix(element)
	}
	return !fields.Empty()
}

func resolveRetiringVolumes(prepared, intent, merged *appsv1.StatefulSet, retiring []corev1.Volume) bool {
	if len(retiring) == 0 {
		return false
	}
	referenced := referencedVolumes(&merged.Spec.Template.Spec)
	for _, claim := range merged.Spec.VolumeClaimTemplates {
		referenced.Delete(claim.Name)
	}
	changed := false
	volumes := merged.Spec.Template.Spec.Volumes
	for _, volume := range retiring {
		i := slices.IndexFunc(volumes, func(v corev1.Volume) bool { return v.Name == volume.Name })
		if i >= 0 && equality.Semantic.DeepEqual(volumes[i].VolumeSource, volume.VolumeSource) {
			continue
		}
		if referenced.Has(volume.Name) {
			intent.Spec.Template.Spec.Volumes = append(intent.Spec.Template.Spec.Volumes, volume)
			changed = true
		} else if i >= 0 {
			// A co-owned fragment is not a complete independent volume definition.
			live := &prepared.Spec.Template.Spec.Volumes
			*live = slices.DeleteFunc(*live, func(v corev1.Volume) bool { return v.Name == volume.Name })
			changed = true
		}
	}
	return changed
}

func defaultRenderedPullPolicies(desired *corev1.PodSpec, external *fieldpath.Set) {
	for _, list := range []struct {
		name       string
		containers []corev1.Container
	}{{"containers", desired.Containers}, {"initContainers", desired.InitContainers}} {
		for i := range list.containers {
			container := &list.containers[i]
			path := append(containerPath(list.name, container.Name), fieldpath.MakePathOrDie("imagePullPolicy")...)
			if container.ImagePullPolicy != "" || external.Has(path) {
				continue
			}
			image, _, digest := strings.Cut(container.Image, "@")
			tagStart := strings.LastIndexByte(image, ':')
			hasTag := tagStart > strings.LastIndexByte(image, '/')
			container.ImagePullPolicy = corev1.PullIfNotPresent
			if hasTag && image[tagStart+1:] == "latest" || !hasTag && !digest {
				container.ImagePullPolicy = corev1.PullAlways
			}
		}
	}
}

func removeSharedRetiredContainers(live, desired *corev1.PodSpec, operator, external *fieldpath.Set) bool {
	changed := false
	for _, list := range []struct {
		name    string
		live    *[]corev1.Container
		desired []corev1.Container
	}{{"containers", &live.Containers, desired.Containers}, {"initContainers", &live.InitContainers, desired.InitContainers}} {
		*list.live = slices.DeleteFunc(*list.live, func(container corev1.Container) bool {
			if slices.ContainsFunc(list.desired, func(wanted corev1.Container) bool { return wanted.Name == container.Name }) {
				return false
			}
			path := containerPath(list.name, container.Name)
			if !operator.Has(path) && !operator.Has(append(path, fieldpath.MakePathOrDie("image")...)) {
				return false
			}
			shared := ownsFieldOrChild(external, path)
			changed = changed || shared
			return shared
		})
	}
	return changed
}

func clearIncompatibleSources(live, intent *corev1.PodSpec) bool {
	changed := false
	for i := range live.Volumes {
		volume := &live.Volumes[i]
		for _, wanted := range intent.Volumes {
			if volume.Name == wanted.Name && volumeSourceKind(volume.VolumeSource) != volumeSourceKind(wanted.VolumeSource) {
				volume.VolumeSource = corev1.VolumeSource{}
				changed = true
				break
			}
		}
	}
	forRenderedContainers(live, intent, func(current, desired *corev1.Container) {
		changed = clearIncompatibleHandlers(current, desired) || changed
		for i := range current.Env {
			env := &current.Env[i]
			j := slices.IndexFunc(desired.Env, func(wanted corev1.EnvVar) bool { return wanted.Name == env.Name })
			if j < 0 {
				continue
			}
			wanted := desired.Env[j]
			// An omitted literal on a rendered name means empty, not unowned.
			if (wanted.ValueFrom != nil || wanted.Value == "") && env.Value != "" {
				env.Value = ""
				changed = true
			}
			if env.ValueFrom != nil && (wanted.ValueFrom == nil || sourceKind(*env.ValueFrom) != sourceKind(*wanted.ValueFrom)) {
				env.ValueFrom = nil
				changed = true
			}
		}
	})
	return changed
}

func volumeSourceKind(source corev1.VolumeSource) int {
	// Kubernetes defaults a missing volume source to EmptyDir.
	if source == (corev1.VolumeSource{}) {
		source.EmptyDir = &corev1.EmptyDirVolumeSource{}
	}
	return sourceKind(source)
}

func clearIncompatibleHandlers(live, desired *corev1.Container) bool {
	changed := false
	for _, pair := range [][2]*corev1.Probe{{live.ReadinessProbe, desired.ReadinessProbe}, {live.LivenessProbe, desired.LivenessProbe}, {live.StartupProbe, desired.StartupProbe}} {
		if pair[0] != nil && pair[1] != nil {
			changed = clearIncompatibleHandler(&pair[0].ProbeHandler, &pair[1].ProbeHandler) || changed
		}
	}
	if live.Lifecycle != nil && desired.Lifecycle != nil {
		changed = clearIncompatibleHandler(live.Lifecycle.PostStart, desired.Lifecycle.PostStart) || changed
		changed = clearIncompatibleHandler(live.Lifecycle.PreStop, desired.Lifecycle.PreStop) || changed
	}
	return changed
}

func clearIncompatibleHandler[T any](live, desired *T) bool {
	if live == nil || desired == nil || sourceKind(*live) == sourceKind(*desired) {
		return false
	}
	var zero T
	*live = zero
	return true
}

// Kubernetes models volume/env sources and probe/lifecycle handlers as unions.
func sourceKind(source any) int {
	fields := reflect.ValueOf(source)
	for i := range fields.NumField() {
		if !fields.Field(i).IsZero() {
			return i
		}
	}
	return -1
}

func forRenderedContainers(live, desired *corev1.PodSpec, apply func(live, desired *corev1.Container)) {
	for _, pair := range [][2][]corev1.Container{{live.Containers, desired.Containers}, {live.InitContainers, desired.InitContainers}} {
		for i := range pair[0] {
			for j := range pair[1] {
				if pair[0][i].Name == pair[1][j].Name {
					apply(&pair[0][i], &pair[1][j])
					break
				}
			}
		}
	}
}
