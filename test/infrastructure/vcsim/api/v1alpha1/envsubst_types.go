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

// EnvSubstSpec defines the desired state of the EnvSubst.
type EnvSubstSpec struct {
	VCenter string              `json:"vCenter,omitempty"`
	Cluster ClusterEnvSubstSpec `json:"cluster,omitempty"`
}

// ClusterEnvSubstSpec defines the spec for the envsubst generator targeting a specific Cluster API cluster.
type ClusterEnvSubstSpec struct {
	// The name of the Cluster API cluster.
	Name string `json:"name"`

	// The Kubernetes version of the Cluster API cluster.
	// NOTE: This variable isn't related to the vcsim server, but we are handling it here
	// in order to have a single point of control for all the variables related to a Cluster API template.
	// Default: v1.28.0
	KubernetesVersion *string `json:"kubernetesVersion,omitempty"`

	// The number of the control plane machines in the Cluster API cluster.
	// NOTE: This variable isn't related to the vcsim server, but we are handling it here
	// in order to have a single point of control for all the variables related to a Cluster API template.
	// Default: 1
	ControlPlaneMachines *int `json:"controlPlaneMachines,omitempty"`

	// The number of the worker machines in the Cluster API cluster.
	// NOTE: This variable isn't related to the vcsim server, but we are handling it here
	// in order to have a single point of control for all the variables related to a Cluster API template.
	// Default: 1
	WorkerMachines *int `json:"workerMachines,omitempty"`

	// Datacenter specifies the Datacenter for the Cluster API cluster.
	// Default: 0 (DC0)
	Datacenter *int `json:"datacenter,omitempty"`

	// Cluster specifies the VCenter Cluster for the Cluster API cluster.
	// Default: 0 (C0)
	Cluster *int `json:"cluster,omitempty"`

	// Datastore specifies the Datastore for the Cluster API cluster.
	// Default: 0 (LocalDS_0)
	Datastore *int `json:"datastore,omitempty"`

	// TODO: model pool selection; if not specified, root ResourcePool named "Resources" will be used
}

// EnvSubstStatus defines the observed state of the EnvSubst.
type EnvSubstStatus struct {
	// variables to use with envsubst when creating the Cluster API cluster.
	Variables map[string]string `json:"variables,omitempty"`
}

// +kubebuilder:resource:path=envsubsts,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:object:root=true

// EnvSubst is the schema for a EnvSubst generator.
type EnvSubst struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EnvSubstSpec   `json:"spec,omitempty"`
	Status EnvSubstStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EnvSubstList contains a list of VCenter.
type EnvSubstList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EnvSubst `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &EnvSubst{}, &EnvSubstList{})
}
