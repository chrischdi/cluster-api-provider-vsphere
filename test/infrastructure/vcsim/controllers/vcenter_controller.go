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
	"bytes"
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
	operatorv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha1"
	topologyv1 "github.com/vmware-tanzu/vm-operator/external/tanzu-topology/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	_ "github.com/vmware/govmomi/govc/vm" // NOTE: this is required to add commands vm.* to cli.Run
	"github.com/vmware/govmomi/pbm"
	pbmsimulator "github.com/vmware/govmomi/pbm/simulator"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vapi/library"
	"github.com/vmware/govmomi/vapi/rest"
	_ "github.com/vmware/govmomi/vapi/simulator" // NOTE: this is required to content library & other vapi methods to the simulator
	"github.com/vmware/govmomi/vim25/soap"
	"sigs.k8s.io/cluster-api-provider-vsphere/packaging/flavorgen/flavors/util"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
	"sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/controllers/images"

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
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachineclasses,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachineclassbindings,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=contentlibraryproviders,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=contentsources,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=contentsourcebindings,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachineimages,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create

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

		// Compute the vcsim URL, binding all interfaces (so it will be accessible both from other pods via the pod IP and via kubectl port-forward on local host);
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

		// Configure one or more vm-operator instances (one for now)

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

			NetworkConfigMapName = "vmoperator-network-config"
			NameserversKey       = "nameservers"    // Key in the NetworkConfigMapName.
			SearchSuffixesKey    = "searchsuffixes" // Key in the NetworkConfigMapName.
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
			storagePolicyID   string
			availabilityZones struct {
				clusterComputeResourcePaths []string
			}
			contentLibrary struct {
				name      string
				datastore string
				item      struct {
					name  string
					files []struct {
						name    string
						content []byte
					}
					itemType    string
					productInfo string
					osInfo      string
				}
			}
			vmOperator struct {
				// This is the namespace where is deployed the vm-operator for this supervisor
				namespace string
			}

			// supervisor maps k8s namespace (is it true??)
			// TODO: rethink about this, it groups k8s settings (while above they are all vCenter related settings)
			namespace struct {
				name         string
				storageClass string
			}
		}{
			// TODO: make this adapt to the changes in the model from the API
			//  This config works for DC0/C0 in vcsim + a Tilt managed deployment
			{
				host:       vCenter.Status.Host,
				username:   vCenter.Status.Username,
				password:   vCenter.Status.Password,
				thumbprint: vCenter.Status.Thumbprint,

				datacenter:      "DC0",
				cluster:         "/DC0/host/DC0_C0",
				folder:          "/DC0/vm",
				resourcePool:    "/DC0/host/DC0_C0/Resources",
				storagePolicyID: "vSAN Default Storage Policy",

				vmOperator: struct{ namespace string }{
					namespace: "vmware-system-vmop", // This is where tilt deploys the vm-operator
				},

				contentLibrary: struct {
					name      string
					datastore string
					item      struct {
						name  string
						files []struct {
							name    string
							content []byte
						}
						itemType    string
						productInfo string
						osInfo      string
					}
				}{
					name:      "",
					datastore: "/DC0/datastore/LocalDS_0",
					item: struct {
						name  string
						files []struct {
							name    string
							content []byte
						}
						itemType    string
						productInfo string
						osInfo      string
					}{
						name: "test-image-ovf",
						files: []struct {
							name    string
							content []byte
						}{
							{
								name:    "ttylinux-pc_i486-16.1.ovf",
								content: images.SampleOVF,
							},
							{
								name:    "ttylinux-pc_i486-16.1-disk1.vmdk",
								content: images.SampleVMDK,
							},
						},
						itemType:    "ovf",
						productInfo: "dummy-productInfo",
						osInfo:      "dummy-OSInfo",
					},
				},

				namespace: struct {
					name         string
					storageClass string
				}{
					name:         corev1.NamespaceDefault,
					storageClass: "vcsim-default",
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

			contentLibraryDatastore, err := s.Finder.Datastore(ctx, config.contentLibrary.datastore)
			if err != nil {
				return ctrl.Result{}, errors.Wrapf(err, "failed to get contentLibraryDatastore %s", config.contentLibrary.datastore)
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

			c, err := pbm.NewClient(ctx, s.Client.Client)
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to get storage policy client")
			}

			storagePolicyID, err := c.ProfileIDByName(ctx, config.storagePolicyID)
			if err != nil {
				return ctrl.Result{}, errors.Wrapf(err, "failed to get storage policy profile %s", config.storagePolicyID)
			}

			// Create StorageClass & bid it to the K8s namespace via a ResourceQuota
			// TODO: refactor this so it is configurable

			storageClass := &storagev1.StorageClass{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Name: config.namespace.storageClass,
				},
				Provisioner: "kubernetes.io/vsphere-volume",
				Parameters: map[string]string{
					"storagePolicyID": storagePolicyID,
				},
			}

			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(storageClass), storageClass); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator StorageClass %s", storageClass.Name)
				}

				if err := r.Client.Create(ctx, storageClass); err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator StorageClass %s", storageClass.Name)
				}
				log.Info("Created vm-operator StorageClass", "StorageClass", klog.KObj(storageClass))
			}

			// TODO: rethink about this, for now we are creating a ResourceQuota with the same name of the StorageClass, might be this is not ok when hooking into a real vCenter
			resourceQuota := &corev1.ResourceQuota{
				TypeMeta: metav1.TypeMeta{},
				ObjectMeta: metav1.ObjectMeta{
					Name:      config.namespace.storageClass,
					Namespace: config.namespace.name,
				},
				Spec: corev1.ResourceQuotaSpec{
					Hard: map[corev1.ResourceName]resource.Quantity{
						corev1.ResourceName(fmt.Sprintf("%s.storageclass.storage.k8s.io/requests.storage", storageClass.Name)): resource.MustParse("1Gi"),
					},
				},
			}

			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(resourceQuota), resourceQuota); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator ResourceQuota %s", resourceQuota.Name)
				}

				if err := r.Client.Create(ctx, resourceQuota); err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator ResourceQuota %s", resourceQuota.Name)
				}
				log.Info("Created vm-operator ResourceQuota", "ResourceQuota", klog.KObj(resourceQuota))
			}

			// Create Availability zones CR in K8s and bind it to the K8s namespace
			// We are creating one availability zone for the cluster as in the example cluster
			// TODO: investigate what options exists to create availability zones,

			availabilityZone := &topologyv1.AvailabilityZone{
				ObjectMeta: metav1.ObjectMeta{
					Name: strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(config.cluster, "/")), "_", "-"), "/", "-"),
				},
				Spec: topologyv1.AvailabilityZoneSpec{
					ClusterComputeResourceMoId: cluster.Reference().Value,
					Namespaces: map[string]topologyv1.NamespaceInfo{
						config.namespace.name: {
							PoolMoId:   resourcePool.Reference().Value,
							FolderMoId: folder.Reference().Value,
						},
					},
				},
			}

			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(availabilityZone), availabilityZone); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to get AvailabilityZone %s", availabilityZone.Name)
				}

				if err := r.Client.Create(ctx, availabilityZone); err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create AvailabilityZone %s", availabilityZone.Name)
				}
				log.Info("Created vm-operator AvailabilityZone", "AvailabilityZone", klog.KObj(availabilityZone))
			}

			// Create vm-operator Secret in K8s
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
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator Secret %s", secret.Name)
				}
				if err := r.Client.Create(ctx, secret); err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator Secret %s", secret.Name)
				}
				log.Info("Created vm-operator Secret", "Secret", klog.KObj(secret))
			}

			// Create vm-operator ConfigMap in K8s
			// This ConfigMap contains settings for the vm-operator instance.

			host, port, err := net.SplitHostPort(config.host)
			if err != nil {
				return ctrl.Result{}, errors.Wrapf(err, "failed to split host %s", config.host)
			}

			providerConfigMap := &corev1.ConfigMap{
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
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(providerConfigMap), providerConfigMap); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator ConfigMap %s", providerConfigMap.Name)
				}
				if err := r.Client.Create(ctx, providerConfigMap); err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator ConfigMap %s", providerConfigMap.Name)
				}
				log.Info("Created vm-operator ConfigMap", "ConfigMap", klog.KObj(providerConfigMap))
			}

			networkConfigMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      NetworkConfigMapName,
					Namespace: config.vmOperator.namespace,
				},
				Data: map[string]string{
					NameserversKey:    "10.20.145.1", // TODO: make this configurable
					SearchSuffixesKey: "",            // Doesn't exist in the example ConfigMap, but it is defined as a const // TODO: is it ok to leave it empty ?
					// ntpservers exists in the example ConfigMap, but it is not defined as a const
				},
			}
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(networkConfigMap), networkConfigMap); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator ConfigMap %s", networkConfigMap.Name)
				}
				if err := r.Client.Create(ctx, networkConfigMap); err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator ConfigMap %s", networkConfigMap.Name)
				}
				log.Info("Created vm-operator ConfigMap", "ConfigMap", klog.KObj(networkConfigMap))
			}

			// Create VirtualMachineClass in K8s and bind it to the K8s namespace
			// TODO: figure out if to add more
			vmClass := &operatorv1.VirtualMachineClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: "best-effort-2xlarge", // TODO: move to config
				},
				Spec: operatorv1.VirtualMachineClassSpec{
					Hardware: operatorv1.VirtualMachineClassHardware{
						Cpus:   8,
						Memory: resource.MustParse("64G"),
					},
				},
			}
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(vmClass), vmClass); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator VirtualMachineClass %s", vmClass.Name)
				}
				if err := r.Client.Create(ctx, vmClass); err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator VirtualMachineClass %s", vmClass.Name)
				}
				log.Info("Created vm-operator VirtualMachineClass", "VirtualMachineClass", klog.KObj(vmClass))
			}

			vmClassBinding := &operatorv1.VirtualMachineClassBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      vmClass.Name,
					Namespace: config.namespace.name,
				},
				ClassRef: operatorv1.ClassReference{
					APIVersion: operatorv1.SchemeGroupVersion.String(),
					Kind:       util.TypeToKind(&operatorv1.VirtualMachineClass{}),
					Name:       vmClass.Name,
				},
			}
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(vmClassBinding), vmClassBinding); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator VirtualMachineClassBinding %s", vmClassBinding.Name)
				}
				if err := r.Client.Create(ctx, vmClassBinding); err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator VirtualMachineClassBinding %s", vmClassBinding.Name)
				}
				log.Info("Created vm-operator VirtualMachineClassBinding", "VirtualMachineClassBinding", klog.KObj(vmClassBinding))
			}

			// Create a ContentLibrary in K8s and in vCenter, bind it to the K8s namespace
			// This requires a set of objects in vc-sim as well as their mapping in K8s
			// - vcsim: a Library containing an Item
			// - k8s: ContentLibraryProvider, ContentSource (both representing the library), a VirtualMachineImage (representing the Item)

			restClient := rest.NewClient(s.Client.Client)
			if err := restClient.Login(ctx, url.UserPassword(config.username, config.password)); err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to login using the rest client")
			}

			libMgr := library.NewManager(restClient)

			contentLibrary := library.Library{
				Name: config.contentLibrary.name,
				Type: "LOCAL",
				Storage: []library.StorageBackings{
					{
						DatastoreID: contentLibraryDatastore.Reference().Value,
						Type:        "DATASTORE",
					},
				},
			}
			libraries, err := libMgr.GetLibraries(ctx)
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to get ContentLibraries")
			}

			var contentLibraryID string
			if len(libraries) > 0 {
				for i := range libraries {
					if libraries[i].Name == contentLibrary.Name {
						contentLibraryID = libraries[i].ID
						break
					}
				}
			}

			if contentLibraryID == "" {
				id, err := libMgr.CreateLibrary(ctx, contentLibrary)
				if err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator ContentLibrary %s", contentLibrary.Name)
				}
				log.Info("Created vm-operator ContentLibrary in vcsim", "ContentLibrary", contentLibrary.Name)
				contentLibraryID = id
			}

			contentSource := &operatorv1.ContentSource{
				ObjectMeta: metav1.ObjectMeta{
					Name: contentLibraryID,
				},
				Spec: operatorv1.ContentSourceSpec{
					ProviderRef: operatorv1.ContentProviderReference{
						Name: contentLibraryID, // NOTE: this should match the ContentLibraryProvider name below
						Kind: "ContentLibraryProvider",
					},
				},
			}
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(contentSource), contentSource); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator ContentSource %s", contentSource.Name)
				}
				if err := r.Client.Create(ctx, contentSource); err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator ContentSource %s", contentSource.Name)
				}
				log.Info("Created vm-operator ContentSource", "ContentSource", klog.KObj(contentSource))
			}

			contentSourceBinding := &operatorv1.ContentSourceBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name:      contentLibraryID,
					Namespace: config.namespace.name,
				},
				ContentSourceRef: operatorv1.ContentSourceReference{
					APIVersion: operatorv1.SchemeGroupVersion.String(),
					Kind:       util.TypeToKind(&operatorv1.ContentSource{}),
					Name:       contentSource.Name,
				},
			}
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(contentSourceBinding), contentSourceBinding); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator ContentSourceBinding %s", contentSourceBinding.Name)
				}
				if err := r.Client.Create(ctx, contentSourceBinding); err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator ContentSourceBinding %s", contentSourceBinding.Name)
				}
				log.Info("Created vm-operator ContentSourceBinding", "ContentSourceBinding", klog.KObj(contentSourceBinding))
			}

			contentLibraryProvider := &operatorv1.ContentLibraryProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name: contentLibraryID,
				},
				Spec: operatorv1.ContentLibraryProviderSpec{
					UUID: contentLibraryID,
				},
			}

			if err := controllerutil.SetOwnerReference(contentSource, contentLibraryProvider, r.Client.Scheme()); err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to set ContentLibraryProvider owner")
			}
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(contentSource), contentLibraryProvider); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator ContentLibraryProvider %s", contentLibraryProvider.Name)
				}
				if err := r.Client.Create(ctx, contentLibraryProvider); err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator ContentLibraryProvider %s", contentLibraryProvider.Name)
				}
				log.Info("Created vm-operator ContentLibraryProvider", "ContentSource", klog.KObj(contentSource), "ContentLibraryProvider", klog.KObj(contentLibraryProvider))
			}

			libraryItem := library.Item{
				Name:      config.contentLibrary.item.name,
				Type:      config.contentLibrary.item.itemType,
				LibraryID: contentLibraryID,
			}

			items, err := libMgr.GetLibraryItems(ctx, contentLibraryID)
			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to get ContentLibraryItems")
			}

			var libraryItemID string
			for _, item := range items {
				if item.Name == libraryItem.Name {
					libraryItemID = item.ID
					break
				}
			}

			if libraryItemID == "" {
				id, err := libMgr.CreateLibraryItem(ctx, libraryItem)
				if err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator ContentLibraryItem %s", libraryItem.Name)
				}
				log.Info("Created vm-operator LibraryItem in vcsim", "ContentLibrary", contentLibrary.Name, "LibraryItem", libraryItem.Name)
				libraryItemID = id
			}

			virtualMachineImage := &operatorv1.VirtualMachineImage{
				ObjectMeta: metav1.ObjectMeta{
					Name: libraryItem.Name,
				},
				Spec: operatorv1.VirtualMachineImageSpec{
					ProductInfo: operatorv1.VirtualMachineImageProductInfo{
						FullVersion: config.contentLibrary.item.productInfo,
					},
					OSInfo: operatorv1.VirtualMachineImageOSInfo{
						Type: config.contentLibrary.item.osInfo,
					},
				},
			}

			if err := controllerutil.SetOwnerReference(contentLibraryProvider, virtualMachineImage, r.Client.Scheme()); err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to set VirtualMachineImage owner")
			}
			if err := r.Client.Get(ctx, client.ObjectKeyFromObject(virtualMachineImage), virtualMachineImage); err != nil {
				if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator VirtualMachineImage %s", virtualMachineImage.Name)
				}
				if err := r.Client.Create(ctx, virtualMachineImage); err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator VirtualMachineImage %s", virtualMachineImage.Name)
				}
				log.Info("Created vm-operator VirtualMachineImage", "ContentSource", klog.KObj(contentSource), "ContentLibraryProvider", klog.KObj(contentLibraryProvider), "VirtualMachineImage", klog.KObj(virtualMachineImage))
			}

			existingFiles, err := libMgr.ListLibraryItemFiles(ctx, libraryItemID)
			if err != nil {
				return ctrl.Result{}, errors.Wrapf(err, "failed to list files for vm-operator libraryItem %s", libraryItem.Name)
			}

			uploadFunc := func(sessionID, file string, content []byte) error {
				info := library.UpdateFile{
					Name:       file,
					SourceType: "PUSH",
					Size:       int64(len(content)),
				}

				update, err := libMgr.AddLibraryItemFile(ctx, sessionID, info)
				if err != nil {
					return err
				}

				u, err := url.Parse(update.UploadEndpoint.URI)
				if err != nil {
					return err
				}

				p := soap.DefaultUpload
				p.ContentLength = info.Size

				return libMgr.Client.Upload(ctx, bytes.NewReader(content), u, &p)
			}

			for _, file := range config.contentLibrary.item.files {
				exists := false
				for _, existingFile := range existingFiles {
					if file.name == existingFile.Name {
						exists = true
					}
				}
				if exists {
					continue
				}

				sessionID, err := libMgr.CreateLibraryItemUpdateSession(ctx, library.Session{LibraryItemID: libraryItemID})
				if err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to start update session for vm-operator libraryItem %s", libraryItem.Name)
				}
				if err := uploadFunc(sessionID, file.name, file.content); err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to upload data for vm-operator libraryItem %s", libraryItem.Name)
				}
				if err := libMgr.CompleteLibraryItemUpdateSession(ctx, sessionID); err != nil {
					return ctrl.Result{}, errors.Wrapf(err, "failed to complete update session for vm-operator libraryItem %s", libraryItem.Name)
				}
				log.Info("Uploaded vm-operator LibraryItemFile in vcsim", "ContentLibrary", contentLibrary.Name, "libraryItem", klog.KObj(virtualMachineImage), "LibraryItemFile", file.name)

				/*
					original := virtualMachineImage.DeepCopy()
					virtualMachineImage.Status.ImageName = libraryItem.Name
					if err := r.Client.Patch(ctx, virtualMachineImage, client.MergeFrom(original)); err != nil {
						if !apierrors.IsAlreadyExists(err) {
							return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator VirtualMachineImage %s", virtualMachineImage.Name)
						}
					}
				*/
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
