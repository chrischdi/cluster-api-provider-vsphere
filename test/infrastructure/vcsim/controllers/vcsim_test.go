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
)

func Test_vcsim_NamesAndPath(t *testing.T) {
	g := NewWithT(t)

	datacenter := 5
	cluster := 3
	datastore := 7

	g.Expect(vcsimDatacenterName(datacenter), "DC5")
	g.Expect(vcsimClusterName(datacenter, cluster), "DC5_C3")
	g.Expect(vcsimClusterPath(datacenter, cluster), "/DC5/host/DC5_C3")
	g.Expect(vcsimDatastoreName(datastore), "LocalDS_7")
	g.Expect(vcsimDatastorePath(datacenter, datastore), "/DC5/datastore/LocalDS_7")
	g.Expect(vcsimResourcePoolPath(datacenter, datastore), "/DC5/host/DC5_C3/Resources")
	g.Expect(vcsimVMFolderName(datacenter), "DC5/vm")
	g.Expect(vcsimVMPath(datacenter, "my-mv"), "/DC5/vm/my-mv")
}

func Test_createVMTemplate(t *testing.T) {
	// TODO: implement
}
