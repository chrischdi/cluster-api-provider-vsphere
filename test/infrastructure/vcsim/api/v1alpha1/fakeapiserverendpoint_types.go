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
	// FakeAPIServerEndpointFinalizer allows FakeAPIServerEndpointReconciler to clean up resources associated with FakeAPIServerEndpoint before
	// removing it from the API server.
	FakeAPIServerEndpointFinalizer = "fakeapiserverendpoint.vcsim.infrastructure.cluster.x-k8s.io"
)

// FakeAPIServerEndpointSpec defines the desired state of the FakeAPIServerEndpoint.
type FakeAPIServerEndpointSpec struct {
}

// FakeAPIServerEndpointStatus defines the observed state of the FakeAPIServerEndpoint.
type FakeAPIServerEndpointStatus struct {
	// The control plane host.
	Host string `json:"host,omitempty"`

	// The control plane port.
	Port int `json:"port,omitempty"`
}

// +kubebuilder:resource:path=fakeapiserverendpoints,scope=Namespaced,categories=cluster-api
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:object:root=true

// FakeAPIServerEndpoint is the schema for a VCenter simulator server.
type FakeAPIServerEndpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FakeAPIServerEndpointSpec   `json:"spec,omitempty"`
	Status FakeAPIServerEndpointStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FakeAPIServerEndpointList contains a list of FakeAPIServerEndpoint.
type FakeAPIServerEndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FakeAPIServerEndpoint `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &FakeAPIServerEndpoint{}, &FakeAPIServerEndpointList{})
}
