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
	"time"

	. "github.com/onsi/gomega"
	operatorv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/v1beta1"
	vmwarev1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/vmware/v1beta1"
	"sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/server/capi/cloud"
	"sigs.k8s.io/cluster-api-provider-vsphere/test/infrastructure/vcsim/server/capi/server"
)

func Test_Reconcile_VirtualMachine(t *testing.T) {
	t.Run("VirtualMachine not yet provisioned should be ignored", func(t *testing.T) {
		g := NewWithT(t)

		vsphereCluster := &vmwarev1.VSphereCluster{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				UID:       "bar",
			},
		}

		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				UID:       "bar",
			},
			Spec: clusterv1.ClusterSpec{
				InfrastructureRef: &corev1.ObjectReference{
					APIVersion: vmwarev1.GroupVersion.String(),
					Kind:       "VSphereCluster",
					Namespace:  vsphereCluster.Namespace,
					Name:       vsphereCluster.Name,
					UID:        vsphereCluster.UID,
				},
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

		vSphereMachine := &vmwarev1.VSphereMachine{
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

		virtualMachine := &operatorv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: vmwarev1.GroupVersion.String(),
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
		crclient := fake.NewClientBuilder().WithObjects(cluster, vsphereCluster, machine, vSphereMachine, virtualMachine).WithScheme(scheme).Build()

		// Start cloud manager & add a resourceGroup for the cluster
		cloudMgr := cloud.NewManager(cloudScheme)
		err := cloudMgr.Start(ctx)
		g.Expect(err).ToNot(HaveOccurred())

		cloudMgr.AddResourceGroup(klog.KObj(cluster).String())
		cloudClient := cloudMgr.GetResourceGroup(klog.KObj(cluster).String()).GetClient()

		r := VirtualMachineReconciler{
			Client:       crclient,
			CloudManager: cloudMgr,
		}

		// Reconcile
		res, err := r.Reconcile(ctx, ctrl.Request{types.NamespacedName{
			Namespace: virtualMachine.Namespace,
			Name:      virtualMachine.Name,
		}})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}))

		// Check the conditionsTracker is waiting for infrastructure ready
		conditionsTracker := &infrav1.VSphereVM{}
		err = cloudClient.Get(ctx, client.ObjectKeyFromObject(virtualMachine), conditionsTracker)
		g.Expect(err).ToNot(HaveOccurred())

		c := conditions.Get(conditionsTracker, VMProvisionedCondition)
		g.Expect(c.Status).To(Equal(corev1.ConditionFalse))
		g.Expect(c.Severity).To(Equal(clusterv1.ConditionSeverityInfo))
		g.Expect(c.Reason).To(Equal(WaitingControlPlaneInitializedReason))
	})

	t.Run("VirtualMachine provisioned gets a node (worker)", func(t *testing.T) {
		g := NewWithT(t)

		vsphereCluster := &vmwarev1.VSphereCluster{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				UID:       "bar",
			},
		}

		cluster := &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				UID:       "bar",
			},
			Spec: clusterv1.ClusterSpec{
				InfrastructureRef: &corev1.ObjectReference{
					APIVersion: vmwarev1.GroupVersion.String(),
					Kind:       "VSphereCluster",
					Namespace:  vsphereCluster.Namespace,
					Name:       vsphereCluster.Name,
					UID:        vsphereCluster.UID,
				},
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

		vSphereMachine := &vmwarev1.VSphereMachine{
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

		virtualMachine := &operatorv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "foo",
				Name:      "bar",
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: vmwarev1.GroupVersion.String(),
						Kind:       "VSphereMachine",
						Name:       vSphereMachine.Name,
						UID:        vSphereMachine.UID,
					},
				},
				Finalizers: []string{
					VMFinalizer, // Adding this to move past the first reconcile
				},
			},
			Status: operatorv1.VirtualMachineStatus{
				// Those values are required to unblock provisioning of node
				BiosUUID:   "foo",
				VmIp:       "1.2.3.4",
				PowerState: operatorv1.VirtualMachinePoweredOn,
			},
		}

		// Controller runtime client
		crclient := fake.NewClientBuilder().WithObjects(cluster, vsphereCluster, machine, vSphereMachine, virtualMachine).WithScheme(scheme).Build()

		// Start cloud manager & add a resourceGroup for the cluster
		cloudMgr := cloud.NewManager(cloudScheme)
		err := cloudMgr.Start(ctx)
		g.Expect(err).ToNot(HaveOccurred())

		cloudMgr.AddResourceGroup(klog.KObj(cluster).String())
		cloudClient := cloudMgr.GetResourceGroup(klog.KObj(cluster).String()).GetClient()

		// Start an http server
		apiServerMux, err := server.NewWorkloadClustersMux(cloudMgr, "127.0.0.1")
		g.Expect(err).ToNot(HaveOccurred())

		r := VirtualMachineReconciler{
			Client:       crclient,
			CloudManager: cloudMgr,
			APIServerMux: apiServerMux,
		}

		// Reconcile
		nodeStartupDuration = 0 * time.Second

		res, err := r.Reconcile(ctx, ctrl.Request{types.NamespacedName{
			Namespace: virtualMachine.Namespace,
			Name:      virtualMachine.Name,
		}})
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(res).To(Equal(ctrl.Result{}))

		// Check the mirrorVSphereMachine reports all provisioned

		conditionsTracker := &infrav1.VSphereVM{}
		err = cloudClient.Get(ctx, client.ObjectKeyFromObject(virtualMachine), conditionsTracker)
		g.Expect(err).ToNot(HaveOccurred())

		c := conditions.Get(conditionsTracker, NodeProvisionedCondition)
		g.Expect(c.Status).To(Equal(corev1.ConditionTrue))
	})
}
