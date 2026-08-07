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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
	"github.com/redguard/redguard/internal/builder"
	redisfake "github.com/redguard/redguard/internal/redisclient/fake"
)

// A StatefulSet rejects every spec update outside a small mutable set, of
// which volumeClaimTemplates is not a member. Two things can make the desired
// spec disagree with the live object on an immutable field: a user editing
// storage, and an operator upgrade that changes what the builder renders. The
// first is refused at admission; the second has to reconcile cleanly, because
// a single rejected update wedges every later pass of that CR, status and
// failover handling included.
var _ = Describe("RedisSentinel storage immutability", func() {
	const resourceName = "storage-immutable-rs"

	key := types.NamespacedName{Name: resourceName, Namespace: "default"}

	BeforeEach(func() {
		rs := newTestSentinel(resourceName, "default")
		rs.Spec.RedisConfig.Storage.StorageClassName = "fast"
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
	})

	AfterEach(func() {
		deleteSentinelAndWait(ctx, key)
	})

	It("rejects a storage size change", func() {
		err := updateSentinel(ctx, key, func(rs *redisv1alpha1.RedisSentinel) {
			rs.Spec.RedisConfig.Storage.Size = resource.MustParse("2Gi")
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("storage is immutable"))
	})

	It("rejects a storageClassName change", func() {
		err := updateSentinel(ctx, key, func(rs *redisv1alpha1.RedisSentinel) {
			rs.Spec.RedisConfig.Storage.StorageClassName = "slow"
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("storage is immutable"))
	})

	It("rejects dropping the storage block", func() {
		err := updateSentinel(ctx, key, func(rs *redisv1alpha1.RedisSentinel) {
			rs.Spec.RedisConfig.Storage = nil
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("storage"))
	})

	It("accepts an update that leaves storage alone", func() {
		Expect(updateSentinel(ctx, key, func(rs *redisv1alpha1.RedisSentinel) {
			rs.Spec.RedisConfig.Replicas = 5
		})).To(Succeed())
	})
})

var _ = Describe("RedisSentinel StatefulSet updates", func() {
	const resourceName = "immutable-sts-rs"

	key := types.NamespacedName{Name: resourceName, Namespace: "default"}

	AfterEach(func() {
		deleteSentinelAndWait(ctx, key)
		for _, name := range []string{resourceName + "-redis", resourceName + "-sentinel"} {
			sts := &appsv1.StatefulSet{}
			sts.Name, sts.Namespace = name, "default"
			_ = k8sClient.Delete(ctx, sts)
		}
	})

	It("leaves immutable fields of a live StatefulSet alone instead of wedging", func() {
		// The live StatefulSets are planted before the CR exists, standing in
		// for objects an earlier operator version created with a different
		// volumeClaimTemplate. Both StatefulSets carry a PVC since Phase 2.
		// The sentinel claim size is not spec-driven, so it is planted by hand.
		planted := newTestSentinel(resourceName, "default")
		planted.Spec.RedisConfig.Storage = &redisv1alpha1.StorageSpec{
			Size:             resource.MustParse("8Gi"),
			StorageClassName: "planted-class",
		}
		for _, sts := range []*appsv1.StatefulSet{
			builder.BuildRedisStatefulSet(planted),
			builder.BuildSentinelStatefulSet(planted),
		} {
			sts.Spec.Replicas = ptr.To(int32(1))
			sts.Spec.Template.Spec.Containers[0].Image = "redis:6-alpine"
			sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("8Gi")
			Expect(k8sClient.Create(ctx, sts)).To(Succeed())
		}

		By("creating a RedisSentinel whose storage no longer matches what is running")
		rs := newTestSentinel(resourceName, "default")
		Expect(rs.Spec.RedisConfig.Storage.Size.String()).To(Equal("1Gi"))
		Expect(k8sClient.Create(ctx, rs)).To(Succeed())

		By("reconciling without error")
		reconcileUntilSettled(ctx, newImmutabilitySpecReconciler(), key)

		owner := &redisv1alpha1.RedisSentinel{}
		Expect(k8sClient.Get(ctx, key, owner)).To(Succeed())

		for _, name := range []string{resourceName + "-redis", resourceName + "-sentinel"} {
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, inDefault(name), sts)).To(Succeed())

			By("keeping the immutable fields of " + name)
			Expect(sts.Spec.VolumeClaimTemplates).To(HaveLen(1))
			vct := sts.Spec.VolumeClaimTemplates[0]
			Expect(vct.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(resource.MustParse("8Gi")),
				"the operator tried to resize a volumeClaimTemplate, which the API server forbids")
			Expect(vct.Spec.StorageClassName).To(HaveValue(Equal("planted-class")))

			By("still converging the mutable fields of " + name)
			Expect(sts.Spec.Replicas).To(HaveValue(Equal(int32(3))))
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal("redis:7-alpine"))
			expectControlledBy(sts, owner)
		}
	})
})

