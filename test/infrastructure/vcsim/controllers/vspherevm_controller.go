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
	"crypto/rsa"
	"fmt"
	"math/rand"
	"path"
	"time"

	"github.com/pkg/errors"
	"github.com/vmware/govmomi/vim25/types"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	capiutil "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/certs"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	"sigs.k8s.io/cluster-api/util/secret"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/identity"
	govmominet "sigs.k8s.io/cluster-api-provider-vsphere/pkg/services/govmomi/net"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/util"
	"sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/server/capi/cloud"
	cclient "sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/server/capi/cloud/runtime/client"
	"sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/server/capi/server"
)

const (
	// VMFinalizer allows this reconciler to cleanup resources before removing the
	// VSphereVM from the API Server.
	VMFinalizer = "vcsim.vspherevm.infrastructure.cluster.x-k8s.io"
)

const (
	// NodeProvisionedCondition documents the status of the provisioning of the node hosted on the vSphereMachine.
	NodeProvisionedCondition clusterv1.ConditionType = "NodeProvisioned"

	// NodeWaitingForInfrastructureReadyReason (Severity=Info) documents a vSphereMachine waiting for the VM to report infrastructure ready.
	NodeWaitingForInfrastructureReadyReason = "WaitingForInfrastructureReady"
)

const (
	// EtcdProvisionedCondition documents the status of the provisioning of the etcd member hosted on the vSphereMachine.
	EtcdProvisionedCondition clusterv1.ConditionType = "EtcdProvisioned"
)

const (
	// APIServerProvisionedCondition documents the status of the provisioning of the APIServer instance hosted on the vSphereMachine.
	APIServerProvisionedCondition clusterv1.ConditionType = "APIServerProvisioned"
)

// defines annotations to be applied to in memory etcd pods in order to track etcd cluster
// info belonging to the etcd member each pod represent.
const (
	// EtcdClusterIDAnnotationName defines the name of the annotation applied to in memory etcd
	// pods to track the cluster ID of the etcd member each pod represent.
	EtcdClusterIDAnnotationName = "etcd.inmemory.infrastructure.cluster.x-k8s.io/cluster-id"

	// EtcdMemberIDAnnotationName defines the name of the annotation applied to in memory etcd
	// pods to track the member ID of the etcd member each pod represent.
	EtcdMemberIDAnnotationName = "etcd.inmemory.infrastructure.cluster.x-k8s.io/member-id"

	// EtcdLeaderFromAnnotationName defines the name of the annotation applied to in memory etcd
	// pods to track leadership status of the etcd member each pod represent.
	// Note: We are tracking the time from an etcd member is leader; if more than one pod has this
	// annotation, the last etcd member that became leader is the current leader.
	// By using this mechanism leadership can be forwarded to another pod with an atomic operation
	// (add/update of the annotation to the pod/etcd member we are forwarding leadership to).
	EtcdLeaderFromAnnotationName = "etcd.inmemory.infrastructure.cluster.x-k8s.io/leader-from"

	// EtcdMemberRemoved is added to etcd pods which have been removed from the etcd cluster.
	EtcdMemberRemoved = "etcd.inmemory.infrastructure.cluster.x-k8s.io/member-removed"
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
	vsphereCluster := &infrav1.VSphereCluster{}
	if err := r.Client.Get(ctx, key, vsphereCluster); err != nil {
		log.Info("VSphereCluster can't be retrieved")
		return ctrl.Result{}, err
	}
	log = log.WithValues("VSphereCluster", klog.KObj(vsphereCluster))

	ctx = ctrl.LoggerInto(ctx, log)

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()

	// Check if there is a mirrorVSphereVM in the resource group.
	// The mirrorVSphereVM is a copy of the vsphere VM that exist in memory only with
	// scope of keeping track of provisioning process of the fake node, etcd, api server, etc.
	// (the process managed by this controller).
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()
	mirrorVSphereVM := &infrav1.VSphereVM{}
	if err := cloudClient.Get(ctx, client.ObjectKeyFromObject(vSphereVM), mirrorVSphereVM); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrap(err, "failed to get mirror VSphereVM")
		}

		mirrorVSphereVM = &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Name:      vSphereVM.Name,
				Namespace: vSphereVM.Namespace,
			},
		}
		if err := cloudClient.Create(ctx, mirrorVSphereVM); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to create mirror VSphereVM")
		}
	}

	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(vSphereVM, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always attempt to Patch the VSphereVM + mirrorVSphereVM object and status after each reconciliation.
	defer func() {
		// NOTE: Patch on VSphereVM will only add/remove a finalizer.
		if err := patchHelper.Patch(ctx, vSphereVM); err != nil {
			log.Error(err, "failed to patch VSphereVM")
			if reterr == nil {
				reterr = err
			}
		}

		// NOTE: Patch on VSphereVM will only track of provisioning process of the fake node, etcd, api server, etc.
		if err := cloudClient.Update(ctx, mirrorVSphereVM); err != nil {
			log.Error(err, "failed to patch mirror VSphereVM")
			if reterr == nil {
				reterr = err
			}
		}
	}()

	// Handle deleted machines
	if !vSphereMachine.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, cluster, vsphereCluster, machine, vSphereMachine, vSphereVM, mirrorVSphereVM)
	}

	// Add finalizer first if not set to avoid the race condition between init and delete.
	// Note: Finalizers in general can only be added when the deletionTimestamp is not set.
	if !controllerutil.ContainsFinalizer(vSphereVM, VMFinalizer) {
		controllerutil.AddFinalizer(vSphereVM, VMFinalizer)
		return ctrl.Result{}, nil
	}

	// Handle non-deleted machines
	return r.reconcileNormal(ctx, cluster, vsphereCluster, machine, vSphereMachine, vSphereVM, mirrorVSphereVM)
}

