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

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	vcsimv1 "sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/api/v1alpha1"
	fakeManager "sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/server/capi/cloud"
	fakeAPIServer "sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/server/capi/server"
)

func Test_Reconcile_ControlPlaneEndpoint(t *testing.T) {
	g := NewWithT(t)

	// Start a manager to handle resources that we are going to store in the fake API servers for the workload clusters.
	workloadClustersManager := fakeManager.NewManager(cloudScheme)
	err := workloadClustersManager.Start(ctx)
	g.Expect(err).ToNot(HaveOccurred())

	// Start an Mux for the API servers for the workload clusters.
	podIP := "127.0.0.1"
	workloadClustersMux, err := fakeAPIServer.NewWorkloadClustersMux(workloadClustersManager, podIP, fakeAPIServer.CustomPorts{
		// NOTE: make sure to use ports different than other tests, so we can run tests in parallel
		MinPort:   fakeAPIServer.DefaultMinPort + 100,
		MaxPort:   fakeAPIServer.DefaultMinPort + 199,
		DebugPort: fakeAPIServer.DefaultDebugPort + 1,
	})
	g.Expect(err).ToNot(HaveOccurred())

	fakeAPIServerEndpoint := &vcsimv1.FakeAPIServerEndpoint{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceDefault,
			Name:      "foo",
			Finalizers: []string{
				vcsimv1.FakeAPIServerEndpointFinalizer, // Adding this to move past the first reconcile
			},
		},
	}

	crclient := fake.NewClientBuilder().WithObjects(fakeAPIServerEndpoint).WithStatusSubresource(fakeAPIServerEndpoint).WithScheme(scheme).Build()
	r := &FakeAPIServerEndpointReconciler{
		Client:       crclient,
		CloudManager: workloadClustersManager,
		APIServerMux: workloadClustersMux,
		PodIp:        podIP,
	}

	// PART 1: Should create a new FakeAPIServerEndpoint

	res, err := r.Reconcile(ctx, ctrl.Request{types.NamespacedName{
		Namespace: fakeAPIServerEndpoint.Namespace,
		Name:      fakeAPIServerEndpoint.Name,
	}})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	// Gets the reconciled object
	err = crclient.Get(ctx, client.ObjectKeyFromObject(fakeAPIServerEndpoint), fakeAPIServerEndpoint)
	g.Expect(err).ToNot(HaveOccurred())

	g.Expect(fakeAPIServerEndpoint.Status.Host).ToNot(BeEmpty())
	g.Expect(fakeAPIServerEndpoint.Status.Port).ToNot(BeZero())

	// Check manager and server internal status
	resourceGroup := klog.KObj(fakeAPIServerEndpoint).String()
	foo := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo",
		},
	}
	g.Expect(workloadClustersManager.GetResourceGroup(resourceGroup).GetClient().Create(ctx, foo)).To(Succeed()) // the operation succeed if the resource group has been created as expected
	g.Expect(workloadClustersMux.ListListeners()).To(HaveKey(resourceGroup))

	// PART 2: Should delete a FakeAPIServerEndpoint

	err = crclient.Delete(ctx, fakeAPIServerEndpoint)

	res, err = r.Reconcile(ctx, ctrl.Request{types.NamespacedName{
		Namespace: fakeAPIServerEndpoint.Namespace,
		Name:      fakeAPIServerEndpoint.Name,
	}})
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	// Check manager and server internal status
	g.Expect(workloadClustersManager.GetResourceGroup(resourceGroup).GetClient().Create(ctx, foo)).ToNot(Succeed()) // the operation fails if the resource group has been deleted as expected
	g.Expect(workloadClustersMux.ListListeners()).ToNot(HaveKey(resourceGroup))
}
