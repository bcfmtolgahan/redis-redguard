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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestInfoFloat(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"1024", 1024},
		{"1.53", 1.53},
		{"0", 0},
		// INFO carries integers and decimals; a field spelled any other way is
		// reported as 0 rather than left unset, so a gauge never keeps a stale
		// sample.
		{"", 0},
		{"not-a-number", 0},
	}
	for _, tc := range cases {
		if got := infoFloat(tc.in); got != tc.want {
			t.Errorf("infoFloat(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSetInfoGaugeSkipsAbsentFields(t *testing.T) {
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "test_gauge", Help: "test"}, []string{"pod"})

	setInfoGauge(gauge, map[string]string{"used_memory": "2048"}, "used_memory", "pod-0")
	if got := testutil.ToFloat64(gauge.WithLabelValues("pod-0")); got != 2048 {
		t.Errorf("gauge = %v, want 2048", got)
	}

	// A field INFO did not carry must not create a series reading zero.
	setInfoGauge(gauge, map[string]string{}, "maxmemory", "pod-1")
	if got := testutil.CollectAndCount(gauge); got != 1 {
		t.Errorf("gauge has %d series, want 1", got)
	}
}

func TestAddInfoCounterDeltaAdvancesByTheIncrease(t *testing.T) {
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_counter", Help: "test"}, []string{"pod"})

	// The first sample only sets the baseline, so the counter carries the
	// increase between the two INFO reads and not the absolute value.
	addInfoCounterDelta(counter, map[string]string{"hits": "10"}, "hits", "pod-0")
	addInfoCounterDelta(counter, map[string]string{"hits": "25"}, "hits", "pod-0")
	if got := testutil.ToFloat64(counter.WithLabelValues("pod-0")); got != 15 {
		t.Errorf("counter = %v, want 15", got)
	}

	addInfoCounterDelta(counter, map[string]string{}, "misses", "pod-1")
	if got := testutil.CollectAndCount(counter); got != 1 {
		t.Errorf("counter has %d series, want 1", got)
	}
}
