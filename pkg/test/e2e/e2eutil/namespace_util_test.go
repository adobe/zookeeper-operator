// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package e2eutil

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestNamespaceLifecycleIsIsolated(t *testing.T) {
	setWaitDurations(t, time.Hour)
	c := waitTestClient(t)
	first, err := CreateNamespace(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CreateNamespace(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !strings.HasPrefix(first, "zk-e2e-") || !strings.HasPrefix(second, "zk-e2e-") {
		t.Fatalf("scenario namespaces must be unique e2e namespaces: %q, %q", first, second)
	}
	if err := DeleteNamespace(t.Context(), c, first); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: first}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted scenario namespace still exists: %v", err)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: second}, &corev1.Namespace{}); err != nil {
		t.Fatalf("cleanup affected the other scenario: %v", err)
	}
	if err := DeleteNamespace(t.Context(), c, first); err != nil {
		t.Fatalf("cleanup of an already deleted namespace failed: %v", err)
	}
}

func TestNamespaceCleanupRejectsUnownedNamespaces(t *testing.T) {
	for _, name := range []string{"", "default", "kube-system", "other"} {
		t.Run(name, func(t *testing.T) {
			c := waitTestClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
			if err := DeleteNamespace(t.Context(), c, name); err == nil {
				t.Fatal("cleanup accepted a namespace not created for an e2e scenario")
			}
			if err := c.Get(t.Context(), client.ObjectKey{Name: "default"}, &corev1.Namespace{}); err != nil {
				t.Fatalf("cleanup deleted the deployment namespace: %v", err)
			}
		})
	}
}

func TestNamespaceCleanupWaitsForFinalization(t *testing.T) {
	setWaitDurations(t, time.Millisecond)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "zk-e2e-terminating", Finalizers: []string{"test.example/hold"},
	}}
	if err := DeleteNamespace(t.Context(), waitTestClient(t, ns), ns.Name); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup did not wait for finalization: %v", err)
	}
}

func TestNamespaceAPIErrorsArePreserved(t *testing.T) {
	apiErr := errors.New("API unavailable")
	for _, tt := range []struct {
		name   string
		faults interceptor.Funcs
		create bool
	}{
		{"create", interceptor.Funcs{Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			return apiErr
		}}, true},
		{"delete", interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
			return apiErr
		}}, false},
		{"get", interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return apiErr
		}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := interceptor.NewClient(waitTestClient(t), tt.faults)
			var err error
			if tt.create {
				_, err = CreateNamespace(t.Context(), c)
			} else {
				err = DeleteNamespace(t.Context(), c, "zk-e2e-unavailable")
			}
			if !errors.Is(err, apiErr) {
				t.Fatalf("API error was lost: %v", err)
			}
		})
	}
}

func TestNamespaceCleanupCancelsInFlightAPIRequests(t *testing.T) {
	setWaitDurations(t, time.Millisecond)
	for _, tt := range []struct {
		name   string
		faults interceptor.Funcs
	}{
		{"delete", interceptor.Funcs{Delete: func(ctx context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
			return blockUntilCanceled(ctx)
		}}},
		{"get", interceptor.Funcs{Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return blockUntilCanceled(ctx)
		}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := interceptor.NewClient(waitTestClient(t), tt.faults)
			if err := DeleteNamespace(t.Context(), c, "zk-e2e-blocked"); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cleanup timeout did not cancel its API request: %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := DeleteNamespace(ctx, c, "zk-e2e-blocked"); !errors.Is(err, context.Canceled) {
				t.Fatalf("caller cancellation was lost: %v", err)
			}
		})
	}
}

func TestNamespacePodDiagnosticsAreBoundedAndScoped(t *testing.T) {
	ours := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ours", Namespace: "zk-e2e-ours"}}
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "zk-e2e-other"}}
	c := interceptor.NewClient(waitTestClient(t, ours, other), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 10*time.Second {
				return errors.New("diagnostic API request has no short deadline")
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return c.List(ctx, list, opts...)
		},
	})
	pods, err := GetNamespacePods(t.Context(), c, "zk-e2e-ours")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0].Name != "ours" {
		t.Fatalf("diagnostics included another scenario: %v", pods)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := GetNamespacePods(ctx, c, "zk-e2e-ours"); !errors.Is(err, context.Canceled) {
		t.Fatalf("diagnostics lost caller cancellation: %v", err)
	}
}