func (r *VSphereVMReconciler) reconcileNormal(ctx context.Context, cluster *clusterv1.Cluster, vsphereCluster *infrav1.VSphereCluster, machine *clusterv1.Machine, _ *infrav1.VSphereMachine, vSphereVM, mirrorVSphereVM *infrav1.VSphereVM) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// If the VM is stuck provisioning waiting for IP (because there is no DHCP service in vcsim), the assign a fake IP.
	if !vSphereVM.Status.Ready && conditions.IsFalse(vSphereVM, infrav1.VMProvisionedCondition) && conditions.GetReason(vSphereVM, infrav1.VMProvisionedCondition) == infrav1.WaitingForIPAllocationReason {
		authSession, err := r.retrieveVcenterSession(ctx, vsphereCluster, vSphereVM)
		if err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to get vcenter session")
		}

		vm, err := authSession.Finder.VirtualMachine(ctx, path.Join(vSphereVM.Spec.Folder, vSphereVM.Name))
		if err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to find vm")
		}

		// Check if the VM already has network status (but it is not yet surfaced in conditions)
		netStatus, err := govmominet.GetNetworkStatus(ctx, authSession.Client.Client, vm.Reference())
		if err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to get vm network status")
		}
		ipAddrs := []string{}
		for _, s := range netStatus {
			ipAddrs = append(ipAddrs, s.IPAddrs...)
		}
		if len(ipAddrs) > 0 {
			log.Info("VM already has ips, waiting for CAPV's VSphereVM controller to update conditions", "ipAddrs", ipAddrs)
			return reconcile.Result{RequeueAfter: 5 * time.Second}, nil // Wait for CAPV's VSphereVM controller to detect the ip address and update conditions
		}

		task, err := vm.PowerOff(ctx)
		if err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to PowerOff vm")
		}
		if err = task.Wait(ctx); err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to PowerOff vm task to complete")
		}

		// Add a fake ip address.
		spec := types.CustomizationSpec{
			NicSettingMap: []types.CustomizationAdapterMapping{
				{
					Adapter: types.CustomizationIPSettings{
						Ip: &types.CustomizationFixedIp{
							IpAddress: "192.168.1.100",
						},
						SubnetMask:    "255.255.255.0",
						Gateway:       []string{"192.168.1.1"},
						DnsServerList: []string{"192.168.1.1"},
						DnsDomain:     "ad.domain",
					},
				},
			},
			Identity: &types.CustomizationLinuxPrep{
				HostName: &types.CustomizationFixedName{
					Name: "hostname",
				},
				Domain:     "ad.domain",
				TimeZone:   "Etc/UTC",
				HwClockUTC: types.NewBool(true),
			},
			GlobalIPSettings: types.CustomizationGlobalIPSettings{
				DnsSuffixList: []string{"ad.domain"},
				DnsServerList: []string{"192.168.1.1"},
			},
		}

		task, err = vm.Customize(ctx, spec)
		if err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to Customize vm")
		}
		if err = task.Wait(ctx); err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to wait for Customize vm task to complete")
		}

		task, err = vm.PowerOn(ctx)
		if err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to PowerOn vm")
		}
		if err = task.Wait(ctx); err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to PowerOn vm task to complete")
		}

		ip, err := vm.WaitForIP(ctx)
		if err != nil {
			return reconcile.Result{}, errors.Wrapf(err, "failed to WaitForIP")
		}
		log.Info("VM gets IP", "ip", ip)

		return reconcile.Result{RequeueAfter: 5 * time.Second}, nil // Wait for CAPV's VSphereVM controller to detect the ip address
	}

	// Check if the infrastructure is ready and the Bios UUID to be set (required for computing the Provide ID), otherwise return and wait for the vsphereVM object to be updated
	if !vSphereVM.Status.Ready || vSphereVM.Spec.BiosUUID == "" {
		log.Info("Waiting for vsphereMachine Controller to report infrastructure ready and to set provider ID")
		conditions.MarkFalse(mirrorVSphereVM, NodeProvisionedCondition, NodeWaitingForInfrastructureReadyReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{}, nil
	}

	// Make sure bootstrap data is available and populated.
	// NOTE: we are not using bootstrap data, but we wait for it in order to simulate a real machine provisioning workflow.
	// TODO: watch for machines
	if machine.Spec.Bootstrap.DataSecretName == nil {
		if !util.IsControlPlaneMachine(machine) && !conditions.IsTrue(cluster, clusterv1.ControlPlaneInitializedCondition) {
			log.Info("Waiting for the control plane to be initialized")
			return ctrl.Result{}, nil
		}

		log.Info("Waiting for the Bootstrap provider controller to set bootstrap data")
		return ctrl.Result{}, nil
	}

	// Call the inner reconciliation methods.
	phases := []func(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, vsphereVM, mirrorVSphereVM *infrav1.VSphereVM) (ctrl.Result, error){
		r.reconcileNormalNode,
		r.reconcileNormalETCD,
		r.reconcileNormalAPIServer,
		r.reconcileNormalScheduler,
		r.reconcileNormalControllerManager,
		r.reconcileNormalKubeadmObjects,
		r.reconcileNormalKubeProxy,
		r.reconcileNormalCoredns,
	}

	res := ctrl.Result{}
	errs := make([]error, 0)
	for _, phase := range phases {
		phaseResult, err := phase(ctx, cluster, machine, vSphereVM, mirrorVSphereVM)
		if err != nil {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			continue
		}
		// TODO: consider if we have to use max(RequeueAfter) instead of min(RequeueAfter) to reduce the pressure on
		//  the reconcile queue for vSphereVMs given that we are requeuing just to wait for some period to expire;
		//  the downside of it is that VSphereVMs "in memory" status will change by "big steps" vs incrementally.
		res = capiutil.LowestNonZeroResult(res, phaseResult)
	}
	return res, kerrors.NewAggregate(errs)
}

