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
	"testing"

	_ "github.com/dougm/pretty"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	_ "github.com/vmware/govmomi/govc/vm"

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
		WithServer(vCenter.Status.Host).
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
