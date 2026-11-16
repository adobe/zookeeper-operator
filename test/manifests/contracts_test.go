// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

//go:build manifests

package manifests

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestContainerSecurityContextIsRendered(t *testing.T) {
	deployment := renderOperator(t, `
securityContext:
  runAsUser: 12345
  runAsNonRoot: true
  allowPrivilegeEscalation: false
  capabilities:
    drop: ["ALL"]
additionalSidecars:
  - name: sidecar
    image: example/sidecar:test
`)
	spec := deployment.Spec.Template.Spec
	if len(spec.Containers) != 2 {
		t.Fatalf("got %d containers, want operator and sidecar", len(spec.Containers))
	}
	user, nonRoot, escalation := int64(12345), true, false
	want := &corev1.SecurityContext{
		RunAsUser:                &user,
		RunAsNonRoot:             &nonRoot,
		AllowPrivilegeEscalation: &escalation,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	if !reflect.DeepEqual(spec.Containers[0].SecurityContext, want) {
		t.Fatalf("operator securityContext = %+v, want %+v", spec.Containers[0].SecurityContext, want)
	}
	if spec.SecurityContext != nil || spec.Containers[1].SecurityContext != nil {
		t.Fatal("operator settings leaked into the Pod or sidecar securityContext")
	}
}

func TestEmptySecurityContextPreservesDefaults(t *testing.T) {
	deployment := renderOperator(t, "securityContext: {}\n")
	spec := deployment.Spec.Template.Spec
	if len(spec.Containers) != 1 {
		t.Fatalf("got %d containers, want only the operator", len(spec.Containers))
	}
	if spec.Containers[0].SecurityContext != nil || spec.SecurityContext != nil {
		t.Fatal("empty values unexpectedly added a container or Pod securityContext")
	}
}

func renderOperator(t *testing.T, values string) appsv1.Deployment {
	t.Helper()
	valuesPath := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(valuesPath, []byte(values), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("helm", "template", "zookeeper-operator",
		"../../charts/zookeeper-operator", "--show-only", "templates/operator.yaml",
		"--values", valuesPath)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, output)
	}
	var deployment appsv1.Deployment
	if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096).Decode(&deployment); err != nil {
		t.Fatalf("decode deployment: %v\n%s", err, output)
	}
	if deployment.Kind != "Deployment" {
		t.Fatalf("rendered kind %q, want Deployment", deployment.Kind)
	}
	return deployment
}
