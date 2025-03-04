/*
Copyright 2019 The Machine Controller Authors.

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

package azure

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2021-11-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2021-05-01/network"
	"github.com/Azure/go-autorest/autorest/to"
	gocache "github.com/patrickmn/go-cache"

	"github.com/kubermatic/machine-controller/pkg/apis/cluster/common"
	clusterv1alpha1 "github.com/kubermatic/machine-controller/pkg/apis/cluster/v1alpha1"
	"github.com/kubermatic/machine-controller/pkg/cloudprovider/common/ssh"
	cloudprovidererrors "github.com/kubermatic/machine-controller/pkg/cloudprovider/errors"
	"github.com/kubermatic/machine-controller/pkg/cloudprovider/instance"
	azuretypes "github.com/kubermatic/machine-controller/pkg/cloudprovider/provider/azure/types"
	cloudprovidertypes "github.com/kubermatic/machine-controller/pkg/cloudprovider/types"
	"github.com/kubermatic/machine-controller/pkg/cloudprovider/util"
	kuberneteshelper "github.com/kubermatic/machine-controller/pkg/kubernetes"
	"github.com/kubermatic/machine-controller/pkg/providerconfig"
	providerconfigtypes "github.com/kubermatic/machine-controller/pkg/providerconfig/types"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	"k8s.io/utils/pointer"
)

const (
	CapabilityPremiumIO = "PremiumIO"
	CapabilityUltraSSD  = "UltraSSDAvailable"
	CapabilityValueTrue = "True"

	machineUIDTag = "Machine-UID"

	finalizerPublicIP   = "kubermatic.io/cleanup-azure-public-ip"
	finalizerPublicIPv6 = "kubermatic.io/cleanup-azure-public-ipv6"
	finalizerNIC        = "kubermatic.io/cleanup-azure-nic"
	finalizerDisks      = "kubermatic.io/cleanup-azure-disks"
	finalizerVM         = "kubermatic.io/cleanup-azure-vm"
)

const (
	envClientID       = "AZURE_CLIENT_ID"
	envClientSecret   = "AZURE_CLIENT_SECRET"
	envTenantID       = "AZURE_TENANT_ID"
	envSubscriptionID = "AZURE_SUBSCRIPTION_ID"
)

type provider struct {
	configVarResolver *providerconfig.ConfigVarResolver
}

type config struct {
	SubscriptionID string
	TenantID       string
	ClientID       string
	ClientSecret   string

	Location              string
	ResourceGroup         string
	VNetResourceGroup     string
	VMSize                string
	VNetName              string
	SubnetName            string
	LoadBalancerSku       string
	RouteTableName        string
	AvailabilitySet       string
	AssignAvailabilitySet *bool
	SecurityGroupName     string
	ImageID               string
	Zones                 []string
	ImagePlan             *compute.Plan
	ImageReference        *compute.ImageReference

	OSDiskSize   int32
	OSDiskSKU    *compute.StorageAccountTypes
	DataDiskSize int32
	DataDiskSKU  *compute.StorageAccountTypes

	AssignPublicIP bool
	Tags           map[string]string
}

type azureVM struct {
	vm          *compute.VirtualMachine
	ipAddresses map[string]v1.NodeAddressType
	status      instance.Status
}

func (vm *azureVM) Addresses() map[string]v1.NodeAddressType {
	return vm.ipAddresses
}

func (vm *azureVM) ID() string {
	return *vm.vm.ID
}

func (vm *azureVM) Name() string {
	return *vm.vm.Name
}

func (vm *azureVM) Status() instance.Status {
	return vm.status
}

var imageReferences = map[providerconfigtypes.OperatingSystem]compute.ImageReference{
	providerconfigtypes.OperatingSystemCentOS: {
		Publisher: to.StringPtr("OpenLogic"),
		Offer:     to.StringPtr("CentOS"),
		Sku:       to.StringPtr("7_9"), // https://docs.microsoft.com/en-us/azure/virtual-machines/linux/using-cloud-init
		Version:   to.StringPtr("latest"),
	},
	providerconfigtypes.OperatingSystemUbuntu: {
		Publisher: to.StringPtr("Canonical"),
		Offer:     to.StringPtr("0001-com-ubuntu-server-focal"),
		Sku:       to.StringPtr("20_04-lts"),
		Version:   to.StringPtr("latest"),
	},
	providerconfigtypes.OperatingSystemRHEL: {
		Publisher: to.StringPtr("RedHat"),
		Offer:     to.StringPtr("rhel-byos"),
		Sku:       to.StringPtr("rhel-lvm83"),
		Version:   to.StringPtr("8.3.20201109"),
	},
	providerconfigtypes.OperatingSystemFlatcar: {
		Publisher: to.StringPtr("kinvolk"),
		Offer:     to.StringPtr("flatcar-container-linux"),
		Sku:       to.StringPtr("stable"),
		Version:   to.StringPtr("2905.2.5"),
	},
	providerconfigtypes.OperatingSystemRockyLinux: {
		Publisher: to.StringPtr("procomputers"),
		Offer:     to.StringPtr("rocky-linux-8-5"),
		Sku:       to.StringPtr("rocky-linux-8-5"),
		Version:   to.StringPtr("8.5.20211118"),
	},
}

var osPlans = map[providerconfigtypes.OperatingSystem]*compute.Plan{
	providerconfigtypes.OperatingSystemFlatcar: {
		Name:      pointer.StringPtr("stable"),
		Publisher: pointer.StringPtr("kinvolk"),
		Product:   pointer.StringPtr("flatcar-container-linux"),
	},
	providerconfigtypes.OperatingSystemRHEL: {
		Name:      pointer.StringPtr("rhel-lvm83"),
		Publisher: pointer.StringPtr("redhat"),
		Product:   pointer.StringPtr("rhel-byos"),
	},
	providerconfigtypes.OperatingSystemRockyLinux: {
		Name:      pointer.StringPtr("rocky-linux-8-5"),
		Publisher: pointer.StringPtr("procomputers"),
		Product:   pointer.StringPtr("rocky-linux-8-5"),
	},
}

var osDiskSKUs = map[compute.StorageAccountTypes]string{
	compute.StorageAccountTypesStandardLRS:    "", // Standard_LRS
	compute.StorageAccountTypesStandardSSDLRS: "", // StandardSSD_LRS
	compute.StorageAccountTypesPremiumLRS:     "", // Premium_LRS
}

var dataDiskSKUs = map[compute.StorageAccountTypes]string{
	compute.StorageAccountTypesStandardLRS:    "", // Standard_LRS
	compute.StorageAccountTypesStandardSSDLRS: "", // StandardSSD_LRS
	compute.StorageAccountTypesPremiumLRS:     "", // Premium_LRS
	compute.StorageAccountTypesUltraSSDLRS:    "", // UltraSSD_LRS
}

var (
	// cacheLock protects concurrent cache misses against a single key. This usually happens when multiple machines get created simultaneously
	// We lock so the first access updates/writes the data to the cache and afterwards everyone reads the cached data
	cacheLock = &sync.Mutex{}
	cache     = gocache.New(10*time.Minute, 10*time.Minute)
)

func getOSImageReference(c *config, os providerconfigtypes.OperatingSystem) (*compute.ImageReference, error) {
	if c.ImageID != "" {
		return &compute.ImageReference{
			ID: to.StringPtr(c.ImageID),
		}, nil
	}

	if c.ImageReference != nil {
		return &compute.ImageReference{
			Version:   c.ImageReference.Version,
			Sku:       c.ImageReference.Sku,
			Offer:     c.ImageReference.Offer,
			Publisher: c.ImageReference.Publisher,
		}, nil
	}

	ref, supported := imageReferences[os]
	if !supported {
		return nil, fmt.Errorf("operating system %q not supported", os)
	}

	return &ref, nil
}

// New returns a digitalocean provider
func New(configVarResolver *providerconfig.ConfigVarResolver) cloudprovidertypes.Provider {
	return &provider{configVarResolver: configVarResolver}
}

func (p *provider) getConfig(provSpec clusterv1alpha1.ProviderSpec) (*config, *providerconfigtypes.Config, error) {
	if provSpec.Value == nil {
		return nil, nil, fmt.Errorf("machine.spec.providerconfig.value is nil")
	}

	pconfig, err := providerconfigtypes.GetConfig(provSpec)
	if err != nil {
		return nil, nil, err
	}

	if pconfig.OperatingSystemSpec.Raw == nil {
		return nil, nil, errors.New("operatingSystemSpec in the MachineDeployment cannot be empty")
	}

	rawCfg, err := azuretypes.GetConfig(*pconfig)
	if err != nil {
		return nil, nil, err
	}

	c := config{}
	c.SubscriptionID, err = p.configVarResolver.GetConfigVarStringValueOrEnv(rawCfg.SubscriptionID, envSubscriptionID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"subscriptionID\" field, error = %v", err)
	}

	c.TenantID, err = p.configVarResolver.GetConfigVarStringValueOrEnv(rawCfg.TenantID, envTenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"tenantID\" field, error = %v", err)
	}

	c.ClientID, err = p.configVarResolver.GetConfigVarStringValueOrEnv(rawCfg.ClientID, envClientID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"clientID\" field, error = %v", err)
	}

	c.ClientSecret, err = p.configVarResolver.GetConfigVarStringValueOrEnv(rawCfg.ClientSecret, envClientSecret)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"clientSecret\" field, error = %v", err)
	}

	c.ResourceGroup, err = p.configVarResolver.GetConfigVarStringValue(rawCfg.ResourceGroup)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"resourceGroup\" field, error = %v", err)
	}

	c.VNetResourceGroup, err = p.configVarResolver.GetConfigVarStringValue(rawCfg.VNetResourceGroup)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"vnetResourceGroup\" field, error = %v", err)
	}

	if c.VNetResourceGroup == "" {
		c.VNetResourceGroup = c.ResourceGroup
	}

	c.Location, err = p.configVarResolver.GetConfigVarStringValue(rawCfg.Location)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"location\" field, error = %v", err)
	}

	c.VMSize, err = p.configVarResolver.GetConfigVarStringValue(rawCfg.VMSize)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"vmSize\" field, error = %v", err)
	}

	c.VNetName, err = p.configVarResolver.GetConfigVarStringValue(rawCfg.VNetName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"vnetName\" field, error = %v", err)
	}

	c.SubnetName, err = p.configVarResolver.GetConfigVarStringValue(rawCfg.SubnetName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"subnetName\" field, error = %v", err)
	}

	c.LoadBalancerSku, err = p.configVarResolver.GetConfigVarStringValue(rawCfg.LoadBalancerSku)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"loadBalancerSku\" field, error = %v", err)
	}

	c.RouteTableName, err = p.configVarResolver.GetConfigVarStringValue(rawCfg.RouteTableName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"routeTableName\" field, error = %v", err)
	}

	c.AssignPublicIP, _, err = p.configVarResolver.GetConfigVarBoolValue(rawCfg.AssignPublicIP)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"assignPublicIP\" field, error = %v", err)
	}

	c.AssignAvailabilitySet = rawCfg.AssignAvailabilitySet

	c.AvailabilitySet, err = p.configVarResolver.GetConfigVarStringValue(rawCfg.AvailabilitySet)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"availabilitySet\" field, error = %v", err)
	}

	c.SecurityGroupName, err = p.configVarResolver.GetConfigVarStringValue(rawCfg.SecurityGroupName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"securityGroupName\" field, error = %v", err)
	}

	c.Zones = rawCfg.Zones
	c.Tags = rawCfg.Tags
	c.OSDiskSize = rawCfg.OSDiskSize
	c.DataDiskSize = rawCfg.DataDiskSize

	if rawCfg.OSDiskSKU != nil {
		c.OSDiskSKU = storageTypePtr(*rawCfg.OSDiskSKU)
	}

	if rawCfg.DataDiskSKU != nil {
		c.DataDiskSKU = storageTypePtr(*rawCfg.DataDiskSKU)
	}

	if rawCfg.ImagePlan != nil && rawCfg.ImagePlan.Name != "" {
		c.ImagePlan = &compute.Plan{
			Name:      pointer.StringPtr(rawCfg.ImagePlan.Name),
			Publisher: pointer.StringPtr(rawCfg.ImagePlan.Publisher),
			Product:   pointer.StringPtr(rawCfg.ImagePlan.Product),
		}
	}

	if rawCfg.ImageReference != nil {
		c.ImageReference = &compute.ImageReference{
			Publisher: pointer.StringPtr(rawCfg.ImageReference.Publisher),
			Offer:     pointer.StringPtr(rawCfg.ImageReference.Offer),
			Sku:       pointer.StringPtr(rawCfg.ImageReference.Sku),
			Version:   pointer.StringPtr(rawCfg.ImageReference.Version),
		}
	}

	c.ImageID, err = p.configVarResolver.GetConfigVarStringValue(rawCfg.ImageID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get image id: %v", err)
	}

	return &c, pconfig, nil
}

func getVMIPAddresses(ctx context.Context, c *config, vm *compute.VirtualMachine) (map[string]v1.NodeAddressType, error) {
	var (
		ipAddresses = map[string]v1.NodeAddressType{}
		err         error
	)

	if vm.VirtualMachineProperties == nil {
		return nil, fmt.Errorf("machine is missing properties")
	}

	if vm.VirtualMachineProperties.NetworkProfile == nil {
		return nil, fmt.Errorf("machine has no network profile")
	}

	if vm.NetworkProfile.NetworkInterfaces == nil {
		return nil, fmt.Errorf("machine has no network interfaces data")
	}

	for n, iface := range *vm.NetworkProfile.NetworkInterfaces {
		if iface.ID == nil || len(*iface.ID) == 0 {
			return nil, fmt.Errorf("interface %d has no ID", n)
		}

		splitIfaceID := strings.Split(*iface.ID, "/")
		ifaceName := splitIfaceID[len(splitIfaceID)-1]
		ipAddresses, err = getNICIPAddresses(ctx, c, ifaceName)
		if vm.NetworkProfile.NetworkInterfaces == nil {
			return nil, fmt.Errorf("failed to get addresses for interface %q: %v", ifaceName, err)
		}
	}

	return ipAddresses, nil
}

func getNICIPAddresses(ctx context.Context, c *config, ifaceName string) (map[string]v1.NodeAddressType, error) {
	ifClient, err := getInterfacesClient(c)
	if err != nil {
		return nil, fmt.Errorf("failed to create interfaces client: %v", err)
	}

	netIf, err := ifClient.Get(ctx, c.ResourceGroup, ifaceName, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get interface %q: %v", ifaceName, err.Error())
	}

	ipAddresses := map[string]v1.NodeAddressType{}

	if netIf.IPConfigurations == nil {
		return ipAddresses, nil
	}

	for _, conf := range *netIf.IPConfigurations {
		var name string
		if conf.Name != nil {
			name = *conf.Name
		} else {
			klog.Warningf("IP configuration of NIC %q was returned with no name, trying to dissect the ID.", ifaceName)
			if conf.ID == nil || len(*conf.ID) == 0 {
				return nil, fmt.Errorf("IP configuration of NIC %q was returned with no ID", ifaceName)
			}
			splitConfID := strings.Split(*conf.ID, "/")
			name = splitConfID[len(splitConfID)-1]
		}

		if c.AssignPublicIP {
			publicIPs, err := getIPAddressStrings(ctx, c, publicIPName(ifaceName))
			if err != nil {
				return nil, fmt.Errorf("failed to retrieve IP string for IP %q: %v", name, err)
			}
			for _, ip := range publicIPs {
				ipAddresses[ip] = v1.NodeExternalIP
			}

			publicIP6s, err := getIPAddressStrings(ctx, c, publicIPv6Name(ifaceName))
			if err != nil {
				return nil, fmt.Errorf("failed to retrieve IP string for IP %q: %v", name, err)
			}
			for _, ip := range publicIP6s {
				ipAddresses[ip] = v1.NodeExternalIP
			}

		}

		internalIPs, err := getInternalIPAddresses(ctx, c, ifaceName, name)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve internal IP string for IP %q: %v", name, err)
		}
		for _, ip := range internalIPs {
			ipAddresses[ip] = v1.NodeInternalIP
		}

	}

	return ipAddresses, nil
}

func getIPAddressStrings(ctx context.Context, c *config, addrName string) ([]string, error) {
	ipClient, err := getIPClient(c)
	if err != nil {
		return nil, fmt.Errorf("failed to create IP address client: %v", err)
	}

	ip, err := ipClient.Get(ctx, c.ResourceGroup, addrName, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get IP %q: %v", addrName, err)
	}

	if ip.IPConfiguration == nil {
		return nil, fmt.Errorf("IP %q has nil IPConfiguration", addrName)
	}

	var ipAddresses []string
	if ip.IPAddress != nil {
		ipAddresses = append(ipAddresses, *ip.IPAddress)
	}

	return ipAddresses, nil
}

func getInternalIPAddresses(ctx context.Context, c *config, inetface, ipconfigName string) ([]string, error) {
	var ipAddresses []string
	ipConfigClient, err := getIPConfigClient(c)
	if err != nil {
		return nil, fmt.Errorf("failed to create IP config client: %v", err)
	}

	internalIP, err := ipConfigClient.Get(ctx, c.ResourceGroup, inetface, ipconfigName)
	if err != nil {
		return nil, fmt.Errorf("failed to get IP config %q: %v", inetface, err)
	}

	if internalIP.ID == nil {
		return nil, fmt.Errorf("private IP %q has nil IPConfiguration", inetface)
	}
	if internalIP.PrivateIPAddress != nil {
		ipAddresses = append(ipAddresses, *internalIP.PrivateIPAddress)
	}

	return ipAddresses, nil
}

func (p *provider) AddDefaults(spec clusterv1alpha1.MachineSpec) (clusterv1alpha1.MachineSpec, error) {
	return spec, nil
}

func getStorageProfile(config *config, providerCfg *providerconfigtypes.Config) (*compute.StorageProfile, error) {
	osRef, err := getOSImageReference(config, providerCfg.OperatingSystem)
	if err != nil {
		return nil, fmt.Errorf("failed to get OSImageReference: %v", err)
	}
	// initial default storage profile, this will use the VMSize default storage profile
	sp := &compute.StorageProfile{
		ImageReference: osRef,
	}
	if config.OSDiskSize != 0 {
		sp.OsDisk = &compute.OSDisk{
			DiskSizeGB:   pointer.Int32Ptr(config.OSDiskSize),
			CreateOption: compute.DiskCreateOptionTypesFromImage,
		}

		if config.OSDiskSKU != nil {
			sp.OsDisk.ManagedDisk = &compute.ManagedDiskParameters{
				StorageAccountType: *config.OSDiskSKU,
			}
		}
	}

	if config.DataDiskSize != 0 {
		sp.DataDisks = &[]compute.DataDisk{
			{
				// this should be in range 0-63 and should be unique per datadisk, since we have only one datadisk, this should be fine
				Lun:          new(int32),
				DiskSizeGB:   pointer.Int32Ptr(config.DataDiskSize),
				CreateOption: compute.DiskCreateOptionTypesEmpty,
			},
		}

		if config.DataDiskSKU != nil {
			(*sp.DataDisks)[0].ManagedDisk = &compute.ManagedDiskParameters{
				StorageAccountType: *config.DataDiskSKU,
			}
		}

	}
	return sp, nil
}

func (p *provider) Create(machine *clusterv1alpha1.Machine, data *cloudprovidertypes.ProviderData, userdata string) (instance.Instance, error) {
	config, providerCfg, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return nil, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("failed to parse MachineSpec, due to %v", err),
		}
	}

	vmClient, err := getVMClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create VM client: %v", err)
	}

	// We genete a random SSH key, since Azure won't let us create a VM without an SSH key or a password
	key, err := ssh.NewKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate ssh key: %v", err)
	}

	ipFamily := providerCfg.Network.GetIPFamily()
	sku := network.PublicIPAddressSkuNameBasic
	if ipFamily == util.DualStack {
		// 1. Cannot specify basic sku PublicIp for an IPv6 network interface ipConfiguration.
		// 2. Different basic sku and standard sku public Ip resources in availability set is not allowed.
		// 1 & 2 means we have to use standard sku in dual-stack configuration.

		// It is not clear from the documentation, but you get the
		// errors if you try mixing skus or try to create IPv6 public IP with
		// basic sku.
		sku = network.PublicIPAddressSkuNameStandard
	}
	var publicIP, publicIPv6 *network.PublicIPAddress
	if config.AssignPublicIP {
		if err = data.Update(machine, func(updatedMachine *clusterv1alpha1.Machine) {
			if !kuberneteshelper.HasFinalizer(updatedMachine, finalizerPublicIP) {
				updatedMachine.Finalizers = append(updatedMachine.Finalizers, finalizerPublicIP)
			}
		}); err != nil {
			return nil, err
		}
		publicIP, err = createOrUpdatePublicIPAddress(context.TODO(), publicIPName(ifaceName(machine)), network.IPVersionIPv4, sku, network.IPAllocationMethodStatic, machine.UID, config)
		if err != nil {
			return nil, fmt.Errorf("failed to create public IP: %v", err)
		}

		if ipFamily == util.DualStack {
			publicIPv6, err = createOrUpdatePublicIPAddress(context.TODO(), publicIPv6Name(ifaceName(machine)), network.IPVersionIPv6, sku, network.IPAllocationMethodStatic, machine.UID, config)
			if err != nil {
				return nil, fmt.Errorf("failed to create public IP: %v", err)
			}
		}
	}

	if err := data.Update(machine, func(updatedMachine *clusterv1alpha1.Machine) {
		if !kuberneteshelper.HasFinalizer(updatedMachine, finalizerNIC) {
			updatedMachine.Finalizers = append(updatedMachine.Finalizers, finalizerNIC)
		}
	}); err != nil {
		return nil, err
	}

	iface, err := createOrUpdateNetworkInterface(context.TODO(), ifaceName(machine), machine.UID, config, publicIP, publicIPv6, ipFamily)
	if err != nil {
		return nil, fmt.Errorf("failed to generate main network interface: %v", err)
	}

	tags := make(map[string]*string, len(config.Tags)+1)
	for k, v := range config.Tags {
		tags[k] = to.StringPtr(v)
	}
	tags[machineUIDTag] = to.StringPtr(string(machine.UID))

	osPlane := osPlans[providerCfg.OperatingSystem]
	if config.ImagePlan != nil {
		osPlane = config.ImagePlan
	}

	adminUserName := getOSUsername(providerCfg.OperatingSystem)
	storageProfile, err := getStorageProfile(config, providerCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to get StorageProfile: %v", err)
	}

	vmSpec := compute.VirtualMachine{
		Location: &config.Location,
		Plan:     osPlane,
		VirtualMachineProperties: &compute.VirtualMachineProperties{
			HardwareProfile: &compute.HardwareProfile{VMSize: compute.VirtualMachineSizeTypes(config.VMSize)},
			NetworkProfile: &compute.NetworkProfile{
				NetworkInterfaces: &[]compute.NetworkInterfaceReference{
					{
						ID:                                  iface.ID,
						NetworkInterfaceReferenceProperties: &compute.NetworkInterfaceReferenceProperties{Primary: to.BoolPtr(true)},
					},
				},
			},
			OsProfile: &compute.OSProfile{
				AdminUsername: to.StringPtr(adminUserName),
				ComputerName:  &machine.Name,
				LinuxConfiguration: &compute.LinuxConfiguration{
					DisablePasswordAuthentication: to.BoolPtr(true),
					SSH: &compute.SSHConfiguration{
						PublicKeys: &[]compute.SSHPublicKey{
							{
								Path:    to.StringPtr(fmt.Sprintf("/home/%s/.ssh/authorized_keys", adminUserName)),
								KeyData: &key.PublicKey,
							},
						},
					},
				},
				CustomData: to.StringPtr(base64.StdEncoding.EncodeToString([]byte(userdata))),
			},
			StorageProfile: storageProfile,
		},
		Tags:  tags,
		Zones: &config.Zones,
	}

	if config.AssignAvailabilitySet == nil && config.AvailabilitySet != "" ||
		config.AssignAvailabilitySet != nil && *config.AssignAvailabilitySet && config.AvailabilitySet != "" {
		// Azure expects the full path to the resource
		asURI := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/availabilitySets/%s", config.SubscriptionID, config.ResourceGroup, config.AvailabilitySet)
		vmSpec.VirtualMachineProperties.AvailabilitySet = &compute.SubResource{ID: to.StringPtr(asURI)}
	}

	klog.Infof("Creating machine %q", machine.Name)
	if err := data.Update(machine, func(updatedMachine *clusterv1alpha1.Machine) {
		if !kuberneteshelper.HasFinalizer(updatedMachine, finalizerDisks) {
			updatedMachine.Finalizers = append(updatedMachine.Finalizers, finalizerDisks)
		}
		if !kuberneteshelper.HasFinalizer(machine, finalizerVM) {
			updatedMachine.Finalizers = append(updatedMachine.Finalizers, finalizerVM)
		}
	}); err != nil {
		return nil, err
	}

	future, err := vmClient.CreateOrUpdate(context.TODO(), config.ResourceGroup, machine.Name, vmSpec)
	if err != nil {
		return nil, fmt.Errorf("trying to create a VM: %v", err)
	}

	err = future.WaitForCompletionRef(context.TODO(), vmClient.Client)
	if err != nil {
		return nil, fmt.Errorf("waiting for operation returned: %v", err.Error())
	}

	vm, err := future.Result(*vmClient)
	if err != nil {
		return nil, fmt.Errorf("decoding result: %v", err.Error())
	}

	// get the actual VM object filled in with additional data
	vm, err = vmClient.Get(context.TODO(), config.ResourceGroup, machine.Name, "")
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve updated data for VM %q: %v", machine.Name, err)
	}

	ipAddresses, err := getVMIPAddresses(context.TODO(), config, &vm)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve IP addresses for VM %q: %v", machine.Name, err.Error())
	}

	status, err := getVMStatus(context.TODO(), config, machine.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve status for VM %q: %v", machine.Name, err.Error())
	}

	return &azureVM{vm: &vm, ipAddresses: ipAddresses, status: status}, nil
}

func (p *provider) Cleanup(machine *clusterv1alpha1.Machine, data *cloudprovidertypes.ProviderData) (bool, error) {
	config, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return false, fmt.Errorf("failed to parse MachineSpec: %v", err)
	}

	_, err = p.get(machine)
	// If a defunct VM got created, the `Get` call returns an error - But not because the request
	// failed but because the VM has an invalid config hence always delete except on err == cloudprovidererrors.ErrInstanceNotFound
	if err != nil {
		if err == cloudprovidererrors.ErrInstanceNotFound {
			return util.RemoveFinalizerOnInstanceNotFound(finalizerVM, machine, data)
		}
		return false, err
	}

	klog.Infof("deleting VM %q", machine.Name)
	if err = deleteVMsByMachineUID(context.TODO(), config, machine.UID); err != nil {
		return false, fmt.Errorf("failed to delete instance for  machine %q: %v", machine.Name, err)
	}

	if err := data.Update(machine, func(updatedMachine *clusterv1alpha1.Machine) {
		updatedMachine.Finalizers = kuberneteshelper.RemoveFinalizer(updatedMachine.Finalizers, finalizerVM)
	}); err != nil {
		return false, err
	}

	klog.Infof("deleting disks of VM %q", machine.Name)
	if err := deleteDisksByMachineUID(context.TODO(), config, machine.UID); err != nil {
		return false, fmt.Errorf("failed to remove disks of machine %q: %v", machine.Name, err)
	}
	if err := data.Update(machine, func(updatedMachine *clusterv1alpha1.Machine) {
		updatedMachine.Finalizers = kuberneteshelper.RemoveFinalizer(updatedMachine.Finalizers, finalizerDisks)
	}); err != nil {
		return false, err
	}

	klog.Infof("deleting network interfaces of VM %q", machine.Name)
	if err := deleteInterfacesByMachineUID(context.TODO(), config, machine.UID); err != nil {
		return false, fmt.Errorf("failed to remove network interfaces of machine %q: %v", machine.Name, err)
	}
	if err := data.Update(machine, func(updatedMachine *clusterv1alpha1.Machine) {
		updatedMachine.Finalizers = kuberneteshelper.RemoveFinalizer(updatedMachine.Finalizers, finalizerNIC)
	}); err != nil {
		return false, err
	}

	klog.Infof("deleting public IP addresses of VM %q", machine.Name)
	if err := deleteIPAddressesByMachineUID(context.TODO(), config, machine.UID); err != nil {
		return false, fmt.Errorf("failed to remove public IP addresses of machine %q: %v", machine.Name, err)
	}
	if err := data.Update(machine, func(updatedMachine *clusterv1alpha1.Machine) {
		updatedMachine.Finalizers = kuberneteshelper.RemoveFinalizer(updatedMachine.Finalizers, finalizerPublicIP)
	}); err != nil {
		return false, err
	}

	return true, nil
}

func getVMByUID(ctx context.Context, c *config, uid types.UID) (*compute.VirtualMachine, error) {
	vmClient, err := getVMClient(c)
	if err != nil {
		return nil, err
	}

	list, err := vmClient.ListAll(ctx, "", "")
	if err != nil {
		return nil, err
	}

	var allServers []compute.VirtualMachine

	for list.NotDone() {
		allServers = append(allServers, list.Values()...)
		if err := list.Next(); err != nil {
			return nil, fmt.Errorf("failed to iterate the result list: %s", err)
		}
	}

	for _, vm := range allServers {
		if vm.Tags != nil && vm.Tags[machineUIDTag] != nil && *vm.Tags[machineUIDTag] == string(uid) {
			return &vm, nil
		}
	}

	return nil, cloudprovidererrors.ErrInstanceNotFound
}

func getVMStatus(ctx context.Context, c *config, vmName string) (instance.Status, error) {
	vmClient, err := getVMClient(c)
	if err != nil {
		return instance.StatusUnknown, err
	}

	iv, err := vmClient.InstanceView(ctx, c.ResourceGroup, vmName)
	if err != nil {
		return instance.StatusUnknown, fmt.Errorf("failed to get instance view for machine %q: %v", vmName, err)
	}

	if iv.Statuses == nil {
		return instance.StatusUnknown, nil
	}

	// it seems that this field should contain two entries: a provisioning status and a power status
	if len(*iv.Statuses) < 2 {
		provisioningStatus := (*iv.Statuses)[0]
		if provisioningStatus.Code == nil {
			klog.Warningf("azure provisioning status has missing code")
			return instance.StatusUnknown, nil
		}

		switch *provisioningStatus.Code {
		case "":
			return instance.StatusUnknown, nil
		case "ProvisioningState/deleting":
			return instance.StatusDeleting, nil
		default:
			klog.Warningf("unknown Azure provisioning status %q", *provisioningStatus.Code)
			return instance.StatusUnknown, nil
		}
	}

	// the second field is supposed to be the power status
	// https://docs.microsoft.com/en-us/azure/virtual-machines/windows/tutorial-manage-vm#vm-power-states
	powerStatus := (*iv.Statuses)[1]
	if powerStatus.Code == nil {
		klog.Warningf("azure power status has missing code")
		return instance.StatusUnknown, nil
	}

	switch *powerStatus.Code {
	case "":
		return instance.StatusUnknown, nil
	case "PowerState/running":
		return instance.StatusRunning, nil
	case "PowerState/starting":
		return instance.StatusCreating, nil
	default:
		klog.Warningf("unknown Azure power status %q", *powerStatus.Code)
		return instance.StatusUnknown, nil
	}
}

func (p *provider) Get(machine *clusterv1alpha1.Machine, _ *cloudprovidertypes.ProviderData) (instance.Instance, error) {
	return p.get(machine)
}

func (p *provider) get(machine *clusterv1alpha1.Machine) (*azureVM, error) {
	config, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to parse MachineSpec: %v", err)
	}

	vm, err := getVMByUID(context.TODO(), config, machine.UID)
	if err != nil {
		if err == cloudprovidererrors.ErrInstanceNotFound {
			return nil, cloudprovidererrors.ErrInstanceNotFound
		}

		return nil, fmt.Errorf("failed to find machine %q by its UID: %v", machine.UID, err)
	}

	ipAddresses, err := getVMIPAddresses(context.TODO(), config, vm)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve IP addresses for VM %v: %v", vm.Name, err)
	}

	status, err := getVMStatus(context.TODO(), config, machine.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve status for VM %v: %v", vm.Name, err)
	}

	return &azureVM{vm: vm, ipAddresses: ipAddresses, status: status}, nil
}

func (p *provider) GetCloudConfig(spec clusterv1alpha1.MachineSpec) (config string, name string, err error) {
	c, _, err := p.getConfig(spec.ProviderSpec)
	if err != nil {
		return "", "", fmt.Errorf("failed to parse config: %v", err)
	}

	var avSet string
	if c.AssignAvailabilitySet == nil && c.AvailabilitySet != "" ||
		c.AssignAvailabilitySet != nil && *c.AssignAvailabilitySet && c.AvailabilitySet != "" {
		avSet = c.AvailabilitySet
	}

	cc := &azuretypes.CloudConfig{
		Cloud:                      "AZUREPUBLICCLOUD",
		TenantID:                   c.TenantID,
		SubscriptionID:             c.SubscriptionID,
		AADClientID:                c.ClientID,
		AADClientSecret:            c.ClientSecret,
		ResourceGroup:              c.ResourceGroup,
		VnetResourceGroup:          c.VNetResourceGroup,
		Location:                   c.Location,
		VNetName:                   c.VNetName,
		SubnetName:                 c.SubnetName,
		LoadBalancerSku:            c.LoadBalancerSku,
		RouteTableName:             c.RouteTableName,
		PrimaryAvailabilitySetName: avSet,
		SecurityGroupName:          c.SecurityGroupName,
		UseInstanceMetadata:        true,
	}

	s, err := azuretypes.CloudConfigToString(cc)
	if err != nil {
		return "", "", fmt.Errorf("failed to convert cloud-config to string: %v", err)
	}

	return s, "azure", nil
}

func validateDiskSKUs(c *config) error {
	if c.OSDiskSKU != nil || c.DataDiskSKU != nil {
		sku, err := getSKU(context.TODO(), c)
		if err != nil {
			return fmt.Errorf("failed to get VM SKU: %w", err)
		}

		if c.OSDiskSKU != nil {
			if _, ok := osDiskSKUs[*c.OSDiskSKU]; !ok {
				return fmt.Errorf("invalid OS disk SKU '%s'", *c.OSDiskSKU)
			}

			if err := supportsDiskSKU(sku, *c.OSDiskSKU, c.Zones); err != nil {
				return err
			}
		}

		if c.DataDiskSKU != nil {
			if _, ok := dataDiskSKUs[*c.DataDiskSKU]; !ok {
				return fmt.Errorf("invalid data disk SKU '%s'", *c.DataDiskSKU)
			}

			// Ultra SSDs do not support availability sets, see for reference:
			// https://docs.microsoft.com/en-us/azure/virtual-machines/disks-enable-ultra-ssd#ga-scope-and-limitations
			if *c.DataDiskSKU == compute.StorageAccountTypesUltraSSDLRS && ((c.AssignAvailabilitySet != nil && *c.AssignAvailabilitySet) || c.AvailabilitySet != "") {
				return fmt.Errorf("data disk SKU '%s' does not support availability sets", *c.DataDiskSKU)
			}

			if err := supportsDiskSKU(sku, *c.DataDiskSKU, c.Zones); err != nil {
				return err
			}
		}
	}

	return nil
}

func (p *provider) Validate(spec clusterv1alpha1.MachineSpec) error {
	c, providerConfig, err := p.getConfig(spec.ProviderSpec)
	if err != nil {
		return fmt.Errorf("failed to parse config: %v", err)
	}

	if c.SubscriptionID == "" {
		return errors.New("subscriptionID is missing")
	}

	if c.TenantID == "" {
		return errors.New("tenantID is missing")
	}

	if c.ClientID == "" {
		return errors.New("clientID is missing")
	}

	if c.ClientSecret == "" {
		return errors.New("clientSecret is missing")
	}

	if c.ResourceGroup == "" {
		return errors.New("resourceGroup is missing")
	}

	if c.VMSize == "" {
		return errors.New("vmSize is missing")
	}

	if c.VNetName == "" {
		return errors.New("vnetName is missing")
	}

	if c.SubnetName == "" {
		return errors.New("subnetName is missing")
	}

	switch f := providerConfig.Network.GetIPFamily(); f {
	case util.Unspecified, util.IPv4:
		//noop
	case util.IPv6:
		return fmt.Errorf(util.ErrIPv6OnlyUnsupported)
	case util.DualStack:
		// validate
	default:
		return fmt.Errorf(util.ErrUnknownNetworkFamily, f)
	}

	vmClient, err := getVMClient(c)
	if err != nil {
		return fmt.Errorf("failed to (create) vm client: %v", err.Error())
	}

	_, err = vmClient.ListAll(context.TODO(), "", "")
	if err != nil {
		return fmt.Errorf("failed to list all: %v", err.Error())
	}

	if _, err := getVirtualNetwork(context.TODO(), c); err != nil {
		return fmt.Errorf("failed to get virtual network: %v", err)
	}

	if _, err := getSubnet(context.TODO(), c); err != nil {
		return fmt.Errorf("failed to get subnet: %v", err)
	}

	if err := validateDiskSKUs(c); err != nil {
		return fmt.Errorf("failed to validate disk SKUs: %w", err)
	}

	_, err = getOSImageReference(c, providerConfig.OperatingSystem)
	return err
}

func ifaceName(machine *clusterv1alpha1.Machine) string {
	return machine.Name + "-netiface"
}

func publicIPName(ifaceName string) string {
	return ifaceName + "-pubip"
}

func publicIPv6Name(ifaceName string) string {
	return ifaceName + "-pubipv6"
}

func (p *provider) MigrateUID(machine *clusterv1alpha1.Machine, newUID types.UID) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	config, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err != nil {
		return cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("failed to parse MachineSpec, due to %v", err),
		}
	}

	vmClient, err := getVMClient(config)
	if err != nil {
		return fmt.Errorf("failed to create VM client: %v", err)
	}

	var publicIP, publicIPv6 *network.PublicIPAddress
	sku := network.PublicIPAddressSkuNameBasic

	if kuberneteshelper.HasFinalizer(machine, finalizerPublicIPv6) {
		sku = network.PublicIPAddressSkuNameStandard
		_, err = createOrUpdatePublicIPAddress(ctx, publicIPv6Name(ifaceName(machine)), network.IPVersionIPv6, sku, network.IPAllocationMethodDynamic, newUID, config)
		if err != nil {
			return fmt.Errorf("failed to update UID on public IP: %v", err)
		}
	}

	if kuberneteshelper.HasFinalizer(machine, finalizerPublicIP) {
		_, err = createOrUpdatePublicIPAddress(ctx, publicIPName(ifaceName(machine)), network.IPVersionIPv4, sku, network.IPAllocationMethodStatic, newUID, config)
		if err != nil {
			return fmt.Errorf("failed to update UID on public IP: %v", err)
		}
	}

	if kuberneteshelper.HasFinalizer(machine, finalizerNIC) {
		_, err = createOrUpdateNetworkInterface(ctx, ifaceName(machine), newUID, config, publicIP, publicIPv6, util.Unspecified)
		if err != nil {
			return fmt.Errorf("failed to update UID on main network interface: %v", err)
		}
	}

	if kuberneteshelper.HasFinalizer(machine, finalizerDisks) {
		disksClient, err := getDisksClient(config)
		if err != nil {
			return fmt.Errorf("failed to get disks client: %v", err)
		}

		disks, err := getDisksByMachineUID(ctx, disksClient, config, machine.UID)
		if err != nil {
			return fmt.Errorf("failed to get disks: %v", err)
		}

		for _, disk := range disks {
			disk.Tags[machineUIDTag] = to.StringPtr(string(newUID))
			future, err := disksClient.CreateOrUpdate(ctx, config.ResourceGroup, *disk.Name, disk)
			if err != nil {
				return fmt.Errorf("failed to update UID for disk %s: %v", *disk.Name, err)
			}
			if err := future.WaitForCompletionRef(ctx, disksClient.Client); err != nil {
				return fmt.Errorf("failed waiting for completion of update UID operation for disk %s: %v", *disk.Name, err)
			}
		}
	}

	tags := map[string]*string{}
	for k, v := range config.Tags {
		tags[k] = to.StringPtr(v)
	}
	tags[machineUIDTag] = to.StringPtr(string(newUID))

	vmSpec := compute.VirtualMachine{Location: &config.Location, Tags: tags}
	future, err := vmClient.CreateOrUpdate(ctx, config.ResourceGroup, machine.Name, vmSpec)
	if err != nil {
		return fmt.Errorf("failed to update UID of the instance: %v", err)
	}

	if err := future.WaitForCompletionRef(ctx, vmClient.Client); err != nil {
		return fmt.Errorf("error waiting for instance to have the updated UID: %v", err)
	}

	return nil
}

func (p *provider) MachineMetricsLabels(machine *clusterv1alpha1.Machine) (map[string]string, error) {
	labels := make(map[string]string)

	c, _, err := p.getConfig(machine.Spec.ProviderSpec)
	if err == nil {
		labels["size"] = c.VMSize
		labels["location"] = c.Location
	}

	return labels, err
}

func (p *provider) SetMetricsForMachines(machines clusterv1alpha1.MachineList) error {
	return nil
}

func getOSUsername(os providerconfigtypes.OperatingSystem) string {
	switch os {
	case providerconfigtypes.OperatingSystemFlatcar:
		return "core"
	default:
		return string(os)
	}
}

func storageTypePtr(storageType string) *compute.StorageAccountTypes {
	storage := compute.StorageAccountTypes(storageType)
	return &storage
}

// supportsDiskSKU validates some disk SKU types against the chosen VM SKU / VM type.
func supportsDiskSKU(vmSKU compute.ResourceSku, diskSKU compute.StorageAccountTypes, zones []string) error {
	// sanity check to make sure the Azure API did not return something bad
	if vmSKU.Name == nil || vmSKU.Capabilities == nil {
		return fmt.Errorf("invalid VM SKU object")
	}

	switch diskSKU {
	case compute.StorageAccountTypesPremiumLRS:
		found := false
		for _, capability := range *vmSKU.Capabilities {
			if *capability.Name == CapabilityPremiumIO && *capability.Value == CapabilityValueTrue {
				found = true
				break
			}
		}

		if !found {
			return fmt.Errorf("VM SKU '%s' does not support disk SKU '%s'", *vmSKU.Name, diskSKU)
		}

	case compute.StorageAccountTypesUltraSSDLRS:
		if vmSKU.LocationInfo == nil || len(*vmSKU.LocationInfo) == 0 || (*vmSKU.LocationInfo)[0].Zones == nil || len(*(*vmSKU.LocationInfo)[0].Zones) == 0 {
			// no zone information found, let's check for capability
			found := false
			for _, capability := range *vmSKU.Capabilities {
				if *capability.Name == CapabilityUltraSSD && *capability.Value == CapabilityValueTrue {
					found = true
					break
				}
			}

			if !found {
				return fmt.Errorf("VM SKU '%s' does not support disk SKU '%s'", *vmSKU.Name, diskSKU)
			}
		} else {
			if (*vmSKU.LocationInfo)[0].ZoneDetails != nil {
				for _, zone := range zones {
					found := false
					for _, details := range *(*vmSKU.LocationInfo)[0].ZoneDetails {
						matchesZone := false
						for _, zoneName := range *details.Name {
							if zone == zoneName {
								matchesZone = true
								break
							}
						}

						// we only check this zone details for capabilities if it actually includes the zone we're checking for
						if matchesZone {
							for _, capability := range *details.Capabilities {
								if *capability.Name == CapabilityUltraSSD && *capability.Value == CapabilityValueTrue {
									found = true
									break
								}
							}
						}
					}

					if !found {
						return fmt.Errorf("VM SKU '%s' does not support disk SKU '%s' in zone '%s'", *vmSKU.Name, diskSKU, zone)
					}
				}
			}
		}
	}

	return nil
}
