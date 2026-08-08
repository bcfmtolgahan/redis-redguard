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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	. "github.com/onsi/ginkgo/v2"

	"github.com/bcfmtolgahan/redis-redguard/test/utils"
)

// activeCluster is the kind cluster every kubectl and helm invocation in this
// suite is aimed at. BeforeSuite is the only place that assigns it, apart from
// the isolation specs, which substitute an impostor to prove the guard bites.
var activeCluster utils.Cluster

// guardViolation ends the run when a command would reach a cluster this run did
// not create. Cleanup nodes discard the error returned by their commands, so a
// refusal that only returned an error would be silent; this writes to stderr
// and fails the suite. The isolation specs replace it to observe a refusal.
var guardViolation = func(name string, args []string, err error) {
	msg := fmt.Sprintf("refusing to run %q: %v", name+" "+strings.Join(args, " "), err)
	_, _ = fmt.Fprintln(os.Stderr, "e2e cluster guard: "+msg)
	Fail(msg, 1)
}

// run executes a command from the project root and returns stdout only.
// stderr is folded into the error instead of the result: kubectl writes
// warnings there, and a warning mixed into a JSON document breaks parsing.
//
// Anything that talks to an API server is rewritten to carry this run's
// kubeconfig and context, so neither the ambient KUBECONFIG nor whichever
// context the shared kubeconfig happens to select can redirect it.
func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)

	if utils.IsClusterCommand(name, args) {
		if err := activeCluster.Verify(); err != nil {
			guardViolation(name, args, err)
			return "", err
		}
		if utils.IsMutating(name, args) {
			if err := activeCluster.VerifyLive(); err != nil {
				guardViolation(name, args, err)
				return "", err
			}
		}
		cmd = activeCluster.Command(name, args...)
	}

	if dir, err := utils.GetProjectDir(); err == nil {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	_, _ = fmt.Fprintf(GinkgoWriter, "running: %s %s\n", name, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s",
			name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func kubectl(args ...string) (string, error) {
	return run("kubectl", args...)
}

// podState is the slice of pod status the specs assert on.
type podState struct {
	Name string
	// UID distinguishes a pod that was replaced from one that merely
	// reconnected: a StatefulSet roll reuses the name and the volume.
	UID      string
	IP       string
	Phase    string
	Ready    bool
	Restarts int
}

// listPods returns the pods matching selector in name order. Pods with a
// deletion timestamp are left out: a terminating pod is not part of the state
// the cluster is converging on.
func listPods(namespace, selector string) ([]podState, error) {
	out, err := kubectl("get", "pods", "-n", namespace, "-l", selector, "-o", "json")
	if err != nil {
		return nil, err
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				UID               string `json:"uid"`
				DeletionTimestamp string `json:"deletionTimestamp"`
			} `json:"metadata"`
			Status struct {
				Phase      string `json:"phase"`
				PodIP      string `json:"podIP"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				ContainerStatuses []struct {
					RestartCount int `json:"restartCount"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse pod list for %q: %w", selector, err)
	}

	pods := make([]podState, 0, len(list.Items))
	for _, item := range list.Items {
		if item.Metadata.DeletionTimestamp != "" {
			continue
		}
		p := podState{
			Name:  item.Metadata.Name,
			UID:   item.Metadata.UID,
			IP:    item.Status.PodIP,
			Phase: item.Status.Phase,
		}
		for _, c := range item.Status.Conditions {
			if c.Type == "Ready" {
				p.Ready = c.Status == "True"
			}
		}
		for _, cs := range item.Status.ContainerStatuses {
			p.Restarts += cs.RestartCount
		}
		pods = append(pods, p)
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	return pods, nil
}

// readyPods filters listPods output down to the pods that pass their readiness probe.
func readyPods(pods []podState) []podState {
	ready := make([]podState, 0, len(pods))
	for _, p := range pods {
		if p.Ready {
			ready = append(ready, p)
		}
	}
	return ready
}

// podUIDs maps pod name to UID, the identity that changes when a pod is
// replaced rather than restarted in place.
func podUIDs(pods []podState) map[string]string {
	uids := make(map[string]string, len(pods))
	for _, p := range pods {
		uids[p.Name] = p.UID
	}
	return uids
}

// podTemplateConfigHash reads the annotation that makes a config change reach
// running pods. An empty result means the StatefulSet would never roll.
func podTemplateConfigHash(namespace, statefulSet string) (string, error) {
	return kubectl("get", "statefulset", statefulSet, "-n", namespace,
		"-o", `jsonpath={.spec.template.metadata.annotations.redis\.redguard\.io/config-hash}`)
}

// redisCLI runs redis-cli inside a pod and returns its trimmed stdout.
func redisCLI(namespace, pod string, args ...string) (string, error) {
	argv := append([]string{"exec", "-n", namespace, pod, "--", "redis-cli"}, args...)
	out, err := kubectl(argv...)
	return strings.TrimSpace(out), err
}

// infoField reads a single `key:value` line out of a redis INFO reply.
func infoField(info, key string) string {
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+":"))
		}
	}
	return ""
}

