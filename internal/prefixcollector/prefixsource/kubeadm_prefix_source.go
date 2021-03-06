// Copyright (c) 2020 Doc.ai and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prefixsource

import (
	"cmd-exclude-prefixes-k8s/internal/prefixcollector"
	"cmd-exclude-prefixes-k8s/internal/utils"
	"context"
	"strings"

	"github.com/networkservicemesh/sdk/pkg/tools/spanhelper"

	apiV1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/watch"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"sigs.k8s.io/cluster-api/bootstrap/kubeadm/types/v1beta2"
)

const (
	// KubeNamespace is KubeAdm ConfigMap namespace
	KubeNamespace = "kube-system"
	// KubeName is KubeAdm ConfigMap name
	KubeName   = "kubeadm-config"
	bufferSize = 4096
)

// KubeAdmPrefixSource is KubeAdm ConfigMap excluded prefix source
type KubeAdmPrefixSource struct {
	configMapInterface v1.ConfigMapInterface
	prefixes           *utils.SynchronizedPrefixesContainer
	ctx                context.Context
	notify             chan<- struct{}
	span               spanhelper.SpanHelper
}

// Prefixes returns prefixes from source
func (kaps *KubeAdmPrefixSource) Prefixes() []string {
	return kaps.prefixes.Load()
}

// NewKubeAdmPrefixSource creates KubeAdmPrefixSource
func NewKubeAdmPrefixSource(ctx context.Context, notify chan<- struct{}) *KubeAdmPrefixSource {
	clientSet := prefixcollector.KubernetesInterface(ctx)
	configMapInterface := clientSet.CoreV1().ConfigMaps(KubeNamespace)
	kaps := KubeAdmPrefixSource{
		configMapInterface: configMapInterface,
		ctx:                ctx,
		notify:             notify,
		prefixes:           utils.NewSynchronizedPrefixesContainer(),
	}

	go kaps.watchKubeAdmConfigMap()
	return &kaps
}

func (kaps *KubeAdmPrefixSource) watchKubeAdmConfigMap() {
	kaps.span = spanhelper.FromContext(kaps.ctx, "Watch kubeadm configMap")
	defer kaps.span.Finish()
	logger := kaps.span.Logger()

	kaps.checkCurrentConfigMap()
	configMapWatch, err := kaps.configMapInterface.Watch(kaps.ctx, metav1.ListOptions{})
	if err != nil {
		logger.Errorf("Error creating config map watch: %v", err)
		return
	}

	for {
		select {
		case <-kaps.ctx.Done():
			return
		case event, ok := <-configMapWatch.ResultChan():
			if !ok {
				return
			}

			if event.Type == watch.Error {
				continue
			}

			configMap, ok := event.Object.(*apiV1.ConfigMap)
			if !ok || configMap.Name != KubeName {
				continue
			}

			if event.Type == watch.Deleted {
				kaps.prefixes.Store([]string(nil))
				kaps.notify <- struct{}{}
				continue
			}

			if err = kaps.setPrefixesFromConfigMap(configMap); err != nil {
				logger.Error(err)
			}
		}
	}
}

func (kaps *KubeAdmPrefixSource) checkCurrentConfigMap() {
	configMap, err := kaps.configMapInterface.Get(kaps.ctx, KubeName, metav1.GetOptions{})
	logger := kaps.span.Logger()

	if err != nil {
		logger.Errorf("Error getting KubeAdm config map : %v", err)
		return
	}

	if err = kaps.setPrefixesFromConfigMap(configMap); err != nil {
		logger.Error(err)
	}
}

func (kaps *KubeAdmPrefixSource) setPrefixesFromConfigMap(configMap *apiV1.ConfigMap) error {
	logger := kaps.span.Logger()

	clusterConfiguration := &v1beta2.ClusterConfiguration{}
	err := yaml.NewYAMLOrJSONDecoder(
		strings.NewReader(configMap.Data["ClusterConfiguration"]), bufferSize,
	).Decode(clusterConfiguration)

	if err != nil {
		return err
	}

	podSubnet := clusterConfiguration.Networking.PodSubnet
	serviceSubnet := clusterConfiguration.Networking.ServiceSubnet

	if podSubnet == "" {
		logger.Error("ClusterConfiguration.Networking.PodSubnet is empty")
	}
	if serviceSubnet == "" {
		logger.Error("ClusterConfiguration.Networking.ServiceSubnet is empty")
	}

	prefixes := []string{podSubnet, serviceSubnet}

	kaps.prefixes.Store(prefixes)
	kaps.notify <- struct{}{}
	logger.Infof("Prefixes sent from kubeadm source: %v", prefixes)

	return nil
}
