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
	"fmt"
	"path"
	"strings"
	"testing"

	_ "github.com/dougm/pretty"
	. "github.com/onsi/gomega"
	_ "github.com/vmware/govmomi/govc/vm"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/pbm"
	pbmTypes "github.com/vmware/govmomi/pbm/types"
	vim25types "github.com/vmware/govmomi/vim25/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
	vcsimv1 "sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/api/v1alpha1"
)

func Test_Reconcile_Server(t *testing.T) {
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

	// Check the VCenter instance has been created in the reconciler internal status
	func() {
		r.lock.RLock()
		defer r.lock.RUnlock()

		key := klog.KObj(vCenter).String()
		g.Expect(r.vcsimInstances).To(HaveKey(key))
		g.Expect(r.vcsimInstances[key]).ToNot(BeNil())
	}()

	// Gets the reconciled object and tests if the VCenter instance actually works
	err = crclient.Get(ctx, client.ObjectKeyFromObject(vCenter), vCenter)
	g.Expect(err).ToNot(HaveOccurred())

	g.Expect(vCenter.Status.Host).ToNot(BeEmpty())
	g.Expect(vCenter.Status.Username).ToNot(BeEmpty())
	g.Expect(vCenter.Status.Password).ToNot(BeEmpty())

	params := session.NewParams().
		WithServer(fmt.Sprintf("https://%s", strings.Replace(vCenter.Status.Host, r.PodIp, "127.0.0.1", 1))). // TODO: use govc address
		WithThumbprint(vCenter.Status.Thumbprint).
		WithUserInfo(vCenter.Status.Username, vCenter.Status.Password)

	s, err := session.GetOrCreate(ctx, params)
	g.Expect(err).ToNot(HaveOccurred())

	v, err := s.GetVersion()
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(v).ToNot(BeEmpty())

	dc, err := s.Finder.Datacenter(ctx, "DC0")
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(dc).ToNot(BeNil())

	// PART 2: Should delete a VCenter
	err = crclient.Delete(ctx, vCenter)

	res, err = r.Reconcile(ctx, ctrl.Request{types.NamespacedName{
		Namespace: vCenter.Namespace,
		Name:      vCenter.Name,
	}})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	// Check the VCenter instance has been created in the reconciler internal status
	func() {
		r.lock.RLock()
		defer r.lock.RUnlock()

		key := klog.KObj(vCenter).String()
		g.Expect(r.vcsimInstances).ToNot(HaveKey(key))
	}()
}

func Test_govmomi_fix(t *testing.T) {
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
