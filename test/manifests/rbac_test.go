// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

//go:build manifests

package manifests

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"slices"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestRBACGrantsDedicatedAccountRequiredPermissions(t *testing.T) {
	for name, resources := range rbacManifests(t) {
		t.Run(name, func(t *testing.T) {
			type roleKey struct{ kind, namespace, name string }
			roles := map[roleKey][]rbacv1.PolicyRule{}
			for _, resource := range resources {
				if resource.GetKind() != "Role" && resource.GetKind() != "ClusterRole" {
					continue
				}
				var role rbacv1.ClusterRole
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, &role); err != nil {
					t.Fatal(err)
				}
				namespace := ""
				if resource.GetKind() == "Role" {
					namespace = resource.GetNamespace()
					if namespace == "" {
						namespace = "default"
					}
				}
				roles[roleKey{resource.GetKind(), namespace, role.Name}] = role.Rules
			}
			bindings := 0
			var granted []rbacv1.PolicyRule
			for _, resource := range resources {
				if resource.GetKind() != "RoleBinding" && resource.GetKind() != "ClusterRoleBinding" {
					continue
				}
				bindings++
				var binding rbacv1.RoleBinding
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, &binding); err != nil {
					t.Fatal(err)
				}
				namespace := binding.Namespace
				if namespace == "" {
					namespace = "default"
				}
				key := roleKey{kind: binding.RoleRef.Kind, name: binding.RoleRef.Name}
				if key.kind == "Role" {
					key.namespace = namespace
				}
				rules, found := roles[key]
				if binding.RoleRef.APIGroup != rbacv1.GroupName || !found ||
					(resource.GetKind() == "ClusterRoleBinding" && key.kind != "ClusterRole") {
					t.Errorf("%s %q references an invalid or missing role: %+v", resource.GetKind(), binding.Name, binding.RoleRef)
					continue
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
					} else if resource.GetKind() == "ClusterRoleBinding" || binding.Namespace == "" || binding.Namespace == "default" {
						granted = append(granted, rules...)
					}
				}
			}
			if bindings == 0 {
				t.Fatal("no RBAC bindings rendered")
			}
			for _, want := range []struct{ resource, verb string }{
				{"zookeeperclusters", "list"},
				{"zookeeperclusters", "watch"},
				{"zookeeperclusters/status", "update"},
			} {
				allowed := false
				for _, rule := range granted {
					if len(rule.ResourceNames) == 0 &&
						(slices.Contains(rule.APIGroups, "zookeeper.pravega.io") || slices.Contains(rule.APIGroups, "*")) &&
						(slices.Contains(rule.Resources, want.resource) || slices.Contains(rule.Resources, "*")) &&
						(slices.Contains(rule.Verbs, want.verb) || slices.Contains(rule.Verbs, "*")) {
						allowed = true
						break
					}
				}
				if !allowed {
					t.Errorf("default/zookeeper-operator cannot %s %s.zookeeper.pravega.io", want.verb, want.resource)
				}
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