func (r *VSphereVMReconciler) reconcileNormalNode(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, vSphereVM, mirrorVSphereVM *infrav1.VSphereVM) (ctrl.Result, error) {
	// Wait for the node/kubelet to start up; node/kubelet start happens a configurable time after the VM is provisioned.
	/*
		provisioningDuration := nodeStartupDuration
		provisioningDuration += time.Duration(rand.Float64() * nodeStartupJitter * float64(provisioningDuration)) //nolint:gosec // Intentionally using a weak random number generator here.

		start := conditions.Get(mirrorVSphereVM, infrav1.VMProvisionedCondition).LastTransitionTime // TODO: figure out what to use as a starting time -> when the infra is ready
		now := time.Now()
		if now.Before(start.Add(provisioningDuration)) {
			conditions.MarkFalse(mirrorVSphereVM, NodeProvisionedCondition, NodeWaitingForStartupTimeoutReason, clusterv1.ConditionSeverityInfo, "")
			return ctrl.Result{RequeueAfter: start.Add(provisioningDuration).Sub(now)}, nil
		}
	*/

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	// Create Node
	// TODO: consider if to handle an additional setting adding a delay in between create node and node ready/provider ID being set
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: mirrorVSphereVM.Name,
		},
		Spec: corev1.NodeSpec{
			ProviderID: calculateProviderID(vSphereVM),
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	if util.IsControlPlaneMachine(machine) {
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels["node-role.kubernetes.io/control-plane"] = ""
	}

	if err := cloudClient.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get node")
		}

		// NOTE: for the first control plane machine we might create the node before etcd and API server pod are running
		// but this is not an issue, because it won't be visible to CAPI until the API server start serving requests.
		if err := cloudClient.Create(ctx, node); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create Node")
		}
	}

	conditions.MarkTrue(mirrorVSphereVM, NodeProvisionedCondition)
	return ctrl.Result{}, nil
}

