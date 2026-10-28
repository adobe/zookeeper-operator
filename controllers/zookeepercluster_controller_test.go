/**
 * Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package controllers

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ghodss/yaml"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/pravega/zookeeper-operator/api/v1beta1"
	"github.com/pravega/zookeeper-operator/pkg/controller/config"
	"github.com/pravega/zookeeper-operator/pkg/utils"
	"github.com/pravega/zookeeper-operator/pkg/zk"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

func TestZookeepercluster(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ZookeeperCluster Controller Spec")
}

func TestGenerateYAMLRejectsInvalidLogging(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		logging v1beta1.LoggingConfig
		field   string
	}{
		{v1beta1.LoggingConfig{Level: "invalid"}, "spec.logging.level"},
		{v1beta1.LoggingConfig{Format: "invalid"}, "spec.logging.format"},
	} {
		z := &v1beta1.ZookeeperCluster{}
		z.Spec.Logging = tc.logging
		r := &ZookeeperClusterReconciler{}
		if err := r.GenerateYAML(z); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Fatalf("expected validation error for %s, got %v", tc.field, err)
		}
	}
	files, err := os.ReadDir(".")
	if err != nil || len(files) != 0 {
		t.Fatalf("invalid logging created output files: %v, error: %v", files, err)
	}
}

func TestGenerateYAMLProjectsDefaultLogging(t *testing.T) {
	t.Chdir(t.TempDir())
	z := &v1beta1.ZookeeperCluster{ObjectMeta: metav1.ObjectMeta{Name: "example"}}
	if err := (&ZookeeperClusterReconciler{}).GenerateYAML(z); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("ZookeeperCluster/StatefulSet.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var sts appsv1.StatefulSet
	if err := yaml.Unmarshal(data, &sts); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-config"], `<root level="INFO">`) {
		t.Fatal("exported StatefulSet does not contain its default Logback config")
	}
	for _, volume := range sts.Spec.Template.Spec.Volumes {
		if volume.Name == "conf" && volume.Projected != nil {
			return
		}
	}
	t.Fatal("exported StatefulSet does not project the default Logback config")
}

type MockZookeeperClient struct {
	// dummy struct
}

func (client *MockZookeeperClient) Connect(zkUri string) (err error) {
	// do nothing
	return nil
}

func (client *MockZookeeperClient) CreateNode(zoo *v1beta1.ZookeeperCluster, zNodePath string) (err error) {
	return nil
}

func (client *MockZookeeperClient) UpdateNode(path string, data string, version int32) (err error) {
	return nil
}

func (client *MockZookeeperClient) NodeExists(zNodePath string) (version int32, err error) {
	return 0, nil
}

func (client *MockZookeeperClient) Close() {
	return
}

var _ = Describe("ZookeeperCluster Controller", func() {
	const (
		Name      = "example"
		Namespace = "default"
	)

	var (
		s            = scheme.Scheme
		mockZkClient = new(MockZookeeperClient)
		r            *ZookeeperClusterReconciler
	)

	Context("Reconcile", func() {
		var (
			res reconcile.Result
			req reconcile.Request
			z   *v1beta1.ZookeeperCluster
		)

		BeforeEach(func() {
			req = reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      Name,
					Namespace: Namespace,
				},
			}
			z = &v1beta1.ZookeeperCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      Name,
					Namespace: Namespace,
				},
			}
			s.AddKnownTypes(v1beta1.GroupVersion, z)
		})

		Context("Logging", func() {
			var cl client.WithWatch

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				z.Spec.Pod.Annotations = map[string]string{"custom": "keep"}
				cl = fake.NewClientBuilder().WithScheme(s).
					WithRuntimeObjects(z, zk.MakeConfigMap(z), zk.MakeStatefulSet(z)).
					WithStatusSubresource(z).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
			})

			It("should not roll an existing StatefulSet with unchanged default logging", func() {
				sts := &appsv1.StatefulSet{}
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				original := sts.Spec.Template.DeepCopy()

				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).NotTo(HaveOccurred())
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				Ω(sts.Spec.Template).To(Equal(*original))
			})

			It("should apply logging updates and repairs without repeated template changes", func() {
				for _, logging := range []v1beta1.LoggingConfig{
					{Level: "WARN", Format: "text"},
					{Level: "WARN", Format: "json"},
					{Level: "INFO", Format: "text"},
				} {
					Ω(cl.Get(context.TODO(), req.NamespacedName, z)).To(Succeed())
					z.Spec.Logging = logging
					Ω(cl.Update(context.TODO(), z)).To(Succeed())
					_, err := r.Reconcile(context.TODO(), req)
					Ω(err).NotTo(HaveOccurred())
					cm := &corev1.ConfigMap{}
					cmKey := types.NamespacedName{Name: z.ConfigMapName(), Namespace: Namespace}
					Ω(cl.Get(context.TODO(), cmKey, cm)).To(Succeed())
					Ω(cm.Data["logback.xml"]).To(ContainSubstring(`<root level="` + logging.Level + `">`))
					sts := &appsv1.StatefulSet{}
					Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
					Ω(sts.Spec.Template.Annotations).To(HaveKeyWithValue("custom", "keep"))
					Ω(sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-config"]).To(Equal(cm.Data["logback.xml"]))
					var conf corev1.Volume
					for _, volume := range sts.Spec.Template.Spec.Volumes {
						if volume.Name == "conf" {
							conf = volume
						}
					}
					Ω(conf.Projected).NotTo(BeNil())
					if logging.Level == "INFO" && logging.Format == "text" {
						Ω(sts.Spec.Template.Annotations).NotTo(HaveKey("zookeeper.pravega.io/logback-hash"))
					} else {
						Ω(sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-hash"]).NotTo(BeEmpty())
					}
					template := sts.Spec.Template.DeepCopy()
					expectedLogback := cm.Data["logback.xml"]
					cm.Data["logback.xml"] = "manual edit"
					Ω(cl.Update(context.TODO(), cm)).To(Succeed())
					_, err = r.Reconcile(context.TODO(), req)
					Ω(err).NotTo(HaveOccurred())
					Ω(cl.Get(context.TODO(), cmKey, cm)).To(Succeed())
					Ω(cm.Data["logback.xml"]).To(Equal(expectedLogback))
					Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
					Ω(sts.Spec.Template).To(Equal(*template))
					Ω(cl.Get(context.TODO(), req.NamespacedName, z)).To(Succeed())
					Ω(z.Spec.TriggerRollingRestart).To(BeFalse())
					Ω(z.Spec.Pod.Annotations).To(Equal(map[string]string{"custom": "keep"}))
				}
			})

			It("should not update the pod template when the ConfigMap update fails", func() {
				sts := &appsv1.StatefulSet{}
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				original := sts.Spec.Template.DeepCopy()
				z.Spec.Logging.Level = "WARN"
				Ω(cl.Update(context.TODO(), z)).To(Succeed())
				r.Client = interceptor.NewClient(cl, interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						if _, ok := obj.(*corev1.ConfigMap); ok {
							return errors.New("config update failed")
						}
						return c.Update(ctx, obj, opts...)
					},
				})
				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).To(MatchError("config update failed"))
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				Ω(sts.Spec.Template).To(Equal(*original))
			})

			It("should reject invalid logging before changing managed resources", func() {
				z.Spec.Logging.Level = "invalid"
				Ω(cl.Update(context.TODO(), z)).To(Succeed())
				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).To(MatchError(ContainSubstring("spec.logging.level")))
			})

			It("should reject logging changes for the previous default image before rolling pods", func() {
				sts := &appsv1.StatefulSet{}
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				original := sts.Spec.Template.DeepCopy()
				z.Spec.Image.Tag = "3.8.4-0.2.15-adobe-20250923"
				z.Spec.Logging.Level = "WARN"
				Ω(cl.Update(context.TODO(), z)).To(Succeed())

				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).To(MatchError(ContainSubstring("upgrade")))
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				Ω(sts.Spec.Template).To(Equal(*original))
			})

			It("should validate fresh logging settings when the informer has an old CRD schema", func() {
				sts := &appsv1.StatefulSet{}
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				original := sts.Spec.Template.DeepCopy()
				z.Spec.Image.Tag = "3.8.4-0.2.15-adobe-20250923"
				Ω(cl.Update(context.TODO(), z)).To(Succeed())
				fresh := z.DeepCopy()
				fresh.Spec.Logging.Level = "WARN"
				r.APIReader = fake.NewClientBuilder().WithScheme(s).WithObjects(fresh).Build()

				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).To(MatchError(ContainSubstring("upgrade")))
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				Ω(sts.Spec.Template).To(Equal(*original))
			})

			It("should finalize a cluster with incompatible logging when it is deleted", func() {
				z.Spec.Image.Tag = "3.8.4-0.2.15-adobe-20250923"
				z.Spec.Logging.Level = "WARN"
				z.Spec.Persistence.VolumeReclaimPolicy = v1beta1.VolumeReclaimPolicyDelete
				z.Finalizers = []string{utils.ZkFinalizer}
				Ω(cl.Update(context.TODO(), z)).To(Succeed())
				Ω(cl.Delete(context.TODO(), z)).To(Succeed())

				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).NotTo(HaveOccurred())
				Ω(cl.Get(context.TODO(), req.NamespacedName, &v1beta1.ZookeeperCluster{})).NotTo(Succeed())
			})

			It("should project default logging when reverting an older customized StatefulSet", func() {
				sts := &appsv1.StatefulSet{}
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-hash"] = "previous"
				Ω(cl.Update(context.TODO(), sts)).To(Succeed())

				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).NotTo(HaveOccurred())
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				Ω(sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-config"]).To(ContainSubstring(`<root level="INFO">`))
				for _, volume := range sts.Spec.Template.Spec.Volumes {
					if volume.Name == "conf" {
						Ω(volume.Projected).NotTo(BeNil())
					}
				}
			})

			It("should project logging when recreating a missing StatefulSet", func() {
				sts := &appsv1.StatefulSet{}
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				Ω(cl.Delete(context.TODO(), sts)).To(Succeed())

				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).NotTo(HaveOccurred())
				Ω(cl.Get(context.TODO(), req.NamespacedName, sts)).To(Succeed())
				Ω(sts.Spec.Template.Annotations["zookeeper.pravega.io/logback-config"]).To(ContainSubstring(`<root level="INFO">`))
			})
		})

		Context("Before defaults are applied", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(z).WithStatusSubresource(z).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("shouldn't error", func() {
				Ω(err).To(BeNil())
			})

			It("should set the default zk spec options", func() {
				foundZk := &v1beta1.ZookeeperCluster{}
				err = cl.Get(context.TODO(), req.NamespacedName, foundZk)
				Ω(err).To(BeNil())
				Ω(foundZk.Spec.Replicas).To(BeEquivalentTo(3))
			})

			It("should requeue the request", func() {
				Ω(res.Requeue).To(BeTrue())
			})
		})

		Context("After defaults are applied", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(z).WithStatusSubresource(z).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not error", func() {
				Ω(err).To(BeNil())
			})

			It("should requeue after ReconcileTime delay", func() {
				Ω(res.RequeueAfter).To(Equal(ReconcileTime))
			})

			It("should create a config-map", func() {
				foundCm := &corev1.ConfigMap{}
				nn := types.NamespacedName{
					Name:      Name + "-configmap",
					Namespace: Namespace,
				}
				err = cl.Get(context.TODO(), nn, foundCm)
				Ω(err).To(BeNil())
			})

			It("should create a stateful-set", func() {
				foundSts := &appsv1.StatefulSet{}
				err = cl.Get(context.TODO(), req.NamespacedName, foundSts)
				Ω(err).To(BeNil())
				Ω(*foundSts.Spec.Replicas).To(BeEquivalentTo(3))
			})

			It("should create a client-service", func() {
				foundSvc := &corev1.Service{}
				nn := types.NamespacedName{
					Name:      Name + "-client",
					Namespace: Namespace,
				}
				err = cl.Get(context.TODO(), nn, foundSvc)
				Ω(err).To(BeNil())
			})

			It("should create a headless-service", func() {
				foundSvc := &corev1.Service{}
				nn := types.NamespacedName{
					Name:      Name + "-headless",
					Namespace: Namespace,
				}
				err = cl.Get(context.TODO(), nn, foundSvc)
				Ω(err).To(BeNil())
			})

			It("should create a pdb", func() {
				foundPdb := &policyv1.PodDisruptionBudget{}
				err = cl.Get(context.TODO(), req.NamespacedName, foundPdb)
				Ω(err).To(BeNil())
			})

		})

		Context("With update to sts", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Spec.Pod.ServiceAccountName = "zookeeper"
				z.Status.Init()
				next := z.DeepCopy()
				st := zk.MakeStatefulSet(z)
				next.Spec.Replicas = 6
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, st).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})

			It("should update the sts", func() {
				foundSts := &appsv1.StatefulSet{}
				err = cl.Get(context.TODO(), req.NamespacedName, foundSts)
				Ω(err).To(BeNil())
				Ω(*foundSts.Spec.Replicas).To(BeEquivalentTo(6))
			})
		})

		Context("With no update to sts", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				next := z.DeepCopy()
				st := zk.MakeStatefulSet(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, st).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})

			It("should update the sts", func() {
				foundSts := &appsv1.StatefulSet{}
				err = cl.Get(context.TODO(), req.NamespacedName, foundSts)
				Ω(err).To(BeNil())
			})

		})

		Context("With update to ImagePullSecrets", func() {
			var (
				cl   client.Client
				err  error
				next *v1beta1.ZookeeperCluster
				sa   *corev1.ServiceAccount
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Spec.Pod.ServiceAccountName = "zookeeper"
				z.Status.Init()
				next = z.DeepCopy()
				sa = zk.MakeServiceAccount(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, sa).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})

			It("should create the service account", func() {
				foundSA := &corev1.ServiceAccount{}
				err = cl.Get(context.TODO(), types.NamespacedName{Name: "zookeeper", Namespace: Namespace}, foundSA)
				Ω(err).To(BeNil())
				Ω(foundSA.ImagePullSecrets).To(HaveLen(0))
			})
			It("should update the service account", func() {
				next.Spec.Pod.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "test-pull-secret"}}
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, sa).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				_, err := r.Reconcile(context.TODO(), req)
				Ω(err).To(BeNil())

				foundSA := &corev1.ServiceAccount{}
				err = cl.Get(context.TODO(), types.NamespacedName{Name: "zookeeper", Namespace: Namespace}, foundSA)
				Ω(err).To(BeNil())
				Ω(foundSA.ImagePullSecrets).To(HaveLen(1))
			})
		})

		Context("upgrading the image for zookeepercluster", func() {
			var (
				cl  client.Client
				err error
			)
			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				next := z.DeepCopy()
				next.Spec.Image.Tag = "0.2.7"
				next.Status.CurrentVersion = "0.2.6"
				next.Status.SetPodsReadyConditionTrue()
				st := zk.MakeStatefulSet(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, st).WithStatusSubresource(next).Build()
				st = &appsv1.StatefulSet{}
				err = cl.Get(context.TODO(), req.NamespacedName, st)
				// changing the Revision value to simulate the upgrade scenario
				st.Status.CurrentRevision = "CurrentRevision"
				st.Status.UpdateRevision = "UpdateRevision"
				cl.Status().Update(context.TODO(), st)
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})

			It("should set the upgrade condition to true", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				Ω(err).To(BeNil())
				Ω(foundZookeeper.Status.IsClusterInUpgradingState()).To(BeTrue())
			})

			It("should set the target version", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				Ω(err).To(BeNil())
				Ω(foundZookeeper.Status.TargetVersion).To(BeEquivalentTo("0.2.7"))
			})

			It("should set the target version", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)

				Ω(err).To(BeNil())
				Ω(foundZookeeper.Status.TargetVersion).To(BeEquivalentTo("0.2.7"))
			})

			It("should check if the cluster is in upgrade failed state", func() {
				z.Status.SetErrorConditionTrue("UpgradeFailed", " ")
				cl.Status().Update(context.TODO(), z)
				res, err = r.Reconcile(context.TODO(), req)
				Ω(err).To(BeNil())
			})
		})

		Context("Checking for upgrade completion for zookeepercluster", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				next := z.DeepCopy()
				next.Spec.Image.Tag = "0.2.7"
				next.Status.CurrentVersion = "0.2.6"
				next.Status.TargetVersion = "0.2.7"
				next.Status.SetUpgradingConditionTrue(" ", " ")
				st := zk.MakeStatefulSet(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, st).WithStatusSubresource(next).Build()
				st = &appsv1.StatefulSet{}
				err = cl.Get(context.TODO(), req.NamespacedName, st)
				// changing the Revision value to simulate the upgrade scenario completion
				st.Status.CurrentRevision = "complete"
				st.Status.UpdateRevision = "complete"
				cl.Status().Update(context.TODO(), st)
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				res, err = r.Reconcile(context.TODO(), req)
				res, err = r.Reconcile(context.TODO(), req)
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})

			It("should set the currentversion to Image.tag", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				Ω(err).To(BeNil())
				Ω(foundZookeeper.Status.CurrentVersion).To(BeEquivalentTo("0.2.7"))
			})

			It("should set the target version to empty", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				Ω(err).To(BeNil())
				Ω(foundZookeeper.Status.TargetVersion).To(BeEquivalentTo(""))
			})
		})

		Context("Checking for upgrade failed for zookeepercluster", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				next := z.DeepCopy()
				next.Status.SetUpgradingConditionTrue(" ", "1")
				next.Status.TargetVersion = "0.2.7"
				st := zk.MakeStatefulSet(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, st).WithStatusSubresource(next).Build()
				st = &appsv1.StatefulSet{}
				err = cl.Get(context.TODO(), req.NamespacedName, st)
				// changing the Revision value to simulate the upgrade scenario
				st.Status.CurrentRevision = "currentRevision"
				st.Status.UpdateRevision = "updateRevision"
				st.Status.UpdatedReplicas = 2
				cl.Status().Update(context.TODO(), st)
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
				// sleeping for 3 seconds
				time.Sleep(3 * time.Second)
				// checking if more than 2 secs have passed from the last update time
				err = checkSyncTimeout(next, " ", 1, 2*time.Second)

			})

			It("checking update replicas", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				_, condition := foundZookeeper.Status.GetClusterCondition(v1beta1.ClusterConditionUpgrading)
				Ω(condition.Message).To(Equal("2"))
			})

			It("should raise an error", func() {
				Ω(err.Error()).To(Equal("progress deadline exceeded"))
			})
		})

		Context("Checking for healing of upgrade failed for zookeepercluster", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				next := z.DeepCopy()
				next.Status.SetErrorConditionTrue("UpgradeFailed", " ")
				next.Status.TargetVersion = "0.2.7"
				next.Status.CurrentVersion = "0.2.6"
				next.Status.ReadyReplicas = 3
				next.Spec.Replicas = 3
				next.Spec.Image.Tag = "0.2.7"
				st := zk.MakeStatefulSet(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, st).Build()
				st = &appsv1.StatefulSet{}
				err = cl.Get(context.TODO(), req.NamespacedName, st)
				// changing the Revision value to simulate the upgrade scenario
				st.Status.CurrentRevision = "updateRevision"
				st.Status.UpdateRevision = "updateRevision"
				st.Status.UpdatedReplicas = 2
				cl.Status().Update(context.TODO(), st)
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
				// sleeping for 3 seconds
				time.Sleep(3 * time.Second)
				// checking if more than 2 secs have passed from the last update time
				err = checkSyncTimeout(next, " ", 1, 2*time.Second)

			})

			It("checking update replicas", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				condition := foundZookeeper.Status.CurrentVersion
				Ω(condition).To(Equal("0.2.7"))
			})

			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})
		})

		Context("Upgrading with Targetversion empty", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				next := z.DeepCopy()
				next.Spec.Image.Tag = "0.2.7"
				next.Status.CurrentVersion = "0.2.6"
				next.Status.TargetVersion = ""
				next.Status.IsClusterInUpgradingState()
				st := zk.MakeStatefulSet(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, st).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})
			It("should set the upgrade condition to false", func() {
				foundZookeeper := &v1beta1.ZookeeperCluster{}
				_ = cl.Get(context.TODO(), req.NamespacedName, foundZookeeper)
				Ω(foundZookeeper.Status.IsClusterInUpgradingState()).To(Equal(false))
			})
		})

		Context("Checking result when request namespace does not contains zookeeper cluster", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(z).WithStatusSubresource(z).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				req.NamespacedName.Namespace = "temp"
				res, err = r.Reconcile(context.TODO(), req)
			})
			It("should have false in reconcile result", func() {
				Ω(res.Requeue).To(Equal(false))
				Ω(err).To(BeNil())
			})
		})

		Context("Checking client", func() {
			var (
				cl    client.Client
				err   error
				count int
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.Status.Init()
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(z).WithStatusSubresource(z).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				err = mockZkClient.Connect("127.0.0.0:2181")
				Ω(err).To(BeNil())
			})
			It("should not raise an error", func() {
				err = r.GenerateYAML(z)
				Ω(err).To(BeNil())
			})
			It("should not raise an error", func() {
				err = r.cleanupOrphanPVCs(z)
				Ω(err).To(BeNil())
			})
			It("should not raise an error", func() {
				z.Status.ReadyReplicas = -1
				z.Spec.Replicas = -1
				err = cl.Update(context.TODO(), z)
				err = r.cleanupOrphanPVCs(z)
				Ω(err).To(BeNil())
			})
			It("should not raise an error", func() {
				count, err = r.getPVCCount(z)
				_, err = r.getPVCList(z)
				Ω(err).To(BeNil())
				Ω(count).To(Equal(0))
			})
			It("should not raise an error", func() {
				_ = cl.Get(context.TODO(), req.NamespacedName, z)
				z.Spec.Persistence.VolumeReclaimPolicy = v1beta1.VolumeReclaimPolicyDelete
				cl.Update(context.TODO(), z)
				err = r.reconcileFinalizers(z)
				Ω(err).To(BeNil())
			})

			It("should delete pvc", func() {
				pvcDelete := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "Name",
						Namespace: "Namespace",
					},
				}
				r.Client.Create(context.TODO(), pvcDelete)
				r.deletePVC(*pvcDelete)
				r.deletePVC(*pvcDelete)
			})

			It("should not raise an error", func() {
				err = r.cleanUpAllPVCs(z)
				_ = os.RemoveAll("ZookeeperCluster")
				Ω(err).To(BeNil())
			})

			It("calling YamlExporterReconciler", func() {
				recon := YAMLExporterReconciler(z)
				Ω(recon).NotTo(BeNil())
			})
		})

		Context("With an update to the client svc", func() {
			var (
				cl  client.Client
				err error
			)

			BeforeEach(func() {
				z.WithDefaults()
				next := z.DeepCopy()
				next.Spec.Ports[0].ContainerPort = 2182
				svc := zk.MakeClientService(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, svc).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
			})

			It("should not raise an error", func() {
				Ω(err).ToNot(HaveOccurred())
			})
		})

		Context("reconcileFinalizers", func() {
			var (
				cl  client.Client
				err error
			)
			BeforeEach(func() {
				z.WithDefaults()
				z.Spec.Persistence = nil
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(z).WithStatusSubresource(z).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
				err = r.reconcileFinalizers(z)
				// update deletion timestamp
				_ = cl.Get(context.TODO(), req.NamespacedName, z)
				now := metav1.Now()
				z.SetDeletionTimestamp(&now)
				cl.Update(context.TODO(), z)
				err = r.reconcileFinalizers(z)
			})
			It("should not raise an error", func() {
				Ω(err).To(BeNil())
			})
		})

		Context("reconcileFinalizers", func() {
			var (
				cl  client.Client
				err error
			)
			BeforeEach(func() {
				z.WithDefaults()
				z.Spec.Persistence = nil
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(z).WithStatusSubresource(z).Build()
			})
			It("should have 1 finalizer, should not raise an error", func() {
				config.DisableFinalizer = false
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				err = r.reconcileFinalizers(z)
				Expect(z.ObjectMeta.Finalizers).To(HaveLen(1))
				Ω(err).To(BeNil())
			})
			It("should have 0 finalizer, should not raise an error", func() {
				config.DisableFinalizer = true
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				err = r.reconcileFinalizers(z)
				Expect(z.ObjectMeta.Finalizers).To(HaveLen(0))
				Ω(err).To(BeNil())
			})
		})

		Context("Checking resource version", func() {
			var (
				sts *appsv1.StatefulSet
			)

			BeforeEach(func() {
				z.WithDefaults()
				z.ResourceVersion = "100"
				sts = &appsv1.StatefulSet{}
				sts.Labels = make(map[string]string)
			})

			It("should return 1 as 100 > 99", func() {
				sts.Labels["owner-rv"] = "99"
				updated := compareResourceVersion(z, sts)
				Ω(updated).To(Equal(1))
			})

			It("should return 0 as 100 == 100", func() {
				sts.Labels["owner-rv"] = "100"
				updated := compareResourceVersion(z, sts)
				Ω(updated).To(Equal(0))
			})

			It("should return -1 as 100 < 101", func() {
				sts.Labels["owner-rv"] = "101"
				updated := compareResourceVersion(z, sts)
				Ω(updated).To(Equal(-1))
			})

			It("should return 1 as 100 > invalid numeric version 'xoxo'", func() {
				sts.Labels["owner-rv"] = "xoxo"
				updated := compareResourceVersion(z, sts)
				Ω(updated).To(Equal(1))
			})

			It("should return 0 as invalid zk version z0z0 == invalid numeric version 'x0x0'", func() {
				sts.Labels["owner-rv"] = "x0x0"
				z.ResourceVersion = "z0z0"
				updated := compareResourceVersion(z, sts)
				Ω(updated).To(Equal(0))
			})
		})

		Context("trigger rolling restart", func() {
			var (
				cl      client.Client
				err     error
				foundZk = &v1beta1.ZookeeperCluster{}
				next    *v1beta1.ZookeeperCluster
				svc     *corev1.Service
			)
			BeforeEach(func() {
				z.WithDefaults()
				next = z.DeepCopy()
				next.Spec.TriggerRollingRestart = true
				svc = zk.MakeClientService(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, svc).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
				err = cl.Get(context.TODO(), req.NamespacedName, foundZk)
			})

			It("should update restartTime annotation and reset triggerRollingRestart to false when triggerRollingRestart is set to true", func() {
				Ω(res.Requeue).To(Equal(true))
				Ω(err).To(BeNil())
				Ω(foundZk.Spec.TriggerRollingRestart).To(Equal(false))
				_, restartTimeExists := foundZk.Spec.Pod.Annotations["restartTime"]
				Ω(restartTimeExists).To(Equal(true))
			})

			It("should not update restartTime annotation when set triggerRollingRestart to false", func() {
				_, restartTimeExists := foundZk.Spec.Pod.Annotations["restartTime"]
				Ω(restartTimeExists).To(Equal(true))

				next.Spec.TriggerRollingRestart = false
				svc = zk.MakeClientService(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, svc).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)

				Ω(res.Requeue).To(Equal(false))
				Ω(err).To(BeNil())
				Ω(foundZk.Spec.TriggerRollingRestart).To(Equal(false))
				_, restartTimeExists = foundZk.Spec.Pod.Annotations["restartTime"]
				Ω(restartTimeExists).To(Equal(true))
			})

			It("should update restartTime annotation to new value when rolling restart is triggered multiple times", func() {
				oldRestartValue, restartTimeExists := foundZk.Spec.Pod.Annotations["restartTime"]
				Ω(restartTimeExists).To(Equal(true))

				// wait 1 second to ensure that restartTime, if set, will have a different value
				time.Sleep(1 * time.Second)

				// update the crd instance
				next.Spec.TriggerRollingRestart = false
				svc = zk.MakeClientService(z)
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
				err = cl.Get(context.TODO(), req.NamespacedName, foundZk)

				// check that restartTime was not updated
				Ω(res.Requeue).To(Equal(false))
				Ω(err).To(BeNil())
				Ω(foundZk.Spec.TriggerRollingRestart).To(Equal(false))
				_, restartTimeExists = foundZk.Spec.Pod.Annotations["restartTime"]
				Ω(restartTimeExists).To(Equal(true))

				// wait 1 second to ensure that restartTime, if set, will have a different value
				time.Sleep(1 * time.Second)

				// update the crd instance to trigger rolling restart
				next.Spec.TriggerRollingRestart = true
				svc = zk.MakeClientService(z)
				cl = fake.NewClientBuilder().WithScheme(scheme.Scheme).WithRuntimeObjects(next, svc).WithStatusSubresource(next).Build()
				r = &ZookeeperClusterReconciler{Client: cl, Scheme: s, ZkClient: mockZkClient}
				res, err = r.Reconcile(context.TODO(), req)
				err = cl.Get(context.TODO(), req.NamespacedName, foundZk)

				// check that restartTime was updated
				Ω(res.Requeue).To(Equal(true))
				Ω(err).To(BeNil())
				Ω(foundZk.Spec.TriggerRollingRestart).To(Equal(false))
				newRestartValue, restartTimeExists := foundZk.Spec.Pod.Annotations["restartTime"]
				Ω(restartTimeExists).To(Equal(true))
				Ω(oldRestartValue).NotTo(Equal(newRestartValue))
			})
		})
	})
})
