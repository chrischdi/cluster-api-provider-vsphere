/*
Copyright 2022 The Kubernetes Authors.

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

package vmware

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	capiutil "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	vmwarev1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/vmware/v1beta1"
)

// IntegrationTestContext is used for integration testing
// Supervisor controllers.
type IntegrationTestContext struct {
	context.Context
	Client            client.Client
	GuestClient       client.Client
	GuestAPIReader    client.Client
	Namespace         string
	VSphereCluster    *vmwarev1.VSphereCluster
	Cluster           *clusterv1.Cluster
	KubeconfigSecret  *corev1.Secret
	VSphereClusterKey client.ObjectKey
	envTest           *envtest.Environment
}

func (*IntegrationTestContext) GetLogger() logr.Logger {
	return logr.Discard()
}

// AfterEach should be invoked by ginko.AfterEach to stop the guest cluster's
// API server.
func (ctx *IntegrationTestContext) AfterEach() {
	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ctx.Namespace,
		},
	}
	By("Destroying integration test namespace")
	Expect(ctx.Client.Delete(ctx, namespace)).To(Succeed())

	if ctx.envTest != nil {
		By("Shutting down guest cluster control plane")
		Expect(ctx.envTest.Stop()).To(Succeed())
	}
}

// NewIntegrationTestContextWithClusters should be invoked by ginkgo.BeforeEach.
//
// This function creates a VSphereCluster with a generated name as well as a
// CAPI Cluster with the same name. The function also creates a test environment
// and starts its API server to serve as the control plane endpoint for the
// guest cluster.
//
// This function returns a IntegrationTest context.
//
// The resources created by this function may be cleaned up by calling AfterEach
// with the IntegrationTestContext returned by this function.
func NewIntegrationTestContextWithClusters(goctx context.Context, integrationTestClient client.Client) *IntegrationTestContext {
	ctx := &IntegrationTestContext{
		Context: goctx,
		Client:  integrationTestClient,
	}

	By("Creating a temporary namespace", func() {
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: uuid.New().String(),
			},
		}
		Expect(ctx.Client.Create(goctx, namespace)).To(Succeed())

		ctx.Namespace = namespace.Name
	})

	vsphereClusterName := capiutil.RandomString(6)
	ctx.Cluster = createCluster(ctx.Namespace, vsphereClusterName)

	var config *rest.Config
	By("Creating guest cluster control plane", func() {
		// Initialize a test environment to simulate the control plane of the guest cluster.
		var err error
		envTest := &envtest.Environment{
			// Add some form of CRD so the CRD object is registered in the
			// scheme...
			CRDDirectoryPaths: []string{
				filepath.Join("../../", "config", "default", "crd"),
				filepath.Join("../../", "config", "supervisor", "crd"),
			},
		}
		envTest.ControlPlane.GetAPIServer().Configure().Set("allow-privileged", "true")
		config, err = envTest.Start()
		Expect(err).ShouldNot(HaveOccurred())
		Expect(config).ShouldNot(BeNil())

		ctx.GuestClient, err = client.New(config, client.Options{})
		Expect(err).ShouldNot(HaveOccurred())
		Expect(ctx.GuestClient).ShouldNot(BeNil())

		// Create the API Reader, a client with no cache.
		ctx.GuestAPIReader, err = client.New(config, client.Options{
			Scheme: ctx.GuestClient.Scheme(),
			Mapper: ctx.GuestClient.RESTMapper(),
		})
		Expect(err).ShouldNot(HaveOccurred())

		ctx.envTest = envTest
	})
	By("Create the kubeconfig secret for the cluster", func() {
		buf, err := writeKubeConfig(config)
		Expect(err).ToNot(HaveOccurred())
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ctx.Namespace,
				Name:      fmt.Sprintf("%s-kubeconfig", ctx.Cluster.Name),
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: clusterv1.GroupVersion.String(),
						Kind:       "Cluster",
						Name:       ctx.Cluster.Name,
						// Using a random id in this case is okay because we don't rely on it to be the correct uid.
						UID: "should-be-uid-of-cluster",
					},
				},
			},
			Data: map[string][]byte{
				"value": buf,
			},
		}
		ctx.KubeconfigSecret = secret
	})

	By("Create a vsphere cluster and wait for it to exist", func() {
		ctx.VSphereCluster = createVSphereCluster(ctx.Namespace, vsphereClusterName, ctx.Cluster.GetName())
		ctx.VSphereClusterKey = client.ObjectKeyFromObject(ctx.VSphereCluster)
	})

	return ctx
}

// CreateAndWait creates and waits for an object to exist.
func CreateAndWait(ctx context.Context, integrationTestClient client.Client, obj client.Object) {
	GinkgoHelper()
	Expect(integrationTestClient.Create(ctx, obj)).To(Succeed())
	Eventually(func() error {
		return integrationTestClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)
	}).Should(Succeed())
}

func createCluster(namespace, name string) *clusterv1.Cluster {
	By("Create the CAPI Cluster and wait for it to exist")
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      fmt.Sprintf("%s-%s", name, capiutil.RandomString(6)),
		},
		Spec: clusterv1.ClusterSpec{
			ClusterNetwork: &clusterv1.ClusterNetwork{
				Pods: &clusterv1.NetworkRanges{
					CIDRBlocks: []string{"1.0.0.0/16"},
				},
				Services: &clusterv1.NetworkRanges{
					CIDRBlocks: []string{"2.0.0.0/16"},
				},
			},
			InfrastructureRef: &corev1.ObjectReference{
				Name:      name,
				Namespace: namespace,
			},
		},
	}
	return cluster
}

func createVSphereCluster(namespace, name, capiClusterName string) *vmwarev1.VSphereCluster {
	vsphereCluster := &vmwarev1.VSphereCluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: capiClusterName},
		},
	}
	return vsphereCluster
}

// writeKubeConfig writes an existing *rest.Config out as the typical kubeconfig YAML data.
func writeKubeConfig(config *rest.Config) ([]byte, error) {
	return clientcmd.Write(api.Config{
		Clusters: map[string]*api.Cluster{
			config.ServerName: {
				Server:                   config.Host,
				CertificateAuthorityData: config.CAData,
			},
		},
		Contexts: map[string]*api.Context{
			config.ServerName: {
				Cluster:  config.ServerName,
				AuthInfo: config.Username,
			},
		},
		AuthInfos: map[string]*api.AuthInfo{
			config.Username: {
				ClientKeyData:         config.KeyData,
				ClientCertificateData: config.CertData,
			},
		},
		CurrentContext: config.ServerName,
	})
}
