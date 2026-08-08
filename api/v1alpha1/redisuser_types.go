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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ACLRule defines Redis ACL permissions. Every entry is a single ACL rule
// token: whitespace and Redis 7 selector syntax are rejected at admission, and
// the operator additionally refuses privilege-escalating rules (nopass, +@all,
// +@admin, +acl, +config, ...) at reconcile.
type ACLRule struct {
	// Categories are ACL categories (+@read, +@write, etc.)
	// +optional
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:items:Pattern=`^[^\s()]+$`
	Categories []string `json:"categories,omitempty"`

	// Commands are allowed Redis commands (+get, +set, -del, etc.)
	// +optional
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:items:Pattern=`^[^\s()]+$`
	Commands []string `json:"commands,omitempty"`

	// Keys are key patterns this user can access (~*, ~app:*, etc.)
	// +optional
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:items:Pattern=`^[^\s()]+$`
	Keys []string `json:"keys,omitempty"`

	// Channels are pub/sub channel patterns (&*, &notifications:*, etc.)
	// +optional
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:items:Pattern=`^[^\s()]+$`
	Channels []string `json:"channels,omitempty"`
}

// RedisUserSpec defines the desired state of RedisUser
type RedisUserSpec struct {
	// RedisClusterRef is the name of a RedisSentinel in this RedisUser's own
	// namespace. It is resolved verbatim, so it must be a DNS-1123 subdomain.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	RedisClusterRef string `json:"redisClusterRef"`

	// Username is the Redis ACL username. The name "default" is reserved: it
	// is the admin account whose password is requirepass, and redefining it
	// would reset the cluster password.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`
	// +kubebuilder:validation:XValidation:rule="self != 'default'",message="username 'default' is reserved for the Redis admin account"
	Username string `json:"username"`

	// PasswordSecretRef names a Secret in this RedisUser's own namespace holding
	// this user's password under the key "password". The name is resolved
	// verbatim, so it must be a DNS-1123 subdomain.
	// +required
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	PasswordSecretRef string `json:"passwordSecretRef"`

	// ACLRules define the permissions for this user. Nothing is granted
	// implicitly: a user with no rules can authenticate and reach no key,
	// channel or command.
	// +optional
	ACLRules ACLRule `json:"aclRules,omitempty"`

	// Enabled indicates whether the user may authenticate. A disabled user
	// keeps its rules but every AUTH for it is rejected.
	// +optional
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"` // pointer: an omitted false is re-defaulted to true on every update
}

// RedisUserStatus defines the observed state of RedisUser.
type RedisUserStatus struct {
	// Phase represents the current phase of the RedisUser
	// +optional
	Phase string `json:"phase,omitempty"`

	// ObservedGeneration is the spec generation this status was computed from.
	// A status whose observedGeneration trails metadata.generation describes the
	// previous spec, not the one in the object.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// AppliedTo tracks which Redis instances have this user configured
	// +optional
	AppliedTo []string `json:"appliedTo,omitempty"`

	// LastPasswordChange is the timestamp of the last password update
	// +optional
	LastPasswordChange *metav1.Time `json:"lastPasswordChange,omitempty"`

	// Conditions represent the current state of the RedisUser resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Username",type=string,JSONPath=`.spec.username`
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.redisClusterRef`
// +kubebuilder:printcolumn:name="Enabled",type=boolean,JSONPath=`.spec.enabled`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RedisUser is the Schema for the redisusers API
type RedisUser struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of RedisUser
	// +required
	Spec RedisUserSpec `json:"spec"`

	// status defines the observed state of RedisUser
	// +optional
	Status RedisUserStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RedisUserList contains a list of RedisUser
type RedisUserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RedisUser `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RedisUser{}, &RedisUserList{})
}
