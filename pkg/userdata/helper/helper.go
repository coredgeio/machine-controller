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

package helper

import (
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	DefaultDockerContainerLogMaxFiles = "5"
	DefaultDockerContainerLogMaxSize  = "100m"
)

func GetServerAddressFromKubeconfig(kubeconfig *clientcmdapi.Config) (string, error) {
	if len(kubeconfig.Clusters) != 1 {
		return "", fmt.Errorf("kubeconfig does not contain exactly one cluster, can not extract server address")
	}
	// Clusters is a map so we have to use range here
	for _, clusterConfig := range kubeconfig.Clusters {
		return strings.Replace(clusterConfig.Server, "https://", "", -1), nil
	}

	return "", fmt.Errorf("no server address found")

}

func GetCACert(kubeconfig *clientcmdapi.Config) (string, error) {
	if len(kubeconfig.Clusters) != 1 {
		return "", fmt.Errorf("kubeconfig does not contain exactly one cluster, can not extract server address")
	}
	// Clusters is a map so we have to use range here
	for _, clusterConfig := range kubeconfig.Clusters {
		return string(clusterConfig.CertificateAuthorityData), nil
	}

	return "", fmt.Errorf("no CACert found")
}

// StringifyKubeconfig marshals a kubeconfig to its text form
func StringifyKubeconfig(kubeconfig *clientcmdapi.Config) (string, error) {
	kubeconfigBytes, err := clientcmd.Write(*kubeconfig)
	if err != nil {
		return "", fmt.Errorf("error writing kubeconfig: %v", err)
	}

	return string(kubeconfigBytes), nil
}

// LoadKernelModules returns a script which is responsible for loading all required kernel modules
// The nf_conntrack_ipv4 module get removed in newer kernel versions
func LoadKernelModulesScript() string {
	return `#!/usr/bin/env bash
set -euo pipefail

modprobe ip_vs
modprobe ip_vs_rr
modprobe ip_vs_wrr
modprobe ip_vs_sh

if modinfo nf_conntrack_ipv4 &> /dev/null; then
  modprobe nf_conntrack_ipv4
else
  modprobe nf_conntrack
fi
`
}

// KernelSettings returns the list of kernel settings required for a kubernetes worker node
// inotify changes according to https://github.com/kubernetes/kubernetes/issues/10421 - better than letting the kubelet die
func KernelSettings() string {
	return `net.bridge.bridge-nf-call-ip6tables = 1
net.bridge.bridge-nf-call-iptables = 1
kernel.panic_on_oops = 1
kernel.panic = 10
net.ipv4.ip_forward = 1
vm.overcommit_memory = 1
fs.inotify.max_user_watches = 1048576
fs.inotify.max_user_instances = 8192
`
}

// JournalDConfig returns the journal config preferable on every node
func JournalDConfig() string {
	// JournaldMaxUse defines the maximum space that journalD logs can occupy.
	// https://www.freedesktop.org/software/systemd/man/journald.conf.html#SystemMaxUse=
	return `[Journal]
SystemMaxUse=5G
`
}

type dockerConfig struct {
	ExecOpts           []string          `json:"exec-opts,omitempty"`
	StorageDriver      string            `json:"storage-driver,omitempty"`
	StorageOpts        []string          `json:"storage-opts,omitempty"`
	LogDriver          string            `json:"log-driver,omitempty"`
	LogOpts            map[string]string `json:"log-opts,omitempty"`
	InsecureRegistries []string          `json:"insecure-registries,omitempty"`
	RegistryMirrors    []string          `json:"registry-mirrors,omitempty"`
}

// DockerConfig returns the docker daemon.json.
func DockerConfig(insecureRegistries, registryMirrors []string, logMaxFiles string, logMaxSize string) (string, error) {
	if len(logMaxSize) > 0 {
		// Parse log max size to ensure that it has the correct units
		logMaxSize = strings.ToLower(logMaxSize)
		logMaxSize = strings.ReplaceAll(logMaxSize, "ki", "k")
		logMaxSize = strings.ReplaceAll(logMaxSize, "mi", "m")
		logMaxSize = strings.ReplaceAll(logMaxSize, "gi", "g")
	} else {
		logMaxSize = DefaultDockerContainerLogMaxSize
	}

	// Default if value is not provided
	if len(logMaxFiles) == 0 {
		logMaxFiles = DefaultDockerContainerLogMaxFiles
	}

	cfg := dockerConfig{
		ExecOpts:      []string{"native.cgroupdriver=systemd"},
		StorageDriver: "overlay2",
		LogDriver:     "json-file",
		LogOpts: map[string]string{
			"max-size": logMaxSize,
			"max-file": logMaxFiles,
		},
		InsecureRegistries: insecureRegistries,
		RegistryMirrors:    registryMirrors,
	}

	b, err := json.Marshal(cfg)
	return string(b), err
}

func ProxyEnvironment(proxy, noProxy string) string {
	return fmt.Sprintf(`HTTP_PROXY=%s
http_proxy=%s
HTTPS_PROXY=%s
https_proxy=%s
NO_PROXY=%s
no_proxy=%s`, proxy, proxy, proxy, proxy, noProxy, noProxy)
}

func SetupNodeIPEnvScript() string {
	return `#!/usr/bin/env bash
echodate() {
  echo "[$(date -Is)]" "$@"
}

# get the default interface IP address
DEFAULT_IFC_IP=$(ip -o  route get 1 | grep -oP "src \K\S+")

# get the full hostname
FULL_HOSTNAME=$(hostname -f)

if [ -z "${DEFAULT_IFC_IP}" ]
then
	echodate "Failed to get IP address for the default route interface"
	exit 1
fi

# write the nodeip_env file
# we need the line below because flatcar has the same string "coreos" in that file
if grep -q coreos /etc/os-release
then
  echo -e "KUBELET_NODE_IP=${DEFAULT_IFC_IP}\nKUBELET_HOSTNAME=${FULL_HOSTNAME}" > /etc/kubernetes/nodeip.conf
elif [ ! -d /etc/systemd/system/kubelet.service.d ]
then
	echodate "Can't find kubelet service extras directory"
	exit 1
else
  echo -e "[Service]\nEnvironment=\"KUBELET_NODE_IP=${DEFAULT_IFC_IP}\"\nEnvironment=\"KUBELET_HOSTNAME=${FULL_HOSTNAME}\"" > /etc/systemd/system/kubelet.service.d/nodeip.conf
fi
	`
}

func SSHConfigAddendum() string {
	return `TrustedUserCAKeys /etc/ssh/trusted-user-ca-keys.pem
CASignatureAlgorithms ecdsa-sha2-nistp256,ecdsa-sha2-nistp384,ecdsa-sha2-nistp521,ssh-ed25519,rsa-sha2-512,rsa-sha2-256,ssh-rsa`
}
