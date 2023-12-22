package controllers

import (
	"context"
	"time"

	"github.com/pkg/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/vmware/govmomi/vim25/types"
	govmominet "sigs.k8s.io/cluster-api-provider-vsphere/pkg/services/govmomi/net"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/session"
)

type vmIPReconciler struct {
	Client            client.Client
	EnableKeepAlive   bool
	KeepAliveDuration time.Duration

	IsVMWaitingforIP  func() bool
	GetVCenterSession func(ctx context.Context) (*session.Session, error)
	GetVMPath         func() string
}

func (r *vmIPReconciler) ReconcileIP(ctx context.Context) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// If the VM is still provisioning, or it already has an IP, return.
	if !r.IsVMWaitingforIP() {
		return reconcile.Result{}, nil
	}

	// Otherwise the VM is stuck waiting for IP (because there is no DHCP service in vcsim), then assign a fake IP.

	authSession, err := r.GetVCenterSession(ctx)
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to get vcenter session")
	}

	vm, err := authSession.Finder.VirtualMachine(ctx, r.GetVMPath())
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to find vm")
	}

	// Check if the VM already has network status (but it is not yet surfaced in conditions)
	netStatus, err := govmominet.GetNetworkStatus(ctx, authSession.Client.Client, vm.Reference())
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to get vm network status")
	}
	ipAddrs := []string{}
	for _, s := range netStatus {
		ipAddrs = append(ipAddrs, s.IPAddrs...)
	}
	if len(ipAddrs) > 0 {
		return reconcile.Result{RequeueAfter: 5 * time.Second}, nil // Wait for the ip address to surface in K8s resources
	}

	task, err := vm.PowerOff(ctx)
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to PowerOff vm")
	}
	if err = task.Wait(ctx); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to PowerOff vm task to complete")
	}

	// Add a fake ip address.
	spec := types.CustomizationSpec{
		NicSettingMap: []types.CustomizationAdapterMapping{
			{
				Adapter: types.CustomizationIPSettings{
					Ip: &types.CustomizationFixedIp{
						IpAddress: "192.168.1.100",
					},
					SubnetMask:    "255.255.255.0",
					Gateway:       []string{"192.168.1.1"},
					DnsServerList: []string{"192.168.1.1"},
					DnsDomain:     "ad.domain",
				},
			},
		},
		Identity: &types.CustomizationLinuxPrep{
			HostName: &types.CustomizationFixedName{
				Name: "hostname",
			},
			Domain:     "ad.domain",
			TimeZone:   "Etc/UTC",
			HwClockUTC: types.NewBool(true),
		},
		GlobalIPSettings: types.CustomizationGlobalIPSettings{
			DnsSuffixList: []string{"ad.domain"},
			DnsServerList: []string{"192.168.1.1"},
		},
	}

	task, err = vm.Customize(ctx, spec)
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to Customize vm")
	}
	if err = task.Wait(ctx); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to wait for Customize vm task to complete")
	}

	task, err = vm.PowerOn(ctx)
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to PowerOn vm")
	}
	if err = task.Wait(ctx); err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to PowerOn vm task to complete")
	}

	ip, err := vm.WaitForIP(ctx)
	if err != nil {
		return reconcile.Result{}, errors.Wrapf(err, "failed to WaitForIP")
	}
	log.Info("VM gets IP", "ip", ip)

	return reconcile.Result{RequeueAfter: 5 * time.Second}, nil // Wait for the ip address to surface in K8s resources
}
