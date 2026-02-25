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
	"strings"

	corev1 "k8s.io/api/core/v1"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	zk_e2eutil "github.com/pravega/zookeeper-operator/pkg/test/e2e/e2eutil"
)

// E2E test for Logback config: script copies operator-generated logback files to
// the runtime config dir, and the desired log level (INFO by default) is applied.
var _ = Describe("Logback config", func() {
	Context("Script copies Logback files and desired log level is applied", func() {
		It("should have logback files in runtime config dir and no DEBUG from ZooKeeper/Jetty when INFO is desired", func() {
			By("creating Zookeeper cluster")
			cluster := zk_e2eutil.NewDefaultCluster(testNamespace)
			cluster.WithDefaults()
			cluster.Status.Init()
			cluster.Spec.Persistence.VolumeReclaimPolicy = "Delete"

			zk, err := zk_e2eutil.CreateCluster(logger, k8sClient, cluster)
			Expect(err).NotTo(HaveOccurred())

			podSize := 3
			Expect(zk_e2eutil.WaitForClusterToBecomeReady(logger, k8sClient, cluster, podSize)).NotTo(HaveOccurred())

			By("fetching a running pod")
			podList, err := zk_e2eutil.GetPods(k8sClient, zk)
			Expect(err).NotTo(HaveOccurred())
			Expect(podList.Items).NotTo(BeEmpty())
			var pod *corev1.Pod
			for i := range podList.Items {
				p := &podList.Items[i]
				if p.Status.Phase == corev1.PodRunning {
					pod = p
					break
				}
			}
			Expect(pod).NotTo(BeNil(), "at least one pod should be Running")

			By("verifying startup script copied Logback files to runtime config dir")
			stdout, stderr, err := zk_e2eutil.PodExec(ctx, cfg, zk.Namespace, pod.Name, "zookeeper", []string{"ls", "/data/conf"})
			Expect(err).NotTo(HaveOccurred(), "exec ls /data/conf: stdout=%q stderr=%q", stdout, stderr)
			Expect(stdout).To(ContainSubstring("logback.xml"), "runtime config dir should contain logback.xml")
			Expect(stdout).To(ContainSubstring("logback-quiet.xml"), "runtime config dir should contain logback-quiet.xml")

			By("verifying desired log level is applied (no DEBUG from ZooKeeper/Jetty when we expect INFO)")
			logs, err := zk_e2eutil.GetPodLogs(ctx, cfg, zk.Namespace, pod.Name, "zookeeper", &corev1.PodLogOptions{TailLines: intPtr(200)})
			Expect(err).NotTo(HaveOccurred())
			// The main regression: when operator config asks for INFO, we must not see DEBUG from these packages.
			// If DEBUG appears, Logback config was not discovered or not applied.
			lines := strings.Split(logs, "\n")
			var debugLines []string
			for _, line := range lines {
				if strings.Contains(line, " DEBUG ") && (strings.Contains(line, "org.apache.zookeeper") || strings.Contains(line, "org.eclipse.jetty")) {
					debugLines = append(debugLines, strings.TrimSpace(line))
				}
			}
			Expect(debugLines).To(BeEmpty(), "desired level is INFO but found DEBUG from ZooKeeper/Jetty (config not applied): %v; logs (excerpt): %s", debugLines, truncate(logs, 1500))

			By("deleting Zookeeper cluster")
			Expect(k8sClient.Delete(ctx, zk)).Should(Succeed())
			Expect(zk_e2eutil.WaitForClusterToTerminate(logger, k8sClient, zk)).NotTo(HaveOccurred())
		})
	})
})

func intPtr(n int) *int64 {
	v := int64(n)
	return &v
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
