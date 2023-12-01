/*
Copyright 2023 The Kubernetes Authors.

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
	"strconv"

	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	vcsimv1 "sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/api/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/server/capi/cloud"
	"sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/server/capi/server"
)

type FakeAPIServerEndpointReconciler struct {
	Client client.Client

	CloudManager cloud.Manager
	APIServerMux *server.WorkloadClustersMux
	PodIp        string

	// WatchFilterValue is the label value used to filter events prior to reconciliation.
	WatchFilterValue string
}

// +kubebuilder:rbac:groups=vcsim.infrastructure.cluster.x-k8s.io,resources=fakeapiserverendpoints,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=vcsim.infrastructure.cluster.x-k8s.io,resources=fakeapiserverendpoints/status,verbs=get;update;patch

func (r *FakeAPIServerEndpointReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the FakeAPIServerEndpoint instance
	fakeAPIServerEndpoint := &vcsimv1.FakeAPIServerEndpoint{}
	if err := r.Client.Get(ctx, req.NamespacedName, fakeAPIServerEndpoint); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(fakeAPIServerEndpoint, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always attempt to Patch the FakeAPIServerEndpoint object and status after each reconciliation.
	defer func() {
		if err := patchHelper.Patch(ctx, fakeAPIServerEndpoint); err != nil {
			log.Error(err, "failed to patch FakeAPIServerEndpoint")
			if reterr == nil {
				reterr = err
			}
		}
	}()

	// Handle deleted machines
	if !fakeAPIServerEndpoint.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, fakeAPIServerEndpoint)
	}

	// Add finalizer first if not set to avoid the race condition between init and delete.
	// Note: Finalizers in general can only be added when the deletionTimestamp is not set.
	if !controllerutil.ContainsFinalizer(fakeAPIServerEndpoint, vcsimv1.FakeAPIServerEndpointFinalizer) {
		controllerutil.AddFinalizer(fakeAPIServerEndpoint, vcsimv1.FakeAPIServerEndpointFinalizer)
		return ctrl.Result{}, nil
	}

	// Handle non-deleted machines
	return r.reconcileNormal(ctx, fakeAPIServerEndpoint)
}

func (r *FakeAPIServerEndpointReconciler) reconcileNormal(ctx context.Context, fakeAPIServerEndpoint *vcsimv1.FakeAPIServerEndpoint) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info("Reconciling VCSim ControlPlaneEndpoint")

	resourceGroup := klog.KObj(fakeAPIServerEndpoint).String() // TODO: this should match the name of the cluster, make this more explicit

	// Initialize a listener for the workload cluster.
	// IMPORTANT: The fact that both the listener and the resourceGroup for a workload cluster have
	// the same name is used as assumptions in other part of the implementation.
	listener, err := r.APIServerMux.InitWorkloadClusterListener(resourceGroup)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to init the listener for the control plane endpoint")
	}

	// Create a resource group for all the resources belonging the workload cluster.
	// NOTE: We are storing in this resource group all the Kubernetes resources that are expected to exist on the workload cluster (e.g Nodes).
	r.CloudManager.AddResourceGroup(resourceGroup)

	fakeAPIServerEndpoint.Status.Host = r.PodIp // NOTE: we are replacing the listener ip with the pod ip so it will be accessible from other pods as well
	fakeAPIServerEndpoint.Status.Port = listener.Port()

	fakeAPIServerEndpoint.Status.EnvSubst = vcsimv1.EnvVars{
		Clusters: []vcsimv1.ClusterEnvVars{
			{
				Name: fakeAPIServerEndpoint.Name,
				Variables: map[string]string{
					"CONTROL_PLANE_ENDPOINT_IP":   fakeAPIServerEndpoint.Status.Host,
					"CONTROL_PLANE_ENDPOINT_PORT": strconv.Itoa(fakeAPIServerEndpoint.Status.Port),
				},
			},
		},
	}

	return ctrl.Result{}, nil
}

func (r *FakeAPIServerEndpointReconciler) reconcileDelete(ctx context.Context, fakeAPIServerEndpoint *vcsimv1.FakeAPIServerEndpoint) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info("Reconciling delete VCSim ControlPlaneEndpoint")

	resourceGroup := klog.KObj(fakeAPIServerEndpoint).String() // TODO: this should match the name of the cluster, make this more explicit

	// Delete the listener for the workload cluster;
	if err := r.APIServerMux.DeleteWorkloadClusterListener(resourceGroup); err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to delete the listener for the control plane endpoint")
	}

	// Delete the resource group hosting all the cloud resources belonging the workload cluster;
	r.CloudManager.DeleteResourceGroup(resourceGroup)

	controllerutil.RemoveFinalizer(fakeAPIServerEndpoint, vcsimv1.FakeAPIServerEndpointFinalizer)

	return ctrl.Result{}, nil
}

// SetupWithManager will add watches for this controller.
func (r *FakeAPIServerEndpointReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&vcsimv1.FakeAPIServerEndpoint{}).
		WithOptions(options).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue)).
		Complete(r)

	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}
	return nil
}
