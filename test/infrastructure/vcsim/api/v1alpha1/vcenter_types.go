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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// VCenterFinalizer allows VCenterReconciler to clean up resources associated with VCenter before
	// removing it from the API server.
	VCenterFinalizer = "vcenter.vcsim.infrastructure.cluster.x-k8s.io"
)

// VCCenterSpec defines the desired state of the VCenter.
type VCCenterSpec struct {
	Model *VCSimModelSpec `json:"model,omitempty"`
}

// VCSimModelSpec defines the model to be used by the VCenter.
type VCSimModelSpec struct {
	// VSphereVersion specifies the VSphere version to use
	// Default: 7.0.0
	VSphereVersion *string `json:"vsphereVersion,omitempty"`

	// Datacenter specifies the number of Datacenter entities to create
	// Name prefix: DC, vcsim flag: -dc
	// Default: 1
	Datacenter *int `json:"datacenter,omitempty"`

	// Cluster specifies the number of ClusterComputeResource entities to create per Datacenter
	// Name prefix: C, vcsim flag: -cluster
	// Default: 1
	Cluster *int `json:"cluster,omitempty"`

	// ClusterHost specifies the number of HostSystems entities to create within a Cluster
	// Name prefix: H, vcsim flag: -host
	// Default: 3
	ClusterHost *int `json:"clusterHost,omitempty"`

	// Pool specifies the number of ResourcePool entities to create per Cluster
	// Note that every cluster has a root ResourcePool named "Resources", as real vCenter does.
	// For example: /DC0/host/DC0_C0/Resources
	// The root ResourcePool is named "RP0" within other object names.
	// When Model.Pool is set to 1 or higher, this creates child ResourcePools under the root pool.
	// Note that this flag is not effective on standalone hosts.
	// For example: /DC0/host/DC0_C0/Resources/DC0_C0_RP1
	// Name prefix: RP, vcsim flag: -pool
	// Default: 0
	// TODO: model pool selection for each cluster; for now ResourcePool named "Resources" will be always used
	Pool *int `json:"pool,omitempty"`

	// Datastore specifies the number of Datastore entities to create
	// Each Datastore will have temporary local file storage and will be mounted
	// on every HostSystem created by the ModelConfig
	// Name prefix: LocalDS, vcsim flag: -ds
	// Default: 1
	Datastore *int `json:"datastore,omitempty"`

	// TODO: consider if to add options for creating more folders, networks, custom storage policies
}

// VCenterStatus defines the observed state of the VCenter.
type VCenterStatus struct {
	// The vcsim server  url's host.
	Host string `json:"host,omitempty"`

	// The vcsim server username.
	Username string `json:"username,omitempty"`

	// The vcsim server password.
	Password string `json:"password,omitempty"`

	// The vcsim server thumbprint.
	Thumbprint string `json:"thumbprint,omitempty"`

	// The output of a envsubst generator.
	EnvSubst EnvVars `json:"envsubst,omitempty"`
}

// EnvVars defines the output of a envsubst generator.
type EnvVars struct {
	// the output of a envsubst generator for each Cluster API cluster.
	Clusters []ClusterEnvVars `json:"clusters,omitempty"`
}

// ClusterEnvVars defines the output of a envsubst generator for a specific Cluster API cluster.
type ClusterEnvVars struct {
	// name of the Cluster API cluster.
	Name string `json:"name,omitempty"`

	// variables to use with envsubst when creating the Cluster API cluster.
	Variables map[string]string `json:"variables,omitempty"`
}

// +kubebuilder:resource:path=vcenters,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:object:root=true

// VCenter is the schema for a VCenter simulator server.
type VCenter struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VCCenterSpec  `json:"spec,omitempty"`
	Status VCenterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VCenterList contains a list of VCenter.
type VCenterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VCenter `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &VCenter{}, &VCenterList{})
}
