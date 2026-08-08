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
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"sigs.k8s.io/yaml"
)

// repoURL is the only repository the release publishes from. The chart is
// served to users who have no other way to find the source.
const repoURL = "https://github.com/bcfmtolgahan/redis-redguard"

const repoRoot = "../.."

type chartMetadata struct {
	Version    string   `json:"version"`
	AppVersion string   `json:"appVersion"`
	Home       string   `json:"home"`
	Sources    []string `json:"sources"`
}

type chartValues struct {
	Operator struct {
		Image struct {
			Tag string `json:"tag"`
		} `json:"image"`
	} `json:"operator"`
}

func readRepoYAML(t *testing.T, rel string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	if err := yaml.Unmarshal(raw, into); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
}

// latestChangelogVersion returns the version of the topmost CHANGELOG entry.
func latestChangelogVersion(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	m := regexp.MustCompile(`(?m)^## \[([0-9]+\.[0-9]+\.[0-9]+)\]`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("CHANGELOG.md has no '## [x.y.z]' heading")
	}
	return string(m[1])
}

// TestChartVersionMatchesChangelog guards the release gate: the release
// workflow refuses to publish unless the chart version equals the tag, and the
// tag is cut from the topmost CHANGELOG entry. A chart left on the previous
// version rejects the tag after it has already been pushed.
func TestChartVersionMatchesChangelog(t *testing.T) {
	want := latestChangelogVersion(t)

	var chart chartMetadata
	readRepoYAML(t, "charts/redguard/Chart.yaml", &chart)
	if chart.Version != want {
		t.Errorf("Chart.yaml version is %q, CHANGELOG is at %q", chart.Version, want)
	}
	if chart.AppVersion != want {
		t.Errorf("Chart.yaml appVersion is %q, CHANGELOG is at %q", chart.AppVersion, want)
	}

	// The chart ships the image tag the release builds, so a stale default
	// silently installs the previous operator from a current chart.
	var values chartValues
	readRepoYAML(t, "charts/redguard/values.yaml", &values)
	if got := values.Operator.Image.Tag; got != "v"+want {
		t.Errorf("values.yaml image tag is %q, want %q", got, "v"+want)
	}
}

// TestChartPointsAtTheRealRepository keeps artifacthub and 'helm show chart'
// from sending users to a repository that is not this one.
func TestChartPointsAtTheRealRepository(t *testing.T) {
	var chart chartMetadata
	readRepoYAML(t, "charts/redguard/Chart.yaml", &chart)

	if chart.Home != repoURL {
		t.Errorf("Chart.yaml home is %q, want %q", chart.Home, repoURL)
	}
	if len(chart.Sources) != 1 || chart.Sources[0] != repoURL {
		t.Errorf("Chart.yaml sources are %q, want [%q]", chart.Sources, repoURL)
	}
}
