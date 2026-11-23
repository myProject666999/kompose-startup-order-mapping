---
layout: default
title: Startup Order, Health Checks and Restart
permalink: /startup-order-mapping/
redirect_from:
  - /docs/startup-order-mapping.md/
  - /docs/startup-order-mapping/
---

# Startup Order, Health Checks and Restart Mapping

* TOC
{:toc}

This document describes how kompose maps the three Compose key groups that
describe "how a service starts, how its health is judged, and what happens
when it dies" — `depends_on`, `healthcheck` and `restart` (including
`deploy.restart_policy`) — to Kubernetes objects, which semantics are lost in
translation, and which warnings are emitted when that happens.

## depends_on

Compose supports three dependency conditions:

| Condition | Compose semantics |
| --------- | ----------------- |
| `service_started` (default) | the depended-on container has been started |
| `service_healthy` | the depended-on container's health check passes |
| `service_completed_successfully` | the depended-on one-shot task exited with code 0 |

Kubernetes has no native startup ordering between workloads. kompose
approximates `service_started` and `service_healthy` with **wait init
containers** on the dependent service: an init container that loops probing a
TCP port of the depended-on service until it accepts connections.

This is only an approximation and is therefore **opt-in**. A connectable port
does not prove that the dependency's container was started, and even less
that its health check passed. kompose emits a warning naming the service, the
dependency and the condition every time it applies this approximation.

`service_completed_successfully` has no reliable declarative equivalent in
Kubernetes (waiting on a Job's completion status would require API access
from the init container; this was considered and is not implemented). kompose
emits a warning and generates **no** wait logic for it.

### Enabling wait init containers

```yaml
services:
  app:
    image: myapp
    labels:
      kompose.depends-on.wait: "true"
      kompose.depends-on.probe-ports: "db=5432,cache=6379"
      # kompose.depends-on.wait-image: "my-registry/busybox:1.36"
    depends_on:
      db:
        condition: service_healthy
      cache:
        condition: service_started
        required: false
```

| Label | Effect |
| ----- | ------ |
| `kompose.depends-on.wait` | Set to `true` to generate wait init containers for this service's `service_started` / `service_healthy` dependencies. Without it, `depends_on` is not converted and a warning is emitted. |
| `kompose.depends-on.probe-ports` | Comma-separated `service=port` pairs selecting the TCP port probed for each dependency. **Required per dependency**: kompose deliberately does not default to the dependency's first port, because an open port does not represent health. A dependency without a configured port is skipped with a warning. |
| `kompose.depends-on.wait-image` | Image of the generated init containers. Default: `busybox:1.36` (only needs `sh` and `nc`). Using the default introduces an external image dependency; pin your own in regulated environments. |

### Behavior details

- **Init container naming and order**: generated containers are named
  `wait-for-<service>` and sorted by depended-on service name, so repeated
  conversions of the same file produce identical output.
- **Coexistence with handwritten init containers**: init containers defined
  through `kompose.init.containers.image` / `.name` / `.command` always come
  **first**; generated ones are appended. On a name collision the handwritten
  container wins and a warning is emitted.
- **`required: false`**: the wait is bounded (30 attempts with 2s sleeps,
  about one minute) and the init container always exits successfully — an
  absent optional dependency never blocks the pod. A required (`required:
  true`, the default) dependency is waited on indefinitely, mirroring how
  Compose blocks the dependent service.
- **Dependency without ports**: no Kubernetes Service is generated for a
  service without ports, so there is nothing to probe. kompose warns and
  skips the wait container instead of generating one that would time out.
- **Missing or cyclic dependencies**: compose-go rejects dependencies on
  undefined services and dependency cycles while loading the compose file, so
  kompose never sees them. The one case that passes loading is `required:
  false` pointing at a service disabled by profiles; it is skipped silently
  (debug log).
- **`depends_on.<service>.restart`**: not supported, reported with a warning.
- **Service groups** (`--service-group-mode`): like label-defined init
  containers, generated wait containers are not applied to grouped services.

### One-shot tasks

A depended-on one-shot task (`service_completed_successfully`) generates a
Pod or CronJob according to the decision matrix below, exactly like any other
service with `restart: no` / `on-failure`. Generating a Kubernetes Job for
one-shot tasks was considered and deferred: since no wait logic is generated
for `service_completed_successfully` anyway, the workload kind of the
dependency does not affect any generated dependency wiring.