func (r *VSphereVMReconciler) reconcileNormalETCD(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, _, mirrorVSphereVM *infrav1.VSphereVM) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// No-op if the Node is not provisioned yet
	if !conditions.IsTrue(mirrorVSphereVM, NodeProvisionedCondition) {
		return ctrl.Result{}, nil
	}

	// Wait for the etcd pod to start up; etcd pod start happens a configurable time after the Node is provisioned.
	/*
		provisioningDuration := etcdStartupDuration
		provisioningDuration += time.Duration(rand.Float64() * etcdStartupJitter * float64(provisioningDuration)) //nolint:gosec // Intentionally using a weak random number generator here.

		start := conditions.Get(mirrorVSphereVM, NodeProvisionedCondition).LastTransitionTime
		now := time.Now()
		if now.Before(start.Add(provisioningDuration)) {
			conditions.MarkFalse(mirrorVSphereVM, EtcdProvisionedCondition, EtcdWaitingForStartupTimeoutReason, clusterv1.ConditionSeverityInfo, "")
			return ctrl.Result{RequeueAfter: start.Add(provisioningDuration).Sub(now)}, nil
		}
	*/

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	// Create the etcd pod
	// TODO: consider if to handle an additional setting adding a delay in between create pod and pod ready
	etcdMember := fmt.Sprintf("etcd-%s", mirrorVSphereVM.Name)
	etcdPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      etcdMember,
			Labels: map[string]string{
				"component": "etcd",
				"tier":      "control-plane",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: mirrorVSphereVM.Name,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	if err := cloudClient.Get(ctx, client.ObjectKeyFromObject(etcdPod), etcdPod); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get etcd Pod")
		}

		// Gets info about the current etcd cluster, if any.
		info, err := r.getEtcdInfo(ctx, cloudClient)
		if err != nil {
			return ctrl.Result{}, err
		}

		// If this is the first etcd member in the cluster, assign a cluster ID
		if info.clusterID == "" {
			for {
				info.clusterID = fmt.Sprintf("%d", rand.Uint32()) //nolint:gosec // weak random number generator is good enough here
				if info.clusterID != "0" {
					break
				}
			}
		}

		// Computes a unique memberID.
		var memberID string
		for {
			memberID = fmt.Sprintf("%d", rand.Uint32()) //nolint:gosec // weak random number generator is good enough here
			if !info.members.Has(memberID) && memberID != "0" {
				break
			}
		}

		// Annotate the pod with the info about the etcd cluster.
		etcdPod.Annotations = map[string]string{
			EtcdClusterIDAnnotationName: info.clusterID,
			EtcdMemberIDAnnotationName:  memberID,
		}

		// If the etcd cluster is being created it doesn't have a leader yet, so set this member as a leader.
		if info.leaderID == "" {
			etcdPod.Annotations[EtcdLeaderFromAnnotationName] = time.Now().Format(time.RFC3339)
		}

		// NOTE: for the first control plane machine we might create the etcd pod before the API server pod is running
		// but this is not an issue, because it won't be visible to CAPI until the API server start serving requests.
		if err := cloudClient.Create(ctx, etcdPod); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create Pod")
		}
	}

	// If there is not yet an etcd member listener for this machine, add it to the server.
	if !r.APIServerMux.HasEtcdMember(resourceGroup, etcdMember) {
		// Getting the etcd CA
		s, err := secret.Get(ctx, r.Client, client.ObjectKeyFromObject(cluster), secret.EtcdCA)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get etcd CA")
		}
		certData, exists := s.Data[secret.TLSCrtDataName]
		if !exists {
			return ctrl.Result{}, errors.Errorf("invalid etcd CA: missing data for %s", secret.TLSCrtDataName)
		}

		cert, err := certs.DecodeCertPEM(certData)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "invalid etcd CA: invalid %s", secret.TLSCrtDataName)
		}

		keyData, exists := s.Data[secret.TLSKeyDataName]
		if !exists {
			return ctrl.Result{}, errors.Errorf("invalid etcd CA: missing data for %s", secret.TLSKeyDataName)
		}

		key, err := certs.DecodePrivateKeyPEM(keyData)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "invalid etcd CA: invalid %s", secret.TLSKeyDataName)
		}

		if err := r.APIServerMux.AddEtcdMember(resourceGroup, etcdMember, cert, key.(*rsa.PrivateKey)); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to start etcd member")
		}
	}

	conditions.MarkTrue(mirrorVSphereVM, EtcdProvisionedCondition)
	return ctrl.Result{}, nil
}

