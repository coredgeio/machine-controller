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

package common

import (
	"fmt"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Constants aren't automatically generated for unversioned packages.
// Instead share the same constant for all versioned packages
type MachineStatusError string

const (
	// Represents that the combination of configuration in the MachineSpec
	// is not supported by this cluster. This is not a transient error, but
	// indicates a state that must be fixed before progress can be made.
	//
	// Example: the ProviderSpec specifies an instance type that doesn't exist,
	InvalidConfigurationMachineError MachineStatusError = "InvalidConfiguration"

	// This indicates that the MachineSpec has been updated in a way that
	// is not supported for reconciliation on this cluster. The spec may be
	// completely valid from a configuration standpoint, but the controller
	// does not support changing the real world state to match the new
	// spec.
	//
	// Example: the responsible controller is not capable of changing the
	// container runtime from docker to rkt.
	UnsupportedChangeMachineError MachineStatusError = "UnsupportedChange"

	// This generally refers to exceeding one's quota in a cloud provider,
	// or running out of physical machines in an on-premise environment.
	InsufficientResourcesMachineError MachineStatusError = "InsufficientResources"

	// There was an error while trying to create a Node to match this
	// Machine. This may indicate a transient problem that will be fixed
	// automatically with time, such as a service outage, or a terminal
	// error during creation that doesn't match a more specific
	// MachineStatusError value.
	//
	// Example: timeout trying to connect to GCE.
	CreateMachineError MachineStatusError = "CreateError"

	// There was an error while trying to update a Node that this
	// Machine represents. This may indicate a transient problem that will be
	// fixed automatically with time, such as a service outage,
	//
	// Example: error updating load balancers
	UpdateMachineError MachineStatusError = "UpdateError"

	// An error was encountered while trying to delete the Node that this
	// Machine represents. This could be a transient or terminal error, but
	// will only be observable if the provider's Machine controller has
	// added a finalizer to the object to more gracefully handle deletions.
	//
	// Example: cannot resolve EC2 IP address.
	DeleteMachineError MachineStatusError = "DeleteError"

	// This error indicates that the machine did not join the cluster
	// as a new node within the expected timeframe after instance
	// creation at the provider succeeded
	//
	// Example use case: A controller that deletes Machines which do
	// not result in a Node joining the cluster within a given timeout
	// and that are managed by a MachineSet
	JoinClusterTimeoutMachineError = "JoinClusterTimeoutError"
)

type ClusterStatusError string

const (
	// InvalidConfigurationClusterError indicates that the cluster
	// configuration is invalid.
	InvalidConfigurationClusterError ClusterStatusError = "InvalidConfiguration"

	// UnsupportedChangeClusterError indicates that the cluster
	// spec has been updated in an unsupported way. That cannot be
	// reconciled.
	UnsupportedChangeClusterError ClusterStatusError = "UnsupportedChange"

	// CreateClusterError indicates that an error was encountered
	// when trying to create the cluster.
	CreateClusterError ClusterStatusError = "CreateError"

	// UpdateClusterError indicates that an error was encountered
	// when trying to update the cluster.
	UpdateClusterError ClusterStatusError = "UpdateError"

	// DeleteClusterError indicates that an error was encountered
	// when trying to delete the cluster.
	DeleteClusterError ClusterStatusError = "DeleteError"
)

type MachineSetStatusError string

const (
	// Represents that the combination of configuration in the MachineTemplateSpec
	// is not supported by this cluster. This is not a transient error, but
	// indicates a state that must be fixed before progress can be made.
	//
	// Example: the ProviderSpec specifies an instance type that doesn't exist.
	InvalidConfigurationMachineSetError MachineSetStatusError = "InvalidConfiguration"
)

type MachineDeploymentStrategyType string

const (
	// Replace the old MachineSet by new one using rolling update
	// i.e. gradually scale down the old MachineSet and scale up the new one.
	RollingUpdateMachineDeploymentStrategyType MachineDeploymentStrategyType = "RollingUpdate"
)

type KubeletFlags string

const (
	ExternalCloudProviderKubeletFlag KubeletFlags = "ExternalCloudProvider"
)

const (
	SystemReservedKubeletConfig       = "SystemReserved"
	KubeReservedKubeletConfig         = "KubeReserved"
	EvictionHardKubeletConfig         = "EvictionHard"
	ContainerLogMaxSizeKubeletConfig  = "ContainerLogMaxSize"
	ContainerLogMaxFilesKubeletConfig = "ContainerLogMaxFiles"
)

const (
	// Annotation prefixes, used on Machine objects to indicate the parameters that been used to create those Machines
	KubeletFeatureGatesAnnotationPrefixV1 = "v1.kubelet-featuregates.machine-controller.kubermatic.io"
	KubeletFlagsGroupAnnotationPrefixV1   = "v1.kubelet-flags.machine-controller.kubermatic.io"
	KubeletConfigAnnotationPrefixV1       = "v1.kubelet-config.machine-controller.kubermatic.io"
)

// SetKubeletFeatureGates marshal and save featureGates into metaobject annotations with
// KubeletFeatureGatesAnnotationPrefixV1 prefix
func SetKubeletFeatureGates(metaobj metav1.Object, featureGates map[string]bool) {
	annts := metaobj.GetAnnotations()
	if annts == nil {
		annts = map[string]string{}
	}
	for k, v := range featureGates {
		annts[fmt.Sprintf("%s/%s", KubeletFeatureGatesAnnotationPrefixV1, k)] = fmt.Sprintf("%t", v)
	}
	metaobj.SetAnnotations(annts)
}

// SetKubeletFlags marshal and save flags into metaobject annotations with KubeletFlagsGroupAnnotationPrefixV1 prefix
func SetKubeletFlags(metaobj metav1.Object, flags map[KubeletFlags]string) {
	annts := metaobj.GetAnnotations()
	if annts == nil {
		annts = map[string]string{}
	}
	for k, v := range flags {
		annts[fmt.Sprintf("%s/%s", KubeletFlagsGroupAnnotationPrefixV1, k)] = v
	}
	metaobj.SetAnnotations(annts)
}

func GetKubeletConfigs(annotations map[string]string) map[string]string {
	configs := map[string]string{}
	for name, value := range annotations {
		if strings.HasPrefix(name, KubeletConfigAnnotationPrefixV1) {
			nameConfigValue := strings.SplitN(name, "/", 2)
			if len(nameConfigValue) != 2 {
				continue
			}
			configs[nameConfigValue[1]] = value
		}
	}
	return configs
}

func GetKubeletFeatureGates(annotations map[string]string) map[string]bool {
	result := map[string]bool{}
	for name, value := range annotations {
		if strings.HasPrefix(name, KubeletFeatureGatesAnnotationPrefixV1) {
			nameGateValue := strings.SplitN(name, "/", 2)
			if len(nameGateValue) != 2 {
				continue
			}
			realBool, _ := strconv.ParseBool(value)
			result[nameGateValue[1]] = realBool
		}
	}
	return result
}

func GetKubeletFlags(annotations map[string]string) map[KubeletFlags]string {
	result := map[KubeletFlags]string{}
	for name, value := range annotations {
		if strings.HasPrefix(name, KubeletFlagsGroupAnnotationPrefixV1) {
			nameFlagValue := strings.SplitN(name, "/", 2)
			if len(nameFlagValue) != 2 {
				continue
			}
			result[KubeletFlags(nameFlagValue[1])] = value
		}
	}
	return result
}

const OperatingSystemLabelV1 = "v1.machine-controller.kubermatic.io/operating-system"

func SetOSLabel(metaobj metav1.Object, osName string) {
	lbs := metaobj.GetLabels()

	if _, found := lbs[OperatingSystemLabelV1]; !found {
		if lbs == nil {
			lbs = map[string]string{}
		}
		lbs[OperatingSystemLabelV1] = osName
		metaobj.SetLabels(lbs)
	}
}
