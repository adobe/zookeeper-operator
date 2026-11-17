// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

//go:build manifests

package manifests

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestDefaultBundleUsesReleaseImage(t *testing.T) {
	data, err := os.ReadFile("../../charts/zookeeper-operator/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096).Decode(&chart); err != nil {
		t.Fatal(err)
	}
	want := "ghcr.io/adobe/zookeeper-operator:" + chart.AppVersion
	assertOperatorImage(t, renderManifests(t, "../../bin/kustomize", "build", "../../config/default"), want, "Always")
	assertOperatorImage(t, renderManifests(t, "helm", "template", "zookeeper-operator", "../../charts/zookeeper-operator"), want, "IfNotPresent")
}

func TestMakeDeployUsesImageOverride(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"Makefile", "go.mod", "config/manager/manager.yaml", "config/manager/kustomization.yaml"} {
		data, err := os.ReadFile("../../" + path)
		if err != nil {
			t.Fatal(err)
		}
		writeFixture(t, filepath.Join(root, path), data, 0600)
	}
	writeFixture(t, filepath.Join(root, "config/default/kustomization.yaml"),
		[]byte("resources:\n- ../manager\n"), 0600)
	tools := filepath.Join(root, "tools")
	writeFixture(t, filepath.Join(tools, "controller-gen"),
		[]byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'Version: v0.99.0'; fi\n"), 0700)
	writeFixture(t, filepath.Join(tools, "kubectl"), []byte("#!/bin/sh\ncat > \"$MANIFEST_OUTPUT\"\n"), 0700)
	// The installer is piped into bash, so return a failing script without network access.
	writeFixture(t, filepath.Join(tools, "curl"),
		[]byte("#!/bin/sh\necho 'echo unexpected Kustomize download >&2; exit 1'\n"), 0700)
	kustomize, err := filepath.Abs("../../bin/kustomize")
	if err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(root, "rendered.yaml")
	command := exec.Command("make", "deploy", "IMG=example/operator:test", "VERSION=regression",
		"KUSTOMIZE="+kustomize, "CONTROLLER_GEN="+filepath.Join(tools, "controller-gen"),
		"CONTROLLER_TOOLS_VERSION=v0.99.0")
	command.Dir = root
	command.Env = append(os.Environ(), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
		"MANIFEST_OUTPUT="+outputPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("make deploy failed: %v\n%s", err, output)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	assertOperatorImage(t, decodeManifests(t, data), "example/operator:test", "Always")
}

func assertOperatorImage(t *testing.T, resources []unstructured.Unstructured, image, policy string) {
	t.Helper()
	for _, resource := range resources {
		if resource.GetKind() != "Deployment" {
			continue
		}
		var deployment appsv1.Deployment
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, &deployment); err != nil {
			t.Fatal(err)
		}
		for _, container := range deployment.Spec.Template.Spec.Containers {
			if container.Name == "zookeeper-operator" {
				if container.Image != image || string(container.ImagePullPolicy) != policy {
					t.Fatalf("operator image/policy = %s/%s, want %s/%s", container.Image, container.ImagePullPolicy, image, policy)
				}
				return
			}
		}
	}
	t.Fatal("operator container not found")
}

func writeFixture(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}