func (r *VSphereVMReconciler) reconcileNormalAPIServer(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, _, mirrorVSphereVM *infrav1.VSphereVM) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// No-op if the Node is not provisioned yet
	if !conditions.IsTrue(mirrorVSphereVM, NodeProvisionedCondition) {
		return ctrl.Result{}, nil
	}

	// Wait for the API server pod to start up; API server pod start happens a configurable time after the Node is provisioned.
	/*
		provisioningDuration := apiServerStartupDuration
		provisioningDuration += time.Duration(rand.Float64() * apiServerStartupJitter * float64(provisioningDuration)) //nolint:gosec // Intentionally using a weak random number generator here.

		start := conditions.Get(mirrorVSphereVM, NodeProvisionedCondition).LastTransitionTime
		now := time.Now()
		if now.Before(start.Add(provisioningDuration)) {
			conditions.MarkFalse(mirrorVSphereVM, APIServerProvisionedCondition, APIServerWaitingForStartupTimeoutReason, clusterv1.ConditionSeverityInfo, "")
			return ctrl.Result{RequeueAfter: start.Add(provisioningDuration).Sub(now)}, nil
		}
	*/

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	// Create the apiserver pod
	// TODO: consider if to handle an additional setting adding a delay in between create pod and pod ready
	apiServer := fmt.Sprintf("kube-apiserver-%s", mirrorVSphereVM.Name)

	apiServerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      apiServer,
			Labels: map[string]string{
				"component": "kube-apiserver",
				"tier":      "control-plane",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: mirrorVSphereVM.Name,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	if err := cloudClient.Get(ctx, client.ObjectKeyFromObject(apiServerPod), apiServerPod); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get apiServer Pod")
		}

		if err := cloudClient.Create(ctx, apiServerPod); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create apiServer Pod")
		}
	}

	// If there is not yet an API server listener for this machine.
	if !r.APIServerMux.HasAPIServer(resourceGroup, apiServer) {
		// Getting the Kubernetes CA
		s, err := secret.Get(ctx, r.Client, client.ObjectKeyFromObject(cluster), secret.ClusterCA)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get cluster CA")
		}
		certData, exists := s.Data[secret.TLSCrtDataName]
		if !exists {
			return ctrl.Result{}, errors.Errorf("invalid cluster CA: missing data for %s", secret.TLSCrtDataName)
		}

		cert, err := certs.DecodeCertPEM(certData)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "invalid cluster CA: invalid %s", secret.TLSCrtDataName)
		}

		keyData, exists := s.Data[secret.TLSKeyDataName]
		if !exists {
			return ctrl.Result{}, errors.Errorf("invalid cluster CA: missing data for %s", secret.TLSKeyDataName)
		}

		key, err := certs.DecodePrivateKeyPEM(keyData)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "invalid cluster CA: invalid %s", secret.TLSKeyDataName)
		}

		// Adding the APIServer.
		// NOTE: When the first APIServer is added, the workload cluster listener is started.
		if err := r.APIServerMux.AddAPIServer(resourceGroup, apiServer, cert, key.(*rsa.PrivateKey)); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to start API server")
		}
	}

	conditions.MarkTrue(mirrorVSphereVM, APIServerProvisionedCondition)
	return ctrl.Result{}, nil
}

