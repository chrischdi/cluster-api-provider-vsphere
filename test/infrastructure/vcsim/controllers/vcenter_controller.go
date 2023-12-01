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
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"

	_ "github.com/dougm/pretty"
	"github.com/pkg/errors"
	"github.com/vmware/govmomi/govc/cli"
	_ "github.com/vmware/govmomi/govc/vm"
	pbmsimulator "github.com/vmware/govmomi/pbm/simulator"
	"github.com/vmware/govmomi/simulator"
	"golang.org/x/crypto/ssh"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"sigs.k8s.io/cluster-api-provider-vsphere/test/helpers/vcsim"
	vcsimv1 "sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/api/v1alpha1"
)

type VCenterReconciler struct {
	Client client.Client
	PodIp  string

	vcsimInstances map[string]*vcsim.Simulator
	sshKeys        map[string]string
	lock           sync.RWMutex

	// WatchFilterValue is the label value used to filter events prior to reconciliation.
	WatchFilterValue string
}

// +kubebuilder:rbac:groups=vcsim.infrastructure.cluster.x-k8s.io,resources=vcenters,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=vcsim.infrastructure.cluster.x-k8s.io,resources=vcenters/status,verbs=get;update;patch

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

	if r.sshKeys == nil {
		r.sshKeys = map[string]string{}
	}

	key := klog.KObj(vCenter).String()

	sshKey, ok := r.sshKeys[key]
	if !ok {
		bitSize := 4096

		privateKey, err := generatePrivateKey(bitSize)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to generate private key")
		}

		publicKeyBytes, err := generatePublicKey(&privateKey.PublicKey)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to generate public key")
		}

		sshKey = string(publicKeyBytes)
		r.sshKeys[key] = sshKey
		log.Info("Created ssh authorized key")
	}

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

	clusters := []vcsimv1.ClusterEnvSubstGeneratorSpec{
		{
			Name: "*",
		},
	}
	if vCenter.Spec.Generators.EnvSubst != nil {
		clusters = append(clusters, vCenter.Spec.Generators.EnvSubst.Clusters...)
	}

	vCenter.Status.EnvSubst.Clusters = make([]vcsimv1.ClusterEnvVars, len(clusters))
	for i, cluster := range clusters {
		clusterEnvVars := vcsimv1.ClusterEnvVars{
			Name: cluster.Name,
			Variables: map[string]string{
				// cluster template variables about the vcsim instance.
				"VSPHERE_SERVER":             fmt.Sprintf("https://%s", vCenter.Status.Host),
				"VSPHERE_PASSWORD":           vCenter.Status.Password,
				"VSPHERE_USERNAME":           vCenter.Status.Username,
				"VSPHERE_TLS_THUMBPRINT":     vCenter.Status.Thumbprint,
				"VSPHERE_DATACENTER":         fmt.Sprintf("DC%d", pointer.IntDeref(cluster.Datacenter, 0)),
				"VSPHERE_DATASTORE":          fmt.Sprintf("LocalDS_%d", pointer.IntDeref(cluster.Datastore, 0)),
				"VSPHERE_FOLDER":             fmt.Sprintf("/DC%d/vm", pointer.IntDeref(cluster.Datacenter, 0)),                                                               // this is the default folder that gets created. TODO: consider if to make it possible to create more (this requires changes to the API)
				"VSPHERE_NETWORK":            fmt.Sprintf("/DC%d/network/VM Network", pointer.IntDeref(cluster.Datacenter, 0)),                                               // this is the default network that gets created. TODO: consider if to make it possible to create more (this requires changes to the API)
				"VSPHERE_RESOURCE_POOL":      fmt.Sprintf("/DC%d/host/DC%[1]d_C%d/Resources", pointer.IntDeref(cluster.Datacenter, 0), pointer.IntDeref(cluster.Cluster, 0)), // all pool have RP as prefix. TODO: make it possible to pick one (0 --> Resources, >0 --> RPn)
				"VSPHERE_STORAGE_POLICY":     "vSAN Default Storage Policy",
				"VSPHERE_TEMPLATE":           fmt.Sprintf("/DC%d/vm/ubuntu-2204-kube-vX", pointer.IntDeref(cluster.Datacenter, 0)),
				"VSPHERE_SSH_AUTHORIZED_KEY": sshKey,

				// other variables required by the cluster template.
				"NAMESPACE":                   vCenter.Namespace,
				"CLUSTER_NAME":                cluster.Name,
				"KUBERNETES_VERSION":          pointer.StringDeref(cluster.KubernetesVersion, "v1.28.0"),
				"CONTROL_PLANE_MACHINE_COUNT": strconv.Itoa(pointer.IntDeref(cluster.ControlPlaneMachines, 1)),
				"WORKER_MACHINE_COUNT":        strconv.Itoa(pointer.IntDeref(cluster.WorkerMachines, 1)),

				// variables to set up govc for working with the vcsim instance.
				"GOVC_URL":      fmt.Sprintf("https://%s:%s@%s/sdk", vCenter.Status.Username, vCenter.Status.Password, strings.Replace(vCenter.Status.Host, r.PodIp, "127.0.0.1", 1)), // NOTE: reverting back to local host because the assumption is that the vcsim pod will be port-forwarded on local host
				"GOVC_INSECURE": "true",
			},
		}
		vCenter.Status.EnvSubst.Clusters[i] = clusterEnvVars
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

// generatePrivateKey creates a RSA Private Key of specified byte size
func generatePrivateKey(bitSize int) (*rsa.PrivateKey, error) {
	// Private Key generation
	privateKey, err := rsa.GenerateKey(rand.Reader, bitSize)
	if err != nil {
		return nil, err
	}

	// Validate Private Key
	err = privateKey.Validate()
	if err != nil {
		return nil, err
	}

	return privateKey, nil
}

// generatePublicKey take a rsa.PublicKey and return bytes suitable for writing to .pub file
// returns in the format "ssh-rsa ..."
func generatePublicKey(privatekey *rsa.PublicKey) ([]byte, error) {
	publicRsaKey, err := ssh.NewPublicKey(privatekey)
	if err != nil {
		return nil, err
	}

	pubKeyBytes := ssh.MarshalAuthorizedKey(publicRsaKey)

	return pubKeyBytes, nil
}
