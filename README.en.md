# Quoin

**An AI SRE workbench for investigating alerts, querying metrics, and running inspections with traceable evidence.**

[简体中文](README.md) · [Deployment guide](docs/getting-started-kubernetes.md) · [User guide](docs/user-guide.md)

Quoin connects to existing Alertmanager, Prometheus, and Thanos services, bringing alerts, metrics, and AI-assisted analysis into one workbench. Start an investigation from an alert, query metrics through conversation, or run inspections and review reports with evidence references.

The detailed operating guides are currently available in Chinese.

## Features

- **Alert management:** receive Alertmanager alerts, follow state changes, and investigate using metrics.
- **AI SRE:** query connected Prometheus/Thanos metrics through conversation, use alerts and query results to assist troubleshooting, and retain the evidence used in the analysis.
- **Metric observation:** validate and enable metric sources, then inspect scrape targets and their observation state.
- **Inspections and reports:** run instant or range queries, organize inspections by source or business view, and customize check descriptions and report instructions. Collect fresh evidence or reanalyze existing evidence independently.
- **Authentication and auditing:** administrator and operator roles, secondary verification, execution auditing, and backup management.

AI analysis requires a configured and enabled model provider. Quoin uses your existing monitoring data; it does not replace Prometheus or install business workloads and exporters.

## Plugin system

Quoin uses plugins to extend platform integrations, bringing alerts, metric queries, resource observation, and inspections into a shared workbench. Each plugin exposes the capabilities it supports; not every plugin provides every capability.

| Plugin | Status | Capabilities or direction |
| --- | --- | --- |
| Prometheus | Available | Metric queries, scrape target observation, and inspection evidence collection |
| Thanos | Available | Prometheus-compatible queries, target observation, and inspection evidence collection |
| Alertmanager | Available | Alert ingestion and source management |
| Browser | Planned | Browser integration |
| Kubernetes (K8s) | Planned | Cluster resource integration |

Current plugins are built and shipped with Quoin. See [plugin development](docs/plugin-development.md) for the extension model. Planned plugins are not available in the current release; their capabilities will be defined in future versions.

## How it works

```text
Business services / exporters ──→ Prometheus / Thanos
                                           │
                                Metric queries and evidence
                                           │
Alertmanager ──→ Stele ──→ Quoin ←────────→ Plinth
                            │
                       Web workbench
```

The default deployment includes five services:

| Service | Responsibility |
| --- | --- |
| gateway | HTTPS entry point for the frontend, API, and incoming alerts |
| frontend | Web workbench |
| quoin | Application API, storage, authentication, and task management |
| plinth | Runtime for query and analysis tasks |
| stele | Alertmanager webhook ingestion and forwarding |

## Getting started

**Kubernetes is the recommended deployment option.** The repository provides plain Kubernetes YAML manifests; Helm is not required.

1. **Prepare your environment:** a Kubernetes cluster, persistent storage, an HTTPS hostname and certificate, and an SMTP or HTTPS delivery channel for login verification codes.
2. **Build the images:** run `make images` from the repository root. Push the four application images to a registry accessible to the cluster, or load them onto the appropriate nodes.
3. **Configure and deploy:** follow the [Kubernetes deployment guide](docs/getting-started-kubernetes.md) to prepare secrets, set the public address, image references, and storage configuration, and apply the manifests.
4. **Initialize the administrator:** open the web interface and use `admin/admin` to enter initialization. Set a permanent password and verify a contact method. The default password stops working after initialization.
5. **Connect monitoring:** register Plinth and follow the [user guide](docs/user-guide.md) to connect Prometheus/Thanos and Alertmanager. Configure a model provider before using AI analysis.

If Kubernetes is not available in your environment, deploy with Docker Compose instead. See the [Docker Compose installation guide](docs/getting-started.md).

> Restrict access before initialization so that others cannot use the public default credentials. Never commit keys, connection credentials, or verification codes to Git.

## Build and development

Source builds require Go **1.25.8 or a compatible newer version**, as declared in `go.mod`. The frontend uses **pnpm 10.30.3**; Node.js 24 is recommended. Container builds use the toolchains pinned in the Dockerfiles.

```bash
pnpm install --frozen-lockfile
make test
make vet
make web-typecheck web-lint web-test web-build
make images
```

`make images` builds `frontend`, `quoin`, `plinth`, and `stele` without starting services. Default image names are `quoin/<component>:v0.1.0-dev`. See the [image build reference](docs/deployment.md#镜像构建) for tags and build options.

## Documentation

| Document | Contents |
| --- | --- |
| [Kubernetes deployment](docs/getting-started-kubernetes.md) | Recommended deployment, TLS, secrets, and persistence |
| [User guide](docs/user-guide.md) | Sources, alerts, AI SRE, inspections, and reports |
| [Deployment and operations reference](docs/deployment.md) | Service topology, backup, recovery, and offline administration |
| [Docker Compose installation](docs/getting-started.md) | Deployment when Kubernetes is not available |
| [Integration and inspection reference](docs/integration-inspection-guide.md) | Source observation, business views, and inspection semantics |
| [Frontend development](docs/web-development.md) | Frontend development and testing |
| [Plugin development](docs/plugin-development.md) | Built-in plugin contracts |

## Project status

Quoin is under active development, with additional plugin integrations planned. Before production use, evaluate the chosen version and verify access control, certificate trust, verification delivery, and backup and recovery procedures. Assess AI-generated analysis against its underlying evidence.

Report bugs and request features through [GitHub Issues](https://github.com/Suknna/quoin/issues). Include the version, reproduction steps, and redacted logs.