## healthcheck

Compose health checks become **liveness probes**. Readiness probes are only
created from the `kompose.service.healthcheck.readiness.*` labels. Compose
has no readiness/liveness distinction; deliberately, the same command is
*not* reused for both probes, because a shared command would make every
health check failure both remove the pod from traffic and restart it.

### test directive handling

The first element of `healthcheck.test` is a directive, not part of the
command:

| Compose | Kubernetes exec probe |
| ------- | --------------------- |
| `test: ["CMD", "cmd", "arg"]` | `command: ["cmd", "arg"]` |
| `test: ["CMD-SHELL", "a \| b"]` or `test: a \| b` | `command: ["sh", "-c", "a \| b"]` |
| `test: ["NONE"]` | health check disabled, no probe |
| `test: []` | no probe (previously panicked) |

`CMD-SHELL` is wrapped in `sh -c`: previously the whole shell string became a
single argv element, which cannot be executed.

### Field mapping

| Compose | Kubernetes probe field |
| ------- | ---------------------- |
| `interval` | `periodSeconds` |
| `timeout` | `timeoutSeconds` |
| `retries` | `failureThreshold` |
| `start_period` | **startup probe** (see below) |

`start_period` was previously mapped to the liveness probe's
`initialDelaySeconds`. The semantics differ: Docker tolerates health check
failures during `start_period`, while Kubernetes does not probe at all during
`initialDelaySeconds`. kompose now generates a **startup probe** with the
same handler, `periodSeconds` = `interval` (default 10s) and
`failureThreshold` = ⌈`start_period` / `periodSeconds`⌉, and the liveness
probe no longer carries an initial delay. This changes existing output; set
the label `kompose.service.healthcheck.liveness.startup_probe.disable:
"true"` to restore the legacy `initialDelaySeconds` mapping.

A health check without any usable command (no `test`, no
`http_get`/`tcp_port` labels) aborts the conversion with an error naming the
service, instead of panicking.

## restart

| Compose `restart` | Pod `restartPolicy` | Notes |
| ----------------- | ------------------- | ----- |
| `""` (default), `always` | `Always` | |
| `no` | `Never` | |
| `on-failure` | `OnFailure` | |
| `on-failure:N` | `OnFailure` | `N` maps to `backoffLimit` when the service becomes a CronJob; on Pods and controllers it has no equivalent and a warning is emitted |
| `unless-stopped` | `Always` | warning emitted (unchanged behavior) |

`deploy.restart_policy.condition` overrides `restart` (unchanged).
`deploy.restart_policy.max_attempts`, `.delay` and `.window` have no
Kubernetes equivalent; each is reported as ignored with a warning naming the
key. When `restart: always` is combined with the cronjob labels, the loader
converts it to `on-failure` (unchanged behavior).

## Workload type decision matrix

The workload kind is decided by four inputs: the `restart` value, the
`kompose.cronjob.schedule` label, an explicitly requested controller
(`--controller` flag or `kompose.controller.type` label), and service
grouping (`--service-group-mode`). Every cell of the matrix is verified by
`TestWorkloadDecisionMatrix`.

