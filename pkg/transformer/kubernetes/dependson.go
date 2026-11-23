/*
Copyright 2017 The Kubernetes Authors All rights reserved.

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

package kubernetes

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kubernetes/kompose/pkg/kobject"
	"github.com/kubernetes/kompose/pkg/loader/compose"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cast"
	api "k8s.io/api/core/v1"
)

// DefaultDependsOnWaitImage is the image used for generated depends_on wait
// init containers when kompose.depends-on.wait-image is not set. It only
// needs a POSIX shell and nc (both shipped by busybox).
const DefaultDependsOnWaitImage = "busybox:1.36"

// dependsOnOptionalWaitAttempts bounds how long a wait init container for a
// required: false dependency waits before giving up and letting the pod
// start anyway (attempts * 2 seconds of sleep between them).
const dependsOnOptionalWaitAttempts = 30

// normalizeDependsOnName normalizes a service name the same way the compose
// loader does, so label keys written with the compose service name match the
// normalized names used internally.
func normalizeDependsOnName(name string) string {
	re := regexp.MustCompile("[._]")
	return strings.ToLower(re.ReplaceAllString(name, "-"))
}

// parseDependsOnProbePorts parses the kompose.depends-on.probe-ports label
// value ("db=5432,cache=6379") into a map from normalized dependency name to
// TCP port. Invalid entries are reported and skipped.
func parseDependsOnProbePorts(value string, serviceName string) map[string]string {
	ports := map[string]string{}
	if value == "" {
		return ports
	}
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, port, found := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		port = strings.TrimSpace(port)
		if !found || name == "" {
			log.Warnf("Invalid entry %q in label %s of service %s, expected <service>=<port> - ignoring", entry, compose.LabelDependsOnProbePorts, serviceName)
			continue
		}
		if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
			log.Warnf("Invalid port %q in label %s of service %s - ignoring entry", port, compose.LabelDependsOnProbePorts, serviceName)
			continue
		}
		ports[normalizeDependsOnName(name)] = port
	}
	return ports
}

// dependsOnWaitCommand returns the shell command probing host:port in a loop.
// A required dependency is waited on indefinitely (the pod stays in Init
// until the dependency is reachable, mirroring how compose blocks the
// dependent service). An optional dependency (required: false) is waited on
// for a bounded time and never blocks the pod from starting.
func dependsOnWaitCommand(host string, port string, required bool) []string {
	if !required {
		script := fmt.Sprintf(
			"for i in $(seq 1 %d); do nc -z -w 2 %s %s && exit 0; echo \"waiting for %s:%s ($i/%d)\"; sleep 2; done; "+
				"echo \"timed out waiting for %s:%s, continuing anyway (depends_on required: false)\"; exit 0",
			dependsOnOptionalWaitAttempts, host, port, host, port, dependsOnOptionalWaitAttempts, host, port)
		return []string{"sh", "-c", script}
	}
	script := fmt.Sprintf(
		"until nc -z -w 2 %s %s; do echo \"waiting for %s:%s\"; sleep 2; done",
		host, port, host, port)
	return []string{"sh", "-c", script}
}

// buildDependsOnInitContainers converts the depends_on entries of a service
// to wait init containers. It only runs when the service opts in through the
// kompose.depends-on.wait label, because probing a TCP port is only an
// approximation of the compose conditions service_started and
// service_healthy. The returned containers are sorted by dependency name
// (DependsOn is already sorted by the loader).
func buildDependsOnInitContainers(service kobject.ServiceConfig, allServices map[string]kobject.ServiceConfig) []kobject.InitContainerConfig {
	if len(service.DependsOn) == 0 {
		return nil
	}

	if !cast.ToBool(service.Labels[compose.LabelDependsOnWait]) {
		log.Warnf("depends_on of service %s is not converted: Kubernetes has no native startup ordering. Set label %s to 'true' to approximate it with wait init containers", service.Name, compose.LabelDependsOnWait)
		return nil
	}

	image := service.Labels[compose.LabelDependsOnWaitImage]
	if image == "" {
		image = DefaultDependsOnWaitImage
	}

	probePorts := parseDependsOnProbePorts(service.Labels[compose.LabelDependsOnProbePorts], service.Name)

	var containers []kobject.InitContainerConfig
	for _, dep := range service.DependsOn {
		switch dep.Condition {
		case "service_started", "service_healthy":
			// handled below
		case "service_completed_successfully":
			log.Warnf("depends_on condition service_completed_successfully for service %s on %s has no declarative Kubernetes equivalent - no wait logic is generated", service.Name, dep.Name)
			continue
		default:
			log.Warnf("depends_on condition %q for service %s on %s is not supported - ignoring", dep.Condition, service.Name, dep.Name)
			continue
		}

		depService, ok := allServices[dep.Name]
		if !ok {
			// compose-go rejects dependencies on undefined services while
			// loading; it only lets a missing target through when the
			// dependency is required: false and the target is disabled by
			// profiles.
			if dep.Required {
				log.Warnf("depends_on target %s of service %s is not part of the converted services - ignoring", dep.Name, service.Name)
			} else {
				log.Debugf("depends_on target %s of service %s is not converted, skipping optional wait", dep.Name, service.Name)
			}
			continue
		}

		if len(depService.Port) == 0 {
			log.Warnf("depends_on target %s of service %s has no ports, so no Service is generated for it and there is nothing to probe - skipping wait init container", dep.Name, service.Name)
			continue
		}

		port := probePorts[dep.Name]
		if port == "" {
			log.Warnf("No probe port configured for dependency %s of service %s: set label %s (for example '%s=<port>'). The first port of the dependency is not used by default, because an open port does not prove the dependency is healthy - skipping wait init container", dep.Name, service.Name, compose.LabelDependsOnProbePorts, dep.Name)
			continue
		}

		log.Warnf("depends_on condition %s of service %s on %s is approximated by probing TCP port %s: a connectable port does not prove the dependency container started or its health check passed", dep.Condition, service.Name, dep.Name, port)
		containers = append(containers, kobject.InitContainerConfig{
			Name:    "wait-for-" + dep.Name,
			Image:   image,
			Command: dependsOnWaitCommand(dep.Name, port, dep.Required),
		})
	}
	sortDependsOnInitContainers(containers)
	return containers
}

// fillDependsOnInitContainers appends the init containers generated from
// depends_on to the pod template. It runs after fillInitContainers, so
// user-defined init containers (kompose.init.containers.* labels) always come
// first and win on name collision.
func fillDependsOnInitContainers(template *api.PodTemplateSpec, service kobject.ServiceConfig) {
	for _, container := range service.DependsOnInitContainers {
		duplicate := false
		for _, existing := range template.Spec.InitContainers {
			if existing.Name == container.Name {
				log.Warnf("Init container %q for service %s is already defined through labels, keeping the user-defined one", container.Name, service.Name)
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		template.Spec.InitContainers = append(template.Spec.InitContainers, api.Container{
			Name:    container.Name,
			Image:   container.Image,
			Command: container.Command,
		})
	}
}

// sortDependsOnInitContainers is a helper keeping generated init containers
// in a deterministic order even if DependsOn was built programmatically
// instead of by the (sorting) compose loader.
func sortDependsOnInitContainers(containers []kobject.InitContainerConfig) {
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
}
