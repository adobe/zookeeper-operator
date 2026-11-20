// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package controllers

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestClearInheritedSecurityOverrides(t *testing.T) {
	root, requestedUID, sidecarUID := int64(0), int64(1000), int64(2000)
	yes, no := true, false
	intent := &corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser: &requestedUID, RunAsNonRoot: &yes,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers:     []corev1.Container{{Name: "zookeeper"}, {Name: "sidecar", SecurityContext: &corev1.SecurityContext{RunAsUser: &sidecarUID}}},
		InitContainers: []corev1.Container{{Name: "init"}},
	}
	live := intent.DeepCopy()
	live.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &yes, ReadOnlyRootFilesystem: &yes}
	live.Containers[1].SecurityContext.RunAsNonRoot = &no
	live.InitContainers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: &root}
	live.Containers = append(live.Containers, corev1.Container{Name: "unmanaged", SecurityContext: &corev1.SecurityContext{RunAsUser: &sidecarUID}})
	if !clearInheritedSecurityOverrides(live, intent) {
		t.Fatal("expected inherited settings to be reconciled")
	}
	zoo, sidecar, init := live.Containers[0].SecurityContext, live.Containers[1].SecurityContext, live.InitContainers[0].SecurityContext
	if zoo.Privileged != nil || zoo.ReadOnlyRootFilesystem == nil || sidecar.RunAsNonRoot != nil || sidecar.RunAsUser == nil || *sidecar.RunAsUser != sidecarUID || init.RunAsUser != nil || live.Containers[2].SecurityContext.RunAsUser == nil {
		t.Fatal("repair did not enforce inheritance while keeping explicit and unrelated settings")
	}
}

func TestClearInheritedRunAsNonRootOverrides(t *testing.T) {
	for _, requested := range []bool{false, true} {
		liveFlag, root := !requested, int64(0)
		intent := &corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &requested},
			Containers:      []corev1.Container{{Name: "zookeeper"}},
		}
		live := intent.DeepCopy()
		live.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsNonRoot: &liveFlag, RunAsUser: &root}
		if !clearInheritedSecurityOverrides(live, intent) {
			t.Fatalf("expected conflicting runAsNonRoot to be repaired for %v", requested)
		}
		sc := live.Containers[0].SecurityContext
		if sc.RunAsNonRoot != nil || (sc.RunAsUser == nil) != requested {
			t.Fatalf("unexpected overrides for requested runAsNonRoot=%v: %+v", requested, sc)
		}
	}
}