// A reconcile pass writes many objects in sequence. Losing an optimistic-
// concurrency race on one of them must not abandon the rest of the pass, so
// createOrUpdate retries the two errors that mean "another writer got there
// first" rather than returning them.
var _ = Describe("RedisSentinel createOrUpdate races", func() {
	const resourceName = "racing-rs"

	stsKey := types.NamespacedName{Name: resourceName + "-redis", Namespace: "default"}

	var desired *appsv1.StatefulSet

	BeforeEach(func() {
		desired = builder.BuildRedisStatefulSet(newTestSentinel(resourceName, "default"))
	})

	AfterEach(func() {
		sts := &appsv1.StatefulSet{}
		sts.Name, sts.Namespace = stsKey.Name, stsKey.Namespace
		_ = k8sClient.Delete(ctx, sts)
	})

	It("retries an update that lost the race", func() {
		Expect(k8sClient.Create(ctx, desired.DeepCopy())).To(Succeed())

		racing := &racingClient{Client: k8sClient, updateConflicts: 2}
		r := &RedisSentinelReconciler{Client: racing, Scheme: k8sClient.Scheme(), RedisFactory: redisfake.NewFactory()}

		desired.Spec.Replicas = ptr.To(int32(5))
		Expect(r.createOrUpdate(ctx, desired)).To(Succeed())
		Expect(racing.updateConflicts).To(Equal(0), "the conflicts were never injected")

		live := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, stsKey, live)).To(Succeed())
		Expect(live.Spec.Replicas).To(HaveValue(Equal(int32(5))))
	})

	It("converges an object another writer created first", func() {
		racing := &racingClient{Client: k8sClient, createRaces: 1}
		r := &RedisSentinelReconciler{Client: racing, Scheme: k8sClient.Scheme(), RedisFactory: redisfake.NewFactory()}

		Expect(r.createOrUpdate(ctx, desired)).To(Succeed())
		Expect(racing.createRaces).To(Equal(0), "the race was never injected")

		live := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, stsKey, live)).To(Succeed())
		Expect(live.Spec.Replicas).To(HaveValue(Equal(int32(3))),
			"the object the other writer created was left unconverged")
	})
})

// racingClient injects the two ways another writer of the same object can
// break a write: a stale resourceVersion on update, and the object already
// existing on create. Only StatefulSets are affected, so the reconciler's
// other writes behave normally.
type racingClient struct {
	client.Client
	updateConflicts int
	createRaces     int
}

var statefulSetResource = schema.GroupResource{Group: "apps", Resource: "statefulsets"}

func (c *racingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*appsv1.StatefulSet); ok && c.updateConflicts > 0 {
		c.updateConflicts--
		return apierrors.NewConflict(statefulSetResource, obj.GetName(), fmt.Errorf("injected"))
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c *racingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	sts, ok := obj.(*appsv1.StatefulSet)
	if !ok || c.createRaces == 0 {
		return c.Client.Create(ctx, obj, opts...)
	}
	c.createRaces--

	// The other writer wins with a diverging spec, so the retry has something
	// to converge rather than a no-op.
	other := sts.DeepCopy()
	other.Spec.Replicas = ptr.To(int32(1))
	if err := c.Client.Create(ctx, other, opts...); err != nil {
		return err
	}
	return apierrors.NewAlreadyExists(statefulSetResource, sts.Name)
}

// newImmutabilitySpecReconciler builds a spec-owned reconciler with no event
// recorder: these specs poll Reconcile, and testRecorder blocks its caller once
// its buffer fills, which would wedge the poll instead of failing it.
func newImmutabilitySpecReconciler() *RedisSentinelReconciler {
	return &RedisSentinelReconciler{
		Client:       k8sClient,
		Scheme:       k8sClient.Scheme(),
		RedisFactory: redisfake.NewFactory(),
	}
}

// updateSentinel applies mutate to a fresh copy of the CR and returns the update
// error. The manager-owned reconciler writes the same object (finalizer, status),
// so a lost optimistic-concurrency race is retried rather than reported.
func updateSentinel(ctx context.Context, key types.NamespacedName, mutate func(*redisv1alpha1.RedisSentinel)) error {
	GinkgoHelper()

	var updateErr error
	Eventually(func(g Gomega) {
		rs := &redisv1alpha1.RedisSentinel{}
		g.Expect(k8sClient.Get(ctx, key, rs)).To(Succeed())
		mutate(rs)
		updateErr = k8sClient.Update(ctx, rs)
		g.Expect(apierrors.IsConflict(updateErr)).To(BeFalse(), "conflict, retrying")
	}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
	return updateErr
}

// deleteSentinelAndWait removes the CR and drives a reconcile until the
// finalizer is gone, so a spec cannot leak objects into the next one.
func deleteSentinelAndWait(ctx context.Context, key types.NamespacedName) {
	GinkgoHelper()

	rs := &redisv1alpha1.RedisSentinel{}
	if err := k8sClient.Get(ctx, key, rs); apierrors.IsNotFound(err) {
		return
	}
	Expect(k8sClient.Delete(ctx, rs)).To(Succeed())

	r := newImmutabilitySpecReconciler()
	Eventually(func(g Gomega) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key, rs))).To(BeTrue())
	}, 20*time.Second, 200*time.Millisecond).Should(Succeed())
}
