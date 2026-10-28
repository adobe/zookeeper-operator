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
	"encoding/json"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pravega/zookeeper-operator/api/v1beta1"
	zk_e2eutil "github.com/pravega/zookeeper-operator/pkg/test/e2e/e2eutil"
)

// E2E test for Logback config: script copies operator-generated logback files to
// the runtime config dir, and the desired log level (INFO by default) is applied.
var _ = Describe("Logback config", func() {
	Context("Script copies Logback files and desired log level is applied", func() {
		It("should apply default logging and automatically roll out level and format changes", func() {
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

			By("verifying helper logging preserves machine-readable stdout")
			stdout, stderr, err = zk_e2eutil.PodExec(ctx, cfg, zk.Namespace, pod.Name, "zookeeper", []string{
				"bash", "-c", "source /usr/local/bin/zookeeperFunctions.sh; zkRunZu get-all localhost:2181",
			})
			Expect(err).NotTo(HaveOccurred(), "helper: stdout=%q stderr=%q", stdout, stderr)
			Expect(stdout).NotTo(BeEmpty())
			for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
				Expect(line).To(MatchRegexp(`^(server\.[0-9]+=.+|version=[0-9a-fA-F]+)$`))
			}
			Expect(stderr).NotTo(ContainSubstring("No SLF4J providers were found"))

			By("verifying the helper JVM has a 256 MiB default heap cap")
			stdout, stderr, err = zk_e2eutil.PodExec(ctx, cfg, zk.Namespace, pod.Name, "zookeeper", []string{
				"bash", "-c", "source /usr/local/bin/zookeeperFunctions.sh; CLIENT_JVMFLAGS='-XX:+PrintFlagsFinal -version' zkRunZu",
			})
			Expect(err).NotTo(HaveOccurred(), "helper JVM flags: stdout=%q stderr=%q", stdout, stderr)
			Expect(stdout).To(MatchRegexp(`(?m)^\s*size_t\s+MaxHeapSize\s*=\s*268435456\b`))

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

			for _, logging := range []v1beta1.LoggingConfig{
				{Level: "INFO", Format: "json"},
				{Level: "WARN", Format: "json"},
				{Level: "INFO", Format: "text"},
			} {
				By("changing only logging to " + logging.Level + "/" + logging.Format)
				Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(zk), zk)).To(Succeed())
				zk.Spec.Logging = logging
				Expect(zk.Spec.TriggerRollingRestart).To(BeFalse())
				Expect(zk_e2eutil.UpdateCluster(logger, k8sClient, zk)).To(Succeed())
				podList = waitForLoggingRollout(zk, podList)

				since := metav1.Now()
				for _, p := range podList.Items {
					stdout, stderr, err := zk_e2eutil.PodExec(ctx, cfg, zk.Namespace, p.Name, "zookeeper",
						[]string{"bash", "-ec", "cmp /conf/logback.xml /data/conf/logback.xml && test ! -L /data/conf/logback.xml"})
					Expect(err).NotTo(HaveOccurred(), "runtime Logback copy: stdout=%q stderr=%q", stdout, stderr)
					stdout, stderr, err = zk_e2eutil.PodExec(ctx, cfg, zk.Namespace, p.Name, "zookeeper",
						[]string{"bash", "-ec", "printf ruok | nc -w 2 127.0.0.1 2181"})
					Expect(err).NotTo(HaveOccurred(), "ruok: stdout=%q stderr=%q", stdout, stderr)
					Expect(stdout).To(Equal("imok"))
					logs, err := zk_e2eutil.GetPodLogs(ctx, cfg, zk.Namespace, p.Name, "zookeeper",
						&corev1.PodLogOptions{SinceTime: &since})
					Expect(err).NotTo(HaveOccurred())
					if logging.Format == "text" {
						Expect(logs).To(ContainSubstring(" INFO "))
						Expect(logs).To(ContainSubstring("Processing ruok command"))
						continue
					}
					Expect(logs).NotTo(ContainSubstring(" INFO "))
					events := 0
					for _, line := range strings.Split(logs, "\n") {
						if !strings.HasPrefix(line, "{") {
							continue
						}
						var event struct {
							Level      string `json:"level"`
							LoggerName string `json:"loggerName"`
							Message    string `json:"message"`
						}
						Expect(json.Unmarshal([]byte(line), &event)).To(Succeed())
						Expect(event.LoggerName).NotTo(BeEmpty())
						Expect(event.Message).NotTo(BeEmpty())
						if logging.Level == "WARN" {
							Expect(event.Level).To(BeElementOf("WARN", "ERROR"))
						} else {
							Expect(event.Level).To(BeElementOf("INFO", "WARN", "ERROR"))
						}
						if strings.HasPrefix(event.LoggerName, "org.apache.zookeeper") {
							events++
						}
					}
					if logging.Level == "INFO" {
						Expect(events).To(BeNumerically(">", 0), "expected actual JSON server log events, got: %s", logs)
					}
				}
			}

			By("deleting Zookeeper cluster")
			Expect(k8sClient.Delete(ctx, zk)).Should(Succeed())
			Expect(zk_e2eutil.WaitForClusterToTerminate(logger, k8sClient, zk)).NotTo(HaveOccurred())
		})
	})

	It("should reject unsupported logging values at CRD admission", func() {
		for _, tc := range []struct {
			logging v1beta1.LoggingConfig
			field   string
		}{
			{v1beta1.LoggingConfig{Level: "invalid"}, "spec.logging.level"},
			{v1beta1.LoggingConfig{Format: "invalid"}, "spec.logging.format"},
		} {
			cluster := zk_e2eutil.NewDefaultCluster(testNamespace)
			cluster.Name = "invalid-logging"
			cluster.WithDefaults()
			cluster.Spec.Logging = tc.logging
			err := k8sClient.Create(ctx, cluster)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected admission rejection, got %v", err)
			Expect(err.Error()).To(ContainSubstring(tc.field))
		}
	})
})

