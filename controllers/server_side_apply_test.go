// Copyright 2026 Adobe. All rights reserved.
// Licensed under the Apache License, Version 2.0.

package controllers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pravega/zookeeper-operator/pkg/zk"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestApplyPreservesIntegerValues(t *testing.T) {
	r, z, sts := newScaleMetadataTestReconciler(t, 1)
	ctx := t.Context()
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
		t.Fatal(err)
	}
	const gracePeriod = 9007199254740993
	z.Spec.Pod.TerminationGracePeriodSeconds = gracePeriod
	applies := 0
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, obj client.Object, patch client.Patch, _ ...client.PatchOption) error {
			if patch.Type() != types.ApplyPatchType {
				t.Fatalf("expected apply, got %v", patch.Type())
			}
			applies++
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			intent := &appsv1.StatefulSet{}
			if err := json.Unmarshal(data, intent); err != nil {
				return err
			}
			if got := intent.Spec.Template.Spec.TerminationGracePeriodSeconds; got == nil || *got != gracePeriod {
				t.Errorf("apply did not preserve the rendered grace period: %s", data)
			}
			return nil
		},
	})
	if err := r.applyStatefulSet(ctx, sts, zk.MakeStatefulSet(z)); err != nil {
		t.Fatal(err)
	}
	if applies != 1 {
		t.Fatalf("expected one apply payload, got %d", applies)
	}
}
