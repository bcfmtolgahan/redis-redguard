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

// S3Config defines S3 storage backend configuration
type S3Config struct {
	// Bucket is the S3 bucket name
	// +required
	Bucket string `json:"bucket"`

	// Region is the AWS region
	// +optional
	Region string `json:"region,omitempty"`

	// Endpoint is the S3-compatible endpoint (for MinIO, etc.)
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Prefix is the path prefix for backups
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// CredentialsSecretRef references a Secret containing AWS credentials
	// Secret must have "accessKeyId" and "secretAccessKey" keys
	// +optional
	CredentialsSecretRef string `json:"credentialsSecretRef,omitempty"`

	// UseIAMRole indicates whether to use IAM role instead of credentials
	// +optional
	UseIAMRole bool `json:"useIAMRole,omitempty"`
}

// RedisBackupSpec defines the desired state of RedisBackup
type RedisBackupSpec struct {
	// RedisClusterRef references the RedisSentinel instance to backup
	// +required
	RedisClusterRef string `json:"redisClusterRef"`

	// Schedule defines cron expression for automated backups
	// Leave empty for one-time backup
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// S3 defines S3 storage configuration
	// +required
	S3 S3Config `json:"s3"`

	// RetentionPolicy defines how many backups to keep
	// +optional
	// +kubebuilder:default=7
	RetentionPolicy int32 `json:"retentionPolicy,omitempty"`

	// Compression enables gzip compression
	// +optional
	// +kubebuilder:default=true
	Compression bool `json:"compression,omitempty"`

	// Suspend pauses scheduled backups
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// RedisBackupStatus defines the observed state of RedisBackup.
type RedisBackupStatus struct {
	// Phase represents the current phase (Pending, Running, Completed, Failed)
	// +optional
	Phase string `json:"phase,omitempty"`

	// LastBackupTime is the timestamp of the last successful backup
	// +optional
	LastBackupTime *metav1.Time `json:"lastBackupTime,omitempty"`

	// NextBackupTime is the scheduled time for the next backup
	// +optional
	NextBackupTime *metav1.Time `json:"nextBackupTime,omitempty"`

	// BackupLocation is the S3 path of the last backup
	// +optional
	BackupLocation string `json:"backupLocation,omitempty"`

	// BackupSize is the size of the last backup in bytes
	// +optional
	BackupSize int64 `json:"backupSize,omitempty"`

	// BackupCount is the total number of backups retained
	// +optional
	BackupCount int32 `json:"backupCount,omitempty"`

	// LastBackupDuration is the duration of the last backup
	// +optional
	LastBackupDuration string `json:"lastBackupDuration,omitempty"`

	// Conditions represent the current state of the RedisBackup resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.redisClusterRef`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Last Backup",type=date,JSONPath=`.status.lastBackupTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RedisBackup is the Schema for the redisbackups API
type RedisBackup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of RedisBackup
	// +required
	Spec RedisBackupSpec `json:"spec"`

	// status defines the observed state of RedisBackup
	// +optional
	Status RedisBackupStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RedisBackupList contains a list of RedisBackup
type RedisBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RedisBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RedisBackup{}, &RedisBackupList{})
}
