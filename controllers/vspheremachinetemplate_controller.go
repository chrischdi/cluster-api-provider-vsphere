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

	vmoprv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vmwarev1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/vmware/v1beta1"
	capvcontext "sigs.k8s.io/cluster-api-provider-vsphere/pkg/context"
)

// +kubebuilder:rbac:groups=vmware.infrastructure.cluster.x-k8s.io,resources=vspheremachinetemplates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vmware.infrastructure.cluster.x-k8s.io,resources=vspheremachinetemplates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachineclasses,verbs=get;list;watch

// AddMachineTemplateControllerToManager adds the machine template controller to the provided
// manager.
func AddMachineTemplateControllerToManager(ctx context.Context, controllerManagerContext *capvcontext.ControllerManagerContext, mgr manager.Manager, supervisorBased bool, options controller.Options) error {
	r := &machineTemplateReconciler{
		Client:          controllerManagerContext.Client,
		Recorder:        mgr.GetEventRecorderFor("vspheremachinetemplate-controller"),
		supervisorBased: supervisorBased,
	}

	if supervisorBased {
		return ctrl.NewControllerManagedBy(mgr).
			// Watch the controlled, infrastructure resource.
			For(&vmwarev1.VSphereMachineTemplate{}).
			WithOptions(options).
			// Watch the CAPI resource that owns this infrastructure resource.
			Watches(
				&vmoprv1.VirtualMachineClass{},
				handler.EnqueueRequestsFromMapFunc(r.enqueueVirtualMachineClassToMachineTemplateRequests),
			).
			WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), controllerManagerContext.WatchFilterValue)).
			Complete(r)
	}

	return nil
}

type machineTemplateReconciler struct {
	Client          client.Client
	Recorder        record.EventRecorder
	supervisorBased bool
}

// Reconcile ensures the back-end state reflects the Kubernetes resource state intent.
func (r *machineTemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch VSphereMachineTemplate object
	machineTemplate := &vmwarev1.VSphereMachineTemplate{}
	if err := r.Client.Get(ctx, req.NamespacedName, machineTemplate); err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	log = log.WithValues("MachineTemplate", klog.KObj(machineTemplate))
	ctx = ctrl.LoggerInto(ctx, log)

	// Fetch the VirtualMachineClass
	vmClass := &vmoprv1.VirtualMachineClass{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: machineTemplate.Spec.Template.Spec.ClassName}, vmClass); err != nil {
		return reconcile.Result{}, err
	}

	patchHelper, err := patch.NewHelper(machineTemplate, r.Client)
	if err != nil {
		return reconcile.Result{}, err
	}

	machineTemplate.Status.Capacity[vmwarev1.VSphereResourceCPU] = *resource.NewQuantity(vmClass.Spec.Hardware.Cpus, resource.DecimalSI)
	machineTemplate.Status.Capacity[vmwarev1.VSphereResourceMemory] = vmClass.Spec.Hardware.Memory

	return reconcile.Result{}, patchHelper.Patch(ctx, machineTemplate)
}

// enqueueClusterToMachineRequests returns a list of VSphereMachine reconcile requests
// belonging to the cluster.
func (r *machineTemplateReconciler) enqueueVirtualMachineClassToMachineTemplateRequests(ctx context.Context, a client.Object) []reconcile.Request {
	requests := []reconcile.Request{}

	machineTemplates := &vmwarev1.VSphereMachineTemplateList{}
	if err := r.Client.List(ctx, machineTemplates, client.InNamespace(a.GetNamespace())); err != nil {
		return nil
	}

	for _, machineTemplate := range machineTemplates.Items {
		if machineTemplate.Spec.Template.Spec.ClassName != a.GetName() {
			continue
		}

		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKey{Namespace: machineTemplate.Namespace, Name: machineTemplate.Name},
		})
	}

	return requests
}
