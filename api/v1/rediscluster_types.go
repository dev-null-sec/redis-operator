/*
Copyright 2025.

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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RedisClusterSpec 定义 RedisCluster 的期望状态
type RedisClusterSpec struct {
	// Replicas 定义集群节点数量
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas,omitempty"`

	// Image Redis 镜像版本
	Image string `json:"image,omitempty"`

	// Port Redis 服务端口
	Port int32 `json:"port,omitempty"`

	// EnableSentinel 是否启用哨兵模式
	EnableSentinel bool `json:"enableSentinel,omitempty"`

	// StorageSize 存储卷大小
	StorageSize string `json:"storageSize,omitempty"`
}

// RedisClusterStatus 定义 RedisCluster 的观测状态
type RedisClusterStatus struct {
	// ReadyReplicas 当前就绪的副本数量
	ReadyReplicas int32 `json:"readyReplicas"`

	// State 集群状态
	State string `json:"state,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// RedisCluster is the Schema for the redisclusters API
type RedisCluster struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of RedisCluster
	// +required
	Spec RedisClusterSpec `json:"spec"`

	// status defines the observed state of RedisCluster
	// +optional
	Status RedisClusterStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RedisClusterList contains a list of RedisCluster
type RedisClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RedisCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RedisCluster{}, &RedisClusterList{})
}
