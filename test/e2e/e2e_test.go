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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	// The samples pin their own namespace, so the specs follow them rather
	// than applying the same manifests somewhere else.
	clusterNamespace = "default"

	sentinelManifest = "config/samples/redis_v1alpha1_redissentinel.yaml"
	userManifest     = "config/samples/redis_v1alpha1_redisuser.yaml"

	// Names created by the sample manifests above.
	sentinelName = "redis-cluster"
	userName     = "app-user"
	userSecret   = "app-user-password"

	// metricsReaderRole grants the scrape itself. The chart ships the
	// authn/authz roles the operator needs to validate a scraper's token,
	// but granting /metrics is the cluster owner's decision.
	metricsReaderRole    = "redguard-e2e-metrics-reader"
	metricsReaderBinding = "redguard-e2e-metrics-reader-binding"
	curlPod              = "curl-metrics"

	// failoverKey is written before the master is killed and read back after.
	failoverKey   = "redguard-e2e:failover"
	failoverValue = "written-before-failover"
)

var (
	redisSelector    = componentSelector(sentinelName, "redis")
	sentinelSelector = componentSelector(sentinelName, "sentinel")
)

var _ = Describe("Redguard", Ordered, func() {
	var controllerPod string

	AfterAll(func() {
		By("deleting the sample resources")
		_, _ = kubectl("delete", "-f", userManifest, "--ignore-not-found", "--timeout=2m")
		_, _ = kubectl("delete", "secret", userSecret, "-n", clusterNamespace, "--ignore-not-found")
		_, _ = kubectl("delete", "-f", sentinelManifest, "--ignore-not-found", "--timeout=5m")

		By("deleting the StatefulSet volumes, which no owner reference reclaims")
		_, _ = kubectl("delete", "pvc", "-n", clusterNamespace, "-l", redisSelector, "--ignore-not-found")

		By("deleting the metrics test fixtures")
		_, _ = kubectl("delete", "clusterrolebinding", metricsReaderBinding, "--ignore-not-found")
		_, _ = kubectl("delete", "clusterrole", metricsReaderRole, "--ignore-not-found")
		_, _ = kubectl("delete", "pod", curlPod, "-n", operatorNamespace, "--ignore-not-found")
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpCluster(clusterNamespace, sentinelName)
		}
	})

	It("runs the operator installed from the chart", func() {
		By("waiting for the controller pod to become ready")
		Eventually(func(g Gomega) {
			pods, err := listPods(operatorNamespace, "control-plane=controller-manager")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pods).To(HaveLen(1), "expected exactly one controller pod, got %v", pods)
			g.Expect(pods[0].Ready).To(BeTrue(), "controller pod is not ready: %+v", pods[0])
			controllerPod = pods[0].Name
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("checking the chart RBAC covers what the operator does at startup")
		logs, err := kubectl("logs", controllerPod, "-n", operatorNamespace)
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).NotTo(ContainSubstring("is forbidden"),
			"the ClusterRole shipped by the chart is missing a permission the operator needs")
		Expect(logs).To(ContainSubstring("Starting workers"),
			"no controller reached its work loop")
	})

	It("brings up a healthy 3-node cluster with one master", func() {
		By("applying the RedisSentinel sample")
		_, err := kubectl("apply", "-f", sentinelManifest)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the cluster to converge on a single master within five minutes")
		Eventually(func(g Gomega) {
			redis, err := listPods(clusterNamespace, redisSelector)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(readyPods(redis)).To(HaveLen(3), "ready redis pods: %v", redis)

			sentinels, err := listPods(clusterNamespace, sentinelSelector)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(readyPods(sentinels)).To(HaveLen(3), "ready sentinel pods: %v", sentinels)

			states, err := replicationStates(clusterNamespace, redisSelector)
			g.Expect(err).NotTo(HaveOccurred())
			masters, replicas := splitByRole(states)
			g.Expect(masters).To(HaveLen(1), "exactly one master expected: %v", states)
			g.Expect(replicas).To(HaveLen(2), "two replicas expected: %v", states)
			for _, replica := range replicas {
				g.Expect(replica.MasterLinkStatus).To(Equal("up"),
					"replica is not in sync with the master: %s", replica)
				g.Expect(replica.MasterHost).To(Equal(masters[0].IP),
					"replica follows a different master than the one that reports role:master: %s", replica)
			}

			status, err := getSentinelStatus(clusterNamespace, sentinelName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status.Phase).To(Equal("Running"), "status: %+v", status)
			g.Expect(status.MasterNode).To(Equal(masters[0].IP+":6379"),
				"the CR reports a master address that is not the pod serving as master")
			g.Expect(status.ReadyReplicas).To(Equal(3))
			g.Expect(status.ReadySentinels).To(Equal(3))

			available, message := status.condition("Available")
			g.Expect(available).To(Equal("True"), "Available condition: %s", message)
		}, 5*time.Minute, 10*time.Second).Should(Succeed())

		By("checking the write Service routes to the master and the replicas Service to every pod")
		Eventually(func(g Gomega) {
			states, err := replicationStates(clusterNamespace, redisSelector)
			g.Expect(err).NotTo(HaveOccurred())
			masters, _ := splitByRole(states)
			g.Expect(masters).To(HaveLen(1))

			writeIPs, err := serviceEndpointIPs(clusterNamespace, sentinelName+"-redis")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(writeIPs).To(ConsistOf(masters[0].IP),
				"the write Service must route to the master and nothing else")

			readIPs, err := serviceEndpointIPs(clusterNamespace, sentinelName+"-redis-replicas")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(readIPs).To(HaveLen(3),
				"the replicas Service must route to every Redis pod")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		// Sentinel learns about replicas from INFO polling and about its peers
		// from the master's pub/sub channel, so the full topology becomes
		// visible a few seconds after the replicas are already in sync.
		By("checking every Sentinel monitors the same master and sees the full quorum")
		Eventually(func(g Gomega) {
			sentinels, err := listPods(clusterNamespace, sentinelSelector)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(readyPods(sentinels)).To(HaveLen(3))

			status, err := getSentinelStatus(clusterNamespace, sentinelName)
			g.Expect(err).NotTo(HaveOccurred())

			for _, s := range readyPods(sentinels) {
				out, err := redisCLI(clusterNamespace, s.Name, "-p", "26379",
					"SENTINEL", "get-master-addr-by-name", sentinelName+"-master")
				g.Expect(err).NotTo(HaveOccurred())
				fields := strings.Fields(out)
				g.Expect(fields).To(HaveLen(2), "unexpected sentinel reply from %s: %q", s.Name, out)
				g.Expect(fields[0]+":"+fields[1]).To(Equal(status.MasterNode),
					"%s monitors a different master than the CR reports", s.Name)

				master, err := redisCLI(clusterNamespace, s.Name, "-p", "26379",
					"SENTINEL", "master", sentinelName+"-master")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(master).To(ContainSubstring("num-slaves\n2"),
					"%s does not see both replicas", s.Name)
				g.Expect(master).To(ContainSubstring("num-other-sentinels\n2"),
					"%s does not see the other two sentinels", s.Name)
			}
		}, 2*time.Minute, 5*time.Second).Should(Succeed())
	})

	It("applies an ACL user to every node of the cluster", func() {
		By("creating the user password secret")
		_, err := kubectl("create", "secret", "generic", userSecret,
			"--from-literal=password=e2e-app-user-password", "-n", clusterNamespace)
		Expect(err).NotTo(HaveOccurred())

		By("applying the RedisUser sample")
		_, err = kubectl("apply", "-f", userManifest)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the RedisUser to report Ready")
		Eventually(func(g Gomega) {
			out, err := kubectl("get", "redisuser", userName, "-n", clusterNamespace, "-o", "json")
			g.Expect(err).NotTo(HaveOccurred())
			var cr struct {
				Status struct {
					Phase     string   `json:"phase"`
					AppliedTo []string `json:"appliedTo"`
					Message   string   `json:"message"`
				} `json:"status"`
			}
			g.Expect(json.Unmarshal([]byte(out), &cr)).To(Succeed())
			g.Expect(cr.Status.Phase).To(Equal("Ready"), "status: %+v", cr.Status)
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("checking the ACL exists on every Redis node, not only on the master")
		pods, err := listPods(clusterNamespace, redisSelector)
		Expect(err).NotTo(HaveOccurred())
		for _, p := range readyPods(pods) {
			users, err := redisCLI(clusterNamespace, p.Name, "ACL", "LIST")
			Expect(err).NotTo(HaveOccurred())
			Expect(users).To(ContainSubstring("user "+userName),
				"%s has no ACL entry for %s, so the user disappears on failover", p.Name, userName)
		}
	})

	It("rolls the Redis pods when the configuration changes", func() {
		By("checking the cluster runs the default eviction policy")
		before, err := listPods(clusterNamespace, redisSelector)
		Expect(err).NotTo(HaveOccurred())
		Expect(readyPods(before)).To(HaveLen(3), "pods: %v", before)
		for _, p := range readyPods(before) {
			policy, err := redisCLI(clusterNamespace, p.Name, "CONFIG", "GET", "maxmemory-policy")
			Expect(err).NotTo(HaveOccurred())
			Expect(policy).To(ContainSubstring("noeviction"),
				"%s already runs the configuration this spec is about to apply", p.Name)
		}

		beforeHash, err := podTemplateConfigHash(clusterNamespace, sentinelName+"-redis")
		Expect(err).NotTo(HaveOccurred())
		Expect(beforeHash).NotTo(BeEmpty(),
			"the pod template carries no config hash, so no config edit can ever reach a running pod")

		By("editing spec.redisConfig.customConfig on the running cluster")
		_, err = kubectl("patch", "redissentinel", sentinelName, "-n", clusterNamespace, "--type=merge",
			"-p", `{"spec":{"redisConfig":{"customConfig":{"maxmemory-policy":"allkeys-lru"}}}}`)
		Expect(err).NotTo(HaveOccurred())

		By("waiting for the operator to change the pod template")
		Eventually(func(g Gomega) {
			hash, err := podTemplateConfigHash(clusterNamespace, sentinelName+"-redis")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(hash).NotTo(BeEmpty())
			g.Expect(hash).NotTo(Equal(beforeHash), "the pod template still fingerprints the old configuration")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for every Redis pod to be replaced and serve the new configuration")
		Eventually(func(g Gomega) {
			pods, err := listPods(clusterNamespace, redisSelector)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(readyPods(pods)).To(HaveLen(3), "pods: %v", pods)

			old := podUIDs(before)
			for _, p := range readyPods(pods) {
				g.Expect(p.UID).NotTo(Equal(old[p.Name]), "%s was never restarted", p.Name)
				policy, err := redisCLI(clusterNamespace, p.Name, "CONFIG", "GET", "maxmemory-policy")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(policy).To(ContainSubstring("allkeys-lru"),
					"%s still runs the configuration it started with", p.Name)
			}
		}, 6*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for the cluster to converge on a single master again")
		Eventually(func(g Gomega) {
			states, err := replicationStates(clusterNamespace, redisSelector)
			g.Expect(err).NotTo(HaveOccurred())
			masters, replicas := splitByRole(states)
			g.Expect(masters).To(HaveLen(1), "the roll left no single master: %v", states)
			g.Expect(replicas).To(HaveLen(2), "the roll left a replica behind: %v", states)
			for _, replica := range replicas {
				g.Expect(replica.MasterLinkStatus).To(Equal("up"), "replica did not resync after the roll: %s", replica)
				g.Expect(replica.MasterHost).To(Equal(masters[0].IP), "replica follows a stale master: %s", replica)
			}

			writeIPs, err := serviceEndpointIPs(clusterNamespace, sentinelName+"-redis")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(writeIPs).To(ConsistOf(masters[0].IP),
				"the write Service did not follow the master through the roll")
		}, 5*time.Minute, 5*time.Second).Should(Succeed())

		// The ACL file lives on the data volume, which the roll reuses. A user
		// that only existed in server memory would be gone from every pod here.
		By("checking the ACL user survived the restart on every node")
		settled, err := listPods(clusterNamespace, redisSelector)
		Expect(err).NotTo(HaveOccurred())
		for _, p := range readyPods(settled) {
			users, err := redisCLI(clusterNamespace, p.Name, "ACL", "LIST")
			Expect(err).NotTo(HaveOccurred())
			Expect(users).To(ContainSubstring("user "+userName),
				"%s lost the ACL user across the restart", p.Name)
		}

		// Three periodic reconcile passes. The rendered config is assembled from
		// a Go map, so a fingerprint taken over the rendered bytes verbatim would
		// differ on each pass and roll the cluster, master included, forever.
		By("checking an unchanged spec does not roll the pods again")
		Consistently(func(g Gomega) {
			now, err := listPods(clusterNamespace, redisSelector)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(podUIDs(now)).To(Equal(podUIDs(settled)), "the pods restarted without a spec change")
		}, 95*time.Second, 10*time.Second).Should(Succeed())
	})

	It("serves authenticated metrics over the endpoint the chart exposes", func() {
		By("granting the operator service account the right to read /metrics")
		_, err := kubectl("create", "clusterrole", metricsReaderRole,
			"--verb=get", "--non-resource-url=/metrics")
		Expect(err).NotTo(HaveOccurred())
		_, err = kubectl("create", "clusterrolebinding", metricsReaderBinding,
			"--clusterrole="+metricsReaderRole,
			fmt.Sprintf("--serviceaccount=%s:%s", operatorNamespace, helmRelease))
		Expect(err).NotTo(HaveOccurred())

		By("requesting a token for that service account")
		token, err := serviceAccountToken(operatorNamespace, helmRelease)
		Expect(err).NotTo(HaveOccurred())
		Expect(token).NotTo(BeEmpty())

		By("scraping the metrics service from inside the cluster")
		overrides := fmt.Sprintf(`{
  "spec": {
    "containers": [{
      "name": "curl",
      "image": "curlimages/curl:8.11.1",
      "command": ["/bin/sh", "-c"],
      "args": ["curl -sS -k -f -H 'Authorization: Bearer %s' https://%s-metrics.%s.svc.cluster.local:8080/metrics"],
      "securityContext": {
        "readOnlyRootFilesystem": true,
        "allowPrivilegeEscalation": false,
        "capabilities": {"drop": ["ALL"]},
        "runAsNonRoot": true,
        "runAsUser": 1000,
        "seccompProfile": {"type": "RuntimeDefault"}
      }
    }],
    "serviceAccountName": "%s"
  }
}`, token, helmRelease, operatorNamespace, helmRelease)

		_, err = kubectl("run", curlPod, "--restart=Never", "-n", operatorNamespace,
			"--image=curlimages/curl:8.11.1", "--overrides", overrides)
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			phase, err := kubectl("get", "pod", curlPod, "-n", operatorNamespace,
				"-o", "jsonpath={.status.phase}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(phase).To(Equal("Succeeded"), "scrape pod phase is %q", phase)
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("checking the operator exports its Redis metrics")
		metrics, err := kubectl("logs", curlPod, "-n", operatorNamespace)
		Expect(err).NotTo(HaveOccurred())
		for _, name := range []string{"redis_cluster_info", "redis_connected_replicas", "redis_sentinel_status"} {
			Expect(metrics).To(ContainSubstring(name), "metric %s is not exported", name)
		}
	})

	It("promotes a replica when the master dies and keeps the data", func() {
		By("finding the current master")
		states, err := replicationStates(clusterNamespace, redisSelector)
		Expect(err).NotTo(HaveOccurred())
		masters, _ := splitByRole(states)
		Expect(masters).To(HaveLen(1), "no single master to fail over from: %v", states)
		oldMaster := masters[0]

		By("writing a key and waiting for both replicas to acknowledge it")
		_, err = redisCLI(clusterNamespace, oldMaster.Pod, "SET", failoverKey, failoverValue)
		Expect(err).NotTo(HaveOccurred())
		acked, err := redisCLI(clusterNamespace, oldMaster.Pod, "WAIT", "2", "10000")
		Expect(err).NotTo(HaveOccurred())
		Expect(acked).To(Equal("2"),
			"the write did not reach both replicas, so surviving the failover would prove nothing")

		By("deleting the master pod")
		_, err = kubectl("delete", "pod", oldMaster.Pod, "-n", clusterNamespace, "--wait=false")
		Expect(err).NotTo(HaveOccurred())

		By("waiting for Sentinel to promote one of the replicas")
		var newMaster replicaState
		Eventually(func(g Gomega) {
			states, err := replicationStates(clusterNamespace, redisSelector)
			g.Expect(err).NotTo(HaveOccurred())
			masters, _ := splitByRole(states)
			g.Expect(masters).To(HaveLen(1), "expected exactly one master during failover: %v", states)
			g.Expect(masters[0].Pod).NotTo(Equal(oldMaster.Pod),
				"the deleted pod is master again; no promotion happened")
			newMaster = masters[0]
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for the CR status to track the new master")
		Eventually(func(g Gomega) {
			status, err := getSentinelStatus(clusterNamespace, sentinelName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(status.MasterNode).NotTo(Equal(oldMaster.IP+":6379"),
				"the CR still points at the pod that was deleted")
			g.Expect(status.MasterNode).To(Equal(newMaster.IP+":6379"),
				"the CR does not report the promoted pod as master")
		}, 3*time.Minute, 5*time.Second).Should(Succeed())

		By("waiting for the write Service endpoints to move to the new master")
		Eventually(func(g Gomega) {
			ips, err := serviceEndpointIPs(clusterNamespace, sentinelName+"-redis")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ips).To(ConsistOf(newMaster.IP),
				"the write Service still routes somewhere other than the promoted master")

			labeled, err := listPods(clusterNamespace,
				redisSelector+",redis.redguard.io/role=master")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(labeled).To(HaveLen(1), "exactly one pod may carry the master role label")
			g.Expect(labeled[0].Name).To(Equal(newMaster.Pod),
				"the master role label sits on a pod that is not the master")
		}, 2*time.Minute, 5*time.Second).Should(Succeed())

		// Eventually: endpoints are already correct above, but kube-proxy may
		// lag a moment behind the Endpoints object.
		By("writing through the write Service")
		Eventually(func(g Gomega) {
			reply, err := redisCLI(clusterNamespace, newMaster.Pod,
				"-h", sentinelName+"-redis", "SET", "redguard-e2e:write-service", "ok")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(reply).To(Equal("OK"),
				"a write through the client Service was not accepted: %q", reply)
		}, 1*time.Minute, 5*time.Second).Should(Succeed())

		By("reading the pre-failover key back from the new master")
		value, err := redisCLI(clusterNamespace, newMaster.Pod, "GET", failoverKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(value).To(Equal(failoverValue), "the write made before the failover was lost")

		By("waiting for the restarted pod to rejoin as a replica of the new master")
		Eventually(func(g Gomega) {
			states, err := replicationStates(clusterNamespace, redisSelector)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(states).To(HaveLen(3), "not all pods are back: %v", states)
			masters, replicas := splitByRole(states)
			g.Expect(masters).To(HaveLen(1), "exactly one master expected: %v", states)
			g.Expect(masters[0].Pod).To(Equal(newMaster.Pod))
			g.Expect(replicas).To(HaveLen(2))
			for _, replica := range replicas {
				g.Expect(replica.MasterLinkStatus).To(Equal("up"),
					"replica did not resync after the failover: %s", replica)
				g.Expect(replica.MasterHost).To(Equal(masters[0].IP),
					"replica follows a stale master address: %s", replica)
			}
		}, 5*time.Minute, 5*time.Second).Should(Succeed())
	})

	// The startup check in the first spec only covers the watches the manager
	// opens before it has reconciled anything. Every permission the operator
	// needs to build, watch and repair a cluster is only exercised by the specs
	// above, so the log has to be re-read once they have all run.
	It("never hits an RBAC denial while running the specs above", func() {
		pods, err := listPods(operatorNamespace, "control-plane=controller-manager")
		Expect(err).NotTo(HaveOccurred())
		Expect(pods).To(HaveLen(1))
		Expect(pods[0].Restarts).To(BeZero(),
			"the controller restarted during the run, so its earlier logs are gone and something crashed it")

		logs, err := kubectl("logs", pods[0].Name, "-n", operatorNamespace)
		Expect(err).NotTo(HaveOccurred())
		Expect(logs).NotTo(ContainSubstring("is forbidden"),
			"the ClusterRole shipped by the chart is missing a permission the operator needs")
		Expect(logs).NotTo(ContainSubstring("cannot list resource"),
			"the ClusterRole shipped by the chart is missing a permission the operator needs")
	})
})

// serviceAccountToken mints a token for a service account through the
// TokenRequest API.
func serviceAccountToken(namespace, name string) (string, error) {
	request := filepath.Join(os.TempDir(), name+"-token-request.json")
	body := `{"apiVersion":"authentication.k8s.io/v1","kind":"TokenRequest"}`
	if err := os.WriteFile(request, []byte(body), 0o600); err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(request) }()

	out, err := kubectl("create", "--raw",
		fmt.Sprintf("/api/v1/namespaces/%s/serviceaccounts/%s/token", namespace, name),
		"-f", request)
	if err != nil {
		return "", err
	}

	var response struct {
		Status struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return "", fmt.Errorf("parse token response: %w", err)
	}
	return response.Status.Token, nil
}
