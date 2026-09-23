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
	"os"

	api "github.com/pravega/zookeeper-operator/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NewDefaultCluster returns a cluster with an empty spec, which will be filled
// with default values. ZK_IMAGE_REPOSITORY / ZK_IMAGE_TAG override the
// ZooKeeper image, so the suite can test an image built from the same commit.
func NewDefaultCluster(namespace string) *api.ZookeeperCluster {
	cluster := &api.ZookeeperCluster{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ZookeeperCluster",
			APIVersion: "zookeeper.pravega.io/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "zookeeper",
			Namespace: namespace,
		},
		Spec: api.ZookeeperClusterSpec{},
	}
	cluster.Spec.Image.Repository = os.Getenv("ZK_IMAGE_REPOSITORY")
	cluster.Spec.Image.Tag = os.Getenv("ZK_IMAGE_TAG")
	return cluster
}

func NewClusterWithVersion(namespace, version string) *api.ZookeeperCluster {
	cluster := NewDefaultCluster(namespace)
	cluster.Spec = api.ZookeeperClusterSpec{
		Image: api.ContainerImage{
			Tag: version,
		},
	}
	return cluster
}

func NewClusterWithEmptyDir(namespace string) *api.ZookeeperCluster {
	cluster := NewDefaultCluster(namespace)
	cluster.Spec = api.ZookeeperClusterSpec{
		StorageType: "ephemeral",
	}
	return cluster
}