func (r *VSphereVMReconciler) reconcileNormalScheduler(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, _, mirrorVSphereVM *infrav1.VSphereVM) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// NOTE: we are creating the scheduler pod to make KCP happy, but we are not implementing any
	// specific behaviour for this component because they are not relevant for stress tests.
	// As a current approximation, we create the scheduler as soon as the API server is provisioned;
	// also, the scheduler is immediately marked as ready.
	if !conditions.IsTrue(mirrorVSphereVM, APIServerProvisionedCondition) {
		return ctrl.Result{}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	schedulerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      fmt.Sprintf("kube-scheduler-%s", mirrorVSphereVM.Name),
			Labels: map[string]string{
				"component": "kube-scheduler",
				"tier":      "control-plane",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: mirrorVSphereVM.Name,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	if err := cloudClient.Create(ctx, schedulerPod); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to create scheduler Pod")
	}

	return ctrl.Result{}, nil
}

func (r *VSphereVMReconciler) reconcileNormalControllerManager(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, _, mirrorVSphereVM *infrav1.VSphereVM) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// NOTE: we are creating the controller manager pod to make KCP happy, but we are not implementing any
	// specific behaviour for this component because they are not relevant for stress tests.
	// As a current approximation, we create the controller manager as soon as the API server is provisioned;
	// also, the controller manager is immediately marked as ready.
	if !conditions.IsTrue(mirrorVSphereVM, APIServerProvisionedCondition) {
		return ctrl.Result{}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	controllerManagerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      fmt.Sprintf("kube-controller-manager-%s", mirrorVSphereVM.Name),
			Labels: map[string]string{
				"component": "kube-controller-manager",
				"tier":      "control-plane",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: mirrorVSphereVM.Name,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	if err := cloudClient.Create(ctx, controllerManagerPod); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to create controller manager Pod")
	}

	return ctrl.Result{}, nil
}

func (r *VSphereVMReconciler) reconcileNormalKubeadmObjects(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, _, _ *infrav1.VSphereVM) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	// create kubeadm ClusterRole and ClusterRoleBinding enforced by KCP
	// NOTE: we create those objects because this is what kubeadm does, but KCP creates
	// ClusterRole and ClusterRoleBinding if not found.

	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubeadm:get-nodes",
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"get"},
				APIGroups: []string{""},
				Resources: []string{"nodes"},
			},
		},
	}
	if err := cloudClient.Create(ctx, role); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to create kubeadm:get-nodes ClusterRole")
	}

	roleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubeadm:get-nodes",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "kubeadm:get-nodes",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind: rbacv1.GroupKind,
				Name: "system:bootstrappers:kubeadm:default-node-token",
			},
		},
	}
	if err := cloudClient.Create(ctx, roleBinding); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to create kubeadm:get-nodes ClusterRoleBinding")
	}

	// create kubeadm config map
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kubeadm-config",
			Namespace: metav1.NamespaceSystem,
		},
		Data: map[string]string{
			"ClusterConfiguration": "",
		},
	}
	if err := cloudClient.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to create kubeadm-config ConfigMap")
	}

	return ctrl.Result{}, nil
}

