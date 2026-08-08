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
// +kubebuilder:validation:XValidation:rule="has(self.credentialsSecretRef) != (has(self.useIAMRole) && self.useIAMRole)",message="exactly one of s3.credentialsSecretRef or s3.useIAMRole must be set: with neither, the AWS SDK signs the request with whatever ambient identity the operator pod carries and never passes the destination allowlist; with both, the Secret wins and useIAMRole says nothing about which identity signed"
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

	// Prefix is the path prefix for backups. RedisBackup writes objects
	// under <prefix>/<namespace>/<clusterName>/ and retention only ever
	// deletes objects under that exact prefix.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// CredentialsSecretRef names a Secret in the referring CR's own namespace
	// holding "accessKeyId" and "secretAccessKey". The name is resolved
	// verbatim, so it must be a DNS-1123 subdomain.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	CredentialsSecretRef string `json:"credentialsSecretRef,omitempty"`

	// UseIAMRole indicates whether to use IAM role instead of credentials.
	// The request then runs under the operator's own AWS identity;
	// RedisBackup rejects destinations whose bucket is not listed in the
	// operator's --allowed-backup-buckets or whose custom endpoint is not
	// in --allowed-backup-endpoints.
	// +optional
	UseIAMRole bool `json:"useIAMRole,omitempty"`
}

// RedisBackupSpec defines the desired state of RedisBackup
type RedisBackupSpec struct {
	// RedisClusterRef references the RedisSentinel instance to backup
	// +required
	RedisClusterRef string `json:"redisClusterRef"`

	// Schedule is a five-field cron expression (minute hour day-of-month month
	// day-of-week), optionally prefixed with TZ=/CRON_TZ=. Descriptors such as
	// @daily are not accepted. Leave empty for a one-time backup. The pattern
	// only fixes the shape; ranges are checked by the controller, which reports
	// a parse failure as Degraded.
	// +optional
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^$|^((TZ|CRON_TZ)=[A-Za-z0-9_+/-]+\s+)?[0-9A-Za-z*/,-]+(\s+[0-9A-Za-z*/,-]+){4}$`
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

	// ObservedGeneration is the spec generation this status was computed from.
	// A status whose observedGeneration trails metadata.generation describes the
	// previous spec, not the one in the object.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

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

	// KeyCount is the number of keys across all databases when the last backup
	// was taken; RedisRestore uses it to verify a restored dataset.
	// +optional
	KeyCount *int64 `json:"keyCount,omitempty"`

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
