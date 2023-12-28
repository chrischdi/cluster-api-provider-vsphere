package controllers

import (
	"context"
	"os"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	operatorv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha1"
	topologyv1 "github.com/vmware-tanzu/vm-operator/external/tanzu-topology/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	vmwarev1 "sigs.k8s.io/cluster-api-provider-vsphere/apis/vmware/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

/*
cat << EOF > /tmp/testbed.yaml
ServerUrl: "${VSPHERE_SERVER}:443"
Username: "${VSPHERE_USERNAME}"
Password: "${VSPHERE_PASSWORD}"
Thumbprint: "${VSPHERE_TLS_THUMBPRINT}"
Datacenter: "${VSPHERE_DATACENTER}"
Cluster: "/${VSPHERE_DATACENTER}/host/cluster0"
Folder: "${VSPHERE_FOLDER}"
ResourcePool: "/${VSPHERE_DATACENTER}/host/cluster0/Resources/${VSPHERE_RESOURCE_POOL}"
StoragePolicyID: "${VSPHERE_STORAGE_POLICY}"
ContentLibrary:
  Name: "capv"
  Datastore: "/${VSPHERE_DATACENTER}/datastore/${VSPHERE_DATASTORE}"
EOF
*/

func Test_reconcileVMOperatorDeployment(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = storagev1.AddToScheme(scheme)
	_ = operatorv1.AddToScheme(scheme)
	_ = vmwarev1.AddToScheme(scheme)
	_ = topologyv1.AddToScheme(scheme)

	const (
		kubeconfigPath  = "/tmp/capi-test.kubeconfig"
		testbedYamlPath = "/tmp/testbed.yaml"
	)
	g := NewWithT(t)

	ctx := context.Background()

	vcenterClusterConfig := VCenterClusterConfig{}
	testbedData, err := os.ReadFile(testbedYamlPath)
	g.Expect(yaml.Unmarshal(testbedData, &vcenterClusterConfig)).ToNot(HaveOccurred())

	config := VMOperatorDeploymentConfig{
		Namespace: "vmware-system-vmop",
		UserNamespace: UserNamespaceConfig{
			Name:         "default", // namespace where we deploy a cluster
			StorageClass: "tkg-shared-ds-sp",
		},
		VCenterCluster: vcenterClusterConfig,
	}

	config.VCenterCluster.ContentLibrary.Item = ContentLibraryItemConfig{
		Name: "ubuntu-2204-kube-v1.29.0",
	}

	// create a config

	// Create a client.Client from a kubeconfig
	kubeconfig, err := os.ReadFile(kubeconfigPath)
	g.Expect(err).ToNot(HaveOccurred())

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	g.Expect(err).ToNot(HaveOccurred())

	restConfig.Timeout = 10 * time.Second

	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	g.Expect(err).ToNot(HaveOccurred())

	// reconcile
	got, err := reconcileVMOperatorDeployment(ctx, c, config)
	g.Expect(err).ToNot(HaveOccurred())
	_ = got
}
