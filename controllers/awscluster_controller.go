/*
Copyright 2019 The Kubernetes Authors.

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
	"time"

	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	infrav1 "sigs.k8s.io/cluster-api-provider-aws/v2/api/v1beta2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/feature"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/scope"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/ec2"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/elb"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/gc"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/instancestate"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/network"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/s3"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/cloud/services/securitygroup"
	"sigs.k8s.io/cluster-api-provider-aws/v2/pkg/logger"
	infrautilconditions "sigs.k8s.io/cluster-api-provider-aws/v2/util/conditions"
	"sigs.k8s.io/cluster-api-provider-aws/v2/util/paused"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	capiannotations "sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/predicates"
)

const (
	deleteRequeueAfter = 20 * time.Second
)

var defaultAWSSecurityGroupRoles = []infrav1.SecurityGroupRole{
	infrav1.SecurityGroupAPIServerLB,
	infrav1.SecurityGroupLB,
	infrav1.SecurityGroupControlPlane,
	infrav1.SecurityGroupNode,
}

// AWSClusterReconciler reconciles a AwsCluster object.
type AWSClusterReconciler struct {
	client.Client
	Recorder                     record.EventRecorder
	ec2ServiceFactory            func(scope.EC2Scope) services.EC2Interface
	networkServiceFactory        func(scope.ClusterScope) services.NetworkInterface
	elbServiceFactory            func(scope.ELBScope) services.ELBInterface
	securityGroupFactory         func(scope.ClusterScope) services.SecurityGroupInterface
	Endpoints                    []scope.ServiceEndpoint
	WatchFilterValue             string
	ExternalResourceGC           bool
	AlternativeGCStrategy        bool
	TagUnmanagedNetworkResources bool
	MaxWaitActiveUpdateDelete    time.Duration
}

// getEC2Service factory func is added for testing purpose so that we can inject mocked EC2Service to the AWSClusterReconciler.
func (r *AWSClusterReconciler) getEC2Service(scope scope.EC2Scope) services.EC2Interface {
	if r.ec2ServiceFactory != nil {
		return r.ec2ServiceFactory(scope)
	}
	return ec2.NewService(scope)
}

// getELBService factory func is added for testing purpose so that we can inject mocked ELBService to the AWSClusterReconciler.
func (r *AWSClusterReconciler) getELBService(scope scope.ELBScope) services.ELBInterface {
	if r.elbServiceFactory != nil {
		return r.elbServiceFactory(scope)
	}
	return elb.NewService(scope)
}

// getNetworkService factory func is added for testing purpose so that we can inject mocked NetworkService to the AWSClusterReconciler.
func (r *AWSClusterReconciler) getNetworkService(scope scope.ClusterScope) services.NetworkInterface {
	if r.networkServiceFactory != nil {
		return r.networkServiceFactory(scope)
	}
	return network.NewService(&scope)
}

// securityGroupRolesForCluster returns the security group roles determined by the cluster configuration.
func securityGroupRolesForCluster(scope scope.ClusterScope) []infrav1.SecurityGroupRole {
	// Copy to ensure we do not modify the package-level variable.
	roles := make([]infrav1.SecurityGroupRole, len(defaultAWSSecurityGroupRoles))
	copy(roles, defaultAWSSecurityGroupRoles)

	if scope.Bastion().Enabled {
		roles = append(roles, infrav1.SecurityGroupBastion)
	}
	return roles
}

// getSecurityGroupService factory func is added for testing purpose so that we can inject mocked SecurityGroupService to the AWSClusterReconciler.
func (r *AWSClusterReconciler) getSecurityGroupService(scope scope.ClusterScope) services.SecurityGroupInterface {
	if r.securityGroupFactory != nil {
		return r.securityGroupFactory(scope)
	}
	return securitygroup.NewService(&scope, securityGroupRolesForCluster(scope))
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsclusters,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsclusterroleidentities;awsclusterstaticidentities,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=awsclustercontrolleridentities,verbs=get;list;watch;create

func (r *AWSClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := logger.FromContext(ctx)

	// Fetch the AWSCluster instance
	awsCluster := &infrav1.AWSCluster{}
	err := r.Get(ctx, req.NamespacedName, awsCluster)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// CNI related security groups gets deleted from the AWSClusters created prior to networkSpec.cni defaulting (5.5) after upgrading controllers.
	// https://github.com/kubernetes-sigs/cluster-api-provider-aws/issues/2084
	// TODO: Remove this after v1alpha4
	// The defaulting must happen before `NewClusterScope` is called since otherwise we keep detecting
	// differences that result in patch operations.
	awsCluster.Default()

	// Fetch the Cluster.
	cluster, err := util.GetOwnerCluster(ctx, r.Client, awsCluster.ObjectMeta)
	if err != nil {
		return reconcile.Result{}, err
	}

	if cluster == nil {
		log.Info("Cluster Controller has not yet set OwnerRef")
		return reconcile.Result{}, nil
	}

	log = log.WithValues("cluster", klog.KObj(cluster))

	if isPaused, conditionChanged, err := paused.EnsurePausedCondition(ctx, r.Client, cluster, awsCluster); err != nil || isPaused || conditionChanged {
		return ctrl.Result{}, err
	}

	// Create the scope.
	clusterScope, err := scope.NewClusterScope(scope.ClusterScopeParams{
		Client:                       r.Client,
		Logger:                       log,
		Cluster:                      cluster,
		AWSCluster:                   awsCluster,
		ControllerName:               "awscluster",
		Endpoints:                    r.Endpoints,
		TagUnmanagedNetworkResources: r.TagUnmanagedNetworkResources,
		MaxWaitActiveUpdateDelete:    r.MaxWaitActiveUpdateDelete,
	})
	if err != nil {
		return reconcile.Result{}, errors.Errorf("failed to create scope: %+v", err)
	}

	// Always close the scope when exiting this function so we can persist any AWSCluster changes.
	defer func() {
		if err := clusterScope.Close(); err != nil && reterr == nil {
			reterr = err
		}
	}()

	// Handle deleted clusters
	if !awsCluster.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, clusterScope)
	}

	// Handle non-deleted clusters
	return r.reconcileNormal(ctx, clusterScope)
}

func (r *AWSClusterReconciler) reconcileDelete(ctx context.Context, clusterScope *scope.ClusterScope) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(clusterScope.AWSCluster, infrav1.ClusterFinalizer) {
		clusterScope.Info("No finalizer on AWSCluster, skipping deletion reconciliation")
		return reconcile.Result{}, nil
	}

	clusterScope.Info("Reconciling AWSCluster delete")

	numDependencies, err := r.dependencyCount(ctx, clusterScope)
	if err != nil {
		clusterScope.Error(err, "error getting AWSCluster dependencies")
		return reconcile.Result{}, err
	}

	if numDependencies > 0 {
		clusterScope.Info("AWSCluster cluster still has dependencies - requeue needed", "dependencyCount", numDependencies)
		return reconcile.Result{RequeueAfter: deleteRequeueAfter}, nil
	}

	clusterScope.Info("AWSCluster has no dependent CAPI resources, proceeding with its deletion")

	ec2svc := r.getEC2Service(clusterScope)
	elbsvc := r.getELBService(clusterScope)
	networkSvc := r.getNetworkService(*clusterScope)
	sgService := r.getSecurityGroupService(*clusterScope)
	s3Service := s3.NewService(clusterScope)

	if feature.Gates.Enabled(feature.EventBridgeInstanceState) {
		instancestateSvc := instancestate.NewService(clusterScope)
		if err := instancestateSvc.DeleteEC2Events(ctx); err != nil {
			// Not deleting the events isn't critical to cluster deletion
			clusterScope.Error(err, "non-fatal: failed to delete EventBridge notifications")
		}
	}

	// In this context we try to delete all the resources that we know about,
	// and run the garbage collector to delete any resources that were tagged, if enabled.
	//
	// The reason the errors are collected and not returned immediately is that we want to
	// try to delete as many resources as possible, and then return the errors.
	// Resources like security groups, or load balancers can depende on each other, especially
	// when external controllers might be using them.
	allErrs := []error{}

	if err := s3Service.DeleteBucket(ctx); err != nil {
		allErrs = append(allErrs, errors.Wrapf(err, "error deleting S3 Bucket"))
	}

	if err := elbsvc.DeleteLoadbalancers(ctx); err != nil {
		allErrs = append(allErrs, errors.Wrapf(err, "error deleting load balancers"))
	}

	if err := ec2svc.DeleteBastion(); err != nil {
		allErrs = append(allErrs, errors.Wrapf(err, "error deleting bastion"))
	}

	if err := sgService.DeleteSecurityGroups(); err != nil {
		allErrs = append(allErrs, errors.Wrap(err, "error deleting security groups"))
	}

	if r.ExternalResourceGC {
		gcSvc := gc.NewService(clusterScope, gc.WithGCStrategy(r.AlternativeGCStrategy))
		if gcErr := gcSvc.ReconcileDelete(ctx); gcErr != nil {
			allErrs = append(allErrs, fmt.Errorf("failed delete reconcile for gc service: %w", gcErr))
		}
	}

	if err := networkSvc.DeleteNetwork(); err != nil {
		allErrs = append(allErrs, errors.Wrap(err, "error deleting network"))
	}

	if len(allErrs) > 0 {
		return reconcile.Result{}, kerrors.NewAggregate(allErrs)
	}

	// Cluster is deleted so remove the finalizer.
	controllerutil.RemoveFinalizer(clusterScope.AWSCluster, infrav1.ClusterFinalizer)
	return reconcile.Result{}, nil
}

func (r *AWSClusterReconciler) reconcileLoadBalancer(ctx context.Context, clusterScope *scope.ClusterScope, awsCluster *infrav1.AWSCluster) (*time.Duration, error) {
	retryAfterDuration := 15 * time.Second
	if clusterScope.AWSCluster.Spec.ControlPlaneLoadBalancer.LoadBalancerType == infrav1.LoadBalancerTypeDisabled {
		clusterScope.Debug("load balancer reconciliation shifted to external provider, checking external endpoint")

		return r.checkForExternalControlPlaneLoadBalancer(clusterScope, awsCluster), nil
	}

	elbService := r.getELBService(clusterScope)

	if err := elbService.ReconcileLoadbalancers(ctx); err != nil {
		clusterScope.Error(err, "failed to reconcile load balancer")
		conditions.MarkFalse(awsCluster, infrav1.LoadBalancerReadyCondition, infrav1.LoadBalancerFailedReason, infrautilconditions.ErrorConditionAfterInit(clusterScope.ClusterObj()), "%s", err.Error())
		return nil, err
	}

	if awsCluster.Status.Network.APIServerELB.DNSName == "" {
		conditions.MarkFalse(awsCluster, infrav1.LoadBalancerReadyCondition, infrav1.WaitForDNSNameReason, clusterv1.ConditionSeverityInfo, "")
		clusterScope.Info("Waiting on API server ELB DNS name")
		return &retryAfterDuration, nil
	}

	conditions.MarkTrue(awsCluster, infrav1.LoadBalancerReadyCondition)

	awsCluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{
		Host: awsCluster.Status.Network.APIServerELB.DNSName,
		Port: clusterScope.APIServerPort(),
	}

	return nil, nil
}

func (r *AWSClusterReconciler) reconcileNormal(ctx context.Context, clusterScope *scope.ClusterScope) (reconcile.Result, error) {
	clusterScope.Info("Reconciling AWSCluster")

	awsCluster := clusterScope.AWSCluster

	// If the AWSCluster doesn't have our finalizer, add it.
	if controllerutil.AddFinalizer(awsCluster, infrav1.ClusterFinalizer) {
		// Register the finalizer immediately to avoid orphaning AWS resources on delete
		if err := clusterScope.PatchObject(); err != nil {
			return reconcile.Result{}, err
		}
	}

	ec2Service := r.getEC2Service(clusterScope)
	networkSvc := r.getNetworkService(*clusterScope)
	sgService := r.getSecurityGroupService(*clusterScope)
	s3Service := s3.NewService(clusterScope)

	if err := networkSvc.ReconcileNetwork(); err != nil {
		clusterScope.Error(err, "failed to reconcile network")
		return reconcile.Result{}, err
	}

	if err := sgService.ReconcileSecurityGroups(); err != nil {
		clusterScope.Error(err, "failed to reconcile security groups")
		conditions.MarkFalse(awsCluster, infrav1.ClusterSecurityGroupsReadyCondition, infrav1.ClusterSecurityGroupReconciliationFailedReason, infrautilconditions.ErrorConditionAfterInit(clusterScope.ClusterObj()), "%s", err.Error())
		return reconcile.Result{}, err
	}

	if err := ec2Service.ReconcileBastion(); err != nil {
		conditions.MarkFalse(awsCluster, infrav1.BastionHostReadyCondition, infrav1.BastionHostFailedReason, infrautilconditions.ErrorConditionAfterInit(clusterScope.ClusterObj()), "%s", err.Error())
		clusterScope.Error(err, "failed to reconcile bastion host")
		return reconcile.Result{}, err
	}

	if feature.Gates.Enabled(feature.EventBridgeInstanceState) {
		instancestateSvc := instancestate.NewService(clusterScope)
		if err := instancestateSvc.ReconcileEC2Events(ctx); err != nil {
			// non fatal error, so we continue
			clusterScope.Error(err, "non-fatal: failed to set up EventBridge")
		}
	}

	if requeueAfter, err := r.reconcileLoadBalancer(ctx, clusterScope, awsCluster); err != nil {
		return reconcile.Result{}, err
	} else if requeueAfter != nil {
		return reconcile.Result{RequeueAfter: *requeueAfter}, err
	}

	if err := s3Service.ReconcileBucket(ctx); err != nil {
		conditions.MarkFalse(awsCluster, infrav1.S3BucketReadyCondition, infrav1.S3BucketFailedReason, clusterv1.ConditionSeverityError, "%s", err.Error())
		return reconcile.Result{}, errors.Wrapf(err, "failed to reconcile S3 Bucket for AWSCluster %s/%s", awsCluster.Namespace, awsCluster.Name)
	}
	conditions.MarkTrue(awsCluster, infrav1.S3BucketReadyCondition)

	for _, subnet := range clusterScope.Subnets().FilterPrivate() {
		found := false
		for _, az := range awsCluster.Status.Network.APIServerELB.AvailabilityZones {
			if az == subnet.AvailabilityZone {
				found = true
				break
			}
		}

		clusterScope.SetFailureDomain(subnet.AvailabilityZone, clusterv1.FailureDomainSpec{
			ControlPlane: found,
		})
	}

	awsCluster.Status.Ready = true
	return reconcile.Result{}, nil
}

func (r *AWSClusterReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	log := logger.FromContext(ctx)
	controller, err := ctrl.NewControllerManagedBy(mgr).
		WithOptions(options).
		For(&infrav1.AWSCluster{}).
		WithEventFilter(predicates.ResourceHasFilterLabel(mgr.GetScheme(), log.GetLogger(), r.WatchFilterValue)).
		WithEventFilter(predicates.ResourceIsNotExternallyManaged(mgr.GetScheme(), log.GetLogger())).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "error creating controller")
	}

	return controller.Watch(
		source.Kind[client.Object](mgr.GetCache(), &clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.requeueAWSClusterForUnpausedCluster(ctx, log)),
			predicates.ClusterUnpaused(mgr.GetScheme(), log.GetLogger()),
		))
}

func (r *AWSClusterReconciler) requeueAWSClusterForUnpausedCluster(_ context.Context, log logger.Wrapper) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []ctrl.Request {
		c, ok := o.(*clusterv1.Cluster)
		if !ok {
			klog.Errorf("Expected a Cluster but got a %T", o)
		}

		log := log.WithValues("objectMapper", "clusterToAWSCluster", "cluster", klog.KRef(c.Namespace, c.Name))

		// Don't handle deleted clusters
		if !c.ObjectMeta.DeletionTimestamp.IsZero() {
			log.Trace("Cluster has a deletion timestamp, skipping mapping.")
			return nil
		}

		// Make sure the ref is set
		if c.Spec.InfrastructureRef == nil {
			log.Trace("Cluster does not have an InfrastructureRef, skipping mapping.")
			return nil
		}

		if c.Spec.InfrastructureRef.GroupVersionKind().Kind != "AWSCluster" {
			log.Trace("Cluster has an InfrastructureRef for a different type, skipping mapping.")
			return nil
		}

		awsCluster := &infrav1.AWSCluster{}
		key := types.NamespacedName{Namespace: c.Spec.InfrastructureRef.Namespace, Name: c.Spec.InfrastructureRef.Name}

		if err := r.Get(ctx, key, awsCluster); err != nil {
			log.Error(err, "Failed to get AWS cluster")
			return nil
		}

		if capiannotations.IsExternallyManaged(awsCluster) {
			log.Trace("AWSCluster is externally managed, skipping mapping.")
			return nil
		}

		log.Trace("Adding request.", "awsCluster", c.Spec.InfrastructureRef.Name)
		return []ctrl.Request{
			{
				NamespacedName: client.ObjectKey{Namespace: c.Namespace, Name: c.Spec.InfrastructureRef.Name},
			},
		}
	}
}

func (r *AWSClusterReconciler) checkForExternalControlPlaneLoadBalancer(clusterScope *scope.ClusterScope, awsCluster *infrav1.AWSCluster) *time.Duration {
	requeueAfterPeriod := 15 * time.Second

	switch {
	case len(awsCluster.Spec.ControlPlaneEndpoint.Host) == 0 && awsCluster.Spec.ControlPlaneEndpoint.Port == 0:
		clusterScope.Info("AWSCluster control plane endpoint is still non-populated")
		conditions.MarkFalse(awsCluster, infrav1.LoadBalancerReadyCondition, infrav1.WaitForExternalControlPlaneEndpointReason, clusterv1.ConditionSeverityInfo, "")

		return &requeueAfterPeriod
	case len(awsCluster.Spec.ControlPlaneEndpoint.Host) == 0:
		clusterScope.Info("AWSCluster control plane endpoint host is still non-populated")
		conditions.MarkFalse(awsCluster, infrav1.LoadBalancerReadyCondition, infrav1.WaitForExternalControlPlaneEndpointReason, clusterv1.ConditionSeverityInfo, "")

		return &requeueAfterPeriod
	case awsCluster.Spec.ControlPlaneEndpoint.Port == 0:
		clusterScope.Info("AWSCluster control plane endpoint port is still non-populated")
		conditions.MarkFalse(awsCluster, infrav1.LoadBalancerReadyCondition, infrav1.WaitForExternalControlPlaneEndpointReason, clusterv1.ConditionSeverityInfo, "")

		return &requeueAfterPeriod
	default:
		conditions.MarkTrue(awsCluster, infrav1.LoadBalancerReadyCondition)

		return nil
	}
}

func (r *AWSClusterReconciler) dependencyCount(ctx context.Context, clusterScope *scope.ClusterScope) (int, error) {
	clusterName := clusterScope.Name()
	namespace := clusterScope.Namespace()

	clusterScope.Info("Looking for AWSCluster dependencies")

	listOptions := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingLabels(map[string]string{clusterv1.ClusterNameLabel: clusterName}),
	}

	machines := &infrav1.AWSMachineList{}
	if err := r.Client.List(ctx, machines, listOptions...); err != nil {
		return 0, fmt.Errorf("failed to list machines for cluster %s/%s: %w", namespace, clusterName, err)
	}

	clusterScope.Debug("Found dependent machines", "count", len(machines.Items))

	return len(machines.Items), nil
}
