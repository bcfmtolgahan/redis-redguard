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
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"

	redisv1alpha1 "github.com/bcfmtolgahan/redis-redguard/api/v1alpha1"
)

// captureExec records every exec issued through the seam as "pod: command" and
// succeeds, so cleanup paths can be asserted without a cluster.
func captureExec(execs *[]string) podExecFn {
	return func(ctx context.Context, cfg *rest.Config, pod *corev1.Pod, container string, command []string, stdin io.Reader) (string, string, error) {
		*execs = append(*execs, pod.Name+": "+strings.Join(command, " "))
		return "", "", nil
	}
}

func anyExecSweepsPayload(execs []string) bool {
	for _, e := range execs {
		if strings.Contains(e, "rm -f") &&
			strings.Contains(e, restorePayloadPath) &&
			strings.Contains(e, restoreMarkerPath) {
			return true
		}
	}
	return false
}

// TestRestore_StagingWritesMarkerBesideThePayload: the marker is the init
// script's only proof a staged payload still belongs to a live restore, so it
// must carry this restore's UID and a deadline, and it must be in place
// before the payload becomes visible under its final name; the reverse order
// leaves a window in which a crash strands an unguarded payload.
func TestRestore_StagingWritesMarkerBesideThePayload(t *testing.T) {
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.UID = "0d9c2f66-uid"
	})
	var execs []string
	fx.r.PodExec = captureExec(&execs)

	ctx := context.Background()
	rs := &redisv1alpha1.RedisSentinel{}
	if err := fx.c.Get(ctx, types.NamespacedName{Name: restoreCluster, Namespace: restoreNS}, rs); err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	if err := fx.r.stageRestorePayload(ctx, rs, fx.get(t), []byte("REDIS0011")); err != nil {
		t.Fatalf("stageRestorePayload: %v", err)
	}
	if len(execs) != 1 {
		t.Fatalf("staging execs = %v, want exactly one", execs)
	}
	cmd := execs[0]

	if !strings.Contains(cmd, "uid=0d9c2f66-uid") {
		t.Errorf("marker does not carry the restore UID: %s", cmd)
	}
	m := regexp.MustCompile(`expires=(\d+)`).FindStringSubmatch(cmd)
	if m == nil {
		t.Fatalf("marker carries no numeric deadline: %s", cmd)
	}
	expires, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	want := before.Add(restoreTimeout).Unix()
	if expires < want-5 || expires > want+60 {
		t.Errorf("marker deadline = %d, want about staging time + restoreTimeout (%d)", expires, want)
	}

	markerIdx := strings.Index(cmd, restoreMarkerPath)
	payloadVisible := strings.LastIndex(cmd, "mv "+restorePayloadPath+".tmp "+restorePayloadPath)
	if payloadVisible == -1 {
		t.Fatalf("payload never moved into its final name: %s", cmd)
	}
	if markerIdx == -1 || markerIdx > payloadVisible {
		t.Errorf("marker must be written before the payload becomes visible: %s", cmd)
	}
}

// TestRestore_FailureAfterStagingRemovesPayload: a run that staged and then
// failed must not leave the payload armed; without the sweep the next pod
// restart inside the marker window still loads it.
func TestRestore_FailureAfterStagingRemovesPayload(t *testing.T) {
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Status.Phase = redisv1alpha1.RestorePhaseRestoring
		r.Status.ObservedGeneration = 1
		r.Status.StartTime = &metav1.Time{Time: time.Now().Add(-restoreTimeout - time.Minute)}
	})
	var execs []string
	fx.r.PodExec = captureExec(&execs)

	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("restoring reconcile: %v", err)
	}

	restore := fx.get(t)
	if restore.Status.Phase != redisv1alpha1.RestorePhaseFailed {
		t.Fatalf("phase = %q, want Failed after the timeout", restore.Status.Phase)
	}
	if !anyExecSweepsPayload(execs) {
		t.Errorf("no exec removed the staged payload and marker; execs: %v", execs)
	}
}

// TestRestore_DeletionSweepsStagedPayload: deleting a mid-flight RedisRestore
// is the other way a staged payload loses its owner. The finalizer sweeps
// every Redis pod best-effort and then lets the object go.
func TestRestore_DeletionSweepsStagedPayload(t *testing.T) {
	fx := newRestoreFixture(t, func(r *redisv1alpha1.RedisRestore) {
		r.Finalizers = []string{restoreFinalizer}
		r.Status.Phase = redisv1alpha1.RestorePhaseRestoring
		r.Status.ObservedGeneration = 1
		r.Status.StartTime = &metav1.Time{Time: time.Now()}
	})
	var execs []string
	fx.r.PodExec = captureExec(&execs)

	ctx := context.Background()
	if err := fx.c.Delete(ctx, fx.get(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("deletion reconcile: %v", err)
	}

	if !anyExecSweepsPayload(execs) {
		t.Errorf("deletion did not sweep the staged payload; execs: %v", execs)
	}
	err := fx.c.Get(ctx, types.NamespacedName{Name: restoreName, Namespace: restoreNS}, &redisv1alpha1.RedisRestore{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("finalizer not removed after the sweep (err=%v)", err)
	}
	for _, e := range execs {
		if !strings.Contains(e, fmt.Sprintf("%s-redis-0", restoreCluster)) {
			continue
		}
		return
	}
	t.Errorf("sweep never reached the redis pod; execs: %v", execs)
}

// TestRestore_FinalizerAddedBeforeAnythingRuns: without the finalizer in
// place before staging, a deletion between staging and consumption skips the
// sweep entirely.
func TestRestore_FinalizerAddedBeforeAnythingRuns(t *testing.T) {
	fx := newRestoreFixture(t, nil)
	if _, err := fx.reconcile(t); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	restore := fx.get(t)
	for _, f := range restore.Finalizers {
		if f == restoreFinalizer {
			return
		}
	}
	t.Errorf("finalizers = %v, want %s present after the first reconcile", restore.Finalizers, restoreFinalizer)
}
