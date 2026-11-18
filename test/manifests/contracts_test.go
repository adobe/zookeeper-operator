//go:build manifests

// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package manifests

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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
	objects := render(t, "helm", "template", "compatibility", "charts/zookeeper-operator", "--kube-version", "1.34.3")
	for _, object := range objects {
		if object.GetKind() == "Deployment" && object.GetAPIVersion() == "apps/v1" {
			return
		}
	}
	t.Fatal("chart did not render an apps/v1 Deployment")
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
				env, found, err := unstructured.NestedSlice(container, "env")
				if err != nil || !found {
					t.Fatalf("operator environment missing: %v", err)
				}
				for _, raw := range env {
					variable := raw.(map[string]interface{})
					if variable["name"] == "WATCH_NAMESPACE" {
						if variable["value"] != "" || variable["valueFrom"] != nil {
							t.Fatalf("e2e operator must watch generated scenario namespaces: %v", variable)
						}
						return
					}
				}
				t.Fatal("e2e WATCH_NAMESPACE missing")
			}
		}
	}
	t.Fatal("operator container not found")
}

func TestE2EOperatorCanWatchScenarioNamespaces(t *testing.T) {
	objects := render(t, "bin/kustomize", "build", "config/e2e")
	var namespace, account string
	roles := map[string][]rbacv1.PolicyRule{}
	var bindings []rbacv1.ClusterRoleBinding
	for _, object := range objects {
		switch object.GetKind() {
		case "Deployment":
			if object.GetName() == "zookeeper-operator" {
				namespace = object.GetNamespace()
				account, _, _ = unstructured.NestedString(object.Object, "spec", "template", "spec", "serviceAccountName")
			}
		case "ClusterRole":
			var role rbacv1.ClusterRole
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &role); err != nil {
				t.Fatal(err)
			}
			roles[role.Name] = role.Rules
		case "ClusterRoleBinding":
			var binding rbacv1.ClusterRoleBinding
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &binding); err != nil {
				t.Fatal(err)
			}
			bindings = append(bindings, binding)
		}
	}
	if namespace == "" || account == "" {
		t.Fatal("operator deployment namespace or ServiceAccount missing")
	}
	var granted []rbacv1.PolicyRule
	for _, binding := range bindings {
		if binding.RoleRef.Kind != "ClusterRole" {
			continue
		}
		for _, subject := range binding.Subjects {
			if subject.Kind == "ServiceAccount" && subject.Namespace == namespace && subject.Name == account {
				granted = append(granted, roles[binding.RoleRef.Name]...)
			}
		}
	}
	for _, want := range []struct{ group, resource, verb string }{
		{"zookeeper.pravega.io", "zookeeperclusters", "list"},
		{"zookeeper.pravega.io", "zookeeperclusters", "watch"},
		{"zookeeper.pravega.io", "zookeeperclusters/status", "update"},
		{"zookeeper.pravega.io", "zookeeperclusters/finalizers", "update"},
		{"apps", "statefulsets", "list"},
		{"apps", "statefulsets", "watch"},
		{"", "pods", "list"},
		{"", "pods", "watch"},
		{"", "services", "watch"},
	} {
		allowed := false
		for _, rule := range granted {
			if len(rule.ResourceNames) == 0 &&
				(slices.Contains(rule.APIGroups, want.group) || slices.Contains(rule.APIGroups, "*")) &&
				(slices.Contains(rule.Resources, want.resource) || slices.Contains(rule.Resources, "*")) &&
				(slices.Contains(rule.Verbs, want.verb) || slices.Contains(rule.Verbs, "*")) {
				allowed = true
				break
			}
		}
		if !allowed {
			t.Errorf("%s/%s cannot %s %s.%s across scenario namespaces", namespace, account, want.verb, want.resource, want.group)
		}
	}
}

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

func TestOperatorHealthProbes(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
		args    []string
	}{
		{"helm", "helm", []string{"template", "zookeeper-operator", "charts/zookeeper-operator", "--kube-version", "1.34.3"}},
		{"kustomize", "bin/kustomize", []string{"build", "config/default"}},
		{"e2e", "bin/kustomize", []string{"build", "config/e2e"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var operator *corev1.Container
			for _, object := range render(t, test.command, test.args...) {
				switch object.GetKind() {
				case "Service":
					var service corev1.Service
					if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &service); err != nil {
						t.Fatal(err)
					}
					for _, port := range service.Spec.Ports {
						if port.Port == 8081 || port.TargetPort.IntVal == 8081 || port.TargetPort.StrVal == "health" {
							t.Error("operator probes must not have a public Service")
						}
					}
				case "Deployment":
					var deployment appsv1.Deployment
					if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &deployment); err != nil {
						t.Fatal(err)
					}
					for _, container := range deployment.Spec.Template.Spec.Containers {
						if container.Name == "zookeeper-operator" {
							operator = &container
						}
					}
				}
			}
			if operator == nil {
				t.Fatal("operator container not found")
			}
			for _, probe := range []struct {
				name   string
				probe  *corev1.Probe
				path   string
				period int32
			}{
				{"liveness", operator.LivenessProbe, "/healthz", 10},
				{"readiness", operator.ReadinessProbe, "/readyz", 5},
			} {
				if probe.probe == nil || probe.probe.HTTPGet == nil {
					t.Errorf("%s HTTP probe missing", probe.name)
					continue
				}
				get := probe.probe.HTTPGet
				if get.Path != probe.path || get.Port.StrVal != "health" ||
					probe.probe.PeriodSeconds != probe.period || probe.probe.FailureThreshold != 3 {
					t.Errorf("%s probe has wrong route, port or timing: %+v", probe.name, probe.probe)
				}
			}
			if !slices.ContainsFunc(operator.Ports, func(port corev1.ContainerPort) bool {
				return port.Name == "health" && port.ContainerPort == 8081
			}) {
				t.Error("named health container port 8081 missing")
			}
		})
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
