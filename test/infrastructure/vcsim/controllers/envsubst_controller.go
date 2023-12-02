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
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	vcsimv1 "sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/api/v1alpha1"
)

type EnvSubstReconciler struct {
	Client client.Client

	PodIp   string
	sshKeys map[string]string
	lock    sync.RWMutex

	// WatchFilterValue is the label value used to filter events prior to reconciliation.
	WatchFilterValue string
}

// +kubebuilder:rbac:groups=vcsim.infrastructure.cluster.x-k8s.io,resources=envsubsts,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=vcsim.infrastructure.cluster.x-k8s.io,resources=envsubsts/status,verbs=get;update;patch

func (r *EnvSubstReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the EnvSubst instance
	envSubst := &vcsimv1.EnvSubst{}
	if err := r.Client.Get(ctx, req.NamespacedName, envSubst); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Fetch the VCenter instance
	if envSubst.Spec.VCenter == "" {
		return ctrl.Result{}, errors.New("Spec.VCenter cannot be empty")
	}

	vCenter := &vcsimv1.VCenter{}
	if err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: envSubst.Namespace,
		Name:      envSubst.Spec.VCenter,
	}, vCenter); err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to get VCenter")
	}
	log = log.WithValues("VCenter", klog.KObj(vCenter))

	// Fetch the FakeAPIServerEndpoint instance
	if envSubst.Spec.Cluster.Name == "" {
		return ctrl.Result{}, errors.New("Spec.Cluster.Name cannot be empty")
	}

	fakeAPIServerEndpoint := &vcsimv1.FakeAPIServerEndpoint{}
	if err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: envSubst.Namespace,
		Name:      envSubst.Spec.Cluster.Name,
	}, fakeAPIServerEndpoint); err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed to get FakeAPIServerEndpoint")
	}
	log = log.WithValues("FakeAPIServerEndpoint", klog.KObj(fakeAPIServerEndpoint))

	ctx = ctrl.LoggerInto(ctx, log)

	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(envSubst, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always attempt to Patch the EnvSubst object and status after each reconciliation.
	defer func() {
		if err := patchHelper.Patch(ctx, envSubst); err != nil {
			log.Error(err, "failed to patch EnvSubst")
			if reterr == nil {
				reterr = err
			}
		}
	}()

	// Handle deleted EnvSubst
	if !fakeAPIServerEndpoint.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, envSubst, vCenter, fakeAPIServerEndpoint)
	}

	// Handle non-deleted EnvSubst
	return r.reconcileNormal(ctx, envSubst, vCenter, fakeAPIServerEndpoint)
}

func (r *EnvSubstReconciler) reconcileNormal(ctx context.Context, envSubst *vcsimv1.EnvSubst, vCenter *vcsimv1.VCenter, fakeAPIServerEndpoint *vcsimv1.FakeAPIServerEndpoint) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info("Reconciling VCSim ControlPlaneEndpoint")

	r.lock.Lock()
	defer r.lock.Unlock()

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

	envSubst.Status.Variables = map[string]string{
		// cluster template variables about the vcsim instance.
		"VSPHERE_SERVER":             fmt.Sprintf("https://%s", vCenter.Status.Host),
		"VSPHERE_PASSWORD":           vCenter.Status.Password,
		"VSPHERE_USERNAME":           vCenter.Status.Username,
		"VSPHERE_TLS_THUMBPRINT":     vCenter.Status.Thumbprint,
		"VSPHERE_DATACENTER":         fmt.Sprintf("DC%d", pointer.IntDeref(envSubst.Spec.Cluster.Datacenter, 0)),
		"VSPHERE_DATASTORE":          fmt.Sprintf("LocalDS_%d", pointer.IntDeref(envSubst.Spec.Cluster.Datastore, 0)),
		"VSPHERE_FOLDER":             fmt.Sprintf("/DC%d/vm", pointer.IntDeref(envSubst.Spec.Cluster.Datacenter, 0)),                                                                             // this is the default folder that gets created. TODO: consider if to make it possible to create more (this requires changes to the API)
		"VSPHERE_NETWORK":            fmt.Sprintf("/DC%d/network/VM Network", pointer.IntDeref(envSubst.Spec.Cluster.Datacenter, 0)),                                                             // this is the default network that gets created. TODO: consider if to make it possible to create more (this requires changes to the API)
		"VSPHERE_RESOURCE_POOL":      fmt.Sprintf("/DC%d/host/DC%[1]d_C%d/Resources", pointer.IntDeref(envSubst.Spec.Cluster.Datacenter, 0), pointer.IntDeref(envSubst.Spec.Cluster.Cluster, 0)), // all pool have RP as prefix. TODO: make it possible to pick one (0 --> Resources, >0 --> RPn)
		"VSPHERE_STORAGE_POLICY":     "vSAN Default Storage Policy",
		"VSPHERE_TEMPLATE":           fmt.Sprintf("/DC%d/vm/ubuntu-2204-kube-vX", pointer.IntDeref(envSubst.Spec.Cluster.Datacenter, 0)),
		"VSPHERE_SSH_AUTHORIZED_KEY": sshKey,

		// other variables required by the cluster template.
		"NAMESPACE":                   vCenter.Namespace,
		"CLUSTER_NAME":                envSubst.Spec.Cluster.Name,
		"KUBERNETES_VERSION":          pointer.StringDeref(envSubst.Spec.Cluster.KubernetesVersion, "v1.28.0"),
		"CONTROL_PLANE_MACHINE_COUNT": strconv.Itoa(pointer.IntDeref(envSubst.Spec.Cluster.ControlPlaneMachines, 1)),
		"WORKER_MACHINE_COUNT":        strconv.Itoa(pointer.IntDeref(envSubst.Spec.Cluster.WorkerMachines, 1)),

		// variables for the fake APIServer endpoint
		"CONTROL_PLANE_ENDPOINT_IP":   fakeAPIServerEndpoint.Status.Host,
		"CONTROL_PLANE_ENDPOINT_PORT": strconv.Itoa(fakeAPIServerEndpoint.Status.Port),

		// variables to set up govc for working with the vcsim instance.
		"GOVC_URL":      fmt.Sprintf("https://%s:%s@%s/sdk", vCenter.Status.Username, vCenter.Status.Password, strings.Replace(vCenter.Status.Host, r.PodIp, "127.0.0.1", 1)), // NOTE: reverting back to local host because the assumption is that the vcsim pod will be port-forwarded on local host
		"GOVC_INSECURE": "true",
	}

	return ctrl.Result{}, nil
}

func (r *EnvSubstReconciler) reconcileDelete(_ context.Context, _ *vcsimv1.EnvSubst, _ *vcsimv1.VCenter, _ *vcsimv1.FakeAPIServerEndpoint) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

// SetupWithManager will add watches for this controller.
func (r *EnvSubstReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	err := ctrl.NewControllerManagedBy(mgr).
		For(&vcsimv1.EnvSubst{}).
		WithOptions(options).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue)).
		Complete(r)

	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}
	return nil
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
