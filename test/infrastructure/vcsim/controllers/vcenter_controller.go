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

const (
	// copied from vm-operator

	providerConfigMapName    = "vsphere.provider.config.vmoperator.vmware.com"
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

	// vcsim objects

	vcsimVMTemplateName = "ubuntu-2204-kube-vX"

	// vm-operator secret

	netConfigMapName       = "vsphere.provider.config.netoperator.vmware.com"
	netConfigServerURLKey  = "server"
	netConfigDatacenterKey = "datacenter"
	netConfigUsernameKey   = "username"
	netConfigPasswordKey   = "password"
	netConfigThumbprintKey = "thumbprint"
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
		model.ServiceContent.About.Version = "7.0.0" // TODO: Make this configurable
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
		// Note: for the sake of testing with vcsim the template doesn't really matter (nor the version of K8s hosted on it)
		// so we create only a VM template with a well-known name.

		// TODO: Investigate how template are supposed to work
		//  we create a template in a datastore, what if many?
		//  we create a template in a cluster, but the generated vm doesn't have the cluster in the path. What if I have many clusters?
		govcURL := fmt.Sprintf("https://%s:%s@%s/sdk", vCenter.Status.Username, vCenter.Status.Password, vCenter.Status.Host)
		datacenters := 1
		if vCenter.Spec.Model != nil {
			datacenters = pointer.IntDeref(vCenter.Spec.Model.Datacenter, model.Datacenter)
		}
		for dc := 0; dc < datacenters; dc++ {
			exit := cli.Run([]string{"vm.create", "-ds=LocalDS_0", fmt.Sprintf("-cluster=DC%d_C0", dc), "-net=VM Network", "-disk=20G", "-on=false", "-k=true", fmt.Sprintf("-u=%s", govcURL), vcsimVMTemplateName})
			if exit != 0 {
				return ctrl.Result{}, errors.New("failed to create vm template")
			}

			exit = cli.Run([]string{"vm.markastemplate", "-k=true", fmt.Sprintf("-u=%s", govcURL), fmt.Sprintf("/DC%d/vm/%s", dc, vcsimVMTemplateName)})
			if exit != 0 {
				return ctrl.Result{}, errors.New("failed to mark vm template")
			}
			log.Info("Created VM template", "name", vcsimVMTemplateName)
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
		// In order to run the vm-operator in standalone mode it is required to provide it with the dependencies it needs to work:
		// - A set of objects/configurations in the vCenter cluster the vm-operator is pointing to
		// - A set of Kubernetes object the vm-operator relies on

		// To mimic the supervisor cluster, there will be only one vm-operator instance for each management cluster;
		// also, the logic below should consider that the instance of the vm-operator is bound to a specific vCenter cluster.

		config := vmOperatorDeploymentConfig{
			// This is where tilt deploys the vm-operator
			namespace: "vmware-system-vmop",

			// Those are config for vCenter cluster DC0/C0 in vcsim.
			vCenterCluster: vCenterClusterConfig{
				serverUrl:       vCenter.Status.Host,
				username:        vCenter.Status.Username,
				password:        vCenter.Status.Password,
				thumbprint:      vCenter.Status.Thumbprint,
				datacenter:      "DC0",
				cluster:         "/DC0/host/DC0_C0",
				folder:          "/DC0/vm",
				resourcePool:    "/DC0/host/DC0_C0/Resources",
				storagePolicyID: "vSAN Default Storage Policy",

				// Those are settings for a fake content library we are going to create given that it doesn't exists in vcsim by default.
				contentLibrary: contentLibraryConfig{
					name:      "kubernetes",
					datastore: "/DC0/datastore/LocalDS_0",
					item: contentLibraryItemConfig{
						name: "test-image-ovf",
						files: []contentLibraryItemFilesConfig{ // TODO: check if we really need both
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
			},

			// The users are expected to store Cluster API clusters to be managed by the vm-operator
			// in the default namespace and to use the "vcsim-default" storage class.
			userNamespace: userNamespaceConfig{
				name:         corev1.NamespaceDefault,
				storageClass: "vcsim-default",
			},
		}

		if ret, err := reconcileVMOperatorDeployment(ctx, r.Client, config); err != nil || !ret.IsZero() {
			return ret, err
		}

		// The vm-operator doesn't take care of the networking part of the VM, which is usually
		// managed by other components in the supervisor cluster.
		// In order to make things to work in vcsim, this is taken care by a fake net-operator.
		// TODO: think about the idea of net-operator, because the current fake net-operator does much more, it also takes care of fake vm-bootstrap.
		//  Also, there is a legacy and a supervisor version of it, so the problem scope is not strictly related to the operator,
		//  while the need of somehow provisioning vCenter configurations to this component is strictly related to the supervisor mode.

		if ret, err := reconcileFakeNetOperatorDeployment(ctx, r.Client, config); err != nil || !ret.IsZero() {
			return ret, err
		}
	}

	return ctrl.Result{}, nil
}

type contentLibraryItemFilesConfig struct {
	name    string
	content []byte
}

type contentLibraryItemConfig struct {
	name        string
	files       []contentLibraryItemFilesConfig
	itemType    string
	productInfo string
	osInfo      string
}

type contentLibraryConfig struct {
	name      string
	datastore string
	item      contentLibraryItemConfig
}

type vCenterClusterConfig struct {
	serverUrl  string
	username   string
	password   string
	thumbprint string

	// supervisor is usually based on a single vCenter cluster
	datacenter      string
	cluster         string
	folder          string
	resourcePool    string
	storagePolicyID string
	// availabilityZones availabilityZonesConfig
	contentLibrary contentLibraryConfig
}

type userNamespaceConfig struct {
	name         string
	storageClass string
}

type vmOperatorDeploymentConfig struct {
	// This is the namespace where is deployed the vm-operator
	namespace string

	// Info about the vCenter cluster the vm-operator is bound to
	vCenterCluster vCenterClusterConfig

	// Info about where the users are expected to store Cluster API clusters to be managed by the vm-operator
	userNamespace userNamespaceConfig
}

func reconcileVMOperatorDeployment(ctx context.Context, c client.Client, config vmOperatorDeploymentConfig) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info("Reconciling requirements for the VMOperator Deployment")

	// Get a Client to VCenter and get holds on the relevant objects that should already exist
	params := session.NewParams().
		WithServer(config.vCenterCluster.serverUrl).
		WithThumbprint(config.vCenterCluster.thumbprint).
		WithUserInfo(config.vCenterCluster.username, config.vCenterCluster.password)

	s, err := session.GetOrCreate(ctx, params)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to connect to VCSim Server instance to read compute clusters")
	}

	datacenter, err := s.Finder.Datacenter(ctx, config.vCenterCluster.datacenter)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to get datacenter %s", config.vCenterCluster.datacenter)
	}

	cluster, err := s.Finder.ClusterComputeResource(ctx, config.vCenterCluster.cluster)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to get cluster %s", config.vCenterCluster.cluster)
	}

	folder, err := s.Finder.Folder(ctx, config.vCenterCluster.folder)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to get folder %s", config.vCenterCluster.folder)
	}

	resourcePool, err := s.Finder.ResourcePool(ctx, config.vCenterCluster.resourcePool)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to get resourcePool %s", config.vCenterCluster.resourcePool)
	}

	contentLibraryDatastore, err := s.Finder.Datastore(ctx, config.vCenterCluster.contentLibrary.datastore)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to get contentLibraryDatastore %s", config.vCenterCluster.contentLibrary.datastore)
	}

	pvmClient, err := pbm.NewClient(ctx, s.Client.Client)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to get storage policy client")
	}

	storagePolicyID, err := pvmClient.ProfileIDByName(ctx, config.vCenterCluster.storagePolicyID)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to get storage policy profile %s", config.vCenterCluster.storagePolicyID)
	}

	// Create StorageClass & bind it to the user namespace via a ResourceQuota
	// TODO: consider if we want to support more than one storage classes

	storageClass := &storagev1.StorageClass{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name: config.userNamespace.storageClass,
		},
		Provisioner: "kubernetes.io/vsphere-volume",
		Parameters: map[string]string{
			"storagePolicyID": storagePolicyID,
		},
	}

	if err := c.Get(ctx, client.ObjectKeyFromObject(storageClass), storageClass); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator StorageClass %s", storageClass.Name)
		}

		if err := c.Create(ctx, storageClass); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator StorageClass %s", storageClass.Name)
		}
		log.Info("Created vm-operator StorageClass", "StorageClass", klog.KObj(storageClass))
	}

	// TODO: rethink about this, for now we are creating a ResourceQuota with the same name of the StorageClass, might be this is not ok when hooking into a real vCenter
	resourceQuota := &corev1.ResourceQuota{
		TypeMeta: metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.userNamespace.storageClass,
			Namespace: config.userNamespace.name,
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: map[corev1.ResourceName]resource.Quantity{
				corev1.ResourceName(fmt.Sprintf("%s.storageclass.storage.k8s.io/requests.storage", storageClass.Name)): resource.MustParse("1Gi"),
			},
		},
	}

	if err := c.Get(ctx, client.ObjectKeyFromObject(resourceQuota), resourceQuota); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator ResourceQuota %s", resourceQuota.Name)
		}

		if err := c.Create(ctx, resourceQuota); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator ResourceQuota %s", resourceQuota.Name)
		}
		log.Info("Created vm-operator ResourceQuota", "ResourceQuota", klog.KObj(resourceQuota))
	}

	// Create Availability zones CR in K8s and bind them to the user namespace
	// NOTE: For now we are creating one availability zone for the cluster as in the example cluster
	// TODO: investigate what options exists to create availability zones, and if we want to support more

	availabilityZone := &topologyv1.AvailabilityZone{
		ObjectMeta: metav1.ObjectMeta{
			Name: strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(config.vCenterCluster.cluster, "/")), "_", "-"), "/", "-"),
		},
		Spec: topologyv1.AvailabilityZoneSpec{
			ClusterComputeResourceMoId: cluster.Reference().Value,
			Namespaces: map[string]topologyv1.NamespaceInfo{
				config.userNamespace.name: {
					PoolMoId:   resourcePool.Reference().Value,
					FolderMoId: folder.Reference().Value,
				},
			},
		},
	}

	if err := c.Get(ctx, client.ObjectKeyFromObject(availabilityZone), availabilityZone); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get AvailabilityZone %s", availabilityZone.Name)
		}

		if err := c.Create(ctx, availabilityZone); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create AvailabilityZone %s", availabilityZone.Name)
		}
		log.Info("Created vm-operator AvailabilityZone", "AvailabilityZone", klog.KObj(availabilityZone))
	}

	// Create vm-operator Secret in K8s
	// This secret contains credentials to access vCenter the vm-operator acts on.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      providerConfigMapName, // using the same name of the config map for consistency.
			Namespace: config.namespace,
		},
		Data: map[string][]byte{
			"password": []byte(config.vCenterCluster.username),
			"username": []byte(config.vCenterCluster.password),
		},
		Type: corev1.SecretTypeOpaque,
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator Secret %s", secret.Name)
		}
		if err := c.Create(ctx, secret); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator Secret %s", secret.Name)
		}
		log.Info("Created vm-operator Secret", "Secret", klog.KObj(secret))
	}

	// Create vm-operator ConfigMap in K8s
	// This ConfigMap contains settings for the vm-operator instance.

	host, port, err := net.SplitHostPort(config.vCenterCluster.serverUrl)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to split host %s", config.vCenterCluster.serverUrl)
	}

	providerConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      providerConfigMapName,
			Namespace: config.namespace,
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
	if err := c.Get(ctx, client.ObjectKeyFromObject(providerConfigMap), providerConfigMap); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator ConfigMap %s", providerConfigMap.Name)
		}
		if err := c.Create(ctx, providerConfigMap); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator ConfigMap %s", providerConfigMap.Name)
		}
		log.Info("Created vm-operator ConfigMap", "ConfigMap", klog.KObj(providerConfigMap))
	}

	// Create VirtualMachineClass in K8s and bind it to the user namespace
	// TODO: figure out if to add more vm classes / if to make them configurable via config
	vmClass := &operatorv1.VirtualMachineClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "best-effort-2xlarge",
		},
		Spec: operatorv1.VirtualMachineClassSpec{
			Hardware: operatorv1.VirtualMachineClassHardware{
				Cpus:   8,
				Memory: resource.MustParse("64G"),
			},
		},
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(vmClass), vmClass); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator VirtualMachineClass %s", vmClass.Name)
		}
		if err := c.Create(ctx, vmClass); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator VirtualMachineClass %s", vmClass.Name)
		}
		log.Info("Created vm-operator VirtualMachineClass", "VirtualMachineClass", klog.KObj(vmClass))
	}

	vmClassBinding := &operatorv1.VirtualMachineClassBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vmClass.Name,
			Namespace: config.userNamespace.name,
		},
		ClassRef: operatorv1.ClassReference{
			APIVersion: operatorv1.SchemeGroupVersion.String(),
			Kind:       util.TypeToKind(&operatorv1.VirtualMachineClass{}),
			Name:       vmClass.Name,
		},
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(vmClassBinding), vmClassBinding); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator VirtualMachineClassBinding %s", vmClassBinding.Name)
		}
		if err := c.Create(ctx, vmClassBinding); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator VirtualMachineClassBinding %s", vmClassBinding.Name)
		}
		log.Info("Created vm-operator VirtualMachineClassBinding", "VirtualMachineClassBinding", klog.KObj(vmClassBinding))
	}

	// Create a ContentLibrary in K8s and in vCenter, bind it to the K8s namespace
	// This requires a set of objects in vc-sim as well as their mapping in K8s
	// - vcsim: a Library containing an Item
	// - k8s: ContentLibraryProvider, ContentSource (both representing the library), a VirtualMachineImage (representing the Item)

	restClient := rest.NewClient(s.Client.Client)
	if err := restClient.Login(ctx, url.UserPassword(config.vCenterCluster.username, config.vCenterCluster.password)); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to login using the rest client")
	}

	libMgr := library.NewManager(restClient)

	contentLibrary := library.Library{
		Name: config.vCenterCluster.contentLibrary.name,
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
	if err := c.Get(ctx, client.ObjectKeyFromObject(contentSource), contentSource); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator ContentSource %s", contentSource.Name)
		}
		if err := c.Create(ctx, contentSource); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator ContentSource %s", contentSource.Name)
		}
		log.Info("Created vm-operator ContentSource", "ContentSource", klog.KObj(contentSource))
	}

	contentSourceBinding := &operatorv1.ContentSourceBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      contentLibraryID,
			Namespace: config.userNamespace.name,
		},
		ContentSourceRef: operatorv1.ContentSourceReference{
			APIVersion: operatorv1.SchemeGroupVersion.String(),
			Kind:       util.TypeToKind(&operatorv1.ContentSource{}),
			Name:       contentSource.Name,
		},
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(contentSourceBinding), contentSourceBinding); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator ContentSourceBinding %s", contentSourceBinding.Name)
		}
		if err := c.Create(ctx, contentSourceBinding); err != nil {
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

	if err := controllerutil.SetOwnerReference(contentSource, contentLibraryProvider, c.Scheme()); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to set ContentLibraryProvider owner")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(contentSource), contentLibraryProvider); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator ContentLibraryProvider %s", contentLibraryProvider.Name)
		}
		if err := c.Create(ctx, contentLibraryProvider); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create vm-operator ContentLibraryProvider %s", contentLibraryProvider.Name)
		}
		log.Info("Created vm-operator ContentLibraryProvider", "ContentSource", klog.KObj(contentSource), "ContentLibraryProvider", klog.KObj(contentLibraryProvider))
	}

	libraryItem := library.Item{
		Name:      config.vCenterCluster.contentLibrary.item.name,
		Type:      config.vCenterCluster.contentLibrary.item.itemType,
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
				FullVersion: config.vCenterCluster.contentLibrary.item.productInfo,
			},
			OSInfo: operatorv1.VirtualMachineImageOSInfo{
				Type: config.vCenterCluster.contentLibrary.item.osInfo,
			},
		},
	}

	if err := controllerutil.SetOwnerReference(contentLibraryProvider, virtualMachineImage, c.Scheme()); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to set VirtualMachineImage owner")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(virtualMachineImage), virtualMachineImage); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get vm-operator VirtualMachineImage %s", virtualMachineImage.Name)
		}
		if err := c.Create(ctx, virtualMachineImage); err != nil {
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

	for _, file := range config.vCenterCluster.contentLibrary.item.files {
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
	}
	return ctrl.Result{}, nil
}

func reconcileFakeNetOperatorDeployment(ctx context.Context, c client.Client, config vmOperatorDeploymentConfig) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info("Reconciling requirements for the Fake net-operator Deployment")

	netOperatorSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      netConfigMapName,
			Namespace: config.namespace,
		},
		StringData: map[string]string{
			netConfigServerURLKey:  config.vCenterCluster.serverUrl,
			netConfigDatacenterKey: config.vCenterCluster.datacenter,
			netConfigUsernameKey:   config.vCenterCluster.username,
			netConfigPasswordKey:   config.vCenterCluster.password,
			netConfigThumbprintKey: config.vCenterCluster.thumbprint,
		},
		Type: corev1.SecretTypeOpaque,
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(netOperatorSecret), netOperatorSecret); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get net-operator Secret %s", netOperatorSecret.Name)
		}
		if err := c.Create(ctx, netOperatorSecret); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create net-operator Secret %s", netOperatorSecret.Name)
		}
		log.Info("Created net-operator Secret", "Secret", klog.KObj(netOperatorSecret))
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
