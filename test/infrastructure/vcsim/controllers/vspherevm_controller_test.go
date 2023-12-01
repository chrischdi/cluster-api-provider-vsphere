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
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/pbm"
	pbmTypes "github.com/vmware/govmomi/pbm/types"
	vim25types "github.com/vmware/govmomi/vim25/types"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
	vcsimv1 "sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/api/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/server/capi/cloud"
	"sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/server/capi/server"
)

var (
	cloudScheme = runtime.NewScheme()
	scheme      = runtime.NewScheme()

	ctx = context.Background()
)

func init() {
	// scheme used for operating on the management cluster.
	_ = clusterv1.AddToScheme(scheme)
	_ = infrav1.AddToScheme(scheme)
	_ = vcsimv1.AddToScheme(scheme)

	// scheme used for operating on the cloud resource.
	_ = infrav1.AddToScheme(cloudScheme)
	_ = corev1.AddToScheme(cloudScheme)
	_ = appsv1.AddToScheme(cloudScheme)
	_ = rbacv1.AddToScheme(cloudScheme)
}

func Test_Reconcile_VSphereVM(t *testing.T) {
	t.Run("VSphereMachine not yet provisioned should be ignored", func(t *testing.T) {
		g := NewWithT(t)

		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				UID:       "bar",
			},
			Spec: clusterv1.ClusterSpec{
				InfrastructureRef: &corev1.ObjectReference{
					APIVersion: infrav1.GroupVersion.String(),
					Kind:       "",
					Namespace:  "foo",
					Name:       "bar",
					UID:        "bar",
				},
			},
		}

		vsphereCluster := &infrav1.VSphereCluster{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				UID:       "bar",
			},
		}

		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				Labels: map[string]string{
					clusterv1.ClusterNameLabel: cluster.Name,
				},
			},
		}

		vSphereMachine := &infrav1.VSphereMachine{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "baz",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: clusterv1.GroupVersion.String(),
						Kind:       "Machine",
						Name:       machine.Name,
						UID:        machine.UID,
					},
				},
			},
		}

		vSphereVM := &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: infrav1.GroupVersion.String(),
						Kind:       "VSphereMachine",
						Name:       vSphereMachine.Name,
						UID:        vSphereMachine.UID,
					},
				},
				Finalizers: []string{
					VMFinalizer, // Adding this to move past the first reconcile
				},
			},
		}

		// Controller runtime client
		crclient := fake.NewClientBuilder().WithObjects(cluster, vsphereCluster, machine, vSphereMachine, vSphereVM).WithScheme(scheme).Build()

		// Start cloud manager & add a resourceGroup for the cluster
		cloudMgr := cloud.NewManager(cloudScheme)
		err := cloudMgr.Start(ctx)
		g.Expect(err).ToNot(HaveOccurred())

		cloudMgr.AddResourceGroup(klog.KObj(cluster).String())
		cloudClient := cloudMgr.GetResourceGroup(klog.KObj(cluster).String()).GetClient()

		// Start an http server
		apiServerMux, err := server.NewWorkloadClustersMux(cloudMgr, "127.0.0.1")
		g.Expect(err).ToNot(HaveOccurred())

		r := VSphereVMReconciler{
			Client:       crclient,
			CloudManager: cloudMgr,
			APIServerMux: apiServerMux,
		}

		// Reconcile
		res, err := r.Reconcile(ctx, ctrl.Request{types.NamespacedName{
			Namespace: vSphereVM.Namespace,
			Name:      vSphereVM.Name,
		}})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}))

		// Check the mirrorVSphereVM is waiting for infrastructure ready
		mirrorVSphereVM := &infrav1.VSphereVM{}
		err = cloudClient.Get(ctx, client.ObjectKeyFromObject(vSphereVM), mirrorVSphereVM)
		g.Expect(err).ToNot(HaveOccurred())

		c := conditions.Get(mirrorVSphereVM, NodeProvisionedCondition)
		g.Expect(c.Status).To(Equal(corev1.ConditionFalse))
		g.Expect(c.Severity).To(Equal(clusterv1.ConditionSeverityInfo))
		g.Expect(c.Reason).To(Equal(NodeWaitingForInfrastructureReadyReason))
	})

	t.Run("VSphereMachine provisioned gets a node (worker)", func(t *testing.T) {
		g := NewWithT(t)

		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				UID:       "bar",
			},
			Spec: clusterv1.ClusterSpec{
				InfrastructureRef: &corev1.ObjectReference{
					APIVersion: infrav1.GroupVersion.String(),
					Kind:       "",
					Namespace:  "foo",
					Name:       "bar",
					UID:        "bar",
				},
			},
		}

		vsphereCluster := &infrav1.VSphereCluster{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				UID:       "bar",
			},
		}

		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				Labels: map[string]string{
					clusterv1.ClusterNameLabel: cluster.Name,
				},
			},
			Spec: clusterv1.MachineSpec{
				Bootstrap: clusterv1.Bootstrap{
					DataSecretName: pointer.String("foo"), // this unblocks node provisioning
				},
			},
		}

		vSphereMachine := &infrav1.VSphereMachine{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: clusterv1.GroupVersion.String(),
						Kind:       "Machine",
						Name:       machine.Name,
						UID:        machine.UID,
					},
				},
			},
		}

		vSphereVM := &infrav1.VSphereVM{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: infrav1.GroupVersion.String(),
						Kind:       "VSphereMachine",
						Name:       vSphereMachine.Name,
						UID:        vSphereMachine.UID,
					},
				},
				Finalizers: []string{
					VMFinalizer, // Adding this to move past the first reconcile
				},
			},
			Status: infrav1.VSphereVMStatus{
				Ready: true, // This unblocks provisioning of node
			},
		}

		// Controller runtime client
		crclient := fake.NewClientBuilder().WithObjects(cluster, vsphereCluster, machine, vSphereMachine, vSphereVM).WithScheme(scheme).Build()

		// Start cloud manager & add a resourceGroup for the cluster
		cloudMgr := cloud.NewManager(cloudScheme)
		err := cloudMgr.Start(ctx)
		g.Expect(err).ToNot(HaveOccurred())

		cloudMgr.AddResourceGroup(klog.KObj(cluster).String())
		cloudClient := cloudMgr.GetResourceGroup(klog.KObj(cluster).String()).GetClient()

		// Start an http server
		apiServerMux, err := server.NewWorkloadClustersMux(cloudMgr, "127.0.0.1")
		g.Expect(err).ToNot(HaveOccurred())

		r := VSphereVMReconciler{
			Client:       crclient,
			CloudManager: cloudMgr,
			APIServerMux: apiServerMux,
		}

		// Reconcile
		res, err := r.Reconcile(ctx, ctrl.Request{types.NamespacedName{
			Namespace: vSphereVM.Namespace,
			Name:      vSphereVM.Name,
		}})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}))

		// Check the mirrorVSphereMachine reports all provisioned
		// TODO: check things actually exists, server started etc.

		mirrorVSphereVM := &infrav1.VSphereVM{}
		err = cloudClient.Get(ctx, client.ObjectKeyFromObject(vSphereVM), mirrorVSphereVM)
		g.Expect(err).ToNot(HaveOccurred())

		c := conditions.Get(mirrorVSphereVM, NodeProvisionedCondition)
		g.Expect(c.Status).To(Equal(corev1.ConditionTrue))
	})
}

