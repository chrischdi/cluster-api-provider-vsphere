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
	"fmt"
	"path"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	capiutil "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/identity"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
	"sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/server/capi/cloud"
	cclient "sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/server/capi/cloud/runtime/client"
	"sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/server/capi/server"
)

type VSphereVMReconciler struct {
	Client            client.Client
	CloudManager      cloud.Manager
	APIServerMux      *server.WorkloadClustersMux
	EnableKeepAlive   bool
	KeepAliveDuration time.Duration

	// WatchFilterValue is the label value used to filter events prior to reconciliation.
	WatchFilterValue string
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=vspherevms,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=vsphereclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=vspheremachines,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=vsphereclusteridentities,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile ensures the back-end state reflects the Kubernetes resource state intent.
func (r *VSphereVMReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the VSphereVM instance
	vSphereVM := &infrav1.VSphereVM{}
	if err := r.Client.Get(ctx, req.NamespacedName, vSphereVM); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Fetch the owner VSphereMachine.
	vSphereMachine, err := util.GetOwnerVSphereMachine(ctx, r.Client, vSphereVM.ObjectMeta)
	// vsphereMachine can be nil in cases where custom mover other than clusterctl
	// moves the resources without ownerreferences set
	// in that case nil vsphereMachine can cause panic and CrashLoopBackOff the pod
	// preventing vspheremachine_controller from setting the ownerref
	if err != nil || vSphereMachine == nil {
		log.Info("Owner VSphereMachine not found, won't reconcile", "key", req.NamespacedName)
		return reconcile.Result{}, nil
	}
	log = log.WithValues("VSphereMachine", klog.KObj(vSphereMachine))

	// Fetch the Machine.
	machine, err := capiutil.GetOwnerMachine(ctx, r.Client, vSphereMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if machine == nil {
		log.Info("Waiting for Machine Controller to set OwnerRef on VSphereMachine")
		return ctrl.Result{}, nil
	}
	log = log.WithValues("Machine", klog.KObj(machine))

	// Fetch the Cluster.
	cluster, err := capiutil.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Info("VSphereMachine owner Machine is missing cluster label or cluster does not exist")
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info(fmt.Sprintf("Please associate this machine with a cluster using the label %s: <name of cluster>", clusterv1.ClusterNameLabel))
		return ctrl.Result{}, nil
	}
	log = log.WithValues("Cluster", klog.KObj(cluster))

	// Return early if the object or Cluster is paused.
	if annotations.IsPaused(cluster, vSphereMachine) {
		log.Info("Reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	// Fetch the VSphereCluster.
	key := client.ObjectKey{
		Namespace: cluster.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	vSphereCluster := &infrav1.VSphereCluster{}
	if err := r.Client.Get(ctx, key, vSphereCluster); err != nil {
		log.Info("VSphereCluster can't be retrieved")
		return ctrl.Result{}, err
	}
	log = log.WithValues("VSphereCluster", klog.KObj(vSphereCluster))

	ctx = ctrl.LoggerInto(ctx, log)

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()

	// Check if there is a conditionsTracker in the resource group.
	// The conditionsTracker is an object stored in memory with the scope of storing conditions used for keeping
	// track of the provisioning process of the fake node, etcd, api server, etc for this specific vSphereVM.
	// (the process managed by this controller).
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()
	conditionsTracker := &infrav1.VSphereVM{} // NOTE: The type of this object doesn't matter as soon as it implements Cluster API's conditions interfaces.
	if err := cloudClient.Get(ctx, client.ObjectKeyFromObject(vSphereVM), conditionsTracker); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrap(err, "failed to get conditionsTracker")
		}

		conditionsTracker = &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vSphereVM.Name,
				Namespace: vSphereVM.Namespace,
			},
		}
		if err := cloudClient.Create(ctx, conditionsTracker); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to create conditionsTracker")
		}
	}

	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(vSphereVM, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always attempt to Patch the VSphereVM + conditionsTracker object and status after each reconciliation.
	defer func() {
		// NOTE: Patch on VSphereVM will only add/remove a finalizer.
		if err := patchHelper.Patch(ctx, vSphereVM); err != nil {
			log.Error(err, "failed to patch VSphereVM")
			if reterr == nil {
				reterr = err
			}
		}

		// NOTE: Patch on conditionsTracker will only track of provisioning process of the fake node, etcd, api server, etc.
		if err := cloudClient.Update(ctx, conditionsTracker); err != nil {
			log.Error(err, "failed to patch conditionsTracker")
			if reterr == nil {
				reterr = err
			}
		}
	}()

	vmR := &vmReconciler{
		Client:            r.Client,
		CloudManager:      r.CloudManager,
		APIServerMux:      r.APIServerMux,
		EnableKeepAlive:   r.EnableKeepAlive,
		KeepAliveDuration: r.KeepAliveDuration,

		// Type specific functions; those functions wraps the differences between legacy and supervisor types,
		// thus allowing to use the same vmReconciler in both scenarios.
		GetVCenterSession: func(ctx context.Context) (*session.Session, error) {
			// Return a connection to the vCenter where the vSphereVM is hosted
			return r.getVCenterSession(ctx, vSphereCluster, vSphereVM)
		},
		IsVMWaitingforIP: func() bool {
			// A vSphereVM is waiting for an IP when not ready VMProvisioned condition is false with reason WaitingForIPAllocation
			return !vSphereVM.Status.Ready && conditions.IsFalse(vSphereVM, infrav1.VMProvisionedCondition) && conditions.GetReason(vSphereVM, infrav1.VMProvisionedCondition) == infrav1.WaitingForIPAllocationReason
		},
		IsVMReady: func() bool {
			// A vSphereVM is ready to provision fake objects hosted on it when both ready and BiosUUID is set (bios id is required when provisioning the node to compute the Provider ID)
			return vSphereVM.Status.Ready && vSphereVM.Spec.BiosUUID != ""
		},
		GetProviderID: func() string {
			// Computes the ProviderID for the node hosted on the vSphereVM
			return util.ConvertUUIDToProviderID(vSphereVM.Spec.BiosUUID)
		},
		GetVMPath: func() string {
			// Return the path where the VM is stored.
			return path.Join(vSphereVM.Spec.Folder, vSphereVM.Name)
		},
	}

	// Handle deleted machines
	if !vSphereMachine.DeletionTimestamp.IsZero() {
		return vmR.reconcileDelete(ctx, cluster, machine, conditionsTracker)
	}

	// Add finalizer first if not set to avoid the race condition between init and delete.
	// Note: Finalizers in general can only be added when the deletionTimestamp is not set.
	if !controllerutil.ContainsFinalizer(vSphereVM, VMFinalizer) {
		controllerutil.AddFinalizer(vSphereVM, VMFinalizer)
		return ctrl.Result{}, nil
	}

	// Handle non-deleted machines
	return vmR.reconcileNormal(ctx, cluster, machine, conditionsTracker)
}

