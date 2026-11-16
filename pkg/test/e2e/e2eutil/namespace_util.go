// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package e2eutil

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const namespacePrefix = "zk-e2e-"

func CreateNamespace(ctx context.Context, c client.Client) (string, error) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: namespacePrefix}}
	if err := c.Create(ctx, ns); err != nil {
		return "", err
	}
	return ns.Name, nil
}

func DeleteNamespace(ctx context.Context, c client.Client, name string) error {
	if !strings.HasPrefix(name, namespacePrefix) {
		return fmt.Errorf("refusing to delete non-e2e namespace %q", name)
	}
	ctx, cancel := context.WithTimeout(ctx, TerminateTimeout)
	defer cancel()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := client.IgnoreNotFound(c.Delete(ctx, ns)); err != nil {
		return err
	}
	return wait.PollUntilContextCancel(ctx, RetryInterval, true, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, client.ObjectKey{Name: name}, &corev1.Namespace{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}

func GetNamespacePods(ctx context.Context, c client.Client, name string) ([]corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pods := &corev1.PodList{}
	err := c.List(ctx, pods, client.InNamespace(name))
	return pods.Items, err
}
