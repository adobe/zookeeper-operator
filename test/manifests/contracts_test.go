//go:build manifests

// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package manifests

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func render(t *testing.T, command string, args ...string) []unstructured.Unstructured {
	t.Helper()
	cmd := exec.Command(command, args...)
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", command, args, err, out)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	var objects []unstructured.Unstructured
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode rendered manifests: %v", err)
		}
		if object.Object != nil {
			objects = append(objects, object)
		}
	}
	if len(objects) == 0 {
		t.Fatal("render produced no Kubernetes resources")
	}
	return objects
}

func TestHelmKubernetesSupportFloor(t *testing.T) {
	cmd := exec.Command("helm", "template", "compatibility", "charts/zookeeper-operator", "--kube-version", "1.33.0")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "requires kubeVersion") {
		t.Fatalf("expected chart Kubernetes support rejection, got %v\n%s", err, out)
	}
	for _, version := range []string{"1.34.3", "1.35.0"} {
		t.Run(version, func(t *testing.T) {
			objects := render(t, "helm", "template", "compatibility", "charts/zookeeper-operator", "--kube-version", version)
			for _, object := range objects {
				if object.GetKind() == "Deployment" && object.GetAPIVersion() == "apps/v1" {
					return
				}
			}
			t.Fatal("chart did not render an apps/v1 Deployment")
		})
	}
}

func TestE2ELocallyLoadedOperator(t *testing.T) {
	objects := render(t, "bin/kustomize", "build", "config/e2e")
	for _, object := range objects {
		if object.GetKind() != "Deployment" || object.GetName() != "zookeeper-operator" {
			continue
		}
		containers, found, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
		if err != nil || !found {
			t.Fatalf("operator containers missing: %v", err)
		}
		for _, raw := range containers {
			container := raw.(map[string]interface{})
			if container["name"] == "zookeeper-operator" {
				if container["image"] != "zookeeper-operator:e2e" || container["imagePullPolicy"] != "IfNotPresent" {
					t.Fatalf("operator must use its locally loaded image: %v", container)
				}
				return
			}
		}
	}
	t.Fatal("operator container not found")
}
