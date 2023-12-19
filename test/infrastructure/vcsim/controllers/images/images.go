package images

import (
	_ "embed"
)

var (
	//go:embed ttylinux-pc_i486-16.1.ovf
	SampleOVF []byte

	//go:embed ttylinux-pc_i486-16.1-disk1.vmdk
	SampleVMDK []byte
)
