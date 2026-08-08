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
)

// TestChartPassesBackupAllowlistFlags renders the chart and checks that the
// backup destination allowlist reaches the manager as flags. By default no
// allowlist flag is rendered, which the operator treats as "IAM-role backups
// disabled". This needs helm but no cluster.
func TestChartPassesBackupAllowlistFlags(t *testing.T) {
	deployment := func(extraArgs ...string) string {
		args := append([]string{"template", helmRelease, chartPath,
			"--namespace", operatorNamespace,
			"--show-only", "templates/deployment.yaml"}, extraArgs...)
		out, err := run("helm", args...)
		if err != nil {
			t.Fatalf("helm template: %v", err)
		}
		return out
	}

	byDefault := deployment()
	for _, flagName := range []string{"--allowed-backup-buckets", "--allowed-backup-endpoints"} {
		if strings.Contains(byDefault, flagName) {
			t.Errorf("default render must not pass %s (empty means the IAM-role path stays disabled):\n%s", flagName, byDefault)
		}
	}

	configured := deployment(
		"--set", "backup.allowedBuckets={corp-backups,dr-backups}",
		"--set", "backup.allowedEndpoints={https://vpce.example.amazonaws.com}",
	)
	for _, want := range []string{
		"--allowed-backup-buckets=corp-backups,dr-backups",
		"--allowed-backup-endpoints=https://vpce.example.amazonaws.com",
	} {
		if !strings.Contains(configured, want) {
			t.Errorf("rendered deployment lacks %q:\n%s", want, configured)
		}
	}
}
