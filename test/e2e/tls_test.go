//go:build e2e
// +build e2e

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

package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	// tlsClusterName is deliberately distinct from the sample cluster so both
	// can coexist in the shared namespace whatever order the containers run in.
	tlsClusterName    = "redis-tls"
	tlsCertSecret     = "redis-tls-cert"
	tlsCASecret       = "redis-tls-ca"
	tlsPasswordSecret = "redis-tls-password"

	tlsRedisReplicas    = 2
	tlsSentinelReplicas = 3
)

var (
	tlsRedisSelector    = componentSelector(tlsClusterName, "redis")
	tlsSentinelSelector = componentSelector(tlsClusterName, "sentinel")
)

// tlsCLI runs redis-cli inside a pod with the client material that pod mounts,
// authenticating from the pod's own REDIS_PASSWORD. Extra args must not need
// shell quoting.
func tlsCLI(namespace, pod, tlsDir string, args ...string) (string, error) {
	script := fmt.Sprintf(
		`REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli --tls --cert %[1]s/tls.crt --key %[1]s/tls.key --cacert %[1]s/ca.crt %[2]s`,
		tlsDir, strings.Join(args, " "))
	out, err := kubectl("exec", "-n", namespace, pod, "--", "sh", "-c", script)
	return strings.TrimSpace(out), err
}

// tlsReplicationStates is replicationStates for a cluster that only answers
// over TLS.
func tlsReplicationStates(namespace, selector string) ([]replicaState, error) {
	pods, err := listPods(namespace, selector)
	if err != nil {
		return nil, err
	}
	states := make([]replicaState, 0, len(pods))
	for _, p := range readyPods(pods) {
		info, err := tlsCLI(namespace, p.Name, "/etc/redis/tls", "info", "replication")
		if err != nil {
			return nil, fmt.Errorf("info replication on %s: %w", p.Name, err)
		}
		states = append(states, replicaState{
			Pod:              p.Name,
			IP:               p.IP,
			Role:             infoField(info, "role"),
			MasterHost:       infoField(info, "master_host"),
			MasterLinkStatus: infoField(info, "master_link_status"),
		})
	}
	return states, nil
}

// generateTLSMaterial builds a throwaway CA and a server certificate whose SANs
// cover every name a redguard pod dials: the probes use the bare pod hostname,
// the init script uses the sentinel headless FQDNs, and clients use the
// Services.
func generateTLSMaterial() (caPEM, certPEM, keyPEM []byte, err error) {
	sans := []string{
		fmt.Sprintf("*.%s-redis-headless.%s.svc.cluster.local", tlsClusterName, clusterNamespace),
		fmt.Sprintf("*.%s-sentinel-headless.%s.svc.cluster.local", tlsClusterName, clusterNamespace),
		tlsClusterName + "-redis",
		fmt.Sprintf("%s-redis.%s.svc.cluster.local", tlsClusterName, clusterNamespace),
		tlsClusterName + "-redis-replicas",
		fmt.Sprintf("%s-redis-replicas.%s.svc.cluster.local", tlsClusterName, clusterNamespace),
		tlsClusterName + "-sentinel",
		fmt.Sprintf("%s-sentinel.%s.svc.cluster.local", tlsClusterName, clusterNamespace),
		"localhost",
	}
	for i := 0; i < tlsRedisReplicas; i++ {
		sans = append(sans, fmt.Sprintf("%s-redis-%d", tlsClusterName, i))
	}
	for i := 0; i < tlsSentinelReplicas; i++ {
		sans = append(sans, fmt.Sprintf("%s-sentinel-%d", tlsClusterName, i))
	}

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "redguard-e2e-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, nil, err
	}

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, err
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: tlsClusterName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		// The same certificate is presented by the servers and by redis-cli,
		// replication links included, so both usages are required.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:    sans,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return nil, nil, nil, err
	}

	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return caPEM, certPEM, keyPEM, nil
}