func waitForLoggingRollout(cluster *v1beta1.ZookeeperCluster, previous *corev1.PodList) *corev1.PodList {
	oldUIDs := make(map[string]types.UID, len(previous.Items))
	for _, pod := range previous.Items {
		oldUIDs[pod.Name] = pod.UID
	}
	var updated *corev1.PodList
	Eventually(func(g Gomega) {
		sts := &appsv1.StatefulSet{}
		g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), sts)).To(Succeed())
		g.Expect(sts.Status.ObservedGeneration).To(BeNumerically(">=", sts.Generation))
		g.Expect(sts.Status.CurrentRevision).To(Equal(sts.Status.UpdateRevision))
		g.Expect(sts.Status.ReadyReplicas).To(Equal(cluster.Spec.Replicas))
		if cluster.Spec.Logging.Level == "INFO" && cluster.Spec.Logging.Format == "text" {
			g.Expect(sts.Spec.Template.Annotations).NotTo(HaveKey("zookeeper.pravega.io/logback-hash"))
		} else {
			g.Expect(sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-hash"]).NotTo(BeEmpty())
		}
		pods, err := zk_e2eutil.GetPods(k8sClient, cluster)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods.Items).To(HaveLen(int(cluster.Spec.Replicas)))
		for _, pod := range pods.Items {
			g.Expect(pod.UID).NotTo(Equal(oldUIDs[pod.Name]))
			g.Expect(pod.DeletionTimestamp).To(BeNil())
			g.Expect(pod.Labels["controller-revision-hash"]).To(Equal(sts.Status.UpdateRevision))
			ready := false
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					ready = true
				}
			}
			g.Expect(ready).To(BeTrue(), "pod %s is not ready", pod.Name)
		}
		updated = pods
	}, 10*time.Minute, 2*time.Second).Should(Succeed())
	return updated
}

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