func Test_FOO(t *testing.T) {
	g := NewWithT(t)

	vCenter := &vcsimv1.VCenter{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceDefault,
			Name:      "foo",
			Finalizers: []string{
				vcsimv1.VCenterFinalizer, // Adding this to move past the first reconcile
			},
		},
		Spec: vcsimv1.VCCenterSpec{},
	}

	crclient := fake.NewClientBuilder().WithObjects(vCenter).WithStatusSubresource(vCenter).WithScheme(scheme).Build()
	r := &VCenterReconciler{
		Client: crclient,
		PodIp:  "127.0.0.1",
	}

	// PART 1: Should create a new VCenter

	res, err := r.Reconcile(ctx, ctrl.Request{types.NamespacedName{
		Namespace: vCenter.Namespace,
		Name:      vCenter.Name,
	}})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	// Gets the reconciled object and tests if the VCenter instance actually works
	err = crclient.Get(ctx, client.ObjectKeyFromObject(vCenter), vCenter)
	g.Expect(err).ToNot(HaveOccurred())

	params := session.NewParams().
		WithServer(fmt.Sprintf("https://%s", strings.Replace(vCenter.Status.Host, r.PodIp, "127.0.0.1", 1))). // TODO: use govc address
		WithThumbprint(vCenter.Status.Thumbprint).
		WithUserInfo(vCenter.Status.Username, vCenter.Status.Password)

	s, err := session.GetOrCreate(ctx, params)
	g.Expect(err).ToNot(HaveOccurred())

	pbmClient, err := pbm.NewClient(ctx, s.Client.Client)
	g.Expect(err).ToNot(HaveOccurred())

	folder, err := s.Finder.FolderOrDefault(ctx, "/DC0/vm")
	g.Expect(err).ToNot(HaveOccurred())

	inventoryPath := path.Join(folder.InventoryPath, "ubuntu-2204-kube-vX")
	vm, err := s.Finder.VirtualMachine(ctx, inventoryPath)
	g.Expect(err).ToNot(HaveOccurred())

	Obj := object.NewVirtualMachine(s.Client.Client, vm.Reference())

	devices, err := Obj.Device(ctx)
	g.Expect(err).ToNot(HaveOccurred())

	disksRefs := make([]pbmTypes.PbmServerObjectRef, 0)
	// diskMap is just an auxiliar map so we don't need to iterate over and over disks to get their configs
	// if we realize they are not on the right storage policy
	diskMap := make(map[string]*vim25types.VirtualDisk)

	disks := devices.SelectByType((*vim25types.VirtualDisk)(nil))

	// We iterate over disks and create an array of disks refs, so we just need to make a single call
	// against vCenter, instead of one call per disk
	// the diskMap is an auxiliar way of, besides the disksRefs, we have a "searchable" disk configuration
	// in case we need to reconfigure a disk, to get its config
	for _, d := range disks {
		disk := d.(*vim25types.VirtualDisk)
		// entities associated with storage policy has key in the form <vm-ID>:<disk>
		diskID := fmt.Sprintf("%s:%d", Obj.Reference().Value, disk.Key)
		diskMap[diskID] = disk

		disksRefs = append(disksRefs, pbmTypes.PbmServerObjectRef{
			ObjectType: string(pbmTypes.PbmObjectTypeVirtualDiskId),
			Key:        diskID,
		})
	}

	diskObjects, err := pbmClient.QueryAssociatedProfiles(ctx, disksRefs)
	g.Expect(err).ToNot(HaveOccurred())
	fmt.Print(diskObjects)
}
