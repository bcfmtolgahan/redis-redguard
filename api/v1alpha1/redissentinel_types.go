/*
Copyright 2026.

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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// RedisConfig defines Redis server configuration
type RedisConfig struct {
	// Replicas is the number of Redis replicas
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	Replicas int32 `json:"replicas,omitempty"`

	// Image is the Redis Docker image
	// +kubebuilder:default="redis:7-alpine"
	Image string `json:"image,omitempty"`

	// Resources defines resource requirements for Redis pods
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Storage defines persistent storage configuration
	// +optional
	Storage *StorageSpec `json:"storage,omitempty"`

	// CustomConfig allows custom Redis configuration
	// +optional
	CustomConfig map[string]string `json:"customConfig,omitempty"`

	// Auth defines authentication configuration
	// +optional
	Auth *AuthConfig `json:"auth,omitempty"`
}

// SentinelConfig defines Sentinel configuration
type SentinelConfig struct {
	// Replicas is the number of Sentinel instances (min 3 recommended)
	// +kubebuilder:validation:Minimum=3
	// +kubebuilder:default=3
	Replicas int32 `json:"replicas,omitempty"`

	// Quorum is the number of Sentinels that need to agree about master failure
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:default=2
	Quorum int32 `json:"quorum,omitempty"`

	// DownAfterMilliseconds is the time in ms before marking instance as down
	// +kubebuilder:default=5000
	DownAfterMilliseconds int32 `json:"downAfterMilliseconds,omitempty"`

	// FailoverTimeout is the failover timeout in milliseconds
	// +kubebuilder:default=10000
	FailoverTimeout int32 `json:"failoverTimeout,omitempty"`

	// ParallelSyncs is the number of replicas that can be reconfigured in parallel
	// +kubebuilder:default=1
	ParallelSyncs int32 `json:"parallelSyncs,omitempty"`

	// Resources defines resource requirements for Sentinel pods
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// CustomConfig allows custom Sentinel configuration
	// +optional
	CustomConfig map[string]string `json:"customConfig,omitempty"`
}

// StorageSpec defines persistent storage configuration
type StorageSpec struct {
	// Size is the storage size
	// +kubebuilder:default="1Gi"
	Size resource.Quantity `json:"size,omitempty"`

	// StorageClassName is the storage class name
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`
}

// AuthConfig defines authentication configuration
type AuthConfig struct {
	// SecretName is the name of the secret containing Redis password
	// Key must be "password"
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// TLSConfig defines TLS/SSL configuration
// +kubebuilder:validation:XValidation:rule="!(has(self.enabled) && self.enabled) || has(self.certificateSecretRef)",message="certificateSecretRef is required when TLS is enabled"
type TLSConfig struct {
	// Enabled indicates if TLS is enabled
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// CertificateSecretRef references a Secret containing TLS certificates
	// Secret must have "tls.crt" and "tls.key" keys
	// +optional
	CertificateSecretRef string `json:"certificateSecretRef,omitempty"`

	// CASecretRef references a Secret containing CA certificate
	// Secret must have "ca.crt" key
	// When unset, servers are verified against the system trust store
	// +optional
	CASecretRef string `json:"caSecretRef,omitempty"`

	// MutualTLS enables mutual TLS authentication
	// +optional
	MutualTLS bool `json:"mutualTLS,omitempty"`
}

// RedisSentinelSpec defines the desired state of RedisSentinel
type RedisSentinelSpec struct {
	// RedisConfig defines Redis server configuration
	// +required
	RedisConfig RedisConfig `json:"redisConfig"`

	// SentinelConfig defines Sentinel configuration
	// +required
	SentinelConfig SentinelConfig `json:"sentinelConfig"`

	// TLS defines TLS/SSL configuration
	// +optional
	TLS *TLSConfig `json:"tls,omitempty"`

	// ServiceType specifies the type of service for external access
	// +kubebuilder:default="ClusterIP"
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`
}

// RedisSentinelStatus defines the observed state of RedisSentinel.
type RedisSentinelStatus struct {
	// Phase represents the current phase of the RedisSentinel
	// +optional
	Phase string `json:"phase,omitempty"`

	// MasterNode is the current Redis master pod name
	// +optional
	MasterNode string `json:"masterNode,omitempty"`

	// ReadyReplicas is the number of ready Redis replicas
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// ReadySentinels is the number of ready Sentinel instances
	// +optional
	ReadySentinels int32 `json:"readySentinels,omitempty"`

	// LastFailoverTime is the timestamp of the last failover
	// +optional
	LastFailoverTime *metav1.Time `json:"lastFailoverTime,omitempty"`

	// Conditions represent the current state of the RedisSentinel resource.
	// Standard condition types:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Master",type=string,JSONPath=`.status.masterNode`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Sentinels",type=integer,JSONPath=`.status.readySentinels`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RedisSentinel is the Schema for the redissentinels API
type RedisSentinel struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of RedisSentinel
	// +required
	Spec RedisSentinelSpec `json:"spec"`

	// status defines the observed state of RedisSentinel
	// +optional
	Status RedisSentinelStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RedisSentinelList contains a list of RedisSentinel
type RedisSentinelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RedisSentinel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RedisSentinel{}, &RedisSentinelList{})
}
