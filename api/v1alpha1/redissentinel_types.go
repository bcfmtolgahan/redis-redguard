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

// Placement controls where one component's pods are scheduled. It is inlined
// into RedisConfig and SentinelConfig: the two are scheduled independently, and
// a node pool sized for Redis is rarely where the Sentinels belong.
type Placement struct {
	// NodeSelector restricts the pods to nodes carrying all of these labels
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Affinity replaces the scheduling rules for these pods. When
	// podAntiAffinity is absent the operator adds a preferred anti-affinity
	// that spreads the component across nodes; set podAntiAffinity to {} to
	// opt out of it entirely.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// Tolerations let the pods schedule onto tainted nodes
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// TopologySpreadConstraints spread the pods across failure domains such
	// as zones. Anti-affinity spreads by node; this is the wider guarantee.
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// PriorityClassName sets the scheduling priority and the order in which
	// the kubelet evicts pods under node pressure
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`
}

// RedisConfig defines Redis server configuration
// +kubebuilder:validation:XValidation:rule="has(self.storage) == has(oldSelf.storage)",message="redisConfig.storage is immutable: adding or removing it would change the StatefulSet volumeClaimTemplates, which Kubernetes forbids. Recreate the RedisSentinel instead."
type RedisConfig struct {
	// Placement controls where the Redis pods are scheduled
	Placement `json:",inline"`

	// Replicas is the number of Redis instances, master included. One is legal
	// and useful for development, but leaves Sentinel with nothing to promote;
	// the operator then reports HighlyAvailable=False on the cluster.
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

	// CustomConfig allows custom Redis configuration. A key is one directive
	// name; keys and values must not contain line breaks, and directives the
	// operator owns (requirepass, masterauth, user, aclfile, tls-*, port,
	// bind, dir, replicaof, slaveof, ...) are rejected at reconcile.
	// +optional
	CustomConfig map[string]string `json:"customConfig,omitempty"`

	// Auth defines authentication configuration
	// +optional
	Auth *AuthConfig `json:"auth,omitempty"`
}

// SentinelConfig defines Sentinel configuration
// +kubebuilder:validation:XValidation:rule="self.quorum <= self.replicas",message="sentinelConfig.quorum must not exceed sentinelConfig.replicas: a quorum larger than the number of Sentinels can never be reached, so no failover can ever start while every pod still reports healthy"
type SentinelConfig struct {
	// Placement controls where the Sentinel pods are scheduled
	Placement `json:",inline"`

	// Replicas is the number of Sentinel instances (min 3 recommended)
	// +kubebuilder:validation:Minimum=3
	// +kubebuilder:default=3
	Replicas int32 `json:"replicas,omitempty"`

	// Quorum is the number of Sentinels that need to agree about master failure
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:default=2
	Quorum int32 `json:"quorum,omitempty"`

	// DownAfterMilliseconds is the time in ms before marking instance as down.
	// Written verbatim into sentinel.conf, where zero means every instance is
	// considered down immediately and permanently.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=5000
	DownAfterMilliseconds int32 `json:"downAfterMilliseconds,omitempty"`

	// FailoverTimeout is the failover timeout in milliseconds. Zero aborts every
	// failover the moment it starts, leaving the cluster without a master.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=10000
	FailoverTimeout int32 `json:"failoverTimeout,omitempty"`

	// ParallelSyncs is the number of replicas that can be reconfigured in
	// parallel. Zero resynchronises no replica after a promotion.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	ParallelSyncs int32 `json:"parallelSyncs,omitempty"`

	// Resources defines resource requirements for Sentinel pods
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// CustomConfig allows custom Sentinel configuration. A key is one
	// directive name or "sentinel <name>"; keys and values must not contain
	// line breaks, and directives the operator owns (sentinel monitor,
	// sentinel auth-pass, port, bind, tls-*, ...) are rejected at reconcile.
	// +optional
	CustomConfig map[string]string `json:"customConfig,omitempty"`
}

// StorageSpec defines persistent storage configuration
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="redisConfig.storage is immutable: a StatefulSet volumeClaimTemplate cannot be resized or moved to another storage class after creation. Resize the PersistentVolumeClaims directly, or recreate the RedisSentinel."
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
	// SecretName names a Secret in this RedisSentinel's own namespace holding
	// the Redis admin password under the key "password". The name is resolved
	// verbatim, so it must be a DNS-1123 subdomain.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	SecretName string `json:"secretName,omitempty"`
}

// TLSConfig defines TLS/SSL configuration
// +kubebuilder:validation:XValidation:rule="!(has(self.enabled) && self.enabled) || has(self.certificateSecretRef)",message="certificateSecretRef is required when TLS is enabled"
type TLSConfig struct {
	// Enabled indicates if TLS is enabled
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// CertificateSecretRef names a Secret in this RedisSentinel's own namespace
	// holding the server certificate under "tls.crt" and its private key under
	// "tls.key". The name is resolved verbatim, so it must be a DNS-1123
	// subdomain.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	CertificateSecretRef string `json:"certificateSecretRef,omitempty"`

	// CASecretRef names a Secret in this RedisSentinel's own namespace holding
	// the CA certificate under "ca.crt". When unset, servers are verified
	// against the system trust store. The name is resolved verbatim, so it must
	// be a DNS-1123 subdomain.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
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

	// ObservedGeneration is the spec generation this status was computed from.
	// A status whose observedGeneration trails metadata.generation describes the
	// previous spec, not the one in the object.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// MasterNode is the host:port address of the current Redis master as
	// reported by Sentinel; the host is the master pod's IP.
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
