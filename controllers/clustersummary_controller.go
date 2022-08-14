/*
Copyright 2022. projectsveltos.io. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2/klogr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	configv1alpha1 "github.com/projectsveltos/cluster-api-feature-manager/api/v1alpha1"
	"github.com/projectsveltos/cluster-api-feature-manager/pkg/deployer"
	"github.com/projectsveltos/cluster-api-feature-manager/pkg/logs"
	"github.com/projectsveltos/cluster-api-feature-manager/pkg/scope"
)

const (
	// deleteRequeueAfter is how long to wait before checking again to see if the cluster still has
	// children during deletion.
	deleteRequeueAfter = 20 * time.Second

	// normalRequeueAfter is how long to wait before checking again to see if the cluster can be moved
	// to ready after or workload features (for instance ingress or reporter) have failed
	normalRequeueAfter = 20 * time.Second
)

// ClusterSummaryReconciler reconciles a ClusterSummary object
type ClusterSummaryReconciler struct {
	*rest.Config
	client.Client
	Scheme               *runtime.Scheme
	Deployer             deployer.DeployerInterface
	ConcurrentReconciles int
	Mux                  sync.Mutex      // use a Mutex to update Map as MaxConcurrentReconciles is higher than one
	ReferenceMap         map[string]*Set // key: Referenced object  name; value: set of all ClusterSummaries referencing the resource
	// Refenced object name is: <kind>-<namespace>-<name> or <kind>-<name>
	ClusterSummaryMap map[string]*Set // key: ClusterSummary name; value: set of referenced resources

	// Reason for the two maps:
	// ClusterSummary references WorkloadRoles. When a WorkloadRole changes, all the ClusterSummaries referencing it need to be
	// reconciled. In order to achieve so, ClusterSummary reconciler could watch for WorkloadRoles. When a WorkloadRole spec changes,
	// find all the ClusterSummaries currently referencing it and reconcile those. Problem is no I/O should be present inside a MapFunc
	// (given a WorkloadRole, return all the ClusterSummary referencing such WorkloadRole).
	// In the MapFunc, if the list ClusterSummaries operation failed, we would be unable to retry or re-enqueue the ClusterSummaries
	// referencing the WorkloadRole that changed.
	// Instead the approach taken is following:
	// - when a ClusterSummary is reconciled, update the ReferenceMap;
	// - in the MapFunc, given the WorkloadRole that changed, we can immeditaly get all the ClusterSummaries needing a reconciliation (by
	// using the ReferenceMap);
	// - if a ClusterSummary is referencing a WorkloadRole but its reconciliation is still queued, when WorkloadRole changes, ReferenceMap
	// won't have such ClusterSummary. This is not a problem as ClusterSummary reconciliation is already queued and will happen.
	//
	// The ClusterSummaryMap is used to update ReferenceMap. Consider following scenarios to understand the need:
	// 1. ClusterSummary A references WorkloadRoles 1 and 2. When reconciled, ReferenceMap will have 1 => A and 2 => A;
	// and ClusterSummaryMap A => 1,2
	// 2. ClusterSummary A changes and now references WorkloadRole 1 only. We ned to remove the entry 2 => A in ReferenceMap. But
	// when we reconcile ClusterSummary we have its current version we don't have its previous version. So we use ClusterSummaryMap (at this
	// point value stored here corresponds to reconciliation #1. We know currently ClusterSummary references WorkloadRole 1 only and looking
	// at ClusterSummaryMap we know it used to reference WorkloadRole 1 and 2. So we can remove 2 => A from ReferenceMap. Only after this
	// update, we update ClusterSummaryMap (so new value will be A => 1)
	//
	// Same logic applies to Kyverno (kyverno configuration references configmaps containing kyverno policies)
}

//+kubebuilder:rbac:groups=config.projectsveltos.io,resources=clustersummaries,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=config.projectsveltos.io,resources=clustersummaries/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=config.projectsveltos.io,resources=clustersummaries/finalizers,verbs=update;patch
//+kubebuilder:rbac:groups=config.projectsveltos.io,resources=workloadroles,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

func (r *ClusterSummaryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	logger := ctrl.LoggerFrom(ctx)
	logger.Info("Reconciling")

	// Fecth the clusterSummary instance
	clusterSummary := &configv1alpha1.ClusterSummary{}
	if err := r.Get(ctx, req.NamespacedName, clusterSummary); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		logger.Error(err, "Failed to fetch clusterSummary")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"Failed to fetch clusterSummary %s",
			req.NamespacedName,
		)
	}

	// Fetch the ClusterFeature.
	clusterFeature, err := getClusterFeatureOwner(ctx, r.Client, clusterSummary)
	if err != nil {
		logger.Error(err, "Failed to get owner clusterFeature")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"Failed to get owner clusterFeature for %s",
			req.NamespacedName,
		)
	}

	clusterSummaryScope, err := scope.NewClusterSummaryScope(scope.ClusterSummaryScopeParams{
		Client:         r.Client,
		Logger:         logger,
		ClusterSummary: clusterSummary,
		ClusterFeature: clusterFeature,
		ControllerName: "clusterfeature",
	})
	if err != nil {
		logger.Error(err, "Failed to create clusterFeatureScope")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"unable to create clusterfeature scope for %s",
			req.NamespacedName,
		)
	}

	// Always close the scope when exiting this function so we can persist any ClusterSummary
	// changes.
	defer func() {
		if err := clusterSummaryScope.Close(ctx); err != nil {
			reterr = err
		}
	}()

	// Handle deleted clusterSummary
	if !clusterSummary.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, clusterSummaryScope, logger)
	}

	// Handle non-deleted clusterSummary
	return r.reconcileNormal(ctx, clusterSummaryScope, logger)
}

func (r *ClusterSummaryReconciler) reconcileDelete(
	ctx context.Context,
	clusterSummaryScope *scope.ClusterSummaryScope,
	logger logr.Logger,
) (reconcile.Result, error) {

	logger.Info("Reconciling ClusterSummary delete")

	if err := r.undeploy(ctx, clusterSummaryScope, logger); err != nil {
		return reconcile.Result{Requeue: true, RequeueAfter: deleteRequeueAfter}, nil
	}

	// Cluster is deleted so remove the finalizer.
	logger.Info("Removing finalizer")
	if controllerutil.ContainsFinalizer(clusterSummaryScope.ClusterSummary, configv1alpha1.ClusterSummaryFinalizer) {
		if finalizersUpdated := controllerutil.RemoveFinalizer(clusterSummaryScope.ClusterSummary,
			configv1alpha1.ClusterSummaryFinalizer); !finalizersUpdated {
			return reconcile.Result{}, fmt.Errorf("failed to remove finalizer")
		}
	}

	logger.Info("Reconcile delete success")

	return reconcile.Result{}, nil
}

func (r *ClusterSummaryReconciler) reconcileNormal(
	ctx context.Context,
	clusterSummaryScope *scope.ClusterSummaryScope,
	logger logr.Logger,
) (reconcile.Result, error) {

	logger.Info("Reconciling ClusterSummary")

	if !controllerutil.ContainsFinalizer(clusterSummaryScope.ClusterSummary, configv1alpha1.ClusterSummaryFinalizer) {
		if err := r.addFinalizer(ctx, clusterSummaryScope); err != nil {
			return reconcile.Result{}, err
		}
	}

	r.generatePolicyNamePrefix(clusterSummaryScope)

	r.updatesMaps(clusterSummaryScope)

	if err := r.deploy(ctx, clusterSummaryScope, logger); err != nil {
		return reconcile.Result{Requeue: true, RequeueAfter: normalRequeueAfter}, nil
	}

	logger.Info("Reconciling ClusterSummary success")
	return reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterSummaryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&configv1alpha1.ClusterSummary{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.ConcurrentReconciles,
		}).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "error creating controller")
	}

	// When ConfigMap changes, according to ConfigMapPredicates,
	// one or more ClusterSummaries need to be reconciled.
	if err := c.Watch(&source.Kind{Type: &corev1.ConfigMap{}},
		handler.EnqueueRequestsFromMapFunc(r.requeueClusterSummaryForConfigMap),
		ConfigMapPredicates(klogr.New().WithValues("predicate", "configmappredicate")),
	); err != nil {
		return err
	}

	// When WorkloadRole changes, according to WorkloadRolePredicates,
	// one or more ClusterSummaries need to be reconciled.
	return c.Watch(&source.Kind{Type: &configv1alpha1.WorkloadRole{}},
		handler.EnqueueRequestsFromMapFunc(r.requeueClusterSummaryForWorkloadRole),
		WorkloadRolePredicates(klogr.New().WithValues("predicate", "workloadrolepredicate")),
	)
}

func (r *ClusterSummaryReconciler) addFinalizer(ctx context.Context, clusterSummaryScope *scope.ClusterSummaryScope) error {
	// If the SveltosCluster doesn't have our finalizer, add it.
	controllerutil.AddFinalizer(clusterSummaryScope.ClusterSummary, configv1alpha1.ClusterSummaryFinalizer)
	// Register the finalizer immediately to avoid orphaning clusterfeature resources on delete
	if err := clusterSummaryScope.PatchObject(ctx); err != nil {
		clusterSummaryScope.Error(err, "Failed to add finalizer")
		return errors.Wrapf(
			err,
			"Failed to add finalizer for %s",
			clusterSummaryScope.Name(),
		)
	}
	return nil
}

func (r *ClusterSummaryReconciler) deploy(ctx context.Context, clusterSummaryScope *scope.ClusterSummaryScope, logger logr.Logger) error {
	workloadErr := r.deployRoles(ctx, clusterSummaryScope, logger)

	kyvernoErr := r.deployKyverno(ctx, clusterSummaryScope, logger)

	prometheusErr := r.deployPrometheus(ctx, clusterSummaryScope, logger)

	if workloadErr != nil {
		return workloadErr
	}

	if kyvernoErr != nil {
		return kyvernoErr
	}

	if prometheusErr != nil {
		return prometheusErr
	}

	return nil
}

func (r *ClusterSummaryReconciler) deployRoles(ctx context.Context, clusterSummaryScope *scope.ClusterSummaryScope, logger logr.Logger) error {
	f := feature{
		id:          configv1alpha1.FeatureRole,
		currentHash: workloadRoleHash,
		deploy:      deployWorkloadRoles,
		getRefs:     getWorkloadRoleRefs,
	}

	return r.deployFeature(ctx, clusterSummaryScope, f, logger)
}

func (r *ClusterSummaryReconciler) deployKyverno(ctx context.Context, clusterSummaryScope *scope.ClusterSummaryScope, logger logr.Logger) error {
	if clusterSummaryScope.ClusterSummary.Spec.ClusterFeatureSpec.KyvernoConfiguration == nil {
		logger.V(logs.LogDebug).Info("no kyverno configuration")
		return nil
	}

	f := feature{
		id:          configv1alpha1.FeatureKyverno,
		currentHash: kyvernoHash,
		deploy:      deployKyverno,
		getRefs:     getKyvernoRefs,
	}

	return r.deployFeature(ctx, clusterSummaryScope, f, logger)
}

func (r *ClusterSummaryReconciler) deployPrometheus(ctx context.Context, clusterSummaryScope *scope.ClusterSummaryScope, logger logr.Logger) error {
	if clusterSummaryScope.ClusterSummary.Spec.ClusterFeatureSpec.PrometheusConfiguration == nil {
		logger.V(logs.LogDebug).Info("no prometheus configuration")
		return nil
	}

	f := feature{
		id:          configv1alpha1.FeaturePrometheus,
		currentHash: prometheusHash,
		deploy:      deployPrometheus,
		getRefs:     getPrometheusRefs,
	}

	return r.deployFeature(ctx, clusterSummaryScope, f, logger)
}

func (r *ClusterSummaryReconciler) undeploy(ctx context.Context, clusterSummaryScope *scope.ClusterSummaryScope, logger logr.Logger) error {
	clusterSummary := clusterSummaryScope.ClusterSummary

	// If CAPI Cluster is not found, there is nothing to clean up.
	cluster := &clusterv1.Cluster{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: clusterSummary.Spec.ClusterNamespace, Name: clusterSummary.Spec.ClusterName}, cluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(logs.LogInfo).Info(fmt.Sprintf("cluster %s/%s not found. Nothing to do.", clusterSummary.Spec.ClusterNamespace, clusterSummary.Spec.ClusterName))
			return nil
		}
		return err
	}

	workloadErr := r.undeployRoles(ctx, clusterSummaryScope, logger)

	kyvernoErr := r.undeployKyverno(ctx, clusterSummaryScope, logger)

	prometheusErr := r.undeployPrometheus(ctx, clusterSummaryScope, logger)

	if workloadErr != nil {
		return workloadErr
	}

	if kyvernoErr != nil {
		return kyvernoErr
	}

	if prometheusErr != nil {
		return prometheusErr
	}

	return nil
}

func (r *ClusterSummaryReconciler) undeployRoles(ctx context.Context, clusterSummaryScope *scope.ClusterSummaryScope, logger logr.Logger) error {
	f := feature{
		id:          configv1alpha1.FeatureRole,
		currentHash: workloadRoleHash,
		deploy:      unDeployWorkloadRoles,
	}

	return r.undeployFeature(ctx, clusterSummaryScope, f, logger)
}

func (r *ClusterSummaryReconciler) undeployKyverno(ctx context.Context, clusterSummaryScope *scope.ClusterSummaryScope, logger logr.Logger) error {
	f := feature{
		id:          configv1alpha1.FeatureKyverno,
		currentHash: kyvernoHash,
		deploy:      unDeployKyverno,
	}

	return r.undeployFeature(ctx, clusterSummaryScope, f, logger)
}

func (r *ClusterSummaryReconciler) undeployPrometheus(ctx context.Context, clusterSummaryScope *scope.ClusterSummaryScope, logger logr.Logger) error {
	f := feature{
		id:          configv1alpha1.FeaturePrometheus,
		currentHash: prometheusHash,
		deploy:      unDeployPrometheus,
	}

	return r.undeployFeature(ctx, clusterSummaryScope, f, logger)
}

func (r *ClusterSummaryReconciler) generatePolicyNamePrefix(clusterSummaryScope *scope.ClusterSummaryScope) {
	if clusterSummaryScope.ClusterSummary.Status.PolicyPrefix == "" {
		// TODO: make sure no two ClusterSummary get same prefix
		const length = 10
		clusterSummaryScope.ClusterSummary.Status.PolicyPrefix = "cs" + util.RandomString(length)
	}
}

func (r *ClusterSummaryReconciler) updatesMaps(clusterSummaryScope *scope.ClusterSummaryScope) {
	currentReferences := r.getCurrentReferences(clusterSummaryScope)

	r.Mux.Lock()
	defer r.Mux.Unlock()

	// Get list of References not referenced anymore by ClusterSummary
	var toBeRemoved []string
	if v, ok := r.ClusterSummaryMap[clusterSummaryScope.Name()]; ok {
		toBeRemoved = v.difference(currentReferences)
	}

	// For each currently referenced instance, add ClusterSummary as consumer
	for referencedResource := range currentReferences.data {
		r.getReferenceMapForEntry(referencedResource).insert(clusterSummaryScope.Name())
	}

	// For each resource not reference anymore, remove ClusterSummary as consumer
	for i := range toBeRemoved {
		referencedResource := toBeRemoved[i]
		r.getReferenceMapForEntry(referencedResource).erase(clusterSummaryScope.Name())
	}

	// Update list of WorklaodRoles currently referenced by ClusterSummary
	r.ClusterSummaryMap[clusterSummaryScope.Name()] = currentReferences
}

func (r *ClusterSummaryReconciler) getCurrentReferences(clusterSummaryScope *scope.ClusterSummaryScope) *Set {
	currentReferences := &Set{}
	for i := range clusterSummaryScope.ClusterSummary.Spec.ClusterFeatureSpec.WorkloadRoleRefs {
		workloadRoleName := clusterSummaryScope.ClusterSummary.Spec.ClusterFeatureSpec.WorkloadRoleRefs[i].Name
		currentReferences.insert(getEntryKey(WorkloadRole, "", workloadRoleName))
	}
	if clusterSummaryScope.ClusterSummary.Spec.ClusterFeatureSpec.KyvernoConfiguration != nil {
		for i := range clusterSummaryScope.ClusterSummary.Spec.ClusterFeatureSpec.KyvernoConfiguration.PolicyRefs {
			cmNamespace := clusterSummaryScope.ClusterSummary.Spec.ClusterFeatureSpec.KyvernoConfiguration.PolicyRefs[i].Namespace
			cmName := clusterSummaryScope.ClusterSummary.Spec.ClusterFeatureSpec.KyvernoConfiguration.PolicyRefs[i].Name
			currentReferences.insert(getEntryKey(ConfigMap, cmNamespace, cmName))
		}
	}
	if clusterSummaryScope.ClusterSummary.Spec.ClusterFeatureSpec.PrometheusConfiguration != nil {
		for i := range clusterSummaryScope.ClusterSummary.Spec.ClusterFeatureSpec.PrometheusConfiguration.PolicyRefs {
			cmNamespace := clusterSummaryScope.ClusterSummary.Spec.ClusterFeatureSpec.PrometheusConfiguration.PolicyRefs[i].Namespace
			cmName := clusterSummaryScope.ClusterSummary.Spec.ClusterFeatureSpec.PrometheusConfiguration.PolicyRefs[i].Name
			currentReferences.insert(getEntryKey(ConfigMap, cmNamespace, cmName))
		}
	}
	return currentReferences
}

func (r *ClusterSummaryReconciler) getReferenceMapForEntry(entry string) *Set {
	s := r.ReferenceMap[entry]
	if s == nil {
		s = &Set{}
		r.ReferenceMap[entry] = s
	}
	return s
}