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
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	redisfake "github.com/redguard/redguard/internal/redisclient/fake"
)

// fakeRDBStream returns a podStreamFn that writes payload to stdout the way
// the exec path would, in small chunks so nothing downstream can rely on a
// single read delivering the whole file.
func fakeRDBStream(payload []byte, execErr error, stderr string) podStreamFn {
	return func(_ context.Context, _ *rest.Config, _ *corev1.Pod, _ string, _ []string, stdout io.Writer) (string, error) {
		for chunk := payload; len(chunk) > 0; {
			n := 7
			if n > len(chunk) {
				n = len(chunk)
			}
			if _, err := stdout.Write(chunk[:n]); err != nil {
				return stderr, err
			}
			chunk = chunk[n:]
		}
		return stderr, execErr
	}
}

// uploadAll drives an s3UploadWriter the way the exec copy does: successive
// writes, then finish, aborting on failure like streamBackupToS3.
func uploadAll(fakeS3 *fakeS3Store, key string, payload []byte) (int64, error) {
	w := &s3UploadWriter{ctx: context.Background(), client: fakeS3, bucket: "corp-backups", key: key}
	if _, err := w.Write(payload); err != nil {
		w.abort()
		return 0, err
	}
	size, err := w.finish()
	if err != nil {
		w.abort()
		return 0, err
	}
	return size, nil
}

func TestUploadWriterSmallObjectUsesSinglePut(t *testing.T) {
	fakeS3 := newFakeS3Store()
	payload := []byte("REDIS0011-single-part-payload")

	size, err := uploadAll(fakeS3, "k", payload)
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	if size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", size, len(payload))
	}
	if !bytes.Equal(fakeS3.data["k"], payload) {
		t.Errorf("stored bytes differ from the stream")
	}
	if fakeS3.mpCreated != 0 {
		t.Errorf("a single-part object must not start a multipart upload")
	}
}

func TestUploadWriterLargeObjectUsesMultipart(t *testing.T) {
	old := backupUploadPartSize
	backupUploadPartSize = 8
	defer func() { backupUploadPartSize = old }()

	fakeS3 := newFakeS3Store()
	payload := []byte("REDIS0011-a-payload-larger-than-one-part")

	size, err := uploadAll(fakeS3, "k", payload)
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	if size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", size, len(payload))
	}
	if fakeS3.mpCompleted != 1 {
		t.Fatalf("expected one completed multipart upload, got %d", fakeS3.mpCompleted)
	}
	if !bytes.Equal(fakeS3.data["k"], payload) {
		t.Errorf("assembled parts differ from the stream")
	}
	if fakeS3.mpAborted != 0 {
		t.Errorf("successful upload was aborted")
	}
}

func TestUploadWriterAbortsOnPartFailure(t *testing.T) {
	old := backupUploadPartSize
	backupUploadPartSize = 8
	defer func() { backupUploadPartSize = old }()

	fakeS3 := newFakeS3Store()
	fakeS3.partErrs[2] = errors.New("part quota exceeded")

	_, err := uploadAll(fakeS3, "k", []byte("REDIS0011-a-payload-larger-than-one-part"))
	if err == nil {
		t.Fatal("a failed part must fail the upload")
	}
	if fakeS3.mpAborted != 1 {
		t.Errorf("failed multipart upload was not aborted; its parts accrue storage charges forever")
	}
	if len(fakeS3.data) != 0 {
		t.Errorf("no object may exist after a failed upload, got %v", fakeS3.putKeys)
	}
}

// backupStreamFixture wires a reconciler over a fake API server, fake Redis
// and fake S3, with the pod exec seam feeding rdb.
type backupStreamFixture struct {
	r       *RedisBackupReconciler
	factory *redisfake.Factory
	c       client.Client
	fakeS3  *fakeS3Store
}

const backupMasterAddr = "10.244.0.20:6379"

