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
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubernetes/kompose/pkg/kobject"
	"github.com/kubernetes/kompose/pkg/loader/compose"
	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// captureWarnings runs fn and returns the warning lines it logged.
func captureWarnings(t *testing.T, fn func()) []string {
	t.Helper()
	var buf bytes.Buffer
	logger := log.StandardLogger()
	originalOut := logger.Out
	logger.SetOutput(&buf)
	defer logger.SetOutput(originalOut)

	fn()

	var warnings []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(line, "level=warning") {
			warnings = append(warnings, line)
		}
	}
	return warnings
}

func countWarningsContaining(warnings []string, substr string) int {
	count := 0
	for _, warning := range warnings {
		if strings.Contains(warning, substr) {
			count++
		}
	}
	return count
}

// loadComposeForTransform loads a compose file from a temp dir through the
// compose loader, the same path the CLI uses.
func loadComposeForTransform(t *testing.T, content string) kobject.KomposeObject {
	t.Helper()
	composeFile := filepath.Join(t.TempDir(), "docker-compose.yaml")
	if err := os.WriteFile(composeFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	c := compose.Compose{}
	komposeObject, err := c.LoadFile([]string{composeFile}, nil, true)
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	return komposeObject
}

type workloadInfo struct {
	kind          string
	restartPolicy api.RestartPolicy
}

// findWorkload returns the kind and restart policy of the first workload
// object (Pod, CronJob or controller) in the converted objects.
func findWorkload(objs []runtime.Object) (workloadInfo, bool) {
	for _, obj := range objs {
		switch o := obj.(type) {
		case *api.Pod:
			return workloadInfo{"Pod", o.Spec.RestartPolicy}, true
		case *appsv1.Deployment:
			return workloadInfo{"Deployment", o.Spec.Template.Spec.RestartPolicy}, true
		case *appsv1.DaemonSet:
			return workloadInfo{"DaemonSet", o.Spec.Template.Spec.RestartPolicy}, true
		case *appsv1.StatefulSet:
			return workloadInfo{"StatefulSet", o.Spec.Template.Spec.RestartPolicy}, true
		case *batchv1.CronJob:
			return workloadInfo{"CronJob", o.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy}, true
		}
	}
	return workloadInfo{}, false
}

func findDeployment(t *testing.T, objs []runtime.Object, name string) *appsv1.Deployment {
	t.Helper()
	for _, obj := range objs {
		if deployment, ok := obj.(*appsv1.Deployment); ok && deployment.Name == name {
			return deployment
		}
	}
	t.Fatalf("Deployment %q not found", name)
	return nil
}

// TestWorkloadDecisionMatrix verifies the workload type decision table cell
// by cell. Dimensions: restart value, kompose.cronjob.schedule label,
// controller selection (--controller / kompose.controller.type behave the
// same here) and service grouping. The full matrix is documented in
// docs/startup-order-mapping.md.
func TestWorkloadDecisionMatrix(t *testing.T) {
	testCases := map[string]struct {
		restart         string
		cronJobSchedule string
		controller      string
		group           bool
		expectedKind    string
		expectedRestart api.RestartPolicy
	}{
		// no controller, no cronjob label, no group
		"default restart, no controller":    {"", "", "", false, "Deployment", api.RestartPolicyAlways},
		"restart always, no controller":     {"always", "", "", false, "Deployment", api.RestartPolicyAlways},
		"restart no, no controller":         {"no", "", "", false, "Pod", api.RestartPolicyNever},
		"restart on-failure, no controller": {"on-failure", "", "", false, "Pod", api.RestartPolicyOnFailure},
		// no controller, cronjob label set, no group
		"default restart, cronjob label":    {"", "0 * * * *", "", false, "Deployment", api.RestartPolicyAlways},
		"restart always, cronjob label":     {"always", "0 * * * *", "", false, "Deployment", api.RestartPolicyAlways},
		"restart no, cronjob label":         {"no", "0 * * * *", "", false, "CronJob", api.RestartPolicyNever},
		"restart on-failure, cronjob label": {"on-failure", "0 * * * *", "", false, "CronJob", api.RestartPolicyOnFailure},
		// explicit controller, no group
		"restart no, deployment controller":                {"no", "", "deployment", false, "Deployment", api.RestartPolicyNever},
		"restart on-failure, deployment":                   {"on-failure", "", "deployment", false, "Deployment", api.RestartPolicyOnFailure},
		"restart always, daemonset controller":             {"always", "", "daemonset", false, "DaemonSet", api.RestartPolicyAlways},
		"restart no, daemonset controller":                 {"no", "", "daemonset", false, "DaemonSet", api.RestartPolicyNever},
		"restart always, statefulset":                      {"always", "", "statefulset", false, "StatefulSet", api.RestartPolicyAlways},
		"restart on-failure, statefulset":                  {"on-failure", "", "statefulset", false, "StatefulSet", api.RestartPolicyOnFailure},
		"restart no, cronjob label, deployment controller": {"no", "0 * * * *", "deployment", false, "Deployment", api.RestartPolicyNever},
		// grouped services ignore the restart-based pod/cronjob branch
		"restart no, grouped":                           {"no", "", "", true, "Deployment", api.RestartPolicyNever},
		"restart on-failure, grouped":                   {"on-failure", "", "", true, "Deployment", api.RestartPolicyOnFailure},
		"restart always, grouped":                       {"always", "", "", true, "Deployment", api.RestartPolicyAlways},
		"restart no, cronjob label, grouped":            {"no", "0 * * * *", "", true, "Deployment", api.RestartPolicyNever},
		"restart always, grouped, daemonset controller": {"always", "", "daemonset", true, "DaemonSet", api.RestartPolicyAlways},
	}

	for name, test := range testCases {
		t.Log("Test case:", name)
		service := kobject.ServiceConfig{
			Name:            "app",
			Image:           "image",
			Restart:         test.restart,
			CronJobSchedule: test.cronJobSchedule,
		}
		// emulate the CLI default: CreateD is only set when no controller
		// was requested (see validateControllers in pkg/app)
		opt := kobject.ConvertOptions{CreateD: test.controller == "", Controller: test.controller}
		if test.group {
			service.Labels = map[string]string{compose.LabelServiceGroup: "appgroup"}
			opt.ServiceGroupMode = "label"
		}
		komposeObject := kobject.KomposeObject{
			ServiceConfigs: map[string]kobject.ServiceConfig{"app": service},
		}

		k := Kubernetes{}
		objs, err := k.Transform(komposeObject, opt)
		if err != nil {
			t.Errorf("%s: k.Transform failed: %v", name, err)
			continue
		}
		workload, found := findWorkload(objs)
		if !found {
			t.Errorf("%s: no workload object generated", name)
			continue
		}
		if workload.kind != test.expectedKind {
			t.Errorf("%s: expected workload %s, got %s", name, test.expectedKind, workload.kind)
		}
		if workload.restartPolicy != test.expectedRestart {
			t.Errorf("%s: expected restartPolicy %s, got %s", name, test.expectedRestart, workload.restartPolicy)
		}
	}
}

func TestDependsOnWaitInitContainers(t *testing.T) {
	komposeObject := loadComposeForTransform(t, `
services:
  app:
    image: nginx
    ports:
      - "8080:80"
    labels:
      kompose.depends-on.wait: "true"
      kompose.depends-on.probe-ports: "db=5432,cache=6379"
    depends_on:
      db:
        condition: service_healthy
      cache:
        condition: service_started
        required: false
      mig:
        condition: service_completed_successfully
  db:
    image: postgres
    ports:
      - "5432:5432"
  cache:
    image: redis
    ports:
      - "6379:6379"
  mig:
    image: migrate
`)

	var objs []runtime.Object
	var err error
	warnings := captureWarnings(t, func() {
		k := Kubernetes{}
		objs, err = k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}

	// 2 approximations (service_started, service_healthy) + 1 skipped
	// service_completed_successfully
	if count := countWarningsContaining(warnings, "depends_on"); count != 3 {
		t.Errorf("Expected 3 depends_on warnings, got %d: %v", count, warnings)
	}
	if count := countWarningsContaining(warnings, "service_completed_successfully"); count != 1 {
		t.Errorf("Expected 1 service_completed_successfully warning, got %d: %v", count, warnings)
	}

	deployment := findDeployment(t, objs, "app")
	initContainers := deployment.Spec.Template.Spec.InitContainers
	if len(initContainers) != 2 {
		t.Fatalf("Expected 2 init containers, got %d: %v", len(initContainers), initContainers)
	}
	// init containers are sorted by depended-on service name
	if initContainers[0].Name != "wait-for-cache" || initContainers[1].Name != "wait-for-db" {
		t.Errorf("Expected init containers [wait-for-cache wait-for-db], got [%s %s]", initContainers[0].Name, initContainers[1].Name)
	}
	for _, container := range initContainers {
		if container.Image != DefaultDependsOnWaitImage {
			t.Errorf("Expected default wait image %q, got %q", DefaultDependsOnWaitImage, container.Image)
		}
	}
	// required dependency: waits until the port is connectable
	if got := strings.Join(initContainers[1].Command, " "); !strings.Contains(got, "until nc -z -w 2 db 5432") {
		t.Errorf("Unexpected wait command for db: %v", initContainers[1].Command)
	}
	// optional dependency (required: false): bounded wait, never blocks
	if got := strings.Join(initContainers[0].Command, " "); !strings.Contains(got, "continuing anyway") {
		t.Errorf("Expected bounded wait command for optional dependency cache, got: %v", initContainers[0].Command)
	}

	// the conversion must be deterministic: converting again yields the
	// same init container order
	k := Kubernetes{}
	objs2, err := k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}
	deployment2 := findDeployment(t, objs2, "app")
	initContainers2 := deployment2.Spec.Template.Spec.InitContainers
	if len(initContainers2) != 2 || initContainers2[0].Name != "wait-for-cache" || initContainers2[1].Name != "wait-for-db" {
		t.Errorf("Init container order is not deterministic: %v", initContainers2)
	}
}

func TestDependsOnNotEnabled(t *testing.T) {
	komposeObject := loadComposeForTransform(t, `
services:
  app:
    image: nginx
    depends_on:
      - db
  db:
    image: postgres
    ports:
      - "5432:5432"
`)

	var objs []runtime.Object
	var err error
	warnings := captureWarnings(t, func() {
		k := Kubernetes{}
		objs, err = k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}

	if count := countWarningsContaining(warnings, "depends_on of service app is not converted"); count != 1 {
		t.Errorf("Expected 1 warning about depends_on not being converted, got %d: %v", count, warnings)
	}
	deployment := findDeployment(t, objs, "app")
	if len(deployment.Spec.Template.Spec.InitContainers) != 0 {
		t.Errorf("Expected no init containers without the opt-in label, got %v", deployment.Spec.Template.Spec.InitContainers)
	}
}

func TestDependsOnTargetWithoutPorts(t *testing.T) {
	komposeObject := loadComposeForTransform(t, `
services:
  app:
    image: nginx
    labels:
      kompose.depends-on.wait: "true"
      kompose.depends-on.probe-ports: "db=5432"
    depends_on:
      - db
  db:
    image: postgres
`)

	var objs []runtime.Object
	var err error
	warnings := captureWarnings(t, func() {
		k := Kubernetes{}
		objs, err = k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}

	if count := countWarningsContaining(warnings, "has no ports"); count != 1 {
		t.Errorf("Expected 1 warning about the dependency having no ports, got %d: %v", count, warnings)
	}
	deployment := findDeployment(t, objs, "app")
	if len(deployment.Spec.Template.Spec.InitContainers) != 0 {
		t.Errorf("Expected no init containers for a port-less dependency, got %v", deployment.Spec.Template.Spec.InitContainers)
	}
}

func TestDependsOnNoProbePortConfigured(t *testing.T) {
	komposeObject := loadComposeFileForTransformNoProbePort(t)

	var objs []runtime.Object
	var err error
	warnings := captureWarnings(t, func() {
		k := Kubernetes{}
		objs, err = k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}

	if count := countWarningsContaining(warnings, "No probe port configured"); count != 1 {
		t.Errorf("Expected 1 warning about the missing probe port, got %d: %v", count, warnings)
	}
	deployment := findDeployment(t, objs, "app")
	if len(deployment.Spec.Template.Spec.InitContainers) != 0 {
		t.Errorf("Expected no init containers without a configured probe port, got %v", deployment.Spec.Template.Spec.InitContainers)
	}
}

func loadComposeFileForTransformNoProbePort(t *testing.T) kobject.KomposeObject {
	t.Helper()
	return loadComposeForTransform(t, `
services:
  app:
    image: nginx
    labels:
      kompose.depends-on.wait: "true"
    depends_on:
      - db
  db:
    image: postgres
    ports:
      - "5432:5432"
`)
}

func TestDependsOnHandwrittenInitContainerCoexist(t *testing.T) {
	komposeObject := loadComposeForTransform(t, `
services:
  app:
    image: nginx
    labels:
      kompose.init.containers.image: "custom-init:1"
      kompose.init.containers.name: "my-init"
      kompose.depends-on.wait: "true"
      kompose.depends-on.probe-ports: "db=5432"
    depends_on:
      - db
  db:
    image: postgres
    ports:
      - "5432:5432"
`)

	k := Kubernetes{}
	objs, err := k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}
	deployment := findDeployment(t, objs, "app")
	initContainers := deployment.Spec.Template.Spec.InitContainers
	if len(initContainers) != 2 {
		t.Fatalf("Expected 2 init containers, got %d", len(initContainers))
	}
	// the handwritten init container comes first, the generated one is appended
	if initContainers[0].Name != "my-init" || initContainers[0].Image != "custom-init:1" {
		t.Errorf("Expected handwritten init container first, got %+v", initContainers[0])
	}
	if initContainers[1].Name != "wait-for-db" {
		t.Errorf("Expected generated init container second, got %+v", initContainers[1])
	}
}

func TestDependsOnHandwrittenInitContainerNameCollision(t *testing.T) {
	komposeObject := loadComposeForTransform(t, `
services:
  app:
    image: nginx
    labels:
      kompose.init.containers.image: "custom-init:1"
      kompose.init.containers.name: "wait-for-db"
      kompose.depends-on.wait: "true"
      kompose.depends-on.probe-ports: "db=5432"
    depends_on:
      - db
  db:
    image: postgres
    ports:
      - "5432:5432"
`)

	var objs []runtime.Object
	var err error
	warnings := captureWarnings(t, func() {
		k := Kubernetes{}
		objs, err = k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}

	deployment := findDeployment(t, objs, "app")
	initContainers := deployment.Spec.Template.Spec.InitContainers
	// on name collision the handwritten init container wins
	if len(initContainers) != 1 {
		t.Fatalf("Expected 1 init container, got %d", len(initContainers))
	}
	if initContainers[0].Name != "wait-for-db" || initContainers[0].Image != "custom-init:1" {
		t.Errorf("Expected the handwritten init container to win, got %+v", initContainers[0])
	}
	if count := countWarningsContaining(warnings, "already defined through labels"); count != 1 {
		t.Errorf("Expected 1 warning about the name collision, got %d: %v", count, warnings)
	}
}

func TestHealthCheckCmdShellProbe(t *testing.T) {
	komposeObject := loadComposeForTransform(t, `
services:
  app:
    image: nginx
    healthcheck:
      test: curl -f http://localhost || exit 1
      interval: 30s
      timeout: 5s
      retries: 3
`)

	k := Kubernetes{}
	objs, err := k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}
	deployment := findDeployment(t, objs, "app")
	probe := deployment.Spec.Template.Spec.Containers[0].LivenessProbe
	if probe == nil || probe.Exec == nil {
		t.Fatalf("Expected an exec liveness probe, got %+v", probe)
	}
	// CMD-SHELL must be wrapped in sh -c, otherwise the whole string would
	// be executed as a single argv element
	expectedCommand := []string{"sh", "-c", "curl -f http://localhost || exit 1"}
	if strings.Join(probe.Exec.Command, "\x00") != strings.Join(expectedCommand, "\x00") {
		t.Errorf("Expected probe command %v, got %v", expectedCommand, probe.Exec.Command)
	}
	if probe.PeriodSeconds != 30 || probe.TimeoutSeconds != 5 || probe.FailureThreshold != 3 {
		t.Errorf("Unexpected probe timings: period=%d timeout=%d failureThreshold=%d",
			probe.PeriodSeconds, probe.TimeoutSeconds, probe.FailureThreshold)
	}
}

func TestHealthCheckNoneAndEmptyTest(t *testing.T) {
	komposeObject := loadComposeForTransform(t, `
services:
  nonesvc:
    image: nginx
    healthcheck:
      test: ["NONE"]
  emptysvc:
    image: nginx
    healthcheck:
      test: []
`)

	k := Kubernetes{}
	objs, err := k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}
	for _, name := range []string{"nonesvc", "emptysvc"} {
		deployment := findDeployment(t, objs, name)
		container := deployment.Spec.Template.Spec.Containers[0]
		if container.LivenessProbe != nil || container.StartupProbe != nil {
			t.Errorf("Expected no probes for service %s, got liveness=%+v startup=%+v", name, container.LivenessProbe, container.StartupProbe)
		}
	}
}

func TestHealthCheckWithoutCommandReturnsError(t *testing.T) {
	komposeObject := loadComposeForTransform(t, `
services:
  app:
    image: nginx
    healthcheck:
      interval: 10s
`)

	k := Kubernetes{}
	_, err := k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	if err == nil {
		t.Fatal("Expected an error for a health check without a command, got nil")
	}
	// the error must name the service instead of being a panic
	if !strings.Contains(err.Error(), "app") {
		t.Errorf("Expected the error to contain the service name, got: %v", err)
	}
}

func TestHealthCheckStartPeriodGeneratesStartupProbe(t *testing.T) {
	komposeObject := loadComposeForTransform(t, `
services:
  app:
    image: nginx
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost"]
      interval: 10s
      start_period: 45s
`)

	k := Kubernetes{}
	objs, err := k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}
	deployment := findDeployment(t, objs, "app")
	container := deployment.Spec.Template.Spec.Containers[0]

	if container.StartupProbe == nil {
		t.Fatal("Expected a startup probe for a health check with start_period")
	}
	// the startup probe covers start_period: failureThreshold * period >= 45s
	startup := container.StartupProbe
	if startup.PeriodSeconds != 10 || startup.FailureThreshold != 5 {
		t.Errorf("Expected startup probe period=10 failureThreshold=5, got period=%d failureThreshold=%d",
			startup.PeriodSeconds, startup.FailureThreshold)
	}
	// the liveness probe no longer carries the start period as initial delay
	if container.LivenessProbe == nil {
		t.Fatal("Expected a liveness probe")
	}
	if container.LivenessProbe.InitialDelaySeconds != 0 {
		t.Errorf("Expected liveness initialDelaySeconds 0 when a startup probe exists, got %d",
			container.LivenessProbe.InitialDelaySeconds)
	}
}

func TestHealthCheckStartPeriodLegacyMapping(t *testing.T) {
	komposeObject := loadComposeForTransform(t, `
services:
  app:
    image: nginx
    labels:
      kompose.service.healthcheck.liveness.startup_probe.disable: "true"
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost"]
      interval: 10s
      start_period: 45s
`)

	k := Kubernetes{}
	objs, err := k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}
	deployment := findDeployment(t, objs, "app")
	container := deployment.Spec.Template.Spec.Containers[0]

	if container.StartupProbe != nil {
		t.Errorf("Expected no startup probe with the disable label, got %+v", container.StartupProbe)
	}
	// legacy behavior: start_period mapped to the liveness initial delay
	if container.LivenessProbe == nil || container.LivenessProbe.InitialDelaySeconds != 45 {
		t.Errorf("Expected liveness initialDelaySeconds 45, got %+v", container.LivenessProbe)
	}
}

