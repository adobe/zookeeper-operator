/**
 * Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package e2e

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	zk_e2eutil "github.com/pravega/zookeeper-operator/pkg/test/e2e/e2eutil"
)

// Test create and recreate a Zookeeper cluster with the same name
var _ = Describe("Delete pods in zk clusters", func() {
	Context("Delete pods, check that pods get recreated", func() {
		It("should keep number of replicas consistent", func() {
			defaultCluster := zk_e2eutil.NewDefaultCluster(testNamespace)

			defaultCluster.WithDefaults()
			defaultCluster.Status.Init()
			defaultCluster.Spec.Persistence.VolumeReclaimPolicy = "Delete"
			By("create zk cluster")
			zk, err := zk_e2eutil.CreateCluster(ctx, logger, k8sClient, defaultCluster)
			Expect(err).NotTo(HaveOccurred())

			// A default zookeeper cluster should have 3 pods
			podSize := 3
			Expect(zk_e2eutil.WaitForClusterToBecomeReady(ctx, logger, k8sClient, zk, podSize)).NotTo(HaveOccurred())

			By("Delete one of the pods")
			podDeleteCount := 1
			Expect(zk_e2eutil.DeletePods(ctx, logger, k8sClient, zk, podDeleteCount)).NotTo(HaveOccurred())

			Expect(zk_e2eutil.WaitForClusterToBecomeReady(ctx, logger, k8sClient, zk, podSize)).NotTo(HaveOccurred())

			By("Delete two of the pods")
			podDeleteCount = 2
			Expect(zk_e2eutil.DeletePods(ctx, logger, k8sClient, zk, podDeleteCount)).NotTo(HaveOccurred())

			Expect(zk_e2eutil.WaitForClusterToBecomeReady(ctx, logger, k8sClient, zk, podSize)).NotTo(HaveOccurred())

			By("Delete all of the pods")
			podDeleteCount = 3
			Expect(zk_e2eutil.DeletePods(ctx, logger, k8sClient, zk, podDeleteCount)).NotTo(HaveOccurred())
			Expect(zk_e2eutil.WaitForClusterToBecomeReady(ctx, logger, k8sClient, zk, podSize)).NotTo(HaveOccurred())

			Expect(zk_e2eutil.DeleteCluster(ctx, logger, k8sClient, zk)).NotTo(HaveOccurred())
			Expect(zk_e2eutil.WaitForClusterToTerminate(ctx, logger, k8sClient, zk)).NotTo(HaveOccurred())
		})
	})
})
