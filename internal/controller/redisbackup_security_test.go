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
	"errors"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
)

// fakeS3Store is an in-memory S3 implementing the subset the backup
// controller uses. Listing filters by prefix and paginates like S3;
// per-key delete errors are injectable.
type fakeS3Store struct {
	objects      map[string]time.Time
	pageSize     int
	deleteErrs   map[string]error
	putKeys      []string
	deleted      []string
	listPrefixes []string
}

func newFakeS3Store() *fakeS3Store {
	return &fakeS3Store{
		objects:    map[string]time.Time{},
		deleteErrs: map[string]error{},
	}
}

func (f *fakeS3Store) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	key := aws.ToString(in.Key)
	f.putKeys = append(f.putKeys, key)
	f.objects[key] = time.Now()
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3Store) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	prefix := aws.ToString(in.Prefix)
	f.listPrefixes = append(f.listPrefixes, prefix)

	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	start := 0
	if in.ContinuationToken != nil {
		start, _ = strconv.Atoi(*in.ContinuationToken)
	}
	end := len(keys)
	if f.pageSize > 0 && start+f.pageSize < end {
		end = start + f.pageSize
	}

	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(end < len(keys))}
	for _, k := range keys[start:end] {
		out.Contents = append(out.Contents, s3types.Object{
			Key:          aws.String(k),
			LastModified: aws.Time(f.objects[k]),
		})
	}
	if end < len(keys) {
		out.NextContinuationToken = aws.String(strconv.Itoa(end))
	}
	return out, nil
}

func (f *fakeS3Store) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	key := aws.ToString(in.Key)
	if err, ok := f.deleteErrs[key]; ok {
		return nil, err
	}
	f.deleted = append(f.deleted, key)
	delete(f.objects, key)
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3Store) calls() int {
	return len(f.putKeys) + len(f.deleted) + len(f.listPrefixes)
}

func testBackup() *redisv1alpha1.RedisBackup {
	return &redisv1alpha1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "prod"},
		Spec: redisv1alpha1.RedisBackupSpec{
			RedisClusterRef: "cluster-a",
			S3: redisv1alpha1.S3Config{
				Bucket: "corp-backups",
				Region: "us-east-1",
				Prefix: "backups",
			},
			RetentionPolicy: 1,
			Compression:     true,
		},
	}
}

func backupReconcilerWithS3(f *fakeS3Store) *RedisBackupReconciler {
	return &RedisBackupReconciler{
		newS3Client: func(context.Context, *redisv1alpha1.RedisBackup) (s3API, error) {
			return f, nil
		},
	}
}

// TestBackupRejectsBucketOutsideAllowlist covers the confused-deputy surface:
// a RedisBackup using the operator's ambient IAM identity must be refused
// before any Redis or S3 side effect unless the bucket is allowlisted, and the
// refusal must be visible in status under its own reason.
func TestBackupRejectsBucketOutsideAllowlist(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := redisv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	backup := testBackup()
	backup.Spec.S3.Bucket = "exfil-bucket"
	backup.Spec.S3.UseIAMRole = true

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(backup).
		WithStatusSubresource(&redisv1alpha1.RedisBackup{}).
		Build()

	fakeS3 := newFakeS3Store()
	r := backupReconcilerWithS3(fakeS3)
	r.Client = c
	r.Scheme = scheme
	r.AllowedBuckets = []string{"corp-backups"}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "nightly", Namespace: "prod"}}
	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Errorf("rejection must not return an error (it would hot-loop with backoff), got: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Errorf("rejection should requeue slowly, got %v", res.RequeueAfter)
	}
	if fakeS3.calls() != 0 {
		t.Errorf("no S3 call may happen for a disallowed destination; got puts=%v deletes=%v lists=%v",
			fakeS3.putKeys, fakeS3.deleted, fakeS3.listPrefixes)
	}

	got := &redisv1alpha1.RedisBackup{}
	if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != "Failed" {
		t.Errorf("phase = %q, want Failed", got.Status.Phase)
	}
	var ready *metav1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == "Ready" {
			ready = &got.Status.Conditions[i]
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
}

// TestRetentionOnlyDeletesOwnClusterObjects pins the retention blast radius:
// the listed prefix is scoped to <prefix>/<namespace>/<cluster>/ and only
// direct children named like backup objects are ever deleted.
func TestRetentionOnlyDeletesOwnClusterObjects(t *testing.T) {
	backup := testBackup()

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	own := []string{
		"backups/prod/cluster-a/backup-20260101-000000.rdb.gz",
		"backups/prod/cluster-a/backup-20260102-000000.rdb.gz",
		"backups/prod/cluster-a/backup-20260103-000000.rdb.gz",
	}
	foreign := []string{
		"backups/staging/cluster-a/backup-20250101-000000.rdb.gz",
		"backups/prod/cluster-a-canary/backup-20250601-000000.rdb.gz",
		"backups/prod/cluster-a/wal/backup-20250101-000000.rdb.gz",
		"backups/prod/cluster-a/notes.txt",
	}

	fakeS3 := newFakeS3Store()
	for i, k := range own {
		fakeS3.objects[k] = t0.AddDate(0, 0, i)
	}
	for _, k := range foreign {
		// Older than every own backup, so an unscoped sort deletes these first.
		fakeS3.objects[k] = t0.AddDate(-1, 0, 0)
	}

	r := backupReconcilerWithS3(fakeS3)
	if err := r.cleanupOldBackups(context.Background(), backup); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}

	wantDeleted := []string{own[0], own[1]}
	sort.Strings(fakeS3.deleted)
	if strings.Join(fakeS3.deleted, ",") != strings.Join(wantDeleted, ",") {
		t.Errorf("deleted %v, want exactly %v", fakeS3.deleted, wantDeleted)
	}
	for _, k := range foreign {
		if _, ok := fakeS3.objects[k]; !ok {
			t.Errorf("retention deleted %s, which belongs to another namespace, cluster, or was never written by the operator", k)
		}
	}
	for _, p := range fakeS3.listPrefixes {
		if p != "backups/prod/cluster-a/" {
			t.Errorf("retention listed prefix %q, want the namespace-and-cluster-scoped %q", p, "backups/prod/cluster-a/")
		}
	}
}