func newBackupStreamFixture(t *testing.T, backup *redisv1alpha1.RedisBackup, rdb []byte) *backupStreamFixture {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := redisv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	rs := newTestSentinel(backup.Spec.RedisClusterRef, backup.Namespace)
	masterPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backup.Spec.RedisClusterRef + "-redis-0",
			Namespace: backup.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/instance":  backup.Spec.RedisClusterRef,
				"app.kubernetes.io/component": "redis",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: strings.TrimSuffix(backupMasterAddr, ":6379"),
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	c := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(rs, masterPod, backup).
		WithStatusSubresource(&redisv1alpha1.RedisBackup{}).
		Build()

	factory := redisfake.NewFactory()
	factory.SetMaster(backupMasterAddr)
	factory.SetKeyspace(backupMasterAddr, map[int]int64{0: 7})

	fakeS3 := newFakeS3Store()
	return &backupStreamFixture{
		r: &RedisBackupReconciler{
			Client:       c,
			Scheme:       scheme,
			RedisFactory: factory,
			PodStream:    fakeRDBStream(rdb, nil, ""),
			newS3Client: func(context.Context, *redisv1alpha1.RedisBackup) (s3API, error) {
				return fakeS3, nil
			},
		},
		factory: factory,
		c:       c,
		fakeS3:  fakeS3,
	}
}