// replicaState is what one Redis server reports about its own replication role.
type replicaState struct {
	Pod              string
	IP               string
	Role             string
	MasterHost       string
	MasterLinkStatus string
}

func (r replicaState) String() string {
	return fmt.Sprintf("%s(%s) role=%s master=%s link=%s",
		r.Pod, r.IP, r.Role, r.MasterHost, r.MasterLinkStatus)
}

// replicationStates asks every ready Redis pod what it thinks its role is.
// Only ready pods are queried, because a pod that is not ready is not yet
// answering on 6379 and would fail the exec for a reason that says nothing
// about replication.
func replicationStates(namespace, selector string) ([]replicaState, error) {
	pods, err := listPods(namespace, selector)
	if err != nil {
		return nil, err
	}

	states := make([]replicaState, 0, len(pods))
	for _, p := range readyPods(pods) {
		info, err := redisCLI(namespace, p.Name, "info", "replication")
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

// splitByRole partitions replication states into masters and replicas.
func splitByRole(states []replicaState) (masters, replicas []replicaState) {
	for _, s := range states {
		switch s.Role {
		case "master":
			masters = append(masters, s)
		case "slave":
			replicas = append(replicas, s)
		}
	}
	return masters, replicas
}

// serviceEndpointIPs returns the ready endpoint addresses of a Service, the
// set of pods traffic to it can actually reach.
func serviceEndpointIPs(namespace, service string) ([]string, error) {
	out, err := kubectl("get", "endpoints", service, "-n", namespace, "-o", "json")
	if err != nil {
		return nil, err
	}

	var endpoints struct {
		Subsets []struct {
			Addresses []struct {
				IP string `json:"ip"`
			} `json:"addresses"`
		} `json:"subsets"`
	}
	if err := json.Unmarshal([]byte(out), &endpoints); err != nil {
		return nil, fmt.Errorf("parse endpoints for %q: %w", service, err)
	}

	var ips []string
	for _, subset := range endpoints.Subsets {
		for _, addr := range subset.Addresses {
			ips = append(ips, addr.IP)
		}
	}
	sort.Strings(ips)
	return ips, nil
}

// sentinelStatus mirrors the RedisSentinel status subresource fields the specs read.
type sentinelStatus struct {
	Phase          string `json:"phase"`
	MasterNode     string `json:"masterNode"`
	ReadyReplicas  int    `json:"readyReplicas"`
	ReadySentinels int    `json:"readySentinels"`
	Conditions     []struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"conditions"`
}

func (s sentinelStatus) condition(name string) (string, string) {
	for _, c := range s.Conditions {
		if c.Type == name {
			return c.Status, c.Message
		}
	}
	return "", "condition not present"
}

func getSentinelStatus(namespace, name string) (sentinelStatus, error) {
	out, err := kubectl("get", "redissentinel", name, "-n", namespace, "-o", "json")
	if err != nil {
		return sentinelStatus{}, err
	}
	var cr struct {
		Status sentinelStatus `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &cr); err != nil {
		return sentinelStatus{}, fmt.Errorf("parse RedisSentinel %s: %w", name, err)
	}
	return cr.Status, nil
}

// dumpCluster writes everything needed to diagnose a failed spec to the
// Ginkgo report: pod state, the generated config, and the logs of every
// component involved.
func dumpCluster(namespace, name string) {
	report := func(title string, args ...string) {
		out, err := kubectl(args...)
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "\n--- %s (failed: %v) ---\n%s\n", title, err, out)
			return
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "\n--- %s ---\n%s\n", title, out)
	}

	report("pods", "get", "pods", "-A", "-o", "wide")
	report("redissentinel", "get", "redissentinel", name, "-n", namespace, "-o", "yaml")
	report("events", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
	report("operator logs", "logs", "-n", operatorNamespace,
		"-l", "control-plane=controller-manager", "--tail=200")
	report("redis configmap", "get", "configmap", name+"-redis-config", "-n", namespace, "-o", "yaml")
	report("sentinel configmap", "get", "configmap", name+"-sentinel-config", "-n", namespace, "-o", "yaml")

	for _, component := range []string{"redis", "sentinel"} {
		pods, err := listPods(namespace, componentSelector(name, component))
		if err != nil {
			_, _ = fmt.Fprintf(GinkgoWriter, "\n--- list %s pods failed: %v ---\n", component, err)
			continue
		}
		for _, p := range pods {
			report("describe "+p.Name, "describe", "pod", p.Name, "-n", namespace)
			report("logs "+p.Name, "logs", p.Name, "-n", namespace, "--tail=100")
			report("previous logs "+p.Name, "logs", p.Name, "-n", namespace, "--previous", "--tail=100")
		}
	}
}

// componentSelector builds the label selector the operator actually stamps on
// its pods. buildLabels emits only the app.kubernetes.io/* set.
func componentSelector(name, component string) string {
	return fmt.Sprintf("app.kubernetes.io/instance=%s,app.kubernetes.io/component=%s", name, component)
}
