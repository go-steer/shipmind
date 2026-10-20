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
| [ax](https://github.com/mastersingh24/ax) + [Agent Substrate](https://github.com/mastersingh24/substrate) | Agents as sandboxed tasks that suspend when idle and wake on demand |
| [substrate-scope](https://github.com/go-steer/substrate-scope) | 3D view of every agent on Substrate |

## Status

**Early: Milestone 1 is under way.** Today the repository holds the bill
of materials ([`versions.yaml`](./versions.yaml), what the agent-substrate
demo cluster runs) and the `shipmind` CLI that checks it. Milestone 1
moves that cluster's deployment here, so it can be rebuilt from this
repository, and adds the first nightly end-to-end scenario. See
[`docs/DESIGN.md`](./docs/DESIGN.md) for the design and the milestones,
and [`docs/decisions.md`](./docs/decisions.md) for the decisions so far.

## Environments

Everything here is generic. A deployment's own values (project, hostnames,
registry paths, sizing) live in a private overlay that builds on this
repository at a pinned commit. The agent-substrate demo cluster's overlay
is `go-steer/shipmind-agent-substrate`.

## Try it

```bash
go run ./cmd/shipmind versions   # what a release pins
go run ./cmd/shipmind check      # are the pins and vendored manifests sound
kubectl kustomize deploy/profiles/substrate/ax   # a profile, rendered
```

## Contributing

See [`CONTRIBUTING.md`](./CONTRIBUTING.md). Agents working in this
repository start at [`AGENTS.md`](./AGENTS.md).

## License

[Apache 2.0](./LICENSE).
