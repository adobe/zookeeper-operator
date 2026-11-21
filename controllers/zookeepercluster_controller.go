/**
 * Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (&the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */
package controllers

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/pravega/zookeeper-operator/pkg/controller/config"
	"github.com/pravega/zookeeper-operator/pkg/utils"
	"github.com/pravega/zookeeper-operator/pkg/yamlexporter"
	"github.com/pravega/zookeeper-operator/pkg/zk"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	zookeeperv1beta1 "github.com/pravega/zookeeper-operator/api/v1beta1"
)

// ReconcileTime is the delay between reconciliations
const ReconcileTime = 30 * time.Second

var log = logf.Log.WithName("controller_zookeepercluster")

var _ reconcile.Reconciler = &ZookeeperClusterReconciler{}

// ZookeeperClusterReconciler reconciles a ZookeeperCluster object
type ZookeeperClusterReconciler struct {
	Client      client.Client
	APIReader   client.Reader
	Log         logr.Logger
	Scheme      *runtime.Scheme
	NewZkClient func() zk.ZookeeperClient
}

type reconcileFun func(ctx context.Context, cluster *zookeeperv1beta1.ZookeeperCluster) error

// +kubebuilder:rbac:groups=zookeeper.pravega.io,resources=zookeeperclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=zookeeper.pravega.io,resources=zookeeperclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods;services;endpoints;persistentvolumeclaims;events;configmaps;secrets;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

