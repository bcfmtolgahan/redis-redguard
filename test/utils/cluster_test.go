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

package utils

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeKubeconfig writes a kubeconfig naming a single context and returns its
// path. Nothing here is a real cluster: the certificate fields kind writes are
// irrelevant to the checks under test and are left out.
func writeKubeconfig(t *testing.T, contextName, clusterName, server string) string {
	t.Helper()
	body := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: %s
clusters:
- cluster:
    server: %s
  name: %s
contexts:
- context:
    cluster: %s
    user: %s
  name: %s
users:
- name: %s
  user: {}
`, contextName, server, clusterName, clusterName, clusterName, contextName, clusterName)

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// foreignKubeconfig fabricates an EKS-shaped kubeconfig. The account and
// cluster names are invented; no real cluster is ever named by a test.
func foreignKubeconfig(t *testing.T) string {
	t.Helper()
	const arn = "arn:aws:eks:eu-west-3:000000000000:cluster/fake-cluster-for-tests"
	return writeKubeconfig(t, arn, arn, "https://FAKE0000000000000000000000000000.gr7.eu-west-3.eks.amazonaws.com")
}

func TestVerifyRejectsForeignContext(t *testing.T) {
	c := Cluster{Name: "redguard-e2e-abcd", Kubeconfig: foreignKubeconfig(t)}

	err := c.Verify()
	if err == nil {
		t.Fatal("Verify accepted a kubeconfig whose current context is not this run's kind cluster")
	}
	if !strings.Contains(err.Error(), "kind-redguard-e2e-abcd") {
		t.Fatalf("error does not name the expected context: %v", err)
	}
}

func TestVerifyRejectsAnotherKindCluster(t *testing.T) {
	path := writeKubeconfig(t, "kind-someone-elses", "kind-someone-elses", "https://127.0.0.1:6443")
	c := Cluster{Name: "redguard-e2e-abcd", Kubeconfig: path}

	if err := c.Verify(); err == nil {
		t.Fatal("Verify accepted a kind cluster this run did not create")
	}
}

func TestVerifyRejectsRemoteServer(t *testing.T) {
	// A context named like ours but pointed at a remote API server is the
	// shape a copied or edited kubeconfig takes.
	path := writeKubeconfig(t, "kind-redguard-e2e-abcd", "kind-redguard-e2e-abcd",
		"https://api.example.invalid:443")
	c := Cluster{Name: "redguard-e2e-abcd", Kubeconfig: path}

	if err := c.Verify(); err == nil {
		t.Fatal("Verify accepted a context whose API server is not local to this machine")
	}
}

func TestVerifyRejectsMissingFile(t *testing.T) {
	c := Cluster{Name: "redguard-e2e-abcd", Kubeconfig: filepath.Join(t.TempDir(), "absent")}

	if err := c.Verify(); err == nil {
		t.Fatal("Verify accepted a kubeconfig that does not exist")
	}
}

func TestVerifyAcceptsOwnCluster(t *testing.T) {
	path := writeKubeconfig(t, "kind-redguard-e2e-abcd", "kind-redguard-e2e-abcd",
		"https://127.0.0.1:52345")
	c := Cluster{Name: "redguard-e2e-abcd", Kubeconfig: path}

	if err := c.Verify(); err != nil {
		t.Fatalf("Verify rejected this run's own cluster: %v", err)
	}
}

func TestCommandIgnoresAmbientKubeconfig(t *testing.T) {
	foreign := foreignKubeconfig(t)
	t.Setenv("KUBECONFIG", foreign)

	c := Cluster{Name: "redguard-e2e-abcd", Kubeconfig: "/run/redguard/kubeconfig"}

	for _, tc := range []struct {
		bin         string
		contextFlag string
	}{
		{"kubectl", "--context=kind-redguard-e2e-abcd"},
		{"/opt/homebrew/bin/helm", "--kube-context=kind-redguard-e2e-abcd"},
	} {
		cmd := c.Command(tc.bin, "get", "namespace")
		args := strings.Join(cmd.Args[1:], " ")

		if !strings.Contains(args, "--kubeconfig=/run/redguard/kubeconfig") {
			t.Errorf("%s: argv does not pin the kubeconfig: %v", tc.bin, cmd.Args)
		}
		if !strings.Contains(args, tc.contextFlag) {
			t.Errorf("%s: argv does not pin the context: %v", tc.bin, cmd.Args)
		}
		if resolved := lastEnv(cmd.Env, "KUBECONFIG"); resolved != "/run/redguard/kubeconfig" {
			t.Errorf("%s: KUBECONFIG resolves to %q, ambient value leaked", tc.bin, resolved)
		}
		if strings.Contains(args, foreign) {
			t.Errorf("%s: ambient kubeconfig reached the argv: %v", tc.bin, cmd.Args)
		}
	}
}

// lastEnv mirrors how the operating system resolves a duplicated variable:
// the final assignment wins.
func lastEnv(env []string, key string) string {
	value := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			value = strings.TrimPrefix(kv, key+"=")
		}
	}
	return value
}

func TestRunRefusesUnpinnedClusterCommand(t *testing.T) {
	// Nothing here may reach a cluster. If the guard is missing the command
	// executes, so the ambient environment is aimed at one that does not exist.
	t.Setenv("KUBECONFIG", foreignKubeconfig(t))

	// The binary is named through a variable because building this command is
	// the whole point of the test, and the audit in pinning_test.go reports a
	// literal one as a call site that skipped the pinning helper.
	bin := "kubectl"
	out, err := Run(exec.Command(bin, "config", "current-context"))
	if err == nil {
		t.Fatalf("Run executed a kubectl invocation that resolves its target from the environment: %q", out)
	}
	if !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("Run failed for the wrong reason: %v", err)
	}
}

func TestRunAcceptsPinnedClusterCommand(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is not installed")
	}
	t.Setenv("KUBECONFIG", foreignKubeconfig(t))

	c := Cluster{
		Name: "redguard-e2e-abcd",
		Kubeconfig: writeKubeconfig(t, "kind-redguard-e2e-abcd", "kind-redguard-e2e-abcd",
			"https://127.0.0.1:52345"),
	}

	// Reading the selected context touches the file only, so this proves which
	// kubeconfig the command resolved without contacting an API server.
	out, err := Run(c.Command("kubectl", "config", "current-context"))
	if err != nil {
		t.Fatalf("Run refused a pinned command: %v", err)
	}
	if strings.TrimSpace(out) != c.Context() {
		t.Fatalf("pinned command resolved context %q, want %q", strings.TrimSpace(out), c.Context())
	}
}

func TestCommandLeavesNonClusterBinariesAlone(t *testing.T) {
	c := Cluster{Name: "redguard-e2e-abcd", Kubeconfig: "/run/redguard/kubeconfig"}

	cmd := c.Command("make", "docker-build")
	if len(cmd.Args) != 2 {
		t.Fatalf("flags were injected into a command that does not take them: %v", cmd.Args)
	}
}

func TestIsMutating(t *testing.T) {
	for _, tc := range []struct {
		name string
		bin  string
		args []string
		want bool
	}{
		{"namespace delete", "kubectl", []string{"delete", "namespace", "redguard-system"}, true},
		{"pvc delete by label", "kubectl", []string{"delete", "pvc", "-n", "default", "-l", "app=redis"}, true},
		{"crd delete", "kubectl", []string{"delete", "crd", "redissentinels.redis.redguard.io"}, true},
		{"delete after global flags", "kubectl", []string{"--namespace", "x", "delete", "pod", "p"}, true},
		{"apply a manifest", "kubectl", []string{"apply", "-f", "config/samples/sentinel.yaml"}, true},
		{"patch a custom resource", "kubectl", []string{"patch", "redissentinel", "x", "--type=merge", "-p", "{}"}, true},
		{"label a namespace", "kubectl", []string{"label", "--overwrite", "namespace", "default", "a=b"}, true},
		{"run a probe pod", "kubectl", []string{"run", "p", "--restart=Never", "--image=busybox"}, true},
		{"evict a pod through the raw API", "kubectl",
			[]string{"create", "--raw", "/api/v1/namespaces/default/pods/p/eviction", "-f", "req.json"}, true},
		{"helm uninstall", "helm", []string{"uninstall", "redguard", "--namespace", "redguard-system"}, true},
		{"helm install", "helm", []string{"upgrade", "--install", "redguard", "charts/redguard"}, true},
		{"an unrecognised subcommand", "kubectl", []string{"some-new-verb", "thing"}, true},

		{"get", "kubectl", []string{"get", "pods", "-A"}, false},
		{"logs", "kubectl", []string{"logs", "pod", "-n", "redguard-system"}, false},
		{"describe", "kubectl", []string{"describe", "pod", "p", "-n", "default"}, false},
		{"wait", "kubectl", []string{"wait", "--for=condition=Ready", "pod", "-n", "default"}, false},
		{"read the current context", "kubectl", []string{"config", "current-context"}, false},
		{"exec into a pod", "kubectl", []string{"exec", "-n", "default", "p", "--", "redis-cli", "ping"}, false},
		{"helm template", "helm", []string{"template", "redguard", "charts/redguard"}, false},
		{"helm list", "helm", []string{"list", "--all-namespaces"}, false},
		{"helm status", "helm", []string{"status", "redguard", "-n", "redguard-system"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsMutating(tc.bin, tc.args); got != tc.want {
				t.Fatalf("IsMutating(%q, %v) = %v, want %v", tc.bin, tc.args, got, tc.want)
			}
		})
	}
}

func TestIsClusterCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		bin  string
		args []string
		want bool
	}{
		{"kubectl", "kubectl", []string{"get", "pods"}, true},
		{"kubectl by path", "/usr/local/bin/kubectl", []string{"get", "pods"}, true},
		{"kubectl on windows", `C:\bin\kubectl.exe`, []string{"get", "pods"}, true},
		{"helm list", "helm", []string{"list", "--all-namespaces"}, true},
		{"helm by path", "/opt/homebrew/bin/helm", []string{"uninstall", "r"}, true},
		{"make", "make", []string{"docker-build"}, false},
		{"kind", "kind", []string{"delete", "cluster"}, false},
		{"docker", "/usr/bin/docker", []string{"rm", "-f", "c"}, false},
		{"a binary that merely starts the same way", "kubectl-not-really", []string{"get"}, false},

		// Rendering a chart reads local files only. Requiring a pinned cluster
		// for it would mean the chart tests, which are plain go tests running
		// outside the Ginkgo suite, could not run at all.
		{"helm template", "helm", []string{"template", "r", "charts/redguard"}, false},
		{"helm lint", "helm", []string{"lint", "charts/redguard"}, false},
		{"helm show values", "helm", []string{"show", "values", "charts/redguard"}, false},

		// --validate makes the renderer call the API server, so it is back to
		// being a cluster command.
		{"helm template --validate", "helm", []string{"template", "--validate", "r", "charts/redguard"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsClusterCommand(tc.bin, tc.args); got != tc.want {
				t.Fatalf("IsClusterCommand(%q, %v) = %v, want %v", tc.bin, tc.args, got, tc.want)
			}
		})
	}
}
