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

// Status.Phase values. RedisSentinel, RedisBackup and RedisUser type the field
// as a plain string, so these are the only definition of the vocabulary that
// the printer columns and the operator's own comparisons share. Condition types
// are a separate vocabulary even where the spelling collides, so "Ready" and
// "Degraded" conditions do not use these.
const (
	phaseCreating            = "Creating"
	phaseScaling             = "Scaling"
	phaseConfiguringSentinel = "ConfiguringSentinel"
	phaseRunning             = "Running"
	phaseCompleted           = "Completed"
	phaseFailed              = "Failed"
	phaseDegraded            = "Degraded"
)
