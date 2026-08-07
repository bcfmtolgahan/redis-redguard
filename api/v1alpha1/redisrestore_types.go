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

// BackupSource defines the source of the backup to restore from
type BackupSource struct {
	// S3 defines S3 storage configuration
	// +required
	S3 S3Config `json:"s3"`

	// BackupPath is the full S3 path to the backup file
	// Example: "backups/myredis/backup-20240101-120000.rdb.gz"
	// +required
	BackupPath string `json:"backupPath"`

	// Compressed indicates if the backup is gzip compressed
	// +optional
	// +kubebuilder:default=true
	Compressed bool `json:"compressed,omitempty"`
}

// RedisRestoreSpec defines the desired state of RedisRestore
type RedisRestoreSpec struct {
	// RedisClusterRef references the RedisSentinel instance to restore
	// +required
	RedisClusterRef string `json:"redisClusterRef"`

	// BackupSource defines where to restore from
	// +required
	BackupSource BackupSource `json:"backupSource"`

	// Force allows restore even if the cluster has existing data
	// WARNING: This will overwrite existing data!
	// +optional
	Force bool `json:"force,omitempty"`

	// SkipDataCheck skips the check for existing data before restore
	// Use with caution as it may cause data loss
	// +optional
	SkipDataCheck bool `json:"skipDataCheck,omitempty"`
}

// RestorePhase represents the current phase of restore operation
type RestorePhase string

const (
	RestorePhasePending     RestorePhase = "Pending"
	RestorePhaseDownloading RestorePhase = "Downloading"
	RestorePhaseRestoring   RestorePhase = "Restoring"
	RestorePhaseVerifying   RestorePhase = "Verifying"
	RestorePhaseCompleted   RestorePhase = "Completed"
	RestorePhaseFailed      RestorePhase = "Failed"
)

// RedisRestoreStatus defines the observed state of RedisRestore
type RedisRestoreStatus struct {
	// Phase represents the current phase of the restore operation
	// +optional
	Phase RestorePhase `json:"phase,omitempty"`

	// ObservedGeneration is the spec generation the recorded phase applies to.
	// A Completed or Failed restore re-runs only when the spec changes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// QuiescedDownAfterMilliseconds records the sentinel down-after-milliseconds
	// value to restore once the master restart is over; zero means the sentinels
	// are not quiesced by this restore.
	// +optional
	QuiescedDownAfterMilliseconds int32 `json:"quiescedDownAfterMilliseconds,omitempty"`

	// StartTime is when the restore operation started
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the restore operation completed
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Duration is the total duration of the restore operation
	// +optional
	Duration string `json:"duration,omitempty"`

	// RestoredFrom contains the backup path that was restored
	// +optional
	RestoredFrom string `json:"restoredFrom,omitempty"`

	// RestoredDataSize is the size of the restored data in bytes
	// +optional
	RestoredDataSize int64 `json:"restoredDataSize,omitempty"`

	// Message provides additional information about the current state
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions represent the current state of the RedisRestore resource
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.redisClusterRef`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Duration",type=string,JSONPath=`.status.duration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RedisRestore is the Schema for the redisrestores API
type RedisRestore struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of RedisRestore
	// +required
	Spec RedisRestoreSpec `json:"spec"`

	// status defines the observed state of RedisRestore
	// +optional
	Status RedisRestoreStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RedisRestoreList contains a list of RedisRestore
type RedisRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []RedisRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RedisRestore{}, &RedisRestoreList{})
}
