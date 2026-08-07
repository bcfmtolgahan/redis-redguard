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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	redisv1alpha1 "github.com/redguard/redguard/api/v1alpha1"
)

// These specs exercise the CRD's CEL rules against a real API server. A spec
// with tls.enabled and no certificateSecretRef describes a cluster whose pods
// listen on TLS only while nothing mounts a certificate, so admission must
// reject it.
var _ = Describe("RedisSentinel TLS validation", func() {
	It("rejects tls.enabled without certificateSecretRef", func() {
		rs := newTestSentinel("tls-missing-cert", "default")
		rs.Spec.TLS = &redisv1alpha1.TLSConfig{Enabled: true}

		err := k8sClient.Create(ctx, rs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("certificateSecretRef is required when TLS is enabled"))
	})

	It("accepts tls.enabled with certificateSecretRef", func() {
		rs := newTestSentinel("tls-with-cert", "default")
		rs.Spec.TLS = &redisv1alpha1.TLSConfig{
			Enabled:              true,
			CertificateSecretRef: "demo-tls",
		}

		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		Expect(k8sClient.Delete(ctx, rs)).To(Succeed())
	})

	It("accepts an explicitly disabled tls block without a certificate", func() {
		rs := newTestSentinel("tls-disabled", "default")
		rs.Spec.TLS = &redisv1alpha1.TLSConfig{Enabled: false}

		Expect(k8sClient.Create(ctx, rs)).To(Succeed())
		Expect(k8sClient.Delete(ctx, rs)).To(Succeed())
	})
})
