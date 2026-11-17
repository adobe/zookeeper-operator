// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

//go:build manifests

package manifests

import (
	"os"
	"testing"
)

func TestChartManagedCRDIsRetained(t *testing.T) {
	found := false
	for _, resource := range renderManifests(t, "helm", "template", "zookeeper-operator", "../../charts/zookeeper-operator") {
		if resource.GetKind() == "CustomResourceDefinition" {
			found = true
			if resource.GetAnnotations()["helm.sh/resource-policy"] != "keep" {
				t.Fatal("chart-managed CRD must survive Helm uninstall")
			}
		}
	}
	if !found {
		t.Fatal("chart-managed mode did not render the CRD")
	}
}

func TestExternalCRDModeOmitsCRD(t *testing.T) {
	for _, resource := range renderManifests(t, "helm", "template", "zookeeper-operator", "../../charts/zookeeper-operator", "--set", "crd.create=false") {
		if resource.GetKind() == "CustomResourceDefinition" {
			t.Fatal("external ownership mode rendered a CRD")
		}
	}
	data, err := os.ReadFile("../../config/crd/bases/zookeeper.pravega.io_zookeeperclusters.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range decodeManifests(t, data) {
		if _, exists := resource.GetAnnotations()["helm.sh/resource-policy"]; exists {
			t.Fatal("external CRD manifest must remain Helm-neutral")
		}
	}
}