func (r *ZookeeperClusterReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	if err := ctx.Err(); err != nil {
		return reconcile.Result{}, err
	}
	ctx = ctrl.LoggerInto(ctx, ctrl.LoggerFrom(ctx).WithValues(
		"Request.Namespace", request.Namespace,
		"Request.Name", request.Name))
	ctrl.LoggerFrom(ctx).Info("Reconciling ZookeeperCluster")

	// Fetch the ZookeeperCluster instance
	instance := &zookeeperv1beta1.ZookeeperCluster{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	err := reader.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile
			// request. Owned objects are automatically garbage collected. For
			// additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}
	if !instance.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, r.reconcileFinalizers(ctx, instance)
	}
	if err := instance.Spec.ValidateLogging(); err != nil {
		return reconcile.Result{}, err
	}
	original := instance.DeepCopy()
	changed := instance.WithDefaults()
	if instance.GetTriggerRollingRestart() {
		ctrl.LoggerFrom(ctx).Info("Restarting zookeeper cluster")
		annotationkey, annotationvalue := getRollingRestartAnnotation()
		if instance.Spec.Pod.Annotations == nil {
			instance.Spec.Pod.Annotations = make(map[string]string)
		}
		instance.Spec.Pod.Annotations[annotationkey] = annotationvalue
		instance.SetTriggerRollingRestart(false)
		changed = true
	}
	if changed {
		ctrl.LoggerFrom(ctx).Info("Setting default settings for zookeeper-cluster")
		if err := r.Client.Patch(ctx, instance, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{Requeue: true}, nil
	}
	for _, fun := range []reconcileFun{
		r.reconcileFinalizers,
		r.reconcileConfigMap,
		r.reconcileStatefulSet,
		r.reconcileClientService,
		r.reconcileHeadlessService,
		r.reconcileAdminServerService,
		r.reconcilePodDisruptionBudget,
		r.reconcileClusterStatus,
	} {
		if err := ctx.Err(); err != nil {
			return reconcile.Result{}, err
		}
		if err = fun(ctx, instance); err != nil {
			return reconcile.Result{}, err
		}
	}
	// Recreate any missing resources every 'ReconcileTime'
	return reconcile.Result{RequeueAfter: ReconcileTime}, nil
}

func getRollingRestartAnnotation() (string, string) {
	return "restartTime", time.Now().Format(time.RFC850)
}

func (r *ZookeeperClusterReconciler) reconcileStatefulSet(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {

	// we cannot upgrade if cluster is in UpgradeFailed
	if instance.Status.IsClusterInUpgradeFailedState() {
		sts := zk.MakeStatefulSet(instance)
		if err = controllerutil.SetControllerReference(instance, sts, r.Scheme); err != nil {
			return err
		}
		foundSts := &appsv1.StatefulSet{}
		err = r.Client.Get(ctx, types.NamespacedName{
			Name:      sts.Name,
			Namespace: sts.Namespace,
		}, foundSts)
		if err == nil {
			if err := requireClusterOwner(instance, foundSts, "statefulset"); err != nil {
				return err
			}
			if foundSts.Status.Replicas == foundSts.Status.ReadyReplicas && foundSts.Status.CurrentRevision == foundSts.Status.UpdateRevision {
				ctrl.LoggerFrom(ctx).Info("failed upgrade completed", "upgrade from:", instance.Status.CurrentVersion, "upgrade to:", instance.Status.TargetVersion)
				instance.Status.CurrentVersion = instance.Status.TargetVersion
				instance.Status.SetErrorConditionFalse()
				return r.clearUpgradeStatus(ctx, instance)
			} else {
				ctrl.LoggerFrom(ctx).Info("Unable to recover failed upgrade, make sure all nodes are running the target version")
			}

		}
	}

	if instance.Status.IsClusterInUpgradeFailedState() {
		return nil
	}
	if instance.Spec.Pod.ServiceAccountName != "default" {
		serviceAccount := zk.MakeServiceAccount(instance)
		if err = controllerutil.SetControllerReference(instance, serviceAccount, r.Scheme); err != nil {
			return err
		}
		// Check if this ServiceAccount already exists
		foundServiceAccount := &corev1.ServiceAccount{}
		err = r.Client.Get(ctx, types.NamespacedName{Name: serviceAccount.Name, Namespace: serviceAccount.Namespace}, foundServiceAccount)
		if err != nil && errors.IsNotFound(err) {
			ctrl.LoggerFrom(ctx).Info("Creating a new ServiceAccount", "ServiceAccount.Namespace", serviceAccount.Namespace, "ServiceAccount.Name", serviceAccount.Name)
			err = r.Client.Create(ctx, serviceAccount)
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			if !equality.Semantic.DeepEqual(foundServiceAccount.ImagePullSecrets, serviceAccount.ImagePullSecrets) {
				original := foundServiceAccount.DeepCopy()
				foundServiceAccount.ImagePullSecrets = serviceAccount.ImagePullSecrets
				ctrl.LoggerFrom(ctx).Info("Updating ServiceAccount", "ServiceAccount.Namespace", serviceAccount.Namespace, "ServiceAccount.Name", serviceAccount.Name)
				err = r.Client.Patch(ctx, foundServiceAccount, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
				if err != nil {
					return err
				}
			}
		}
	}
	sts := zk.MakeStatefulSet(instance)
	if err = controllerutil.SetControllerReference(instance, sts, r.Scheme); err != nil {
		return err
	}
	foundSts := &appsv1.StatefulSet{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name:      sts.Name,
		Namespace: sts.Namespace,
	}, foundSts)
	if err != nil && errors.IsNotFound(err) {
		ctrl.LoggerFrom(ctx).Info("Creating a new Zookeeper StatefulSet",
			"StatefulSet.Namespace", sts.Namespace,
			"StatefulSet.Name", sts.Name)
		zk.EnsureLogbackProjection(instance, nil, sts)
		return r.Client.Create(ctx, sts, client.FieldOwner(createFieldManager))
	} else if err != nil {
		return err
	} else {
		if err := requireClusterOwner(instance, foundSts, "statefulset"); err != nil {
			return err
		}
		// Read the parent after the child. Its resource version cannot fence stale parent intent.
		if err := r.requireCurrentClusterSpec(ctx, instance); err != nil {
			return err
		}
		zk.EnsureLogbackProjection(instance, foundSts, sts)
		foundSTSSize := *foundSts.Spec.Replicas
		newSTSSize := *sts.Spec.Replicas
		if newSTSSize != foundSTSSize {
			zkCtx, cancel := context.WithTimeout(ctx, zk.OperationTimeout)
			defer cancel()
			zkClient := r.NewZkClient()
			defer zkClient.Close()
			zkUri := utils.GetZkServiceUri(instance)
			err = zkClient.Connect(zkCtx, zkUri)
			if err != nil {
				return fmt.Errorf("error storing cluster size: %w", err)
			}
			ctrl.LoggerFrom(ctx).Info("Connected to ZK", "ZKURI", zkUri)

			path := utils.GetMetaPath(instance)
			version, err := zkClient.NodeExists(zkCtx, path)
			if err != nil {
				return fmt.Errorf("error doing exists check for znode %s: %w", path, err)
			}
			if err := r.requireCurrentClusterSpec(zkCtx, instance); err != nil {
				return err
			}
			if err := r.requireCurrentStatefulSet(zkCtx, foundSts); err != nil {
				return err
			}

			data := "CLUSTER_SIZE=" + strconv.Itoa(int(newSTSSize))
			ctrl.LoggerFrom(ctx).Info("Updating Cluster Size.", "New Data:", data, "Version", version)
			if err := zkClient.UpdateNode(zkCtx, path, data, version); err != nil {
				return fmt.Errorf("error updating cluster size in znode %s: %w", path, err)
			}
		}
		err = r.updateStatefulSet(ctx, instance, foundSts, sts)
		if err != nil {
			return err
		}
		return r.upgradeStatefulSet(ctx, instance, foundSts)
	}
}

func (r *ZookeeperClusterReconciler) updateStatefulSet(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster, foundSts *appsv1.StatefulSet, sts *appsv1.StatefulSet) (err error) {
	ctrl.LoggerFrom(ctx).Info("Updating StatefulSet",
		"StatefulSet.Namespace", foundSts.Namespace,
		"StatefulSet.Name", foundSts.Name)
	if err := r.applyStatefulSet(ctx, foundSts, sts); err != nil {
		return err
	}
	instance.Status.Replicas = foundSts.Status.Replicas
	instance.Status.ReadyReplicas = foundSts.Status.ReadyReplicas
	return nil
}

func (r *ZookeeperClusterReconciler) upgradeStatefulSet(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster, foundSts *appsv1.StatefulSet) (err error) {

	// Getting the upgradeCondition from the zk clustercondition
	_, upgradeCondition := instance.Status.GetClusterCondition(zookeeperv1beta1.ClusterConditionUpgrading)

	if upgradeCondition == nil {
		// Initially set upgrading condition to false
		instance.Status.SetUpgradingConditionFalse()
		return nil
	}

	// Setting the upgrade condition to true to trigger the upgrade
	// When the zk cluster is upgrading Statefulset CurrentRevision and UpdateRevision are not equal and zk cluster image tag is not equal to CurrentVersion
	if upgradeCondition.Status == corev1.ConditionFalse {
		if instance.Status.IsClusterInReadyState() && foundSts.Status.CurrentRevision != foundSts.Status.UpdateRevision && instance.Spec.Image.Tag != instance.Status.CurrentVersion {
			instance.Status.TargetVersion = instance.Spec.Image.Tag
			instance.Status.SetPodsReadyConditionFalse()
			instance.Status.SetUpgradingConditionTrue("", "")
		}
	}

	// checking if the upgrade is in progress
	if upgradeCondition.Status == corev1.ConditionTrue {
		// checking when the targetversion is empty
		if instance.Status.TargetVersion == "" {
			ctrl.LoggerFrom(ctx).Info("upgrading to an unknown version: cancelling upgrade process")
			return r.clearUpgradeStatus(ctx, instance)
		}
		// Checking for upgrade completion
		if foundSts.Status.CurrentRevision == foundSts.Status.UpdateRevision {
			instance.Status.CurrentVersion = instance.Status.TargetVersion
			ctrl.LoggerFrom(ctx).Info("upgrade completed")
			return r.clearUpgradeStatus(ctx, instance)
		}
		// updating the upgradecondition if upgrade is in progress
		if foundSts.Status.CurrentRevision != foundSts.Status.UpdateRevision {
			ctrl.LoggerFrom(ctx).Info("upgrade in progress")
			if fmt.Sprint(foundSts.Status.UpdatedReplicas) != upgradeCondition.Message {
				instance.Status.UpdateProgress(zookeeperv1beta1.UpdatingZookeeperReason, fmt.Sprint(foundSts.Status.UpdatedReplicas))
			} else {
				err = checkSyncTimeout(instance, zookeeperv1beta1.UpdatingZookeeperReason, foundSts.Status.UpdatedReplicas, 10*time.Minute)
				if err != nil {
					instance.Status.SetErrorConditionTrue("UpgradeFailed", err.Error())
					return r.patchClusterStatus(ctx, instance)
				} else {
					return nil
				}
			}
		}
	}
	return r.patchClusterStatus(ctx, instance)
}

func (r *ZookeeperClusterReconciler) clearUpgradeStatus(ctx context.Context, z *zookeeperv1beta1.ZookeeperCluster) (err error) {
	z.Status.SetUpgradingConditionFalse()
	z.Status.TargetVersion = ""
	return r.patchClusterStatus(ctx, z)
}

func checkSyncTimeout(z *zookeeperv1beta1.ZookeeperCluster, reason string, updatedReplicas int32, t time.Duration) error {
	lastCondition := z.Status.GetLastCondition()
	if lastCondition == nil {
		return nil
	}
	if lastCondition.Reason == reason && lastCondition.Message == fmt.Sprint(updatedReplicas) {
		// if reason and message are the same as before, which means there is no progress since the last reconciling,
		// then check if it reaches the timeout.
		parsedTime, _ := time.Parse(time.RFC3339, lastCondition.LastUpdateTime)
		if time.Now().After(parsedTime.Add(t)) {
			// timeout
			return fmt.Errorf("progress deadline exceeded")
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) reconcileClientService(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	svc := zk.MakeClientService(instance)
	if err = controllerutil.SetControllerReference(instance, svc, r.Scheme); err != nil {
		return err
	}
	foundSvc := &corev1.Service{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name:      svc.Name,
		Namespace: svc.Namespace,
	}, foundSvc)
	if err != nil && errors.IsNotFound(err) {
		ctrl.LoggerFrom(ctx).Info("Creating new client service",
			"Service.Namespace", svc.Namespace,
			"Service.Name", svc.Name)
		return r.Client.Create(ctx, svc, client.FieldOwner(createFieldManager))
	} else if err != nil {
		return err
	} else {
		ctrl.LoggerFrom(ctx).Info("Updating existing client service",
			"Service.Namespace", foundSvc.Namespace,
			"Service.Name", foundSvc.Name)
		err = r.applyService(ctx, instance, foundSvc, svc)
		if err != nil {
			return err
		}
		port := instance.ZookeeperPorts().Client
		instance.Status.InternalClientEndpoint = fmt.Sprintf("%s:%d",
			foundSvc.Spec.ClusterIP, port)
		if foundSvc.Spec.Type == "LoadBalancer" {
			for _, i := range foundSvc.Status.LoadBalancer.Ingress {
				if i.IP != "" {
					instance.Status.ExternalClientEndpoint = fmt.Sprintf("%s:%d",
						i.IP, port)
				}
			}
		} else {
			instance.Status.ExternalClientEndpoint = "N/A"
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) reconcileHeadlessService(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	svc := zk.MakeHeadlessService(instance)
	if err = controllerutil.SetControllerReference(instance, svc, r.Scheme); err != nil {
		return err
	}
	foundSvc := &corev1.Service{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name:      svc.Name,
		Namespace: svc.Namespace,
	}, foundSvc)
	if err != nil && errors.IsNotFound(err) {
		ctrl.LoggerFrom(ctx).Info("Creating new headless service",
			"Service.Namespace", svc.Namespace,
			"Service.Name", svc.Name)
		return r.Client.Create(ctx, svc, client.FieldOwner(createFieldManager))
	} else if err != nil {
		return err
	} else {
		ctrl.LoggerFrom(ctx).Info("Updating existing headless service",
			"Service.Namespace", foundSvc.Namespace,
			"Service.Name", foundSvc.Name)
		err = r.applyService(ctx, instance, foundSvc, svc)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) reconcileAdminServerService(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	svc := zk.MakeAdminServerService(instance)
	if err = controllerutil.SetControllerReference(instance, svc, r.Scheme); err != nil {
		return err
	}
	foundSvc := &corev1.Service{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name:      svc.Name,
		Namespace: svc.Namespace,
	}, foundSvc)
	if err != nil && errors.IsNotFound(err) {
		ctrl.LoggerFrom(ctx).Info("Creating admin server service",
			"Service.Namespace", svc.Namespace,
			"Service.Name", svc.Name)
		return r.Client.Create(ctx, svc, client.FieldOwner(createFieldManager))
	} else if err != nil {
		return err
	} else {
		ctrl.LoggerFrom(ctx).Info("Updating existing admin server service",
			"Service.Namespace", foundSvc.Namespace,
			"Service.Name", foundSvc.Name)
		err = r.applyService(ctx, instance, foundSvc, svc)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) reconcilePodDisruptionBudget(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	pdb := zk.MakePodDisruptionBudget(instance)
	if err = controllerutil.SetControllerReference(instance, pdb, r.Scheme); err != nil {
		return err
	}
	foundPdb := &policyv1.PodDisruptionBudget{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name:      pdb.Name,
		Namespace: pdb.Namespace,
	}, foundPdb)
	if err != nil && errors.IsNotFound(err) {
		ctrl.LoggerFrom(ctx).Info("Creating new pod-disruption-budget",
			"PodDisruptionBudget.Namespace", pdb.Namespace,
			"PodDisruptionBudget.Name", pdb.Name)
		err = r.Client.Create(ctx, pdb)
		if err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return err
	}
	original := foundPdb.DeepCopy()
	foundPdb.Spec.MinAvailable = nil
	foundPdb.Spec.MaxUnavailable = pdb.Spec.MaxUnavailable
	foundPdb.Spec.Selector = pdb.Spec.Selector
	if equality.Semantic.DeepEqual(original.Spec, foundPdb.Spec) {
		return nil
	}
	return r.Client.Patch(ctx, foundPdb, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
}

func (r *ZookeeperClusterReconciler) reconcileConfigMap(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	cm := zk.MakeConfigMap(instance)
	if err = controllerutil.SetControllerReference(instance, cm, r.Scheme); err != nil {
		return err
	}
	foundCm := &corev1.ConfigMap{}
	err = r.Client.Get(ctx, types.NamespacedName{
		Name:      cm.Name,
		Namespace: cm.Namespace,
	}, foundCm)
	if err != nil && errors.IsNotFound(err) {
		ctrl.LoggerFrom(ctx).Info("Creating a new Zookeeper Config Map",
			"ConfigMap.Namespace", cm.Namespace,
			"ConfigMap.Name", cm.Name)
		err = r.Client.Create(ctx, cm)
		if err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return err
	} else {
		if equality.Semantic.DeepEqual(foundCm.Data, cm.Data) &&
			equality.Semantic.DeepEqual(foundCm.BinaryData, cm.BinaryData) {
			return nil
		}
		ctrl.LoggerFrom(ctx).Info("Updating existing config-map",
			"ConfigMap.Namespace", foundCm.Namespace,
			"ConfigMap.Name", foundCm.Name)
		original := foundCm.DeepCopy()
		zk.SyncConfigMap(foundCm, cm)
		err = r.Client.Patch(ctx, foundCm, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) reconcileClusterStatus(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	if instance.Status.IsClusterInUpgradingState() || instance.Status.IsClusterInUpgradeFailedState() {
		return nil
	}
	instance.Status.Init()
	foundPods := &corev1.PodList{}
	labelSelector := labels.SelectorFromSet(map[string]string{"app": instance.GetName()})
	listOps := &client.ListOptions{
		Namespace:     instance.Namespace,
		LabelSelector: labelSelector,
	}
	err = r.Client.List(ctx, foundPods, listOps)
	if err != nil {
		return err
	}
	var (
		readyMembers   []string
		unreadyMembers []string
	)
	for _, p := range foundPods.Items {
		ready := true
		for _, c := range p.Status.ContainerStatuses {
			if !c.Ready {
				ready = false
			}
		}
		if ready {
			readyMembers = append(readyMembers, p.Name)
		} else {
			unreadyMembers = append(unreadyMembers, p.Name)
		}
	}
	sort.Strings(readyMembers)
	sort.Strings(unreadyMembers)
	instance.Status.Members.Ready = readyMembers
	instance.Status.Members.Unready = unreadyMembers

	// If Cluster is in a ready state...
	if instance.Spec.Replicas == instance.Status.ReadyReplicas && (!instance.Status.MetaRootCreated) {
		zkCtx, cancel := context.WithTimeout(ctx, zk.OperationTimeout)
		defer cancel()
		zkClient := r.NewZkClient()
		defer zkClient.Close()
		ctrl.LoggerFrom(ctx).Info("Cluster is Ready, Creating ZK Metadata...")
		zkUri := utils.GetZkServiceUri(instance)
		err := zkClient.Connect(zkCtx, zkUri)
		if err != nil {
			return fmt.Errorf("error creating cluster metaroot, connect to zk failed: %w", err)
		}
		metaPath := utils.GetMetaPath(instance)
		ctrl.LoggerFrom(ctx).Info("Connected to zookeeper:", "ZKUri", zkUri, "Creating Path", metaPath)
		if err := zkClient.CreateNode(zkCtx, instance, metaPath); err != nil {
			return fmt.Errorf("error creating cluster metadata path %s: %w", metaPath, err)
		}
		ctrl.LoggerFrom(ctx).Info("Metadata znode created.")
		instance.Status.MetaRootCreated = true
	}
	ctrl.LoggerFrom(ctx).Info("Updating zookeeper status",
		"StatefulSet.Namespace", instance.Namespace,
		"StatefulSet.Name", instance.Name)
	if instance.Status.ReadyReplicas == instance.Spec.Replicas {
		instance.Status.SetPodsReadyConditionTrue()
	} else {
		instance.Status.SetPodsReadyConditionFalse()
	}
	if instance.Status.CurrentVersion == "" && instance.Status.IsClusterInReadyState() {
		instance.Status.CurrentVersion = instance.Spec.Image.Tag
	}
	return r.patchClusterStatus(ctx, instance)
}

func (r *ZookeeperClusterReconciler) patchClusterStatus(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) error {
	current := &zookeeperv1beta1.ZookeeperCluster{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(instance), current); err != nil {
		return err
	}
	if current.ResourceVersion != instance.ResourceVersion || current.UID != instance.UID {
		return staleCluster(instance)
	}
	if equality.Semantic.DeepEqual(current.Status, instance.Status) {
		return nil
	}
	desired := current.DeepCopy()
	desired.Status = instance.Status
	if err := r.Client.Status().Patch(ctx, desired, client.MergeFromWithOptions(current, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	instance.ResourceVersion = desired.ResourceVersion
	instance.Status = desired.Status
	return nil
}

func staleCluster(instance *zookeeperv1beta1.ZookeeperCluster) error {
	return errors.NewConflict(schema.GroupResource{Group: zookeeperv1beta1.GroupVersion.Group, Resource: "zookeeperclusters"}, instance.Name, fmt.Errorf("reconcile snapshot is stale"))
}

func (r *ZookeeperClusterReconciler) requireCurrentClusterSpec(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	current := &zookeeperv1beta1.ZookeeperCluster{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(instance), current); err != nil {
		return err
	}
	if current.UID != instance.UID || current.Generation != instance.Generation || !current.DeletionTimestamp.IsZero() {
		return staleCluster(instance)
	}
	return nil
}

func (r *ZookeeperClusterReconciler) requireCurrentStatefulSet(ctx context.Context, observed *appsv1.StatefulSet) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	current := &appsv1.StatefulSet{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(observed), current); err != nil {
		return err
	}
	if current.UID != observed.UID || current.ResourceVersion != observed.ResourceVersion || !current.DeletionTimestamp.IsZero() {
		return errors.NewConflict(appsv1.Resource("statefulsets"), observed.Name, fmt.Errorf("resize snapshot is stale"))
	}
	return nil
}

// YAMLExporterReconciler returns a fake Reconciler which is being used for generating YAML files
func YAMLExporterReconciler(zookeepercluster *zookeeperv1beta1.ZookeeperCluster) *ZookeeperClusterReconciler {
	var scheme = scheme.Scheme
	scheme.AddKnownTypes(zookeeperv1beta1.GroupVersion, zookeepercluster)
	return &ZookeeperClusterReconciler{
		Client:      fake.NewClientBuilder().WithRuntimeObjects(zookeepercluster).Build(),
		Scheme:      scheme,
		NewZkClient: func() zk.ZookeeperClient { return new(zk.DefaultZookeeperClient) },
	}
}

// GenerateYAML generated secondary resource of ZookeeperCluster resources YAML files
func (r *ZookeeperClusterReconciler) GenerateYAML(inst *zookeeperv1beta1.ZookeeperCluster) error {
	if err := inst.Spec.ValidateLogging(); err != nil {
		return err
	}
	if inst.WithDefaults() {
		fmt.Println("set default values")
	}
	for _, fun := range []func(*zookeeperv1beta1.ZookeeperCluster) error{
		r.yamlConfigMap,
		r.yamlStatefulSet,
		r.yamlClientService,
		r.yamlHeadlessService,
		r.yamlPodDisruptionBudget,
	} {
		if err := fun(inst); err != nil {
			return err
		}
	}
	return nil
}

// yamlStatefulSet will generates YAML file for StatefulSet
func (r *ZookeeperClusterReconciler) yamlStatefulSet(instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	sts := zk.MakeStatefulSet(instance)
	zk.EnsureLogbackProjection(instance, nil, sts)

	subdir, err := yamlexporter.CreateOutputSubDir("ZookeeperCluster", sts.Labels["component"])
	return yamlexporter.GenerateOutputYAMLFile(subdir, sts.Kind, sts)
}

// yamlClientService will generates YAML file for zookeeper Client service
func (r *ZookeeperClusterReconciler) yamlClientService(instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	svc := zk.MakeClientService(instance)

	subdir, err := yamlexporter.CreateOutputSubDir("ZookeeperCluster", "client")
	if err != nil {
		return err
	}
	return yamlexporter.GenerateOutputYAMLFile(subdir, svc.Kind, svc)
}

// yamlHeadlessService will generates YAML file for zookeeper headless service
func (r *ZookeeperClusterReconciler) yamlHeadlessService(instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	svc := zk.MakeHeadlessService(instance)

	subdir, err := yamlexporter.CreateOutputSubDir("ZookeeperCluster", "headless")
	if err != nil {
		return err
	}
	return yamlexporter.GenerateOutputYAMLFile(subdir, svc.Kind, svc)
}

// yamlPodDisruptionBudget will generates YAML file for zookeeper PDB
func (r *ZookeeperClusterReconciler) yamlPodDisruptionBudget(instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	pdb := zk.MakePodDisruptionBudget(instance)

	subdir, err := yamlexporter.CreateOutputSubDir("ZookeeperCluster", "pdb")
	if err != nil {
		return err
	}
	return yamlexporter.GenerateOutputYAMLFile(subdir, pdb.Kind, pdb)
}

// yamlConfigMap will generates YAML file for Zookeeper configmap
func (r *ZookeeperClusterReconciler) yamlConfigMap(instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	cm := zk.MakeConfigMap(instance)

	subdir, err := yamlexporter.CreateOutputSubDir("ZookeeperCluster", "config")
	if err != nil {
		return err
	}
	return yamlexporter.GenerateOutputYAMLFile(subdir, cm.Kind, cm)
}

func (r *ZookeeperClusterReconciler) reconcileFinalizers(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	if instance.DeletionTimestamp.IsZero() {
		if instance.Spec.Persistence != nil && instance.Spec.Persistence.VolumeReclaimPolicy != zookeeperv1beta1.VolumeReclaimPolicyDelete {
			return nil
		}
		if !utils.ContainsString(instance.ObjectMeta.Finalizers, utils.ZkFinalizer) && !config.DisableFinalizer {
			original := instance.DeepCopy()
			instance.ObjectMeta.Finalizers = append(instance.ObjectMeta.Finalizers, utils.ZkFinalizer)
			if err = r.Client.Patch(ctx, instance, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
		return r.cleanupOrphanPVCs(ctx, instance)
	}
	if utils.ContainsString(instance.Finalizers, utils.ZkFinalizer) {
		if instance.Spec.Persistence != nil && instance.Spec.Persistence.VolumeReclaimPolicy == zookeeperv1beta1.VolumeReclaimPolicyDelete {
			if err = r.cleanUpAllPVCs(ctx, instance); err != nil {
				return err
			}
		}
		original := instance.DeepCopy()
		instance.Finalizers = utils.RemoveString(instance.Finalizers, utils.ZkFinalizer)
		if err = r.Client.Patch(ctx, instance, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) getPVCCount(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (pvcCount int, err error) {
	pvcList, err := r.getPVCList(ctx, instance)
	if err != nil {
		return -1, err
	}
	pvcCount = len(pvcList.Items)
	return pvcCount, nil
}

func (r *ZookeeperClusterReconciler) cleanupOrphanPVCs(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	// this check should make sure we do not delete the PVCs before the STS has scaled down
	if instance.Status.ReadyReplicas == instance.Spec.Replicas {
		pvcCount, err := r.getPVCCount(ctx, instance)
		if err != nil {
			return err
		}
		ctrl.LoggerFrom(ctx).Info("cleanupOrphanPVCs", "PVC Count", pvcCount, "ReadyReplicas Count", instance.Status.ReadyReplicas)
		if pvcCount > int(instance.Spec.Replicas) {
			pvcList, err := r.getPVCList(ctx, instance)
			if err != nil {
				return err
			}
			for _, pvcItem := range pvcList.Items {
				// delete only Orphan PVCs
				if utils.IsPVCOrphan(pvcItem.Name, instance.Spec.Replicas) {
					if err := r.deletePVC(ctx, pvcItem); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) getPVCList(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (pvList corev1.PersistentVolumeClaimList, err error) {
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{"app": instance.GetName(), "uid": string(instance.UID)},
	})
	pvclistOps := &client.ListOptions{
		Namespace:     instance.Namespace,
		LabelSelector: selector,
	}
	pvcList := &corev1.PersistentVolumeClaimList{}
	err = r.Client.List(ctx, pvcList, pvclistOps)
	return *pvcList, err
}

func (r *ZookeeperClusterReconciler) cleanUpAllPVCs(ctx context.Context, instance *zookeeperv1beta1.ZookeeperCluster) (err error) {
	pvcList, err := r.getPVCList(ctx, instance)
	if err != nil {
		return err
	}
	for _, pvcItem := range pvcList.Items {
		if err := r.deletePVC(ctx, pvcItem); err != nil {
			return err
		}
	}
	return nil
}

func (r *ZookeeperClusterReconciler) deletePVC(ctx context.Context, pvcItem corev1.PersistentVolumeClaim) error {
	pvcDelete := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcItem.Name,
			Namespace: pvcItem.Namespace,
		},
	}
	ctrl.LoggerFrom(ctx).Info("Deleting PVC", "With Name", pvcItem.Name)
	err := client.IgnoreNotFound(r.Client.Delete(ctx, pvcDelete))
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Error deleting PVC.", "Name", pvcDelete.Name)
	}
	return err
}

func (r *ZookeeperClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	primary := &zookeeperv1beta1.ZookeeperCluster{}
	owned := []client.Object{&appsv1.StatefulSet{}, &corev1.Service{}, &corev1.Pod{}}
	controller := ctrl.NewControllerManagedBy(mgr).For(primary)
	for _, object := range owned {
		controller.Owns(object)
	}
	// Initialize the same watches eagerly so manager startup waits for their caches.
	for _, object := range append([]client.Object{primary}, owned...) {
		if _, err := mgr.GetCache().GetInformer(context.TODO(), object, cache.BlockUntilSynced(false)); err != nil {
			return fmt.Errorf("initialize cache for %T: %w", object, err)
		}
	}
	return controller.
		WithEventFilter(reconcileEventPredicate()).
		Complete(r)
}

func reconcileEventPredicate() predicate.Predicate {
	return predicate.Or(predicate.GenerationChangedPredicate{}, predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldSTS, oldOK := e.ObjectOld.(*appsv1.StatefulSet)
			newSTS, newOK := e.ObjectNew.(*appsv1.StatefulSet)
			return oldOK && newOK && !equality.Semantic.DeepEqual(oldSTS.Status, newSTS.Status)
		},
	})
}
