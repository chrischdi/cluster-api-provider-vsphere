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
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"

	_ "github.com/dougm/pretty"
	"github.com/pkg/errors"
	topologyv1 "github.com/vmware-tanzu/vm-operator/external/tanzu-topology/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/vmware/govmomi/govc/cli"
	_ "github.com/vmware/govmomi/govc/vm"
	pbmsimulator "github.com/vmware/govmomi/pbm/simulator"
	"github.com/vmware/govmomi/simulator"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"

	"sigs.k8s.io/cluster-api-provider-vsphere/internal/test/helpers/vcsim"
	vcsimv1 "sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/api/v1alpha1"
)

type VCenterReconciler struct {
	Client         client.Client
	SupervisorMode bool

	vcsimInstances map[string]*vcsim.Simulator
	lock           sync.RWMutex

	// WatchFilterValue is the label value used to filter events prior to reconciliation.
	WatchFilterValue string
}

// +kubebuilder:rbac:groups=vcsim.infrastructure.cluster.x-k8s.io,resources=vcenters,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=vcsim.infrastructure.cluster.x-k8s.io,resources=vcenters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=topology.tanzu.vmware.com,resources=availabilityzones,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create

func (r *VCenterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the VCenter instance
	vCenter := &vcsimv1.VCenter{}
	if err := r.Client.Get(ctx, req.NamespacedName, vCenter); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(vCenter, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always attempt to Patch the VCenter object and status after each reconciliation.
	defer func() {
		if err := patchHelper.Patch(ctx, vCenter); err != nil {
			log.Error(err, "failed to patch VCenter")
			if reterr == nil {
				reterr = err
			}
		}
	}()

	// Handle deleted machines
	if !vCenter.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, vCenter)
	}

	// Add finalizer first if not set to avoid the race condition between init and delete.
	// Note: Finalizers in general can only be added when the deletionTimestamp is not set.
	if !controllerutil.ContainsFinalizer(vCenter, vcsimv1.VCenterFinalizer) {
		controllerutil.AddFinalizer(vCenter, vcsimv1.VCenterFinalizer)
		return ctrl.Result{}, nil
	}

	// Handle non-deleted machines
	return r.reconcileNormal(ctx, vCenter)
}

