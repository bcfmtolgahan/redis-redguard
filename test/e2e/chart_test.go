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
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// podSecurity is the slice of a rendered pod template the restricted Pod
// Security Standard evaluates.
type podSecurity struct {
	Spec struct {
		Template struct {
			Spec struct {
				HostNetwork     bool `json:"hostNetwork"`
				SecurityContext struct {
					RunAsNonRoot   *bool  `json:"runAsNonRoot"`
					RunAsUser      *int64 `json:"runAsUser"`
					RunAsGroup     *int64 `json:"runAsGroup"`
					SeccompProfile struct {
						Type string `json:"type"`
					} `json:"seccompProfile"`
				} `json:"securityContext"`
				Containers []struct {
					Name            string `json:"name"`
					SecurityContext struct {
						AllowPrivilegeEscalation *bool `json:"allowPrivilegeEscalation"`
						ReadOnlyRootFilesystem   *bool `json:"readOnlyRootFilesystem"`
						Privileged               *bool `json:"privileged"`
						Capabilities             struct {
							Drop []string `json:"drop"`
							Add  []string `json:"add"`
						} `json:"capabilities"`
					} `json:"securityContext"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// TestChartOperatorPodMeetsRestrictedPSS renders the chart with its default
// values and checks the operator Deployment against the restricted Pod
// Security Standard. The chart is the documented install path, so a chart that
// renders a non-compliant pod cannot be installed in a namespace the project's
// own documentation tells users to lock down. This needs helm but no cluster.
func TestChartOperatorPodMeetsRestrictedPSS(t *testing.T) {
	out, err := run("helm", "template", helmRelease, chartPath,
		"--namespace", operatorNamespace,
		"--show-only", "templates/deployment.yaml")
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}

	var d podSecurity
	if err := yaml.Unmarshal([]byte(stripHelmSourceComment(out)), &d); err != nil {
		t.Fatalf("parse rendered deployment: %v\n%s", err, out)
	}

	pod := d.Spec.Template.Spec
	if pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Errorf("pod runAsNonRoot = %v, want true", pod.SecurityContext.RunAsNonRoot)
	}
	if pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser == 0 {
		t.Errorf("pod runAsUser = %v, want a non-zero uid", pod.SecurityContext.RunAsUser)
	}
	if pod.SecurityContext.RunAsGroup == nil || *pod.SecurityContext.RunAsGroup == 0 {
		t.Errorf("pod runAsGroup = %v, want a non-zero gid", pod.SecurityContext.RunAsGroup)
	}
	if got := pod.SecurityContext.SeccompProfile.Type; got != "RuntimeDefault" {
		t.Errorf("pod seccompProfile.type = %q, want RuntimeDefault", got)
	}
	if pod.HostNetwork {
		t.Error("pod uses the host network")
	}

	if len(pod.Containers) == 0 {
		t.Fatal("rendered deployment has no containers")
	}
	for _, c := range pod.Containers {
		sc := c.SecurityContext
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("%s: allowPrivilegeEscalation = %v, want false", c.Name, sc.AllowPrivilegeEscalation)
		}
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("%s: readOnlyRootFilesystem = %v, want true", c.Name, sc.ReadOnlyRootFilesystem)
		}
		if sc.Privileged != nil && *sc.Privileged {
			t.Errorf("%s: privileged = true", c.Name)
		}
		if !contains(sc.Capabilities.Drop, "ALL") {
			t.Errorf("%s: capabilities.drop = %v, want [ALL]", c.Name, sc.Capabilities.Drop)
		}
		if len(sc.Capabilities.Add) > 0 {
			t.Errorf("%s: capabilities.add = %v, want none", c.Name, sc.Capabilities.Add)
		}
	}
}

// stripHelmSourceComment drops the "# Source: ..." separator helm prints ahead
// of each rendered document so the result is a single YAML document.
func stripHelmSourceComment(rendered string) string {
	var kept []string
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "# Source:") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
