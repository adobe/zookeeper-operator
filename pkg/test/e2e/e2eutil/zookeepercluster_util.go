/**
 * Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package e2eutil

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	api "github.com/pravega/zookeeper-operator/api/v1beta1"
)

var (
	RetryInterval    = time.Second * 2
	ReadyTimeout     = time.Minute * 15
	UpgradeTimeout   = time.Minute * 25
	TerminateTimeout = time.Minute * 15
)

// CreateCluster creates a ZookeeperCluster CR with the desired spec
func CreateCluster(ctx context.Context, logger logr.Logger, k8client client.Client, z *api.ZookeeperCluster) (*api.ZookeeperCluster, error) {
	logger.Info(fmt.Sprintf("creating zookeeper cluster: %s\n", z.Name))
	err := k8client.Create(ctx, z)
	if err != nil {
		return nil, fmt.Errorf("failed to create CR: %w", err)
	}

	zk := &api.ZookeeperCluster{}
	err = k8client.Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: z.Name}, zk)
	if err != nil {
		return nil, fmt.Errorf("failed to obtain created CR: %w", err)
	}
	logger.Info(fmt.Sprintf("created zookeeper cluster: %s\n", zk.Name))
	return z, nil
}

// DeleteCluster deletes the ZookeeperCluster CR specified by cluster spec
func DeleteCluster(ctx context.Context, logger logr.Logger, k8client client.Client, z *api.ZookeeperCluster) error {
	logger.Info(fmt.Sprintf("deleting zookeeper cluster: %s", z.Name))
	err := k8client.Delete(ctx, z)
	if err != nil {
		return fmt.Errorf("failed to delete CR: %w", err)
	}
	logger.Info(fmt.Sprintf("deleted zookeeper cluster: %s", z.Name))
	return nil
}

// UpdateCluster updates the ZookeeperCluster CR
func UpdateCluster(ctx context.Context, logger logr.Logger, k8client client.Client, z *api.ZookeeperCluster) error {
	logger.Info(fmt.Sprintf("updating zookeeper cluster: %s", z.Name))
	err := k8client.Update(ctx, z)
	if err != nil {
		return fmt.Errorf("failed to update CR: %w", err)
	}
	logger.Info(fmt.Sprintf("updated zookeeper cluster: %s", z.Name))
	return nil
}

// GetCluster returns the latest ZookeeperCluster CR
func GetCluster(ctx context.Context, logger logr.Logger, k8client client.Client, z *api.ZookeeperCluster) (*api.ZookeeperCluster, error) {
	zk := &api.ZookeeperCluster{}
	err := k8client.Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: z.Name}, zk)
	if err != nil {
		return nil, fmt.Errorf("failed to obtain created CR: %w", err)
	}
	logger.Info(fmt.Sprintf("zk cluster has ready replicas %v", zk.Status.ReadyReplicas))
	return zk, nil
}

// WaitForClusterToBecomeReady will wait until all cluster pods are ready
func WaitForClusterToBecomeReady(ctx context.Context, logger logr.Logger, k8client client.Client, z *api.ZookeeperCluster, size int) error {
	logger.Info(fmt.Sprintf("waiting for cluster pods to become ready: %s", z.Name))
	err := wait.PollUntilContextTimeout(ctx, RetryInterval, ReadyTimeout, true, func(ctx context.Context) (done bool, err error) {
		cluster, err := GetCluster(ctx, logger, k8client, z)
		if err != nil {
			return false, err
		}

		logger.Info(fmt.Sprintf("waiting for pods to become ready (%d/%d), pods (%v)", cluster.Status.ReadyReplicas, size, cluster.Status.Members.Ready))
		_, condition := cluster.Status.GetClusterCondition(api.ClusterConditionPodsReady)
		if condition != nil && condition.Status == corev1.ConditionTrue && cluster.Status.ReadyReplicas == int32(size) {
			return true, nil
		}
		return false, nil
	})

	if err != nil {
		return err
	}
	logger.Info(fmt.Sprintf("zookeeper cluster ready: %s", z.Name))
	return nil

}

// WaitForClusterToUpgrade will wait until all pods are upgraded
func WaitForClusterToUpgrade(ctx context.Context, logger logr.Logger, k8client client.Client, z *api.ZookeeperCluster, targetVersion string) error {
	logger.Info(fmt.Sprintf("waiting for cluster to upgrade: %s", z.Name))
	err := wait.PollUntilContextTimeout(ctx, RetryInterval, UpgradeTimeout, true, func(ctx context.Context) (done bool, err error) {
		cluster, err := GetCluster(ctx, logger, k8client, z)
		if err != nil {
			return false, err
		}

		_, upgradeCondition := cluster.Status.GetClusterCondition(api.ClusterConditionUpgrading)
		_, errorCondition := cluster.Status.GetClusterCondition(api.ClusterConditionError)

		if errorCondition != nil && errorCondition.Status == corev1.ConditionTrue {
			return false, fmt.Errorf("failed upgrading cluster: [%s] %s", errorCondition.Reason, errorCondition.Message)
		}

		if upgradeCondition != nil && upgradeCondition.Status == corev1.ConditionFalse && cluster.Status.CurrentVersion == targetVersion {
			// Cluster upgraded
			return true, nil
		}
		return false, nil
	})

	if err != nil {
		return err
	}

	logger.Info(fmt.Sprintf("zookeeper cluster upgraded: %s", z.Name))
	return nil
}

// WaitForClusterToTerminate will wait until all cluster pods are terminated
func WaitForClusterToTerminate(ctx context.Context, logger logr.Logger, k8client client.Client, z *api.ZookeeperCluster) error {
	logger.Info(fmt.Sprintf("waiting for zookeeper cluster to terminate: %s", z.Name))
	listOptions := []client.ListOption{
		client.InNamespace(z.GetNamespace()),
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(map[string]string{"app": z.GetName()})},
	}

	// Wait for Pods to terminate
	err := wait.PollUntilContextTimeout(ctx, RetryInterval, TerminateTimeout, true, func(ctx context.Context) (done bool, err error) {
		podList := corev1.PodList{}
		err = k8client.List(ctx, &podList, listOptions...)
		if err != nil {
			return false, err
		}

		var names []string
		for i := range podList.Items {
			pod := &podList.Items[i]
			names = append(names, pod.Name)
		}
		logger.Info(fmt.Sprintf("waiting for pods to terminate, running pods (%v)", names))
		if len(names) != 0 {
			return false, nil
		}
		return true, nil
	})

	if err != nil {
		return err
	}

	// Wait for PVCs to terminate
	err = wait.PollUntilContextTimeout(ctx, RetryInterval, TerminateTimeout, true, func(ctx context.Context) (done bool, err error) {
		pvcList := corev1.PersistentVolumeClaimList{}
		err = k8client.List(ctx, &pvcList, listOptions...)
		if err != nil {
			return false, err
		}

		var names []string
		for i := range pvcList.Items {
			pvc := &pvcList.Items[i]
			names = append(names, pvc.Name)
		}
		logger.Info(fmt.Sprintf("waiting for pvc to terminate (%v)", names))
		if len(names) != 0 {
			return false, nil
		}
		err = k8client.Get(ctx, types.NamespacedName{Namespace: z.Namespace, Name: z.Name}, &api.ZookeeperCluster{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
	if err == nil {
		logger.Info(fmt.Sprintf("zookeeper cluster terminated: %s", z.Name))
	}
	return err
}
func DeletePods(ctx context.Context, logger logr.Logger, k8client client.Client, z *api.ZookeeperCluster, size int) error {
	listOptions := []client.ListOption{
		client.InNamespace(z.GetNamespace()),
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(map[string]string{"app": z.GetName()})},
	}
	podList := corev1.PodList{}
	err := k8client.List(ctx, &podList, listOptions...)
	if err != nil {
		return err
	}
	if size < 0 || size > len(podList.Items) {
		return fmt.Errorf("cannot delete %d pods from %d", size, len(podList.Items))
	}
	pod := &corev1.Pod{}

	for i := 0; i < size; i++ {
		pod = &podList.Items[i]
		logger.Info(fmt.Sprintf("podname: %v", pod.Name))
		err = k8client.Delete(ctx, pod)
		if err != nil {
			return fmt.Errorf("failed to delete pod: %w", err)
		}

		logger.Info(fmt.Sprintf("deleted zookeeper pod: %s", pod.Name))

	}
	return WaitForPodsReplaced(ctx, logger, k8client, z, podList.Items[:size])
}

// WaitForPodsReplaced excludes stale readiness from before a deletion or restart.
func WaitForPodsReplaced(ctx context.Context, logger logr.Logger, k8client client.Client, z *api.ZookeeperCluster, previous []corev1.Pod) error {
	oldUIDs := make(map[types.UID]bool, len(previous))
	for _, pod := range previous {
		oldUIDs[pod.UID] = true
	}
	return wait.PollUntilContextTimeout(ctx, RetryInterval, ReadyTimeout, true, func(ctx context.Context) (bool, error) {
		pods, err := GetPods(ctx, k8client, z)
		if err != nil {
			return false, err
		}
		if len(pods.Items) != int(z.Spec.Replicas) {
			return false, nil
		}
		for _, pod := range pods.Items {
			if oldUIDs[pod.UID] || pod.DeletionTimestamp != nil {
				return false, nil
			}
			ready := false
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
					ready = true
				}
			}
			if !ready {
				return false, nil
			}
		}
		logger.Info("replacement pods are ready", "cluster", z.Name)
		return true, nil
	})
}
func GetPods(ctx context.Context, k8client client.Client, z *api.ZookeeperCluster) (*corev1.PodList, error) {
	listOptions := []client.ListOption{
		client.InNamespace(z.GetNamespace()),
		client.MatchingLabels(map[string]string{"app": z.GetName()}),
	}
	podList := corev1.PodList{}
	err := k8client.List(ctx, &podList, listOptions...)
	return &podList, err
}
func CheckAdminService(ctx context.Context, logger logr.Logger, k8client client.Client, z *api.ZookeeperCluster) error {
	serviceList := corev1.ServiceList{}
	listOptions := []client.ListOption{client.InNamespace(z.GetNamespace()),
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(map[string]string{"app": z.GetName()})}}
	err := k8client.List(ctx, &serviceList, listOptions...)
	if err != nil {
		return err
	}

	for _, sn := range serviceList.Items {
		if sn.Name == "zookeeper-admin-server" {
			logger.Info(fmt.Sprintf("Admin service is enabled servicenameis %v", sn.Name))
			return nil
		}
	}
	return fmt.Errorf("Admin Service is not enabled")
}
