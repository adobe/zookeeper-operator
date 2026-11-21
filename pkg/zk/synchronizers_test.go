/**
 * Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package zk_test

import (
	"github.com/pravega/zookeeper-operator/api/v1beta1"
	"github.com/pravega/zookeeper-operator/pkg/zk"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Synchronizers", func() {

	Context("with a valid update of config map", func() {
		var value string
		BeforeEach(func() {
			z := &v1beta1.ZookeeperCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "example",
					Namespace: "default",
				},
			}
			z.WithDefaults()
			cm1 := zk.MakeConfigMap(z)
			cm2 := cm1.DeepCopy()
			cm2.Data["k1"] = "v1"
			zk.SyncConfigMap(cm1, cm2)
			value = cm1.Data["k1"]
		})
		It("should have value as v1 for cm1.Data's key k1", func() {
			Ω(value).To(BeEquivalentTo("v1"))
		})
	})
})