func (r VSphereVMReconciler) getVCenterSession(ctx context.Context, vSphereCluster *infrav1.VSphereCluster, vSphereVM *infrav1.VSphereVM) (*session.Session, error) {
	if vSphereCluster.Spec.IdentityRef == nil {
		return nil, errors.New("vcsim do not support using credentials provided to the manager")
	}

	creds, err := identity.GetCredentials(ctx, r.Client, vSphereCluster, "capv-system") // TODO: implement support for CAPV deployed in arbitrary ns (TBD if we need this)
	if err != nil {
		return nil, errors.Wrap(err, "failed to retrieve credentials from IdentityRef")
	}

	params := session.NewParams().
		WithServer(vSphereVM.Spec.Server).
		WithDatacenter(vSphereVM.Spec.Datacenter).
		WithUserInfo(creds.Username, creds.Password).
		WithThumbprint(vSphereVM.Spec.Thumbprint).
		WithFeatures(session.Feature{
			EnableKeepAlive:   r.EnableKeepAlive,
			KeepAliveDuration: r.KeepAliveDuration,
		})

	return session.GetOrCreate(ctx, params)
}

// SetupWithManager will add watches for this controller.
func (r *VSphereVMReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.VSphereVM{}).
		WithOptions(options).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue)).
		Complete(r)

	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}
	return nil
}

func (r *VSphereVMReconciler) getEtcdInfo(ctx context.Context, cloudClient cclient.Client) (etcdInfo, error) {
	etcdPods := &corev1.PodList{}
	if err := cloudClient.List(ctx, etcdPods,
		client.InNamespace(metav1.NamespaceSystem),
		client.MatchingLabels{
			"component": "etcd",
			"tier":      "control-plane"},
	); err != nil {
		return etcdInfo{}, errors.Wrap(err, "failed to list etcd members")
	}

	if len(etcdPods.Items) == 0 {
		return etcdInfo{}, nil
	}

	info := etcdInfo{
		members: sets.New[string](),
	}
	var leaderFrom time.Time
	for _, pod := range etcdPods.Items {
		if _, ok := pod.Annotations[EtcdMemberRemoved]; ok {
			continue
		}
		if info.clusterID == "" {
			info.clusterID = pod.Annotations[EtcdClusterIDAnnotationName]
		} else if pod.Annotations[EtcdClusterIDAnnotationName] != info.clusterID {
			return etcdInfo{}, errors.New("invalid etcd cluster, members have different cluster ID")
		}
		memberID := pod.Annotations[EtcdMemberIDAnnotationName]
		info.members.Insert(memberID)

		if t, err := time.Parse(time.RFC3339, pod.Annotations[EtcdLeaderFromAnnotationName]); err == nil {
			if t.After(leaderFrom) {
				info.leaderID = memberID
				leaderFrom = t
			}
		}
	}

	if info.leaderID == "" {
		// TODO: consider if and how to automatically recover from this case
		//  note: this can happen also when reading etcd members in the server, might be it is something we have to take case before deletion...
		//  for now it should not be an issue because KCP forward etcd leadership before deletion.
		return etcdInfo{}, errors.New("invalid etcd cluster, no leader found")
	}

	return info, nil
}
