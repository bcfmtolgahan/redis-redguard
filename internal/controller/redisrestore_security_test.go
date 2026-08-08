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
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
)

// TestRestoreRejectsBucketOutsideAllowlist mirrors the backup-side coverage on
// the read path: a RedisRestore using the operator's ambient IAM identity can
// read any object that identity reaches, including other tenants' backups, so
// it must be refused before any Redis or S3 side effect, terminally and under
// its own reason.
func TestRestoreRejectsBucketOutsideAllowlist(t *testing.T) {
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Spec.BackupSource.S3.CredentialsSecretRef = ""
		r.Spec.BackupSource.S3.UseIAMRole = true
		r.Spec.BackupSource.S3.Bucket = "exfil-bucket"
	})
	fx.r.AllowedBuckets = []string{"corp-backups"}

	res, err := fx.reconcile(t)
	if err != nil {
		t.Errorf("rejection must not return an error (it would hot-loop with backoff), got: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("rejection is terminal and must not requeue, got %+v", res)
	}
	if calls := fx.factory.Calls(); len(calls) != 0 {
		t.Errorf("no Redis call may happen for a disallowed destination, got %v", calls)
	}

	restore := fx.get(t)
	if restore.Status.Phase != redisv1alpha1.RestorePhaseFailed {
		t.Errorf("phase = %q, want Failed", restore.Status.Phase)
	}
	if !strings.Contains(restore.Status.Message, "exfil-bucket") {
		t.Errorf("message %q does not name the rejected bucket", restore.Status.Message)
	}
	var ready *metav1.Condition
	for i := range restore.Status.Conditions {
		if restore.Status.Conditions[i].Type == "Ready" {
			ready = &restore.Status.Conditions[i]
		}
	}
	if ready == nil {
		t.Fatal("no Ready condition set")
	}
	if ready.Reason != "DestinationNotAllowed" {
		t.Errorf("condition reason = %q, want DestinationNotAllowed", ready.Reason)
	}
	if ready.Status != metav1.ConditionFalse {
		t.Errorf("condition status = %q, want False", ready.Status)
	}

	// The rejection is sticky for this generation: the next reconcile must not
	// re-run anything.
	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("reconcile of a rejected restore: %v", err)
	}
	if calls := fx.factory.Calls(); len(calls) != 0 {
		t.Errorf("rejected restore dialed Redis on requeue: %v", calls)
	}
}

// TestRestoreDestinationPolicy exercises the same allowlist matrix as the
// backup side: the IAM-role path fails closed with no allowlist, buckets and
// custom endpoints must both be allowlisted, and tenant-supplied credentials
// are never restricted.
func TestRestoreDestinationPolicy(t *testing.T) {
	cases := []struct {
		name             string
		useIAMRole       bool
		bucket           string
		endpoint         string
		allowedBuckets   []string
		allowedEndpoints []string
		wantErr          string
	}{
		{
			name:       "iam role with no allowlist fails closed",
			useIAMRole: true,
			bucket:     "any-bucket",
			wantErr:    "--allowed-backup-buckets",
		},
		{
			name:           "iam role with bucket outside allowlist",
			useIAMRole:     true,
			bucket:         "exfil-bucket",
			allowedBuckets: []string{"corp-backups"},
			wantErr:        "exfil-bucket",
		},
		{
			name:           "iam role with allowlisted bucket",
			useIAMRole:     true,
			bucket:         "corp-backups",
			allowedBuckets: []string{"corp-backups"},
		},
		{
			name:           "iam role with custom endpoint not allowlisted",
			useIAMRole:     true,
			bucket:         "corp-backups",
			endpoint:       "https://attacker.example",
			allowedBuckets: []string{"corp-backups"},
			wantErr:        "attacker.example",
		},
		{
			name:             "iam role with allowlisted endpoint",
			useIAMRole:       true,
			bucket:           "corp-backups",
			endpoint:         "https://bucket.vpce-1234.s3.us-east-1.vpce.amazonaws.com",
			allowedBuckets:   []string{"corp-backups"},
			allowedEndpoints: []string{"https://bucket.vpce-1234.s3.us-east-1.vpce.amazonaws.com"},
		},
		{
			name:   "tenant credentials are unrestricted",
			bucket: "tenant-bucket",
		},
		{
			name:     "tenant credentials with custom endpoint are unrestricted",
			bucket:   "tenant-bucket",
			endpoint: "https://minio.tenant.svc:9000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := &redisv1alpha1.RedisRestore{
				Spec: redisv1alpha1.RedisRestoreSpec{
					BackupSource: redisv1alpha1.BackupSource{
						S3: redisv1alpha1.S3Config{
							Bucket:     tc.bucket,
							Endpoint:   tc.endpoint,
							UseIAMRole: tc.useIAMRole,
						},
						BackupPath: "backups/prod/cluster-a/nightly/backup-1.rdb.gz",
					},
				},
			}
			if !tc.useIAMRole {
				restore.Spec.BackupSource.S3.CredentialsSecretRef = "tenant-creds"
			}

			r := &RedisRestoreReconciler{
				AllowedBuckets:   tc.allowedBuckets,
				AllowedEndpoints: tc.allowedEndpoints,
			}
			err := r.validateRestoreDestination(restore)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected rejection: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("destination must be rejected, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestMainWiresRestoreAllowlists guards the wiring: the backup and restore
// reconcilers must both receive the allowlist flags, or the restore path stays
// a confused deputy however the operator is configured.
func TestMainWiresRestoreAllowlists(t *testing.T) {
	src, err := os.ReadFile("../../cmd/main.go")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)controller\.RedisRestoreReconciler\{.*?\}\)\.SetupWithManager`).Find(src)
	if block == nil {
		t.Fatal("cmd/main.go does not construct a RedisRestoreReconciler")
	}
	for _, field := range []string{"AllowedBuckets", "AllowedEndpoints"} {
		if !strings.Contains(string(block), field) {
			t.Errorf("cmd/main.go builds RedisRestoreReconciler without %s; the restore destination policy is unenforced", field)
		}
	}
}

// TestRestoreS3ClientEnforcesDestinationPolicy pins the defence in depth: even
// a future call site that skips the reconcile-entry check cannot reach AWS with
// the operator's identity.
func TestRestoreS3ClientEnforcesDestinationPolicy(t *testing.T) {
	restore := &redisv1alpha1.RedisRestore{
		Spec: redisv1alpha1.RedisRestoreSpec{
			BackupSource: redisv1alpha1.BackupSource{
				S3: redisv1alpha1.S3Config{
					Bucket:     "exfil-bucket",
					UseIAMRole: true,
				},
				BackupPath: "any",
			},
		},
	}
	r := &RedisRestoreReconciler{AllowedBuckets: []string{"corp-backups"}}
	if _, err := r.createS3Client(context.Background(), restore); err == nil {
		t.Fatal("createS3Client must refuse a disallowed IAM-role destination")
	} else if !strings.Contains(err.Error(), "exfil-bucket") {
		t.Errorf("error %q does not name the rejected bucket", err)
	}
}
