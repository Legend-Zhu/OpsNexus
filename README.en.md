# OpsGaurd — Intelligent Ops Automation Platform

OpsGaurd unifies management of multiple **network-isolated** Docker Swarm clusters from a single control plane, covering container orchestration, monitoring & alerting, AI patrol inspections, AI troubleshooting, MLOps, and image distribution.

The platform consists of a management server (single Go binary + Vue3 SPA) and per-cluster Workers. It relies only on a one-way "management → cluster Worker" gRPC path — clusters never need to reach back to the management side, so isolated networks require no reverse firewall holes.

## Key Features

| Domain | Description |
| --- | --- |
| Container orchestration | Service deploy / update / scale / restart / remove, async operation readiness, rolling updates |
| Monitoring & alerting | Port / HTTP / log / resource periodic probes, event aggregation into alerts, rules management, auto notify (Feishu / webhook / SMS) |
| AI patrol inspections | YAML inspection flows + cron scheduling + AI inspection reports + report delivery + auto alert on anomalies |
| AI troubleshooting | Embedded AiNexus gateway (OpenAI / Anthropic compatible endpoints), ReAct agent + MCP tools that operate clusters directly |
| MLOps | Prompt Hub (versioned prompts), model onboarding & toggle, call-level usage metering & cost reports, monthly budgets |
| Image registry | Embedded OCI registry (/v2 protocol); cluster docker daemons pull/push through a gRPC tunnel relay; in-page zip upload & build |
| Identity | Local users + SSO (OIDC RP); OpsGaurd also acts as an OIDC IdP issuing tokens to in-cluster systems (e.g. r-nacos) |
| Inventory management | List-based registration & periodic probing of non-swarm standalone containers / host services |
| Audit & security | Sensitive-op audit trail, command black/white lists, log redaction, Bearer tokens + optional mTLS |

## Design Principles

1. **One-way network policy** — all communication relies on the "management → Worker" one-way path. Event delivery (`SubscribeEvents` / `SubscribeAudit` bidi streams) and in-cluster access to the management IdP / registry (`Tunnel` reverse tunnel) are all driven by the server.
2. **Zero external dependencies** — no MySQL / Redis / message queue. The management side uses embedded LevelDB, Workers use pure-Go SQLite. Deploying OpsGaurd needs nothing but Docker.
3. **Offline-first** — target environments are air-gapped intranets; installers, images, even proto codegen (`genpatch`) have offline paths. See `deploy/offline/` runbook.
4. **Single-binary management server** — the server process hosts REST API, embedded AI gateway, embedded OCI registry, embedded OIDC IdP, and the SPA static files.

## Architecture

```
                         ┌────────────────────────────────────────────────┐
                         │              Management host                   │
    Browser ──HTTP/SSE──▶│  opsguard-server (Go single binary)            │
                         │  ├─ REST API  /api/v1/*                        │
                         │  ├─ SPA static hosting (Vue3 dist)             │
                         │  ├─ AiNexus AI gateway /ainexus/*              │
                         │  ├─ Embedded OCI registry /v2/*                │
                         │  ├─ Embedded OIDC IdP /api/v1/idp/*            │
                         │  └─ LevelDB (./data)                           │
                         └───────────┬────────────────────────────────────┘
                                     │  gRPC (initiated by server, bearer: worker token)
        ┌────────────────────────────┼────────────────────────────┐
        ▼                            ▼                            ▼
 ┌──────────────┐            ┌──────────────┐            ┌──────────────┐
 │ Cluster A    │            │ Cluster B    │   ……       │ Cluster N    │
 │ Worker       │            │ Worker       │            │ Worker       │
 │ (swarm global│            │ leader runs  │            │              │
 │  one per node)│           │ write ops    │            │              │
 │ HTTP :8080   │            │              │            │              │
 │ gRPC :9080   │            │              │            │              │
 │ SQLite queue │            │              │            │              │
 │ docker.sock  │            │              │            │              │
 └──────────────┘            └──────────────┘            └──────────────┘

 In-cluster consumers (via local Worker entry, no reverse holes):
   ├─ /idp-proxy/*   → in-cluster systems access the management IdP (Tunnel)
   └─ /v2/*          → docker daemon pull/push the embedded registry (Tunnel)
```

The server is always the gRPC **client**; the Worker is the server (gRPC `:9080`). Workers also expose an **MCP server** (`/mcp` Streamable HTTP + stdio, 20 ops tools) that the embedded AiNexus gateway calls to operate clusters for real.

## Repository Layout

```
├── OpsGaurdWeb/
│   ├── server/        # Management server in Go (REST API + AI gateway + OCI registry + OIDC IdP + SPA)
│   └── web/           # Vue3 SPA frontend
├── Worker/            # Per-cluster Worker (Go static binary, swarm global mode)
├── proto/             # opsguard.proto — the gRPC contract between server and Worker
├── deploy/            # Offline deployment runbook (offline/) and intranet egress proxy (internet-proxy/)
└── docs/              # Technical overview and subsystem design docs (Chinese)
```

## Tech Stack

| Component | Language/Runtime | Key dependencies | Storage |
| --- | --- | --- | --- |
| server | Go 1.25 | gin, grpc + protobuf, goleveldb, coreos/go-oidc, mcp-go, robfig/cron | LevelDB |
| Worker | Go 1.25 | grpc, modelcontextprotocol/go-sdk, modernc.org/sqlite, self-built Docker Engine/Swarm REST client | SQLite |
| web | TypeScript 5.7 | Vue 3.5, Vite 6, Element Plus, Pinia, vue-router, axios, js-yaml | — |

## Getting Started

The production form is Docker Swarm: the management server runs as a single `docker run` container (`:8080`), and each managed cluster runs one Worker per node as a swarm `global` service. Target environments are usually air-gapped; the offline install flow (docker static bundle → swarm init/join → offline image load) is documented in:

- **`deploy/offline/README.md`** — offline deployment runbook (real cluster topologies and pitfalls)
- **`docs/OpsGaurd-系统技术总览.md`** — configuration reference (`config.yaml` / `agent-config.yaml` / env vars / port matrix)

For connected environments you can build and deploy directly: `OpsGaurdWeb/deploy/Dockerfile` (management server, ships with the frontend dist), `Worker/deploy/Dockerfile` (Worker).

## Documentation (docs/)

All docs are in Chinese. Start with [OpsGaurd 系统技术总览](docs/OpsGaurd-系统技术总览.md) for the architecture, communication, storage, security, and deployment panorama, then dive into the per-subsystem design docs (Worker, web, AiNexus/MCP, MLOps, r-nacos, image tunnel relay, cluster inventory, IdP integration, K8s replacement).

## Development & Build

- **proto changes**: `proto/gen.sh` regenerates both sides; when protoc or the module proxy is unavailable, `Worker/cmd/genpatch` applies offline patches instead.
- **Worker build**: `Worker/deploy/Dockerfile` (CGO_ENABLED=0 static), version injected via `-ldflags`.
- **server build**: `OpsGaurdWeb/deploy/Dockerfile`, ships with `web/dist`; frontend built separately via `npm run build` (vue-tsc checks).
- **Frontend dev**: Vite dev server (5173) under `web/`, `/api` proxied to `http://localhost:8090`.

## License

[GNU Affero General Public License v3.0](LICENSE)

## Mirrors

- Gitee: https://gitee.com/legeosoft_legendzhu/OpsGaurd
- GitHub: https://github.com/Legend-Zhu/OpsNexus
