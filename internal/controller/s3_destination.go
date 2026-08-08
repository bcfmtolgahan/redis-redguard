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

package controller

import (
	"fmt"
	"slices"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
)

// validateS3Destination enforces the operator-level destination policy shared
// by RedisBackup and RedisRestore: with useIAMRole the request runs under the
// operator's own AWS identity, so any namespace user could otherwise aim that
// identity at an arbitrary bucket (writing on the backup side, reading any
// reachable object on the restore side) or at an arbitrary endpoint that
// receives requests signed with it. Tenant-supplied credentials
// (credentialsSecretRef) carry only privileges the tenant already holds and
// are not restricted. Both controllers share the one --allowed-backup-buckets
// and --allowed-backup-endpoints configuration; an empty bucket allowlist
// disables the IAM-role path entirely.
func validateS3Destination(s3cfg *redisv1alpha1.S3Config, allowedBuckets, allowedEndpoints []string) error {
	if !s3cfg.UseIAMRole {
		return nil
	}
	if len(allowedBuckets) == 0 {
		return fmt.Errorf("s3.useIAMRole is set but the operator runs without --allowed-backup-buckets; the IAM-role path is disabled, use s3.credentialsSecretRef instead")
	}
	if !slices.Contains(allowedBuckets, s3cfg.Bucket) {
		return fmt.Errorf("bucket %q is not in the operator's --allowed-backup-buckets", s3cfg.Bucket)
	}
	if ep := s3cfg.Endpoint; ep != "" && !slices.Contains(allowedEndpoints, ep) {
		return fmt.Errorf("endpoint %q is not in the operator's --allowed-backup-endpoints; a custom endpoint would redirect requests signed with the operator's identity", ep)
	}
	return nil
}
