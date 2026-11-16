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
	"context"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	api "github.com/pravega/zookeeper-operator/api/v1beta1"
	zk_e2eutil "github.com/pravega/zookeeper-operator/pkg/test/e2e/e2eutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

var _ = Describe("Operations with multiple clusters", func() {
	Context("Perform create, update, delete and recreate operations on 3 clusters", func() {
		It("should isolate lifecycle operations across clusters", func() {
			podUID := func(cluster *api.ZookeeperCluster) types.UID {
				var uid types.UID
				ExpectWithOffset(1, wait.PollUntilContextTimeout(ctx, zk_e2eutil.RetryInterval, zk_e2eutil.ReadyTimeout, true, func(ctx context.Context) (bool, error) {
					pods, err := zk_e2eutil.GetPods(ctx, k8sClient, cluster)
					if err != nil {
						return false, err
					}
					if len(pods.Items) != 1 || pods.Items[0].DeletionTimestamp != nil {
						return false, nil
					}
					pod := pods.Items[0]
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
							uid = pod.UID
							return true, nil
						}
					}
					return false, nil
				})).To(Succeed(), "expected one live ready pod for %s/%s", cluster.Namespace, cluster.Name)
				return uid
			}

			By("creating three single-member clusters; dedicated tests retain quorum-sized coverage")
			defaultCluster := zk_e2eutil.NewDefaultCluster(testNamespace)

			defaultCluster.Spec.Replicas = 1
			defaultCluster.WithDefaults()
			defaultCluster.Status.Init()
			defaultCluster.Name = "zk1"
			defaultCluster.Spec.Persistence.VolumeReclaimPolicy = "Delete"

			zk1, err := zk_e2eutil.CreateCluster(ctx, logger, k8sClient, defaultCluster)
			Expect(err).NotTo(HaveOccurred())

			podSize := 1
			Expect(zk_e2eutil.WaitForClusterToBecomeReady(ctx, logger, k8sClient, zk1, podSize)).NotTo(HaveOccurred())
			zk1UID := podUID(zk1)

			defaultCluster = zk_e2eutil.NewDefaultCluster(testNamespace)

			defaultCluster.Spec.Replicas = 1
			defaultCluster.WithDefaults()
			defaultCluster.Status.Init()
			defaultCluster.Spec.Persistence.VolumeReclaimPolicy = "Delete"
			defaultCluster.Name = "zk2"
			initialVersion := "0.2.7"
			upgradeVersion := "0.2.9"
			defaultCluster.Spec.Image = api.ContainerImage{
				Repository: "pravega/zookeeper",
				Tag:        initialVersion,
			}
			zk2, err := zk_e2eutil.CreateCluster(ctx, logger, k8sClient, defaultCluster)
			Expect(err).NotTo(HaveOccurred())

			Expect(zk_e2eutil.WaitForClusterToBecomeReady(ctx, logger, k8sClient, zk2, podSize)).NotTo(HaveOccurred())
			zk2UID := podUID(zk2)
			Expect(podUID(zk1)).To(Equal(zk1UID), "creating zk2 must not replace zk1")

			// This is to get the latest Zookeeper cluster object
			zk2, err = zk_e2eutil.GetCluster(ctx, logger, k8sClient, zk2)
			Expect(err).NotTo(HaveOccurred())
			Expect(zk2.Status.CurrentVersion).To(Equal(initialVersion))

			defaultCluster = zk_e2eutil.NewDefaultCluster(testNamespace)

			defaultCluster.Spec.Replicas = 1
			defaultCluster.WithDefaults()
			defaultCluster.Status.Init()
			defaultCluster.Name = "zk3"
			defaultCluster.Spec.Persistence.VolumeReclaimPolicy = "Delete"

			zk3, err := zk_e2eutil.CreateCluster(ctx, logger, k8sClient, defaultCluster)
			Expect(err).NotTo(HaveOccurred())

			Expect(zk_e2eutil.WaitForClusterToBecomeReady(ctx, logger, k8sClient, zk3, podSize)).NotTo(HaveOccurred())
			zk3UID := podUID(zk3)
			Expect(podUID(zk1)).To(Equal(zk1UID), "creating zk3 must not replace zk1")
			Expect(podUID(zk2)).To(Equal(zk2UID), "creating zk3 must not replace zk2")

			// This is to get the latest zk cluster object
			zk1, err = zk_e2eutil.GetCluster(ctx, logger, k8sClient, zk1)
			Expect(err).NotTo(HaveOccurred())

			By("scaling zk1 from one to three members, peaking at five ZooKeeper pods")
			zk1.Spec.Replicas = 3
			podSize = 3

			Expect(zk_e2eutil.UpdateCluster(ctx, logger, k8sClient, zk1)).NotTo(HaveOccurred())

			Expect(zk_e2eutil.WaitForClusterToBecomeReady(ctx, logger, k8sClient, zk1, podSize)).NotTo(HaveOccurred())
			Expect(podUID(zk2)).To(Equal(zk2UID), "scaling zk1 must not replace zk2")
			Expect(podUID(zk3)).To(Equal(zk3UID), "scaling zk1 must not replace zk3")

			zk1, err = zk_e2eutil.GetCluster(ctx, logger, k8sClient, zk1)
			Expect(err).NotTo(HaveOccurred())

			By("scaling zk1 back to one member")
			zk1.Spec.Replicas = 1
			podSize = 1

			Expect(zk_e2eutil.UpdateCluster(ctx, logger, k8sClient, zk1)).NotTo(HaveOccurred())

			Expect(zk_e2eutil.WaitForClusterToBecomeReady(ctx, logger, k8sClient, zk1, podSize)).NotTo(HaveOccurred())
			Expect(wait.PollUntilContextTimeout(ctx, zk_e2eutil.RetryInterval, zk_e2eutil.ReadyTimeout, true, func(ctx context.Context) (bool, error) {
				pods, err := zk_e2eutil.GetPods(ctx, k8sClient, zk1)
				return err == nil && len(pods.Items) == podSize, err
			})).To(Succeed(), "scaled-down pods must terminate before continuing")
			zk1UID = podUID(zk1)
			Expect(podUID(zk2)).To(Equal(zk2UID), "scaling down zk1 must not replace zk2")
			Expect(podUID(zk3)).To(Equal(zk3UID), "scaling down zk1 must not replace zk3")

			// This is to get the latest Zookeeper cluster object
			zk2, err = zk_e2eutil.GetCluster(ctx, logger, k8sClient, zk2)
			Expect(err).NotTo(HaveOccurred())

			By("upgrading zk2 without disturbing its neighbors")
			zk2.Spec.Image.Tag = upgradeVersion

			Expect(zk_e2eutil.UpdateCluster(ctx, logger, k8sClient, zk2)).NotTo(HaveOccurred())

			Expect(zk_e2eutil.WaitForClusterToUpgrade(ctx, logger, k8sClient, zk2, upgradeVersion)).NotTo(HaveOccurred())

			// This is to get the latest Zookeeper cluster object
			zk2, err = zk_e2eutil.GetCluster(ctx, logger, k8sClient, zk2)
			Expect(err).NotTo(HaveOccurred())

			Expect(zk2.Spec.Image.Tag).To(Equal(upgradeVersion))
			Expect(zk2.Status.CurrentVersion).To(Equal(upgradeVersion))
			Expect(zk2.Status.TargetVersion).To(Equal(""))
			Expect(zk_e2eutil.WaitForClusterToBecomeReady(ctx, logger, k8sClient, zk2, podSize)).NotTo(HaveOccurred())
			upgradedUID := podUID(zk2)
			Expect(upgradedUID).NotTo(Equal(zk2UID), "upgrading zk2 must replace its pod")
			zk2UID = upgradedUID
			Expect(podUID(zk1)).To(Equal(zk1UID), "upgrading zk2 must not replace zk1")
			Expect(podUID(zk3)).To(Equal(zk3UID), "upgrading zk2 must not replace zk3")

			// This is to get the latest zk cluster object
			zk3, err = zk_e2eutil.GetCluster(ctx, logger, k8sClient, zk3)
			Expect(err).NotTo(HaveOccurred())

			By("replacing zk3's pod without disturbing its neighbors")
			podDeleteCount := 1
			Expect(zk_e2eutil.DeletePods(ctx, logger, k8sClient, zk3, podDeleteCount)).NotTo(HaveOccurred())

			Expect(zk_e2eutil.WaitForClusterToBecomeReady(ctx, logger, k8sClient, zk3, podSize)).NotTo(HaveOccurred())
			zk3UID = podUID(zk3)
			Expect(podUID(zk1)).To(Equal(zk1UID), "recovering zk3 must not replace zk1")
			Expect(podUID(zk2)).To(Equal(zk2UID), "recovering zk3 must not replace zk2")

			By("deleting and recreating zk1 while both neighbors remain ready")
			Expect(zk_e2eutil.DeleteCluster(ctx, logger, k8sClient, zk1)).NotTo(HaveOccurred())

			Expect(zk_e2eutil.WaitForClusterToTerminate(ctx, logger, k8sClient, zk1)).NotTo(HaveOccurred())
			Expect(podUID(zk2)).To(Equal(zk2UID), "deleting zk1 must not replace zk2")
			Expect(podUID(zk3)).To(Equal(zk3UID), "deleting zk1 must not replace zk3")

			//Recreating cluster with same name
			defaultCluster = zk_e2eutil.NewDefaultCluster(testNamespace)
			defaultCluster.Spec.Replicas = 1
			defaultCluster.WithDefaults()
			defaultCluster.Status.Init()
			defaultCluster.Name = "zk1"
			defaultCluster.Spec.Persistence.VolumeReclaimPolicy = "Delete"

			zk1, err = zk_e2eutil.CreateCluster(ctx, logger, k8sClient, defaultCluster)
			Expect(err).NotTo(HaveOccurred())

			Expect(zk_e2eutil.WaitForClusterToBecomeReady(ctx, logger, k8sClient, zk1, podSize)).NotTo(HaveOccurred())
			Expect(podUID(zk1)).NotTo(Equal(zk1UID), "recreating zk1 must create a new pod")
			Expect(podUID(zk2)).To(Equal(zk2UID), "recreating zk1 must not replace zk2")
			Expect(podUID(zk3)).To(Equal(zk3UID), "recreating zk1 must not replace zk3")

			By("deleting all three clusters")
			Expect(zk_e2eutil.DeleteCluster(ctx, logger, k8sClient, zk1)).NotTo(HaveOccurred())
			Expect(zk_e2eutil.WaitForClusterToTerminate(ctx, logger, k8sClient, zk1)).NotTo(HaveOccurred())
			Expect(zk_e2eutil.DeleteCluster(ctx, logger, k8sClient, zk2)).NotTo(HaveOccurred())
			Expect(zk_e2eutil.WaitForClusterToTerminate(ctx, logger, k8sClient, zk2)).NotTo(HaveOccurred())
			Expect(zk_e2eutil.DeleteCluster(ctx, logger, k8sClient, zk3)).NotTo(HaveOccurred())
			Expect(zk_e2eutil.WaitForClusterToTerminate(ctx, logger, k8sClient, zk3)).NotTo(HaveOccurred())
		})
	})
})