// TestRetentionPaginatesAcrossListPages seeds more backups than one list page
// returns; unpaginated retention undercounts and silently stops pruning.
func TestRetentionPaginatesAcrossListPages(t *testing.T) {
	backup := testBackup()
	backup.Spec.RetentionPolicy = 2

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fakeS3 := newFakeS3Store()
	fakeS3.pageSize = 2
	var keys []string
	for i := 1; i <= 5; i++ {
		k := "backups/prod/cluster-a/backup-2026010" + strconv.Itoa(i) + "-000000.rdb.gz"
		keys = append(keys, k)
		fakeS3.objects[k] = t0.AddDate(0, 0, i)
	}

	r := backupReconcilerWithS3(fakeS3)
	if err := r.cleanupOldBackups(context.Background(), backup); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}

	wantDeleted := keys[:3]
	sort.Strings(fakeS3.deleted)
	if strings.Join(fakeS3.deleted, ",") != strings.Join(wantDeleted, ",") {
		t.Errorf("deleted %v, want the three oldest %v", fakeS3.deleted, wantDeleted)
	}
	if len(fakeS3.listPrefixes) < 3 {
		t.Errorf("listing was not paginated: %d list calls for 5 objects at page size 2", len(fakeS3.listPrefixes))
	}
}

// TestRetentionSurfacesDeleteErrors requires a failed delete to be returned to
// the caller while the remaining deletes are still attempted.
func TestRetentionSurfacesDeleteErrors(t *testing.T) {
	backup := testBackup()

	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldest := "backups/prod/cluster-a/backup-20260101-000000.rdb.gz"
	second := "backups/prod/cluster-a/backup-20260102-000000.rdb.gz"
	newest := "backups/prod/cluster-a/backup-20260103-000000.rdb.gz"

	fakeS3 := newFakeS3Store()
	fakeS3.objects[oldest] = t0
	fakeS3.objects[second] = t0.AddDate(0, 0, 1)
	fakeS3.objects[newest] = t0.AddDate(0, 0, 2)
	fakeS3.deleteErrs[oldest] = errors.New("access denied")

	r := backupReconcilerWithS3(fakeS3)
	err := r.cleanupOldBackups(context.Background(), backup)
	if err == nil {
		t.Fatal("a failed delete must surface as an error, got nil")
	}
	if !strings.Contains(err.Error(), oldest) || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("error should name the failed key and cause, got: %v", err)
	}
	if len(fakeS3.deleted) != 1 || fakeS3.deleted[0] != second {
		t.Errorf("remaining deletes must still be attempted, deleted = %v, want [%s]", fakeS3.deleted, second)
	}
}

// TestUploadKeyIsNamespaceAndClusterScoped pins the object layout retention
// relies on: uploads land under <prefix>/<namespace>/<cluster>/ so the
// retention prefix and the upload path can never diverge.
func TestUploadKeyIsNamespaceAndClusterScoped(t *testing.T) {
	backup := testBackup()

	fakeS3 := newFakeS3Store()
	r := backupReconcilerWithS3(fakeS3)

	location, err := r.uploadToS3(context.Background(), backup, []byte("REDIS0011-not-really"))
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	if len(fakeS3.putKeys) != 1 {
		t.Fatalf("expected exactly one PutObject, got %v", fakeS3.putKeys)
	}
	key := fakeS3.putKeys[0]
	want := regexp.MustCompile(`^backups/prod/cluster-a/backup-\d{8}-\d{6}\.rdb\.gz$`)
	if !want.MatchString(key) {
		t.Errorf("upload key = %q, want match for %v", key, want)
	}
	if location != "s3://corp-backups/"+key {
		t.Errorf("reported location = %q, want s3://corp-backups/%s", location, key)
	}
}

// TestBackupDestinationPolicy exercises the allowlist matrix: the IAM-role
// path fails closed with no allowlist, buckets and custom endpoints must both
// be allowlisted, and tenant-supplied credentials are never restricted.
func TestBackupDestinationPolicy(t *testing.T) {
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
			backup := testBackup()
			backup.Spec.S3.Bucket = tc.bucket
			backup.Spec.S3.Endpoint = tc.endpoint
			backup.Spec.S3.UseIAMRole = tc.useIAMRole
			if !tc.useIAMRole {
				backup.Spec.S3.CredentialsSecretRef = "tenant-creds"
			}

			r := &RedisBackupReconciler{
				AllowedBuckets:   tc.allowedBuckets,
				AllowedEndpoints: tc.allowedEndpoints,
			}
			err := r.validateBackupDestination(backup)
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

// TestMainExposesBackupAllowlistFlags guards the wiring: the allowlist is only
// enforceable if cluster admins can actually configure it.
func TestMainExposesBackupAllowlistFlags(t *testing.T) {
	src, err := os.ReadFile("../../cmd/main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, flagName := range []string{"allowed-backup-buckets", "allowed-backup-endpoints"} {
		if !strings.Contains(string(src), flagName) {
			t.Errorf("cmd/main.go does not define --%s; the backup destination policy cannot be configured", flagName)
		}
	}
}