// The TLS cluster runs with the CA in its own Secret, the layout that used to
// leave every pod in StartError: the CA was subPath-mounted into the directory
// the certificate volume already occupied, which the kubelet refuses. The
// plaintext-to-sentinel defect is covered by the same cluster: with TLS enabled
// sentinel listens only on tls-port, so a pod only becomes ready at all if the
// init script's sentinel query and both probes carry the client flags.
var _ = Describe("TLS-enabled cluster", Ordered, func() {
	var workDir string

	BeforeAll(func() {
		var err error
		workDir, err = os.MkdirTemp("", "redguard-e2e-tls")
		Expect(err).NotTo(HaveOccurred())

		By("generating a CA and a server certificate")
		caPEM, certPEM, keyPEM, err := generateTLSMaterial()
		Expect(err).NotTo(HaveOccurred())
		for name, data := range map[string][]byte{
			"ca.crt": caPEM, "tls.crt": certPEM, "tls.key": keyPEM,
		} {
			Expect(os.WriteFile(filepath.Join(workDir, name), data, 0o600)).To(Succeed())
		}

		By("creating the certificate Secret and the CA in a distinct Secret")
		_, err = kubectl("create", "secret", "generic", tlsCertSecret, "-n", clusterNamespace,
			"--from-file=tls.crt="+filepath.Join(workDir, "tls.crt"),
			"--from-file=tls.key="+filepath.Join(workDir, "tls.key"))
		Expect(err).NotTo(HaveOccurred())
		_, err = kubectl("create", "secret", "generic", tlsCASecret, "-n", clusterNamespace,
			"--from-file=ca.crt="+filepath.Join(workDir, "ca.crt"))
		Expect(err).NotTo(HaveOccurred())
		_, err = kubectl("create", "secret", "generic", tlsPasswordSecret, "-n", clusterNamespace,
			"--from-literal=password=e2e-tls-password")
		Expect(err).NotTo(HaveOccurred())

		manifest := fmt.Sprintf(`apiVersion: redis.redguard.io/v1alpha1
kind: RedisSentinel
metadata:
  name: %s
  namespace: %s
spec:
  redisConfig:
    replicas: %d
    auth:
      secretName: %s
    storage:
      size: 1Gi
  sentinelConfig:
    replicas: %d
    quorum: 2
  tls:
    enabled: true
    certificateSecretRef: %s
    caSecretRef: %s
`, tlsClusterName, clusterNamespace, tlsRedisReplicas, tlsPasswordSecret,
			tlsSentinelReplicas, tlsCertSecret, tlsCASecret)
		manifestPath := filepath.Join(workDir, "cluster.yaml")
		Expect(os.WriteFile(manifestPath, []byte(manifest), 0o600)).To(Succeed())

		By("applying the TLS RedisSentinel")
		_, err = kubectl("apply", "-f", manifestPath)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("deleting the TLS cluster and its fixtures")
		_, _ = kubectl("delete", "redissentinel", tlsClusterName, "-n", clusterNamespace,
			"--ignore-not-found", "--timeout=5m")
		for i := 0; i < tlsRedisReplicas; i++ {
			_, _ = kubectl("delete", "pvc", fmt.Sprintf("data-%s-redis-%d", tlsClusterName, i),
				"-n", clusterNamespace, "--ignore-not-found")
		}
		for i := 0; i < tlsSentinelReplicas; i++ {
			_, _ = kubectl("delete", "pvc", fmt.Sprintf("sentinel-data-%s-sentinel-%d", tlsClusterName, i),
				"-n", clusterNamespace, "--ignore-not-found")
		}
		for _, secret := range []string{tlsCertSecret, tlsCASecret, tlsPasswordSecret} {
			_, _ = kubectl("delete", "secret", secret, "-n", clusterNamespace, "--ignore-not-found")
		}
		if workDir != "" {
			_ = os.RemoveAll(workDir)
		}
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpCluster(clusterNamespace, tlsClusterName)
		}
	})

	It("starts every pod and reaches Running with the CA in its own Secret", func() {
		Eventually(func(g Gomega) {
			redis, err := listPods(clusterNamespace, tlsRedisSelector)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(readyPods(redis)).To(HaveLen(tlsRedisReplicas), "ready redis pods: %v", redis)
			// A restart here means a pod was probe-killed while its init script
			// was still waiting on the TLS-only sentinels.
			for _, p := range redis {
				g.Expect(p.Restarts).To(BeZero(), "%s restarted during boot: %+v", p.Name, p)
			}

			sentinels, err := listPods(clusterNamespace, tlsSentinelSelector)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(readyPods(sentinels)).To(HaveLen(tlsSentinelReplicas), "ready sentinel pods: %v", sentinels)

			status, err := getSentinelStatus(clusterNamespace, tlsClusterName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status.Phase).To(Equal("Running"), "status: %+v", status)
			available, message := status.condition("Available")
			g.Expect(available).To(Equal("True"), "Available condition: %s", message)
		}, 5*time.Minute, 10*time.Second).Should(Succeed())
	})

	It("replicates over TLS with a single master", func() {
		Eventually(func(g Gomega) {
			states, err := tlsReplicationStates(clusterNamespace, tlsRedisSelector)
			g.Expect(err).NotTo(HaveOccurred())
			masters, replicas := splitByRole(states)
			g.Expect(masters).To(HaveLen(1), "exactly one master expected: %v", states)
			g.Expect(replicas).To(HaveLen(tlsRedisReplicas-1), "replica missing: %v", states)
			for _, replica := range replicas {
				g.Expect(replica.MasterLinkStatus).To(Equal("up"),
					"replica is not in sync over TLS: %s", replica)
				g.Expect(replica.MasterHost).To(Equal(masters[0].IP),
					"replica follows a different master: %s", replica)
			}
		}, 3*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("serves sentinel only over TLS and agrees on the master", func() {
		By("checking a plaintext query is refused, which proves tls-port is what answers")
		sentinelPod := tlsClusterName + "-sentinel-0"
		out, err := kubectl("exec", "-n", clusterNamespace, sentinelPod, "--",
			"redis-cli", "-p", "26379", "ping")
		Expect(err).To(HaveOccurred(), "sentinel answered a plaintext PING: %q", out)

		By("querying the master address over TLS from every sentinel")
		states, err := tlsReplicationStates(clusterNamespace, tlsRedisSelector)
		Expect(err).NotTo(HaveOccurred())
		masters, _ := splitByRole(states)
		Expect(masters).To(HaveLen(1))

		pods, err := listPods(clusterNamespace, tlsSentinelSelector)
		Expect(err).NotTo(HaveOccurred())
		for _, p := range readyPods(pods) {
			reply, err := tlsCLI(clusterNamespace, p.Name, "/etc/sentinel/tls",
				"-p", "26379", "SENTINEL", "get-master-addr-by-name", tlsClusterName+"-master")
			Expect(err).NotTo(HaveOccurred())
			fields := strings.Fields(reply)
			Expect(fields).To(HaveLen(2), "unexpected sentinel reply from %s: %q", p.Name, reply)
			Expect(fields[0]).To(Equal(masters[0].IP),
				"%s monitors a different master than the one serving role:master", p.Name)
		}
	})
})
