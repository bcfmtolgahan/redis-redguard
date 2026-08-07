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
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// evictPod posts to the eviction subresource, the call kubectl drain makes and
// the only one a PodDisruptionBudget can refuse.
func evictPod(namespace, pod string) (string, error) {
	body := fmt.Sprintf(
		`{"apiVersion":"policy/v1","kind":"Eviction","metadata":{"name":%q,"namespace":%q}}`,
		pod, namespace)
	request := filepath.Join(os.TempDir(), "redguard-eviction.json")
	if err := os.WriteFile(request, []byte(body), 0o600); err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(request) }()

	return kubectl("create", "--raw",
		fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/eviction", namespace, pod), "-f", request)
}

// allowedDisruptions is what the budget currently permits. It drops to 0 while
// a pod is missing, which is what makes a second eviction fail.
func allowedDisruptions(namespace, name string) (int, error) {
	out, err := kubectl("get", "poddisruptionbudget", name, "-n", namespace,
		"-o", "jsonpath={.status.disruptionsAllowed}")
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); err != nil {
		return 0, fmt.Errorf("parse disruptionsAllowed %q: %w", out, err)
	}
	return n, nil
}

// podTemplateAffinity returns the scheduling rules the operator stamped on a
// StatefulSet's pod template.
func podTemplateAffinity(namespace, statefulSet string) (string, error) {
	return kubectl("get", "statefulset", statefulSet, "-n", namespace,
		"-o", "jsonpath={.spec.template.spec.affinity}")
}

// statefulSetReplicas is the size the operator currently asks for, which is not
// the spec's desired size while a scale-down is being held back.
func statefulSetReplicas(namespace, name string) (int, error) {
	out, err := kubectl("get", "statefulset", name, "-n", namespace, "-o", "jsonpath={.spec.replicas}")
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); err != nil {
		return 0, fmt.Errorf("parse replicas %q: %w", out, err)
	}
	return n, nil
}

// warningEvents returns the Warning events recorded against the RedisSentinel
// for one reason.
func warningEvents(namespace, reason string) (string, error) {
	return kubectl("get", "events", "-n", namespace,
		"--field-selector", "reason="+reason,
		"-o", "jsonpath={range .items[*]}{.type}{' '}{.message}{'\\n'}{end}")
}