| restart | cronjob schedule | controller | grouped | workload | restartPolicy |
| ------- | ---------------- | ---------- | ------- | -------- | ------------- |
| '' (default) | no | none | no | Deployment | Always |
| '' (default) | no | none | yes | Deployment | Always |
| '' (default) | no | deployment | no | Deployment | Always |
| '' (default) | no | deployment | yes | Deployment | Always |
| '' (default) | no | daemonset | no | DaemonSet | Always |
| '' (default) | no | daemonset | yes | DaemonSet | Always |
| '' (default) | no | statefulset | no | StatefulSet | Always |
| '' (default) | no | statefulset | yes | StatefulSet | Always |
| '' (default) | yes | none | no | Deployment | Always |
| '' (default) | yes | none | yes | Deployment | Always |
| '' (default) | yes | deployment | no | Deployment | Always |
| '' (default) | yes | deployment | yes | Deployment | Always |
| '' (default) | yes | daemonset | no | DaemonSet | Always |
| '' (default) | yes | daemonset | yes | DaemonSet | Always |
| '' (default) | yes | statefulset | no | StatefulSet | Always |
| '' (default) | yes | statefulset | yes | StatefulSet | Always |
| always | no | none | no | Deployment | Always |
| always | no | none | yes | Deployment | Always |
| always | no | deployment | no | Deployment | Always |
| always | no | deployment | yes | Deployment | Always |
| always | no | daemonset | no | DaemonSet | Always |
| always | no | daemonset | yes | DaemonSet | Always |
| always | no | statefulset | no | StatefulSet | Always |
| always | no | statefulset | yes | StatefulSet | Always |
| always | yes | none | no | Deployment | Always |
| always | yes | none | yes | Deployment | Always |
| always | yes | deployment | no | Deployment | Always |
| always | yes | deployment | yes | Deployment | Always |
| always | yes | daemonset | no | DaemonSet | Always |
| always | yes | daemonset | yes | DaemonSet | Always |
| always | yes | statefulset | no | StatefulSet | Always |
| always | yes | statefulset | yes | StatefulSet | Always |
| no | no | none | no | Pod | Never |
| no | no | none | yes | Deployment | Never |
| no | no | deployment | no | Deployment | Never |
| no | no | deployment | yes | Deployment | Never |
| no | no | daemonset | no | DaemonSet | Never |
| no | no | daemonset | yes | DaemonSet | Never |
| no | no | statefulset | no | StatefulSet | Never |
| no | no | statefulset | yes | StatefulSet | Never |
| no | yes | none | no | CronJob | Never |
| no | yes | none | yes | Deployment | Never |
| no | yes | deployment | no | Deployment | Never |
| no | yes | deployment | yes | Deployment | Never |
| no | yes | daemonset | no | DaemonSet | Never |
| no | yes | daemonset | yes | DaemonSet | Never |
| no | yes | statefulset | no | StatefulSet | Never |
| no | yes | statefulset | yes | StatefulSet | Never |
| on-failure | no | none | no | Pod | OnFailure |
| on-failure | no | none | yes | Deployment | OnFailure |
| on-failure | no | deployment | no | Deployment | OnFailure |
| on-failure | no | deployment | yes | Deployment | OnFailure |
| on-failure | no | daemonset | no | DaemonSet | OnFailure |
| on-failure | no | daemonset | yes | DaemonSet | OnFailure |
| on-failure | no | statefulset | no | StatefulSet | OnFailure |
| on-failure | no | statefulset | yes | StatefulSet | OnFailure |
| on-failure | yes | none | no | CronJob | OnFailure |
| on-failure | yes | none | yes | Deployment | OnFailure |
| on-failure | yes | deployment | no | Deployment | OnFailure |
| on-failure | yes | deployment | yes | Deployment | OnFailure |
| on-failure | yes | daemonset | no | DaemonSet | OnFailure |
| on-failure | yes | daemonset | yes | DaemonSet | OnFailure |
| on-failure | yes | statefulset | no | StatefulSet | OnFailure |
| on-failure | yes | statefulset | yes | StatefulSet | OnFailure |

Rules of thumb:

- `restart: no` / `on-failure` **without** a requested controller yields a
  bare Pod, or a CronJob when `kompose.cronjob.schedule` is set. Bare Pods
  get no rolling updates and no self-healing — request a controller
  explicitly if you need them.
- Grouped services always take the controller branch; the restart-based
  Pod/CronJob branch is skipped.
- The `kompose.cronjob.schedule` label only takes effect on the
  Pod/CronJob branch; with a controller or a long-running restart policy it
  is ignored.
- Deployments, DaemonSets and StatefulSets only support `restartPolicy:
  Always` in Kubernetes. kompose still writes the mapped value (e.g.
  `Never`) when `restart: no` is combined with a controller — such a
  manifest is rejected by the API server. This is existing behavior and is
  kept for compatibility; avoid the combination.

## Warnings

All degradation warnings go through `log.Warnf` and include the service name
and the Compose key involved, for example:

```
WARN depends_on condition service_healthy of service app on db is approximated by probing TCP port 5432: ...
WARN deploy.restart_policy.max_attempts in service app is not supported - ignoring
```
