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
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	custmetrics "github.com/bcfmtolgahan/redis-redguard/pkg/metrics"
)

// infoFloat reads one INFO field as a sample. A field Redis spells in some
// other form yields 0; there is nothing better to publish and the scrape must
// not carry the previous pass's value.
func infoFloat(s string) float64 {
	var v float64
	_, _ = fmt.Sscanf(s, "%f", &v)
	return v
}

// setInfoGauge publishes info[key], if INFO carried it at all. An absent field
// leaves the series alone rather than creating one that reads zero.
func setInfoGauge(gauge *prometheus.GaugeVec, info map[string]string, key string, labels ...string) {
	raw, ok := info[key]
	if !ok {
		return
	}
	gauge.WithLabelValues(labels...).Set(infoFloat(raw))
}

// addInfoCounterDelta advances counter by the increase in info[key] since the
// last pass. INFO reports these absolute and they restart at zero with the
// node, which is what AddCounterDelta absorbs.
func addInfoCounterDelta(counter *prometheus.CounterVec, info map[string]string, key string, labels ...string) {
	raw, ok := info[key]
	if !ok {
		return
	}
	custmetrics.AddCounterDelta(counter, infoFloat(raw), labels...)
}
