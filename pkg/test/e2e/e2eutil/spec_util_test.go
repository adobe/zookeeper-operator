/**
 * Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package e2eutil

import (
	"testing"

	api "github.com/pravega/zookeeper-operator/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
)

func TestClusterImageOverrides(t *testing.T) {
	for _, tt := range []struct {
		name string
		new  func(string) *api.ZookeeperCluster
	}{
		{name: "default", new: NewDefaultCluster},
		{name: "emptyDir", new: NewClusterWithEmptyDir},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ZK_IMAGE_REPOSITORY", "zookeeper")
			t.Setenv("ZK_IMAGE_TAG", "e2e")
			t.Setenv("ZK_IMAGE_PULL_POLICY", "IfNotPresent")

			cluster := tt.new("test")
			cluster.WithDefaults()
			want := api.ContainerImage{
				Repository: "zookeeper",
				Tag:        "e2e",
				PullPolicy: corev1.PullIfNotPresent,
			}
			if cluster.Spec.Image != want {
				t.Fatalf("image = %+v, want %+v", cluster.Spec.Image, want)
			}
			if tt.name == "emptyDir" && cluster.Spec.StorageType != "ephemeral" {
				t.Fatalf("storage type = %q, want ephemeral", cluster.Spec.StorageType)
			}
		})
	}
}

func TestClusterImageDefaultsWithoutOverrides(t *testing.T) {
	t.Setenv("ZK_IMAGE_REPOSITORY", "")
	t.Setenv("ZK_IMAGE_TAG", "")
	t.Setenv("ZK_IMAGE_PULL_POLICY", "")

	cluster := NewDefaultCluster("test")
	cluster.WithDefaults()
	if cluster.Spec.Image.Repository != api.DefaultZkContainerRepository ||
		cluster.Spec.Image.Tag != api.DefaultZkContainerVersion ||
		cluster.Spec.Image.PullPolicy != corev1.PullAlways {
		t.Fatalf("unexpected default image: %+v", cluster.Spec.Image)
	}
}
