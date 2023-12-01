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
	// VCSimControlPlaneEndpointFinalizer allows VCSimControlPlaneEndpointReconciler to clean up resources associated with VCSimControlPlaneEndpoint before
	// removing it from the API server.
	VCSimControlPlaneEndpointFinalizer = "controlplaneendpoint.vcsim.infrastructure.cluster.x-k8s.io"
)

// VCSimControlPlaneEndpointSpec defines the desired state of the VCSimControlPlaneEndpoint.
type VCSimControlPlaneEndpointSpec struct {
}

// VCSimControlPlaneEndpointClusterStatus defines the observed state of the VCSimControlPlaneEndpoint.
type VCSimControlPlaneEndpointClusterStatus struct {
	// The control plane host.
	Host string `json:"host,omitempty"`

	// The control plane port.
	Port int `json:"port,omitempty"`

	// The output of a envsubst generator, which is automatically triggered
	// in order to make it easier to consume the endpoint from a Cluster API template.
	// NOTE: Cluster API cluster name must be equal to the control plane endpoint name
	// in order to comply with the assumptions hard coded in the fake API server implementation.
	EnvSubst EnvVars `json:"envsubst,omitempty"`
}

// +kubebuilder:resource:path=vcsimcontrolplaneendpoints,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:object:root=true

// VCSimControlPlaneEndpoint is the schema for a VCenter simulator server.
type VCSimControlPlaneEndpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VCSimControlPlaneEndpointSpec          `json:"spec,omitempty"`
	Status VCSimControlPlaneEndpointClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VCSimControlPlaneEndpointList contains a list of VCSimControlPlaneEndpoint.
type VCSimControlPlaneEndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VCSimControlPlaneEndpoint `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &VCSimControlPlaneEndpoint{}, &VCSimControlPlaneEndpointList{})
}
