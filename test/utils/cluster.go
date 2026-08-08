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
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// Cluster is the kind cluster one e2e run created together with the private
// kubeconfig that is the only path this run takes to reach it. Every value in
// here is per-run: two suites executing at once hold different ones and cannot
// reach, or delete, each other's cluster.
type Cluster struct {
	// Name is the kind cluster name, as passed to --name.
	Name string
	// Kubeconfig is a file written by this run and read by nothing else.
	Kubeconfig string
	// Kubectl and Helm are the binaries to invoke; empty means the default.
	Kubectl string
	Helm    string
}

// Context is the context name kind writes for a cluster. kind names the
// context, the cluster entry and the user identically.
func (c Cluster) Context() string { return "kind-" + c.Name }

func (c Cluster) kubectlBin() string {
	if c.Kubectl != "" {
		return c.Kubectl
	}
	return "kubectl"
}

// NewKindCluster copies a kind cluster's credentials into a directory of this
// run's own and verifies the result describes that cluster. The returned
// function removes the credentials.
func NewKindCluster(kindBin, name string) (Cluster, func(), error) {
	if name == "" {
		return Cluster{}, func() {}, fmt.Errorf("no kind cluster name given; set KIND_CLUSTER")
	}
	if kindBin == "" {
		kindBin = "kind"
	}

	out, err := exec.Command(kindBin, "get", "kubeconfig", "--name", name).Output()
	if err != nil {
		return Cluster{}, func() {}, fmt.Errorf("read kubeconfig for kind cluster %q: %w", name, err)
	}

	dir, err := os.MkdirTemp("", "redguard-e2e-")
	if err != nil {
		return Cluster{}, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		cleanup()
		return Cluster{}, func() {}, err
	}

	c := Cluster{Name: name, Kubeconfig: path}
	if err := c.Verify(); err != nil {
		cleanup()
		return Cluster{}, func() {}, err
	}
	return c, cleanup, nil
}

// kubeconfig is the subset of a kubeconfig these checks read.
type kubeconfig struct {
	CurrentContext string `json:"current-context"`
	Clusters       []struct {
		Name    string `json:"name"`
		Cluster struct {
			Server string `json:"server"`
		} `json:"cluster"`
	} `json:"clusters"`
	Contexts []struct {
		Name    string `json:"name"`
		Context struct {
			Cluster string `json:"cluster"`
		} `json:"context"`
	} `json:"contexts"`
}

// Verify checks that the pinned kubeconfig still describes this run's kind
// cluster and nothing else. It reads the file only, so it is cheap enough to
// run ahead of every command.
func (c Cluster) Verify() error {
	if c.Name == "" || c.Kubeconfig == "" {
		return fmt.Errorf("no kind cluster is pinned for this run")
	}

	raw, err := os.ReadFile(c.Kubeconfig)
	if err != nil {
		return fmt.Errorf("read pinned kubeconfig %s: %w", c.Kubeconfig, err)
	}
	var cfg kubeconfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse pinned kubeconfig %s: %w", c.Kubeconfig, err)
	}

	want := c.Context()
	if cfg.CurrentContext != want {
		return fmt.Errorf("pinned kubeconfig %s selects context %q, not %q",
			c.Kubeconfig, cfg.CurrentContext, want)
	}

	clusterEntry := ""
	for _, ctx := range cfg.Contexts {
		if ctx.Name == want {
			clusterEntry = ctx.Context.Cluster
		}
	}
	if clusterEntry != want {
		return fmt.Errorf("pinned kubeconfig %s maps context %q to cluster %q, not %q",
			c.Kubeconfig, want, clusterEntry, want)
	}

	for _, cl := range cfg.Clusters {
		if cl.Name != want {
			continue
		}
		return verifyLocalServer(cl.Cluster.Server)
	}
	return fmt.Errorf("pinned kubeconfig %s has no cluster entry named %q", c.Kubeconfig, want)
}

