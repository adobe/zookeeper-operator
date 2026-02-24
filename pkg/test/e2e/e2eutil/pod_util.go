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
	"bytes"
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// GetPodLogs returns recent logs from a pod's container.
// If opts is nil, default options (no tail limit) are used.
func GetPodLogs(ctx context.Context, config *restclient.Config, namespace, podName, containerName string, opts *corev1.PodLogOptions) (string, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", fmt.Errorf("create clientset: %w", err)
	}
	if opts == nil {
		opts = &corev1.PodLogOptions{}
	}
	if containerName != "" {
		opts.Container = containerName
	}
	req := clientset.CoreV1().Pods(namespace).GetLogs(podName, opts)
	logStream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("stream logs: %w", err)
	}
	defer logStream.Close()
	var buf bytes.Buffer
	_, err = io.Copy(&buf, logStream)
	if err != nil {
		return "", fmt.Errorf("read logs: %w", err)
	}
	return buf.String(), nil
}

// PodExec runs a command in a pod container and returns stdout and stderr.
func PodExec(ctx context.Context, config *restclient.Config, namespace, podName, containerName string, command []string) (stdout, stderr string, err error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", "", fmt.Errorf("create clientset: %w", err)
	}
	opts := &corev1.PodExecOptions{
		Container: containerName,
		Command:   command,
		Stdout:    true,
		Stderr:    true,
	}
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(podName).
		SubResource("exec").
		VersionedParams(opts, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("create executor: %w", err)
	}
	var outBuf, errBuf bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &outBuf,
		Stderr: &errBuf,
	})
	if err != nil {
		return outBuf.String(), errBuf.String(), fmt.Errorf("stream exec: %w", err)
	}
	return outBuf.String(), errBuf.String(), nil
}
