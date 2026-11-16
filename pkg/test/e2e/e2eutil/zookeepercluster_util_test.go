// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package e2eutil

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	api "github.com/pravega/zookeeper-operator/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestWaitsCheckCompletedOperationsImmediately(t *testing.T) {
	setWaitDurations(t, time.Hour)
	cluster := &api.ZookeeperCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "zk", Namespace: "default"},
		Status: api.ZookeeperClusterStatus{
			ReadyReplicas:  2,
			CurrentVersion: "new",
			Conditions: []api.ClusterCondition{
				{Type: api.ClusterConditionPodsReady, Status: corev1.ConditionTrue},
				{Type: api.ClusterConditionUpgrading, Status: corev1.ConditionFalse},
				{Type: api.ClusterConditionError, Status: corev1.ConditionFalse},
			},
		},
	}
	c := waitTestClient(t, cluster)
	for _, tt := range []struct {
		name string
		wait func() error
	}{
		{"ready", func() error { return WaitForClusterToBecomeReady(t.Context(), logr.Discard(), c, cluster, 2) }},
		{"upgraded", func() error { return WaitForClusterToUpgrade(t.Context(), logr.Discard(), c, cluster, "new") }},
		{"terminated", func() error {
			return WaitForClusterToTerminate(t.Context(), logr.Discard(), waitTestClient(t), cluster)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.wait(); err != nil {
				t.Fatalf("completed operation was not checked immediately: %v", err)
			}
		})
	}
}

func TestTerminationWaitsForClusterFinalization(t *testing.T) {
	setWaitDurations(t, time.Millisecond)
	cluster := &api.ZookeeperCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "zk", Namespace: "default"},
	}
	if err := WaitForClusterToTerminate(t.Context(), logr.Discard(), waitTestClient(t, cluster), cluster); err == nil {
		t.Fatal("termination succeeded while the CR still exists; immediate recreation would conflict")
	}
}

func TestWaitTimeoutCancelsInFlightAPIRequest(t *testing.T) {
	setWaitDurations(t, time.Millisecond)
	cluster := &api.ZookeeperCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "zk", Namespace: "default"},
		Spec:       api.ZookeeperClusterSpec{Replicas: 2},
	}
	c := &blockingAPIClient{Client: waitTestClient(t)}
	for _, tt := range []struct {
		name string
		wait func(context.Context) error
	}{
		{"ready", func(ctx context.Context) error {
			return WaitForClusterToBecomeReady(ctx, logr.Discard(), c, cluster, 2)
		}},
		{"upgrade", func(ctx context.Context) error {
			return WaitForClusterToUpgrade(ctx, logr.Discard(), c, cluster, "new")
		}},
		{"termination", func(ctx context.Context) error {
			return WaitForClusterToTerminate(ctx, logr.Discard(), c, cluster)
		}},
		{"replacement", func(ctx context.Context) error {
			return WaitForPodsReplaced(ctx, logr.Discard(), c, cluster, nil)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.wait(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("poll timeout did not cancel its API request: %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := tt.wait(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("caller cancellation was lost: %v", err)
			}
		})
	}
}

type blockingAPIClient struct{ client.Client }

func (*blockingAPIClient) Get(ctx context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return blockUntilCanceled(ctx)
}

func (*blockingAPIClient) List(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return blockUntilCanceled(ctx)
}

func blockUntilCanceled(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(200 * time.Millisecond):
		return errors.New("API request outlived the poll deadline")
	}
}

func TestDeletePodsWaitsForReadyReplacements(t *testing.T) {
	setWaitDurations(t, time.Millisecond)
	cluster := &api.ZookeeperCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "zk", Namespace: "default"},
		Spec:       api.ZookeeperClusterSpec{Replicas: 2},
	}
	old := readyTestPod("zk-0", "old")
	other := readyTestPod("zk-1", "other")
	replacement := readyTestPod("zk-0", "new")
	unready := *replacement.DeepCopy()
	unready.Status.Conditions[0].Status = corev1.ConditionFalse
	terminating := *replacement.DeepCopy()
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	listErr := errors.New("list failed")
	for _, tt := range []struct {
		name    string
		current []corev1.Pod
		err     error
		wantErr bool
	}{
		{"old UID still present", []corev1.Pod{old, other}, nil, true},
		{"replacement missing", []corev1.Pod{other}, nil, true},
		{"replacement unready", []corev1.Pod{unready, other}, nil, true},
		{"replacement terminating", []corev1.Pod{terminating, other}, nil, true},
		{"ready replacement", []corev1.Pod{replacement, other}, nil, false},
		{"API error", nil, listErr, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := &replacingPodClient{
				Client:  waitTestClient(t, old.DeepCopy(), other.DeepCopy()),
				current: tt.current,
				listErr: tt.err,
			}
			err := DeletePods(t.Context(), logr.Discard(), c, cluster, 1)
			if (err != nil) != tt.wantErr {
				t.Fatalf("DeletePods returned %v, want error: %v", err, tt.wantErr)
			}
			if tt.err != nil && !errors.Is(err, tt.err) {
				t.Fatalf("API error was lost: %v", err)
			}
		})
	}
}

func setWaitDurations(t *testing.T, interval time.Duration) {
	t.Helper()
	oldInterval, oldReady, oldUpgrade, oldTerminate := RetryInterval, ReadyTimeout, UpgradeTimeout, TerminateTimeout
	t.Cleanup(func() {
		RetryInterval, ReadyTimeout, UpgradeTimeout, TerminateTimeout = oldInterval, oldReady, oldUpgrade, oldTerminate
	})
	RetryInterval = interval
	ReadyTimeout, UpgradeTimeout, TerminateTimeout = 30*time.Millisecond, 30*time.Millisecond, 30*time.Millisecond
}

func waitTestClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := api.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).Build()
}

func readyTestPod(name, uid string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID(uid),
			Labels: map[string]string{"app": "zk"},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
}

type replacingPodClient struct {
	client.Client
	deleted bool
	current []corev1.Pod
	listErr error
}

func (c *replacingPodClient) Delete(context.Context, client.Object, ...client.DeleteOption) error {
	c.deleted = true
	return nil
}

func (c *replacingPodClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.deleted {
		if c.listErr != nil {
			return c.listErr
		}
		list.(*corev1.PodList).Items = c.current
		return nil
	}
	return c.Client.List(ctx, list, opts...)
}
