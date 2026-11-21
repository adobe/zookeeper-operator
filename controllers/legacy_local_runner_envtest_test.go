//go:build envtest

// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package controllers

import (
	"maps"
	"slices"
	"testing"

	"github.com/pravega/zookeeper-operator/pkg/zk"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/csaupgrade"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
)

func TestEnvtestPreservesUnrelatedMainManager(t *testing.T) {
	ctx, cl, z := newEnvtestCluster(t)
	sts := zk.MakeStatefulSet(z)
	sts.Labels = maps.Clone(sts.Labels)
	sts.Labels["owner-rv"] = z.ResourceVersion
	if err := controllerutil.SetControllerReference(z, sts, cl.Scheme()); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, sts, client.FieldOwner("zookeeper-operator")); err != nil {
		t.Fatal(err)
	}
	const key = "example.com/unrelated"
	sts.Annotations = map[string]string{key: "keep"}
	if err := cl.Update(ctx, sts, client.FieldOwner("main")); err != nil {
		t.Fatal(err)
	}
	if err := convergenceReconciler(cl).reconcileStatefulSet(ctx, z); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	owners := csaupgrade.FindFieldsOwners(sts.ManagedFields, metav1.ManagedFieldsOperationUpdate, fieldpath.NewSet(fieldpath.MakePathOrDie("metadata", "annotations", key)))
	if sts.Annotations[key] != "keep" || !slices.ContainsFunc(owners, func(owner metav1.ManagedFieldsEntry) bool { return owner.Manager == "main" }) {
		t.Fatal("an unrelated main manager must retain its annotation and ownership")
	}
}