// verifyLocalServer rejects any API server that is not on this machine. kind
// publishes its API server on the loopback interface; a remote endpoint means
// the file is not the one kind wrote.
func verifyLocalServer(server string) error {
	u, err := url.Parse(server)
	if err != nil {
		return fmt.Errorf("parse API server %q: %w", server, err)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("API server %q is not local; a kind cluster is reached over loopback", server)
}

// VerifyLive additionally asks the API server to identify itself. kind stamps
// every node with providerID kind://<runtime>/<cluster>/<node>, which no other
// provider produces and which carries the cluster name, so a reply proves both
// that the target is kind and that it is this run's cluster.
func (c Cluster) VerifyLive() error {
	if err := c.Verify(); err != nil {
		return err
	}

	cmd := c.Command(c.kubectlBin(), "get", "nodes", "--request-timeout=30s",
		"-o", `jsonpath={range .items[*]}{.spec.providerID}{"\n"}{end}`)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("identify cluster %q: %w", c.Name, err)
	}

	nodes := 0
	for _, line := range strings.Split(string(out), "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		nodes++

		// kind://<runtime>/<cluster>/<node>
		rest, isKind := strings.CutPrefix(id, "kind://")
		parts := strings.Split(rest, "/")
		if !isKind || len(parts) < 3 || parts[1] != c.Name {
			return fmt.Errorf("node providerID %q does not belong to kind cluster %q", id, c.Name)
		}
	}
	if nodes == 0 {
		return fmt.Errorf("cluster %q reports no nodes, so it cannot be identified", c.Name)
	}
	return nil
}

// Command builds a kubectl or helm invocation that targets this cluster no
// matter what KUBECONFIG holds or which context the shared kubeconfig selects.
// The flags are what actually decide the target; the environment is set as
// well so anything the tool spawns inherits the same answer.
func (c Cluster) Command(name string, args ...string) *exec.Cmd {
	if !IsClusterCommand(name, args) {
		return exec.Command(name, args...)
	}

	pinned := []string{"--kubeconfig=" + c.Kubeconfig}
	if binaryName(name) == "helm" {
		pinned = append(pinned, "--kube-context="+c.Context())
	} else {
		pinned = append(pinned, "--context="+c.Context())
	}

	cmd := exec.Command(name, append(pinned, args...)...)
	cmd.Env = append(os.Environ(), "KUBECONFIG="+c.Kubeconfig)
	return cmd
}

// binaryName reduces a path to the tool it invokes.
func binaryName(name string) string {
	base := filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	return strings.TrimSuffix(base, ".exe")
}

// offlineHelmVerbs render or inspect local files and never open a connection,
// so they need no cluster. Requiring one would block the chart tests, which are
// plain go tests and run outside the Ginkgo suite that pins the cluster.
var offlineHelmVerbs = map[string]bool{
	"template": true, "lint": true, "show": true, "version": true,
	"env": true, "repo": true, "search": true, "create": true,
	"package": true, "dependency": true, "docs": true,
}

// IsClusterCommand reports whether an invocation talks to a Kubernetes API
// server and therefore has to be pinned to this run's cluster.
func IsClusterCommand(name string, args []string) bool {
	switch binaryName(name) {
	case "kubectl":
		return true
	case "helm":
		// --validate sends the rendered manifests to the API server, which
		// turns an otherwise offline render into a cluster command.
		for _, arg := range args {
			if arg == "--validate" {
				return true
			}
		}
		return !offlineHelmVerbs[firstVerb(args)]
	}
	return false
}

// readOnlyVerbs are the subcommands that only observe. Everything else is
// treated as a write, so a subcommand nobody listed here is guarded rather than
// waved through. kubectl exec is counted as a read: it names a pod that only
// this run creates, and the identity check it would otherwise trigger runs on
// every poll of a cluster that is still converging.
var readOnlyVerbs = map[string]map[string]bool{
	"kubectl": {
		"get": true, "describe": true, "logs": true, "wait": true,
		"exec": true, "config": true, "version": true, "explain": true,
		"api-resources": true, "api-versions": true, "cluster-info": true,
		"top": true, "events": true, "auth": true, "diff": true,
		"port-forward": true, "proxy": true, "cp": true,
	},
	"helm": {
		"list": true, "status": true, "get": true, "history": true,
		"template": true, "lint": true, "show": true, "version": true,
		"env": true, "search": true, "repo": true,
	},
}

// IsMutating reports whether an invocation would change cluster state. These
// pay for an identity check against the live API server before they run; a read
// that went to the wrong cluster can be repaired by rerunning it, a write
// cannot. helm install and upgrade count: writing a release into a cluster this
// run does not own is as damaging as deleting one.
func IsMutating(name string, args []string) bool {
	reads, ok := readOnlyVerbs[binaryName(name)]
	if !ok {
		return false
	}
	return !reads[firstVerb(args)]
}

// firstVerb returns the subcommand, skipping the global flags that may precede
// it and the values they consume.
func firstVerb(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
		// A flag written as --name=value carries its value; one written as
		// --name value consumes the next argument.
		if !strings.Contains(arg, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
		}
	}
	return ""
}
