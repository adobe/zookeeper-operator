// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

//go:build manifests

package manifests

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestRBACBindsDedicatedAccount(t *testing.T) {
	for name, resources := range rbacManifests(t) {
		t.Run(name, func(t *testing.T) {
			bindings := 0
			for _, resource := range resources {
				if resource.GetKind() != "RoleBinding" && resource.GetKind() != "ClusterRoleBinding" {
					continue
				}
				bindings++
				var binding rbacv1.RoleBinding
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, &binding); err != nil {
					t.Fatal(err)
				}
				if len(binding.Subjects) == 0 {
					t.Errorf("%s %q has no subjects", resource.GetKind(), resource.GetName())
				}
				for _, subject := range binding.Subjects {
					namespace := subject.Namespace
					if namespace == "" && resource.GetKind() == "RoleBinding" {
						namespace = resource.GetNamespace()
					}
					if subject.Kind != "ServiceAccount" || subject.Name != "zookeeper-operator" || namespace != "default" {
						t.Errorf("unexpected operator role subject: %+v", subject)
					}
				}
			}
			if bindings == 0 {
				t.Fatal("no RBAC bindings rendered")
			}
		})
	}
}

func TestRBACUsesActualCRDGroup(t *testing.T) {
	for name, resources := range rbacManifests(t) {
		t.Run(name, func(t *testing.T) {
			found := false
			for _, resource := range resources {
				if resource.GetKind() != "Role" && resource.GetKind() != "ClusterRole" {
					continue
				}
				var role rbacv1.ClusterRole
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, &role); err != nil {
					t.Fatal(err)
				}
				for _, rule := range role.Rules {
					for _, group := range rule.APIGroups {
						if group == "zookeeper.pravega.io" {
							found = true
						}
						if group == "zookeeper.pravega.io.zookeeper.pravega.io" {
							t.Error("RBAC grants permissions to a nonexistent duplicated API group")
						}
					}
				}
			}
			if !found {
				t.Fatal("no permissions for zookeeper.pravega.io")
			}
		})
	}
}

func rbacManifests(t *testing.T) map[string][]unstructured.Unstructured {
	t.Helper()
	result := map[string][]unstructured.Unstructured{
		"kustomize": renderManifests(t, "../../bin/kustomize", "build", "../../config/default"),
		"helm":      renderManifests(t, "helm", "template", "zookeeper-operator", "../../charts/zookeeper-operator", "--namespace", "default"),
	}
	for _, scope := range []string{"default", "all"} {
		data, err := os.ReadFile("../../config/rbac/" + scope + "_ns_rbac.yaml")
		if err != nil {
			t.Fatal(err)
		}
		result[scope] = decodeManifests(t, data)
	}
	return result
}

func renderManifests(t *testing.T, tool string, args ...string) []unstructured.Unstructured {
	t.Helper()
	command := exec.Command(tool, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", tool, args, err, &stderr)
	}
	return decodeManifests(t, output)
}

func decodeManifests(t *testing.T, data []byte) []unstructured.Unstructured {
	t.Helper()
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var resources []unstructured.Unstructured
	for {
		var resource unstructured.Unstructured
		err := decoder.Decode(&resource)
		if err == io.EOF {
			return resources
		}
		if err != nil {
			t.Fatal(err)
		}
		if resource.Object != nil {
			resources = append(resources, resource)
		}
	}
}
