# shipmind

The go-steer agent platform, assembled: every component pinned to a
tested version, deployed as one system, and tested end to end. The first
agent it ships is an SRE agent for Kubernetes: lookout notices something
wrong in a cluster, an agent diagnoses it, and people approve anything
that changes the cluster.

shipmind is a distribution. The components live in their own
repositories; this one decides which versions run together and proves
that they do.

| Component | What it does |
|---|---|
| [mast](https://github.com/go-steer/mast) | Autonomous agent harness |
| [core-agent](https://github.com/go-steer/core-agent) | Interactive and autonomous agent harness |
| [mast-web](https://github.com/go-steer/mast-web) | Web UI for agent sessions and approvals |
| [switchboard](https://github.com/go-steer/switchboard) | Slack and Google Chat integration |
| [k8s-lookout](https://github.com/go-steer/k8s-lookout) | Kubernetes event detection and diagnostics |
| [ax](https://github.com/mastersingh24/ax) + [Agent Substrate](https://github.com/agent-substrate/substrate) | Agents as sandboxed tasks that suspend when idle and wake on demand |
| [substrate-scope](https://github.com/go-steer/substrate-scope) | 3D view of every agent on Substrate |

## Status

**Milestone 1 is done.** The agent-substrate demo cluster deploys from
this repository: every component pinned in
[`versions.yaml`](./versions.yaml) and built by its CI, the substrate
profile rendered and applied with `shipmind up`, `shipmind status`
reporting no drift, and an end-to-end scenario running nightly in the
cluster. Milestone 2 adds the standalone profile and kind tests on every
PR. See [`docs/DESIGN.md`](./docs/DESIGN.md) for the design and the
milestones, and [`docs/decisions.md`](./docs/decisions.md) for the
decisions so far.

## Environments

Everything here is generic. A deployment's own values (project, hostnames,
registry paths, sizing) live in a private overlay that builds on this
repository at a pinned commit. The agent-substrate demo cluster's overlay
is `go-steer/shipmind-deploy`.

## Try it

```bash
go run ./cmd/shipmind versions   # what a release pins
go run ./cmd/shipmind check      # are the pins and vendored manifests sound
kubectl kustomize deploy/profiles/substrate      # a profile, rendered
go run ./cmd/shipmind status <env-dir>          # an environment against its cluster (read only)
go run ./cmd/shipmind slack-app <env-dir>       # the environment's Slack app manifest
go run ./cmd/shipmind scenario run -h           # an end-to-end scenario against a deployment
```

## Contributing

See [`CONTRIBUTING.md`](./CONTRIBUTING.md). Agents working in this
repository start at [`AGENTS.md`](./AGENTS.md).

## License

[Apache 2.0](./LICENSE).
