/**
 * Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (&the "License");
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
	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("Perform rolling restart on zk cluster", func() {
	Context("Check rolling restart operation", func() {
		It("should perform rolling restart", func() {
			cluster := zk_e2eutil.NewDefaultCluster(testNamespace)

			cluster.WithDefaults()
			cluster.Status.Init()
			cluster.Spec.Persistence.VolumeReclaimPolicy = "Delete"

			zk, err := zk_e2eutil.CreateCluster(ctx, logger, k8sClient, cluster)
			Expect(err).NotTo(HaveOccurred())

			// A default Zookeepercluster should have 3 replicas
			podSize := 3
			Expect(zk_e2eutil.WaitForClusterToBecomeReady(ctx, logger, k8sClient, zk, podSize)).NotTo(HaveOccurred())

			// This is to get the latest Zookeeper cluster object
			zk, err = zk_e2eutil.GetCluster(ctx, logger, k8sClient, zk)
			Expect(err).NotTo(HaveOccurred())
			podList, err := zk_e2eutil.GetPods(ctx, k8sClient, zk)
			Expect(err).NotTo(HaveOccurred())
			for i := 0; i < len(podList.Items); i++ {
				Expect(podList.Items[i].Annotations).NotTo(HaveKey("restartTime"))
			}
			Expect(zk.GetTriggerRollingRestart()).To(Equal(false))

			waitForRestart := func(previous []corev1.Pod) {
				Expect(zk_e2eutil.WaitForPodsReplaced(ctx, logger, k8sClient, zk, previous)).NotTo(HaveOccurred())
				Eventually(func() (bool, error) {
					current, err := zk_e2eutil.GetCluster(ctx, logger, k8sClient, zk)
					if err != nil {
						return false, err
					}
					return current.GetTriggerRollingRestart(), nil
				}, zk_e2eutil.ReadyTimeout, zk_e2eutil.RetryInterval).Should(BeFalse())
				Expect(zk_e2eutil.WaitForClusterToBecomeReady(ctx, logger, k8sClient, zk, podSize)).NotTo(HaveOccurred())
			}

			// Trigger a rolling restart
			zk.Spec.TriggerRollingRestart = true
			err = zk_e2eutil.UpdateCluster(ctx, logger, k8sClient, zk)
			Expect(err).NotTo(HaveOccurred())
			waitForRestart(podList.Items)

			zk, err = zk_e2eutil.GetCluster(ctx, logger, k8sClient, zk)
			Expect(err).NotTo(HaveOccurred())
			newPodList, err := zk_e2eutil.GetPods(ctx, k8sClient, zk)
			Expect(err).NotTo(HaveOccurred())
			firstRestartTime := map[string]string{}
			for i := 0; i < len(newPodList.Items); i++ {
				Expect(newPodList.Items[i].Annotations).To(HaveKey("restartTime"))
				firstRestartTime[newPodList.Items[i].Name] = newPodList.Items[i].Annotations["restartTime"]
			}
			Expect(zk.GetTriggerRollingRestart()).To(Equal(false))

			// Trigger a rolling restart again
			zk.Spec.TriggerRollingRestart = true
			err = zk_e2eutil.UpdateCluster(ctx, logger, k8sClient, zk)
			Expect(err).NotTo(HaveOccurred())
			waitForRestart(newPodList.Items)

			zk, err = zk_e2eutil.GetCluster(ctx, logger, k8sClient, zk)
			Expect(err).NotTo(HaveOccurred())
			newPodList2, err := zk_e2eutil.GetPods(ctx, k8sClient, zk)
			Expect(err).NotTo(HaveOccurred())
			for i := 0; i < len(newPodList2.Items); i++ {
				Expect(newPodList2.Items[i].Annotations).To(HaveKey("restartTime"))
				Expect(newPodList2.Items[i].Annotations["restartTime"]).NotTo(Equal(firstRestartTime[newPodList2.Items[i].Name]))
			}
			Expect(zk.GetTriggerRollingRestart()).To(Equal(false))

			// Delete cluster
			Expect(zk_e2eutil.DeleteCluster(ctx, logger, k8sClient, zk)).NotTo(HaveOccurred())

			Expect(zk_e2eutil.WaitForClusterToTerminate(ctx, logger, k8sClient, zk)).NotTo(HaveOccurred())
		})
	})
})