func (r *VSphereVMReconciler) reconcileNormalKubeProxy(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, _, _ *infrav1.VSphereVM) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// TODO: Add provisioning time for KubeProxy.

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	// Create the kube-proxy-daemonset
	kubeProxyDaemonSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      "kube-proxy",
			Labels: map[string]string{
				"component": "kube-proxy",
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "kube-proxy",
							Image: fmt.Sprintf("registry.k8s.io/kube-proxy:%s", *machine.Spec.Version),
						},
					},
				},
			},
		},
	}
	if err := cloudClient.Get(ctx, client.ObjectKeyFromObject(kubeProxyDaemonSet), kubeProxyDaemonSet); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get kube-proxy DaemonSet")
		}

		if err := cloudClient.Create(ctx, kubeProxyDaemonSet); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create kube-proxy DaemonSet")
		}
	}
	return ctrl.Result{}, nil
}

func (r *VSphereVMReconciler) reconcileNormalCoredns(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, _, _ *infrav1.VSphereVM) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// TODO: Add provisioning time for CoreDNS.

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	// Create the coredns configMap.
	corednsConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      "coredns",
		},
		Data: map[string]string{
			"Corefile": "ANG",
		},
	}
	if err := cloudClient.Get(ctx, client.ObjectKeyFromObject(corednsConfigMap), corednsConfigMap); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get coreDNS configMap")
		}

		if err := cloudClient.Create(ctx, corednsConfigMap); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create coreDNS configMap")
		}
	}
	// Create the coredns deployment.
	corednsDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      "coredns",
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "coredns",
							Image: "registry.k8s.io/coredns/coredns:v1.10.1",
						},
					},
				},
			},
		},
	}

	if err := cloudClient.Get(ctx, client.ObjectKeyFromObject(corednsDeployment), corednsDeployment); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get coreDNS deployment")
		}

		if err := cloudClient.Create(ctx, corednsDeployment); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create coreDNS deployment")
		}
	}
	return ctrl.Result{}, nil
}

func (r *VSphereVMReconciler) reconcileDelete(ctx context.Context, cluster *clusterv1.Cluster, _ *infrav1.VSphereCluster, machine *clusterv1.Machine, _ *infrav1.VSphereMachine, vSphereVM, mirrorVSphereVM *infrav1.VSphereVM) (ctrl.Result, error) {
	// Call the inner reconciliation methods.
	phases := []func(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, vSphereVM, mirrorVSphereVM *infrav1.VSphereVM) (ctrl.Result, error){
		// TODO: revisit order when we implement behaviour for the deletion workflow
		r.reconcileDeleteNode,
		r.reconcileDeleteETCD,
		r.reconcileDeleteAPIServer,
		r.reconcileDeleteScheduler,
		r.reconcileDeleteControllerManager,
		// Note: We are not deleting kubeadm objects because they exist in K8s, they are not related to a specific machine.
	}

	res := ctrl.Result{}
	errs := make([]error, 0)
	for _, phase := range phases {
		phaseResult, err := phase(ctx, cluster, machine, vSphereVM, mirrorVSphereVM)
		if err != nil {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			continue
		}
		res = capiutil.LowestNonZeroResult(res, phaseResult)
	}
	if res.IsZero() && len(errs) == 0 {
		controllerutil.RemoveFinalizer(vSphereVM, infrav1.VMFinalizer)
	}
	return res, kerrors.NewAggregate(errs)
}