func TestRestartOnFailureRetriesCronJob(t *testing.T) {
	komposeObject := loadComposeForTransform(t, `
services:
  app:
    image: nginx
    restart: "on-failure:5"
    labels:
      kompose.cronjob.schedule: "0 * * * *"
`)

	k := Kubernetes{}
	objs, err := k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}
	for _, obj := range objs {
		if cronJob, ok := obj.(*batchv1.CronJob); ok {
			if cronJob.Spec.JobTemplate.Spec.BackoffLimit == nil || *cronJob.Spec.JobTemplate.Spec.BackoffLimit != 5 {
				t.Errorf("Expected backoffLimit 5, got %v", cronJob.Spec.JobTemplate.Spec.BackoffLimit)
			}
			if cronJob.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy != api.RestartPolicyOnFailure {
				t.Errorf("Expected restartPolicy OnFailure, got %s", cronJob.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy)
			}
			return
		}
	}
	t.Fatal("Expected a CronJob object")
}

func TestRestartOnFailureRetriesBarePod(t *testing.T) {
	komposeObject := loadComposeForTransform(t, `
services:
  app:
    image: nginx
    restart: "on-failure:5"
`)

	var objs []runtime.Object
	var err error
	warnings := captureWarnings(t, func() {
		k := Kubernetes{}
		objs, err = k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}

	workload, found := findWorkload(objs)
	if !found || workload.kind != "Pod" {
		t.Fatalf("Expected a Pod, got %v", workload.kind)
	}
	if workload.restartPolicy != api.RestartPolicyOnFailure {
		t.Errorf("Expected restartPolicy OnFailure, got %s", workload.restartPolicy)
	}
	if count := countWarningsContaining(warnings, "no Kubernetes equivalent on a bare Pod"); count != 1 {
		t.Errorf("Expected 1 warning about the ignored retry count, got %d: %v", count, warnings)
	}
}

func TestRestartOnFailureRetriesDeployment(t *testing.T) {
	komposeObject := loadComposeForTransform(t, `
services:
  app:
    image: nginx
    restart: "on-failure:5"
`)

	var objs []runtime.Object
	var err error
	warnings := captureWarnings(t, func() {
		k := Kubernetes{}
		objs, err = k.Transform(komposeObject, kobject.ConvertOptions{Controller: "deployment"})
	})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}

	workload, found := findWorkload(objs)
	if !found || workload.kind != "Deployment" {
		t.Fatalf("Expected a Deployment, got %v", workload.kind)
	}
	if count := countWarningsContaining(warnings, "only supported for cronjob workloads"); count != 1 {
		t.Errorf("Expected 1 warning about the ignored retry count, got %d: %v", count, warnings)
	}
}