func (r *VCenterReconciler) reconcileNormal(ctx context.Context, vCenter *vcsimv1.VCenter) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info("Reconciling VCSim Server")

	// TODO: move the code with the lock in a sub func so the lock is shorter
	r.lock.Lock()
	defer r.lock.Unlock()

	if r.vcsimInstances == nil {
		r.vcsimInstances = map[string]*vcsim.Simulator{}
	}

	key := klog.KObj(vCenter).String()
	vcsimInstance, ok := r.vcsimInstances[key]
	if !ok {
		// Define the model for the VCSim instance, starting from simulator.VPX
		// and changing version + all the setting specified in the spec.
		// NOTE: it is necessary to create the model before passing it to the builder
		// in order to register the endpoint for handling request about storage policies.
		model := simulator.VPX()
		model.ServiceContent.About.Version = "7.0.0" // TODO: consider if to change other fields for version 7.0.0 (same inside the if for custom version)
		if vCenter.Spec.Model != nil {
			model.ServiceContent.About.Version = pointer.StringDeref(vCenter.Spec.Model.VSphereVersion, model.ServiceContent.About.Version)
			model.Datacenter = pointer.IntDeref(vCenter.Spec.Model.Datacenter, model.Datacenter)
			model.Cluster = pointer.IntDeref(vCenter.Spec.Model.Cluster, model.Cluster)
			model.ClusterHost = pointer.IntDeref(vCenter.Spec.Model.ClusterHost, model.ClusterHost)
			model.Pool = pointer.IntDeref(vCenter.Spec.Model.Pool, model.Pool)
			model.Datastore = pointer.IntDeref(vCenter.Spec.Model.Datastore, model.Datastore)
		}
		if err := model.Create(); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create VCSim Server model")
		}
		model.Service.RegisterSDK(pbmsimulator.New())

		// Compute the vcsim URL, binding all interfaces interface (so it will be accessible both from outside and via kubectl port-forward);
		// a random port will be used unless we are reconciling a previously existing vCenter after a restart;
		// in case of restart it will try to re-use the port previously assigned, but the internal status of vcsim will be lost.
		// NOTE: re-using the same port might be racy with other vcsimURL being created using a random port,
		// but we consider this risk acceptable for testing purposes.
		host := "0.0.0.0"
		port := "0"
		if vCenter.Status.Host != "" {
			_, port, _ = net.SplitHostPort(vCenter.Status.Host)
		}
		vcsimURL, err := url.Parse(fmt.Sprintf("https://%s", net.JoinHostPort(host, port)))
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to parse VCSim Server url")
		}

		// Start the vcsim instance
		vcsimInstance, err = vcsim.NewBuilder().
			WithModel(model).
			SkipModelCreate().
			WithUrl(vcsimURL).
			Build()

		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create VCSim Server instance")
		}
		r.vcsimInstances[key] = vcsimInstance
		log.Info("Created VCSim Server", "url", vcsimInstance.ServerURL())

		vCenter.Status.Host = vcsimInstance.ServerURL().Host
		vCenter.Status.Username = vcsimInstance.Username()
		vCenter.Status.Password = vcsimInstance.Password()

		// Add a VM template

		// TODO: figure this out better.
		//  we create a template in a datastore, what if many?
		//  we create a template in a datacenter, cluster, but the vm doesn't have the cluster in the path. What if I have many clusters?
		govcURL := fmt.Sprintf("https://%s:%s@%s/sdk", vCenter.Status.Username, vCenter.Status.Password, vCenter.Status.Host)
		datacenters := 1
		if vCenter.Spec.Model != nil {
			datacenters = pointer.IntDeref(vCenter.Spec.Model.Datacenter, model.Datacenter)
		}
		for dc := 0; dc < datacenters; dc++ {
			exit := cli.Run([]string{"vm.create", "-ds=LocalDS_0", fmt.Sprintf("-cluster=DC%d_C0", dc), "-net=VM Network", "-disk=20G", "-on=false", "-k=true", fmt.Sprintf("-u=%s", govcURL), "ubuntu-2204-kube-vX"})
			if exit != 0 {
				return ctrl.Result{}, errors.New("failed to create vm template")
			}

			exit = cli.Run([]string{"vm.markastemplate", "-k=true", fmt.Sprintf("-u=%s", govcURL), fmt.Sprintf("/DC%d/vm/ubuntu-2204-kube-vX", dc)})
			if exit != 0 {
				return ctrl.Result{}, errors.New("failed to mark vm template")
			}
			log.Info("Created VM template", "name", "ubuntu-2204-kube-vX")
		}
	}

	if vCenter.Status.Thumbprint == "" {
		config := &tls.Config{InsecureSkipVerify: true}
		addr := vCenter.Status.Host
		conn, err := tls.Dial("tcp", addr, config)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to connect to VCSim Server instance to infert thumbprint")
		}
		defer conn.Close()

		cert := conn.ConnectionState().PeerCertificates[0]
		vCenter.Status.Thumbprint = ThumbprintSHA1(cert)
	}

	if r.SupervisorMode {
		// TODO: this code should be refactored somewhere else so it can be used in the E2E tests as well (when running against real vCenters)
		//   in order to make it easier to do so, i'm defining local variables for all the info used down below.

		const (
			// copied from from vm-operator

			ProviderConfigMapName    = "vsphere.provider.config.vmoperator.vmware.com"
			vcPNIDKey                = "VcPNID"
			vcPortKey                = "VcPort"
			vcCredsSecretNameKey     = "VcCredsSecretName" //nolint:gosec
			datacenterKey            = "Datacenter"
			resourcePoolKey          = "ResourcePool"
			folderKey                = "Folder"
			datastoreKey             = "Datastore"
			networkNameKey           = "Network"
			scRequiredKey            = "StorageClassRequired"
			useInventoryKey          = "UseInventoryAsContentSource"
			insecureSkipTLSVerifyKey = "InsecureSkipTLSVerify"
			caFilePathKey            = "CAFilePath"
		)

		supervisorConfigs := []struct {
			host       string
			username   string
			password   string
			thumbprint string

			// supervisor is usually based on a single vCenter cluster
			datacenter        string
			cluster           string
			folder            string
			resourcePool      string
			availabilityZones struct {
				clusterComputeResourcePaths []string
			}
			vmOperator struct {
				// This is the namespace where is deployed the vm-operator for this supervisor
				namespace string
			}
		}{
			// TODO: make this adapt to the changes in the model from the API
			//  This config works for DC0/C0 in vcsim + a Tilt managed deployment
			{
				host:       vCenter.Status.Host,
				username:   vCenter.Status.Username,
				password:   vCenter.Status.Password,
				thumbprint: vCenter.Status.Thumbprint,

				datacenter:   "DC0",
				cluster:      "/DC0/host/DC0_C0",
				folder:       "/DC0/vm",
				resourcePool: "/DC0/host/DC0_C0/Resources",

				vmOperator: struct{ namespace string }{
					namespace: "vmware-system-vmop", // This is where tilt deploys the vm-operator
				},
			},
		}

		for _, config := range supervisorConfigs {
			// In order to run the vm-operator in standalone it is required to provide it with the dependencies it needs to work:
			// - A set of objects/configurations in the vCenter cluster the vm-operator is pointing to
			// - A set of Kubernetes object the vm-operator relies on

			// Get a Client to VCenter
			params := session.NewParams().
				WithServer(config.host).
				WithThumbprint(config.thumbprint).
				WithUserInfo(config.username, config.password)

			s, err := session.GetOrCreate(ctx, params)
			if err != nil {
				return ctrl.Result{}, errors.Wrapf(err, "failed to connect to VCSim Server instance to read compute clusters")
			}

			datacenter, err := s.Finder.Datacenter(ctx, config.datacenter)
			if err != nil {
				return ctrl.Result{}, errors.Wrapf(err, "failed to get datacenter %s", config.datacenter)
			}

			cluster, err := s.Finder.ClusterComputeResource(ctx, config.cluster)
			if err != nil {
				return ctrl.Result{}, errors.Wrapf(err, "failed to get cluster %s", config.cluster)
			}

			folder, err := s.Finder.Folder(ctx, config.folder)
			if err != nil {
				return ctrl.Result{}, errors.Wrapf(err, "failed to get folder %s", config.folder)
			}

			resourcePool, err := s.Finder.ResourcePool(ctx, config.resourcePool)
			if err != nil {
				return ctrl.Result{}, errors.Wrapf(err, "failed to get resourcePool %s", config.resourcePool)
			}

			// Create Availability zones CR
			// We are creating one availability zone for the cluster as in the example cluster
			// TODO: investigate what options exists to create availability zones,

			availabilityZone := &topologyv1.AvailabilityZone{
				ObjectMeta: metav1.ObjectMeta{
					Name: strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(config.cluster, "/")), "_", "-"), "/", "-"),
				},
				Spec: topologyv1.AvailabilityZoneSpec{
					ClusterComputeResourceMoId: cluster.Reference().Value,
				},
			}

			if err := r.Client.Create(ctx, availabilityZone); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create availability zone %s", availabilityZone.Name)
				}
			}

			// Create vm-operator Secret
			// This secret contains credentials to access vCenter the vm-operator acts on.
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ProviderConfigMapName, // using the same name of the config map for consistency.
					Namespace: config.vmOperator.namespace,
				},
				Data: map[string][]byte{
					"password": []byte(config.username),
					"username": []byte(config.password),
				},
				Type: corev1.SecretTypeOpaque,
			}
			if err := r.Client.Create(ctx, secret); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator Secret %s", secret.Name)
				}
			}

			// Create vm-operator ConfigMap
			// This ConfigMap contains settings for the vm-operator instance.

			host, port, err := net.SplitHostPort(config.host)
			if err != nil {
				return ctrl.Result{}, errors.Wrapf(err, "failed to split host %s", config.host)
			}

			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      ProviderConfigMapName,
					Namespace: config.vmOperator.namespace,
				},
				Data: map[string]string{
					caFilePathKey: "", // Leaving this empty because we don't have (yet) a solution to inject a CA file into the vm-operator pod.
					// Cluster exists in the example ConfigMap, but it is not defined as a const
					// ContentSource exists in the example ConfigMap, but it is not defined as a const
					// CtrlVmVmAATag exists in the example ConfigMap, but it is not defined as a const
					datastoreKey:             "", // Doesn't exist in the example ConfigMap, but it is defined as a const // TODO: is it ok to leave it empty (Comment in the code says: Only set in simulated testing env) ?
					datacenterKey:            datacenter.Reference().Value,
					folderKey:                folder.Reference().Value, // Doesn't exist in the example ConfigMap, but it is defined as a const
					insecureSkipTLSVerifyKey: "true",                   // Using this given that we don't have (yet) a solution to inject a CA file into the vm-operator pod.
					// IsRestrictedNetwork exists in the example ConfigMap, but it is not defined as a const
					networkNameKey:       "",                             // Doesn't exist in the example ConfigMap, but it is defined as a const // TODO: is it ok to leave it empty (Comment in the code says: Only set in simulated testing env) ?
					resourcePoolKey:      resourcePool.Reference().Value, // Doesn't exist in the example ConfigMap, but it is defined as a const
					scRequiredKey:        "true",
					useInventoryKey:      "false", // This is the vale from the example config // TODO: investigate more
					vcCredsSecretNameKey: secret.Name,
					vcPNIDKey:            host,
					vcPortKey:            port,
					// VmVmAntiAffinityTagCategoryName exists in the example ConfigMap, but it is not defined as a const
					// WorkerVmVmAATag exists in the example ConfigMap, but it is not defined as a const
				},
			}
			if err := r.Client.Create(ctx, configMap); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator ConfigMap %s", configMap.Name)
				}
			}
		}
	}

	return ctrl.Result{}, nil
}

func (r *VCenterReconciler) reconcileDelete(ctx context.Context, vCenter *vcsimv1.VCenter) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info("Reconciling delete VCSim Server")

	r.lock.Lock()
	defer r.lock.Unlock()

	key := klog.KObj(vCenter).String()
	vcsimInstance, ok := r.vcsimInstances[key]
	if ok {
		log.Info("Deleting VCSim Server")
		vcsimInstance.Destroy()
		delete(r.vcsimInstances, key)
	}

	controllerutil.RemoveFinalizer(vCenter, vcsimv1.VCenterFinalizer)

	return ctrl.Result{}, nil
}

// SetupWithManager will add watches for this controller.
func (r *VCenterReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&vcsimv1.VCenter{}).
		WithOptions(options).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue)).
		Complete(r)

	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}

	return nil
}

// ThumbprintSHA1 returns the thumbprint of the given cert in the same format used by the SDK and Client.SetThumbprint.
func ThumbprintSHA1(cert *x509.Certificate) string {
	sum := sha1.Sum(cert.Raw)
	hex := make([]string, len(sum))
	for i, b := range sum {
		hex[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(hex, ":")
}