func (r *VSphereVMReconciler) reconcileDeleteNode(ctx context.Context, cluster *clusterv1.Cluster, _ *clusterv1.Machine, _, mirrorVSphereVM *infrav1.VSphereVM) (ctrl.Result, error) {
	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	// Delete Node
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: mirrorVSphereVM.Name,
		},
	}

	// TODO(killianmuldoon): check if we can drop this given that the MachineController is already draining pods and deleting nodes.
	if err := cloudClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to delete Node")
	}

	return ctrl.Result{}, nil
}

func (r *VSphereVMReconciler) reconcileDeleteETCD(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, _, mirrorVSphereVM *infrav1.VSphereVM) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	etcdMember := fmt.Sprintf("etcd-%s", mirrorVSphereVM.Name)
	etcdPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      etcdMember,
		},
	}
	if err := cloudClient.Delete(ctx, etcdPod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to delete etcd Pod")
	}
	if err := r.APIServerMux.DeleteEtcdMember(resourceGroup, etcdMember); err != nil {
		return ctrl.Result{}, err
	}

	// TODO: if all the etcd members are gone, cleanup all the k8s objects from the resource group.
	// note: it is not possible to delete the resource group, because cloud resources should be preserved.
	// given that, in order to implement this it is required to find a way to identify all the k8s resources (might be via gvk);
	// also, deletion must happen suddenly, without respecting finalizers or owner references links.

	return ctrl.Result{}, nil
}

func (r *VSphereVMReconciler) reconcileDeleteAPIServer(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, _, mirrorVSphereVM *infrav1.VSphereVM) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	apiServer := fmt.Sprintf("kube-apiserver-%s", mirrorVSphereVM.Name)
	apiServerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      apiServer,
		},
	}
	if err := cloudClient.Delete(ctx, apiServerPod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to delete apiServer Pod")
	}
	if err := r.APIServerMux.DeleteAPIServer(resourceGroup, apiServer); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *VSphereVMReconciler) reconcileDeleteScheduler(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, _, mirrorVSphereVM *infrav1.VSphereVM) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	schedulerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      fmt.Sprintf("kube-scheduler-%s", mirrorVSphereVM.Name),
		},
	}
	if err := cloudClient.Delete(ctx, schedulerPod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to scheduler Pod")
	}

	return ctrl.Result{}, nil
}

func (r *VSphereVMReconciler) reconcileDeleteControllerManager(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, _, mirrorVSphereVM *infrav1.VSphereVM) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	controllerManagerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      fmt.Sprintf("kube-controller-manager-%s", mirrorVSphereVM.Name),
		},
	}
	if err := cloudClient.Delete(ctx, controllerManagerPod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to controller manager Pod")
	}

	return ctrl.Result{}, nil
}

func (r VSphereVMReconciler) retrieveVcenterSession(ctx context.Context, vsphereCluster *infrav1.VSphereCluster, vsphereVM *infrav1.VSphereVM) (*session.Session, error) {
	if vsphereCluster.Spec.IdentityRef == nil {
		return nil, errors.New("vcsim do not support using credentials provided to the manager")
	}

	creds, err := identity.GetCredentials(ctx, r.Client, vsphereCluster, "capv-system") // TODO: implement support for CAPV deployed in arbitrary ns (TBD if we need this)
	if err != nil {
		return nil, errors.Wrap(err, "failed to retrieve credentials from IdentityRef")
	}

	params := session.NewParams().
		WithServer(vsphereVM.Spec.Server).
		WithDatacenter(vsphereVM.Spec.Datacenter).
		WithUserInfo(creds.Username, creds.Password).
		WithThumbprint(vsphereVM.Spec.Thumbprint).
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

func calculateProviderID(vSphereVM *infrav1.VSphereVM) string {
	return util.ConvertUUIDToProviderID(vSphereVM.Spec.BiosUUID)
}

type etcdInfo struct {
	clusterID string
	leaderID  string
	members   sets.Set[string]
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