func (fx *backupStreamFixture) reconcile(t *testing.T, backup *redisv1alpha1.RedisBackup) ctrl.Result {
	t.Helper()
	res, err := fx.r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: backup.Name, Namespace: backup.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (fx *backupStreamFixture) get(t *testing.T, backup *redisv1alpha1.RedisBackup) *redisv1alpha1.RedisBackup {
	t.Helper()
	got := &redisv1alpha1.RedisBackup{}
	if err := fx.c.Get(context.Background(), types.NamespacedName{Name: backup.Name, Namespace: backup.Namespace}, got); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestBackupStreamsRDBToS3 runs a whole one-time backup through the exec seam
// and asserts the stored object gunzips back to the exact RDB bytes, and that
// KeyCount lands in status together with the location it describes.
func TestBackupStreamsRDBToS3(t *testing.T) {
	backup := testBackup()
	rdb := append([]byte("REDIS0011"), bytes.Repeat([]byte("payload"), 100)...)
	fx := newBackupStreamFixture(t, backup, rdb)

	fx.reconcile(t, backup)

	got := fx.get(t, backup)
	if got.Status.Phase != phaseCompleted {
		t.Fatalf("phase = %q (conditions %+v), want Completed", got.Status.Phase, got.Status.Conditions)
	}
	if len(fx.fakeS3.putKeys) != 1 {
		t.Fatalf("expected exactly one stored object, got %v", fx.fakeS3.putKeys)
	}
	key := fx.fakeS3.putKeys[0]
	if got.Status.BackupLocation != "s3://corp-backups/"+key {
		t.Errorf("backupLocation = %q, want s3://corp-backups/%s", got.Status.BackupLocation, key)
	}

	gz, err := gzip.NewReader(bytes.NewReader(fx.fakeS3.data[key]))
	if err != nil {
		t.Fatalf("stored object is not gzip: %v", err)
	}
	stored, err := io.ReadAll(gz)
	if cerr := gz.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("stored object does not decompress: %v", err)
	}
	if !bytes.Equal(stored, rdb) {
		t.Errorf("stored RDB differs from the streamed one (%d vs %d bytes)", len(stored), len(rdb))
	}
	if got.Status.BackupSize != int64(len(fx.fakeS3.data[key])) {
		t.Errorf("backupSize = %d, want the uploaded %d", got.Status.BackupSize, len(fx.fakeS3.data[key]))
	}

	if got.Status.KeyCount == nil || *got.Status.KeyCount != 7 {
		t.Errorf("keyCount = %v, want 7 recorded together with the location", got.Status.KeyCount)
	}
}

// TestBackupRejectsNonRDBStream pins the header check the buffered path had:
// an object that is not an RDB must never be uploaded.
func TestBackupRejectsNonRDBStream(t *testing.T) {
	backup := testBackup()
	fx := newBackupStreamFixture(t, backup, []byte("NOTRDB-anything"))

	fx.reconcile(t, backup)

	got := fx.get(t, backup)
	if got.Status.Phase != phaseFailed {
		t.Fatalf("phase = %q, want Failed for a non-RDB stream", got.Status.Phase)
	}
	if len(fx.fakeS3.putKeys) != 0 || fx.fakeS3.mpCreated != 0 {
		t.Errorf("invalid stream reached S3: puts=%v multiparts=%d", fx.fakeS3.putKeys, fx.fakeS3.mpCreated)
	}
}

// TestBackupEmptyRDBStreamFails pins the empty-dump refusal.
func TestBackupEmptyRDBStreamFails(t *testing.T) {
	backup := testBackup()
	fx := newBackupStreamFixture(t, backup, nil)

	fx.reconcile(t, backup)

	got := fx.get(t, backup)
	if got.Status.Phase != phaseFailed {
		t.Fatalf("phase = %q, want Failed for an empty RDB", got.Status.Phase)
	}
	if len(fx.fakeS3.putKeys) != 0 {
		t.Errorf("empty stream was uploaded: %v", fx.fakeS3.putKeys)
	}
}

// TestBackupFailedUploadKeepsPreviousKeyCount pins the KeyCount pairing: a
// failed upload must leave both KeyCount and BackupLocation describing the
// previous successful object, or a restore of that object verifies against
// the wrong count and deterministically fails.
func TestBackupFailedUploadKeepsPreviousKeyCount(t *testing.T) {
	backup := testBackup()
	prev := int64(3)
	backup.Status = redisv1alpha1.RedisBackupStatus{
		KeyCount:       &prev,
		BackupLocation: "s3://corp-backups/backups/prod/cluster-a/nightly/backup-old.rdb.gz",
	}
	fx := newBackupStreamFixture(t, backup, append([]byte("REDIS0011"), []byte("x")...))
	fx.fakeS3.putErr = errors.New("access denied")

	fx.reconcile(t, backup)

	got := fx.get(t, backup)
	if got.Status.Phase != phaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if got.Status.KeyCount == nil || *got.Status.KeyCount != prev {
		t.Errorf("keyCount = %v, want the previous 3: the new count belongs to an object that was never stored", got.Status.KeyCount)
	}
	if got.Status.BackupLocation != backup.Status.BackupLocation {
		t.Errorf("backupLocation = %q changed on a failed run", got.Status.BackupLocation)
	}
}

// TestBackupKeyCountFailureClearsStaleCount pins the other half of the
// pairing: when the count cannot be taken, the previous run's count must not
// survive next to the new location.
func TestBackupKeyCountFailureClearsStaleCount(t *testing.T) {
	backup := testBackup()
	prev := int64(3)
	backup.Status = redisv1alpha1.RedisBackupStatus{
		KeyCount:       &prev,
		BackupLocation: "s3://corp-backups/backups/prod/cluster-a/nightly/backup-old.rdb.gz",
	}
	fx := newBackupStreamFixture(t, backup, append([]byte("REDIS0011"), []byte("x")...))
	fx.factory.SetError(backupMasterAddr, "GetKeyspaceInfo", fmt.Errorf("info unavailable"))

	fx.reconcile(t, backup)

	got := fx.get(t, backup)
	if got.Status.Phase != phaseCompleted {
		t.Fatalf("phase = %q, want Completed: a missing count is not a failed backup", got.Status.Phase)
	}
	if got.Status.KeyCount != nil {
		t.Errorf("keyCount = %d, want cleared: the previous run's count does not describe the new object", *got.Status.KeyCount)
	}
	if got.Status.BackupLocation == backup.Status.BackupLocation {
		t.Errorf("backupLocation still names the previous object after a successful run")
	}
}