func TestRestartUnlessStopped(t *testing.T) {
	var komposeObject kobject.KomposeObject
	warnings := captureWarnings(t, func() {
		komposeObject = loadComposeForTransform(t, `
services:
  app:
    image: nginx
    restart: unless-stopped
`)
	})

	if count := countWarningsContaining(warnings, "unless-stopped"); count != 1 {
		t.Errorf("Expected 1 warning about unless-stopped, got %d: %v", count, warnings)
	}

	k := Kubernetes{}
	objs, err := k.Transform(komposeObject, kobject.ConvertOptions{CreateD: true})
	if err != nil {
		t.Fatalf("k.Transform failed: %v", err)
	}
	workload, found := findWorkload(objs)
	if !found || workload.kind != "Deployment" || workload.restartPolicy != api.RestartPolicyAlways {
		t.Errorf("Expected Deployment with restartPolicy Always, got %v", workload)
	}
}

func TestDeployRestartPolicyIgnoredKeys(t *testing.T) {
	var komposeObject kobject.KomposeObject
	warnings := captureWarnings(t, func() {
		komposeObject = loadComposeForTransform(t, `
services:
  app:
    image: nginx
    deploy:
      restart_policy:
        condition: on-failure
        delay: 5s
        max_attempts: 3
        window: 120s
`)
	})

	for _, key := range []string{"deploy.restart_policy.delay", "deploy.restart_policy.max_attempts", "deploy.restart_policy.window"} {
		if count := countWarningsContaining(warnings, key); count != 1 {
			t.Errorf("Expected 1 warning about %s being ignored, got %d: %v", key, count, warnings)
		}
	}
	if komposeObject.ServiceConfigs["app"].Restart != "on-failure" {
		t.Errorf("Expected restart on-failure from deploy.restart_policy.condition, got %q",
			komposeObject.ServiceConfigs["app"].Restart)
	}
}
