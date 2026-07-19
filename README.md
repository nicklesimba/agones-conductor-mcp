<p align="center">
  <img src="assets/logo.svg" width="120" alt="agones-conductor-mcp logo" />
</p>

<h1 align="center">agones-conductor-mcp</h1>

<p align="center">
  <a href="https://github.com/nicklesimba/agones-conductor-mcp/actions/workflows/ci.yml"><img src="https://github.com/nicklesimba/agones-conductor-mcp/actions/workflows/ci.yml/badge.svg" alt="CI" /></a>
  <a href="https://github.com/nicklesimba/agones-conductor-mcp/actions/workflows/e2e.yml"><img src="https://github.com/nicklesimba/agones-conductor-mcp/actions/workflows/e2e.yml/badge.svg" alt="E2E" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-blue.svg" alt="License: Apache-2.0" /></a>
</p>

An MCP server for [Agones](https://agones.dev), the Kubernetes-native dedicated game server platform. Requires an Agones cluster you already have access to (see [Requirements](#requirements)).

Generic Kubernetes MCP servers see an Agones cluster as opaque custom resources. They don't know that an `Allocated` GameServer has a live match with real players on it, that Fleets scale down by reaping `Ready` servers first, or that a FleetAutoscaler at its ceiling means players are about to queue. This server speaks Agones natively, so an AI assistant can investigate incidents, answer capacity questions, and operate fleets safely.

## Example prompts

- "Why can't EU players get into matches?"
- "Which fleets are close to their autoscaler ceiling?"
- "Show me every Unhealthy server and what happened to it"
- "Scale the practice fleet down to 10 without touching live matches"
- "Allocate me a server from the ranked fleet"

A real example, "How full is the ranked fleet?", calling `fleet_capacity`:

```json
{"fleets":[{"fleet":"ranked","namespace":"default","allocated":3,"ready":2,"utilizationPct":60,"autoscalerCeiling":6,"atCeiling":false}]}
```

→ "The ranked fleet is at 60% utilization: 3 matches in progress, 2 servers free, autoscaler can grow up to 6 before it's out of room."

## Tools

| Tool | Type | Description |
|---|---|---|
| `list_fleets` | read | Fleets with desired/ready/allocated/reserved counts |
| `list_gameservers` | read | GameServers, filterable by state or fleet |
| `get_gameserver` | read | Full detail for one GameServer: labels, annotations, counters/lists, node, image, termination status |
| `gameserver_events` | read | Kubernetes events for a GameServer (diagnose failures) |
| `fleet_events` | read | Events for a Fleet and its GameServerSets: answers "why did the fleet scale down?" |
| `autoscaler_events` | read | Events for a FleetAutoscaler: when and why it scaled |
| `gameserver_logs` | read | Container logs for a GameServer (verified to be one first; this is not a general pod log reader). Defaults to the game container, `container` overrides; `previous=true` for a crashed instance |
| `list_autoscalers` | read | FleetAutoscalers with policy, bounds, scaling-limited status |
| `fleet_capacity` | read | Cross-fleet utilization report computed from live GameServer state, not the aggregated Fleet status, which lags a few seconds behind real allocation events. Utilization is allocated / (allocated + ready): the share of playable servers in use |
| `create_fleet` | write | Create a Fleet with a single-container GameServer template: port policy and protocol (UDP/TCP/TCPUDP), resources, scheduling, initial Counters/Lists |
| `delete_fleet` | write | Delete a Fleet and its GameServers; refuses if any are Allocated unless `force=true` |
| `scale_fleet` | write | Set fleet replicas; scale-down never disrupts live matches |
| `allocate_gameserver` | write | Allocate a server for a match; filter and update Counters/Lists at allocation time, stamp match labels/annotations onto the server, or set `preferReuse` to pack players onto running matches with room first |
| `delete_gameserver` | write | Delete a server; refuses Allocated servers unless `force=true` |
| `update_fleet_image` | write | Update a Fleet's container image, triggering Agones's own allocation-aware rolling update |
| `update_fleet_resources` | write | Update a Fleet container's CPU/memory requests and limits, triggering the same rolling update |
| `update_fleet_health` | write | Update a Fleet's health-check settings (disabled, periodSeconds, failureThreshold, initialDelaySeconds), triggering the same rolling update |
| `update_fleet_env` | write | Set or remove environment variables on a Fleet's container, triggering the same rolling update |
| `rollout_status` | read | Rolling-update progress: current vs previous GameServerSets, percent complete, and a warning when old-version servers still have live matches blocking completion |
| `create_autoscaler` | write | Create a FleetAutoscaler: Buffer policy (keep N Ready servers) or Counter/List policy (scale on aggregate capacity, e.g. total free player slots), with a configurable sync interval |
| `update_autoscaler` | write | Update a FleetAutoscaler's buffer size, replica bounds, or sync interval |
| `delete_autoscaler` | write | Delete a FleetAutoscaler; the Fleet and its GameServers are unaffected |
| `agones_health` | read | Is Agones itself healthy? Controller/allocator/extensions/ping pod readiness and restarts |
| `list_clusters` | read | List configured clusters and which one is the default (only meaningful in multi-cluster mode) |
| `open_match_create_ticket` | write | Enter a player/party into [Open Match](https://open-match.dev) matchmaking, returns a ticket ID |
| `open_match_ticket_status` | read | Check a ticket: still searching, or matched with a connection assignment |
| `open_match_cancel_ticket` | write | Withdraw a ticket from matchmaking |

Every tool above except `list_clusters` also accepts an optional `cluster` argument (see [Multi-cluster](#multi-cluster)). Every write tool except `allocate_gameserver` accepts `dryRun: true` for a server-side validation pass that persists nothing, so an agent can show its work before the real call. The three `open_match_*` tools additionally require Open Match connectivity to be configured (see [Open Match](#open-match)) and return a clear error if it isn't.

All 27 tools are exercised against a live Agones cluster (kind + Agones 1.57), not just unit-tested against fakes - and not on the honor system: the same 45-check suite runs in public CI on every push (`test/e2e/mcp_e2e.py`, [e2e.yml](.github/workflows/e2e.yml)), driving the real binary over MCP stdio as a least-privilege ServiceAccount against a fresh kind cluster. Highlights of what it covers, plus one-off deep verifications done during development:

- `delete_gameserver`'s safety gate: refusal without `force`, success with it, and a race test proving the refusal still holds if a server is allocated between the check and the delete.
- `scale_fleet` scale-down/up leaving live allocations untouched, including under a simulated concurrent write (the FleetAutoscaler writes `Fleet.Spec.Replicas` too).
- A real `update_fleet_image` rollout that `rollout_status` correctly reported as stalled at 50% while two old-version servers still had live matches.
- A full `create_fleet` → `create_autoscaler` → `update_autoscaler` → `delete_autoscaler` → `delete_fleet` round trip, including a call deliberately shaped to violate Agones's own admission-webhook rule (`minReplicas` must be 0 or `>= bufferSize`) to confirm it's rejected client-side with a clear message instead of a raw webhook error.
- `update_fleet_resources` and `update_fleet_health`, each called twice in sequence to confirm a partial update (setting only one field) leaves the fields set by the first call untouched - checked against raw `kubectl get fleet -o jsonpath`, not just the tool's own reported output.
- Counters/Lists end to end on a real fleet: `create_fleet` declaring an initial Counter and List, `list_gameservers` showing that initial state on both Ready servers, `allocate_gameserver` applying a Counter increment and a List append at allocation time, then a second `allocate_gameserver` call with a Counter selector correctly skipping the now-lower-capacity server and picking the other one.
- Multi-cluster routing (both the happy path and the unknown-cluster error) against a live server started with `AGONES_MCP_CLUSTERS` set, including the loud failure on a typo'd `AGONES_MCP_CONTEXT`.
- A full Open Match round trip against a live Open Match deployment: `open_match_create_ticket` created real tickets, a deployed match function and director matched and assigned them, `open_match_ticket_status` observed the resulting connection assignment, and `open_match_cancel_ticket` removed an unmatched ticket (confirmed by a subsequent `NotFound` on status).
- Every read and write tool re-run end to end over MCP stdio under `deploy/rbac.yaml` exactly as committed, bound to a fresh, otherwise-empty ServiceAccount: the full fleet lifecycle, `gameserver_logs` included, plus the negative paths (invalid state filter rejected, empty image rejected, log reads of non-GameServer pods refused). Nothing failed for lack of permission, and a matrix check confirmed every verb the role does NOT grant is denied.

## Safety model

- Every destructive operation is a separate, explicitly-named tool, and the dangerous ones fail closed.
- `delete_gameserver` refuses to delete an `Allocated` server (live players) unless `force=true` is passed, and reports the disconnection consequence when forced. Its refusal survives races: a `ResourceVersion` precondition catches a server that becomes Allocated between the check and the delete.
- `delete_fleet` applies the same live-match refusal, with one honest caveat: the check spans multiple objects, so no single precondition can close its race window completely. An allocation landing in the same instant as the delete can still be disrupted.
- `scale_fleet` relies on Agones scheduling semantics: scale-down removes `Ready` servers first and never disrupts `Allocated` ones.
- Tool outputs are compact summaries, not raw Kubernetes objects, so agents reason over signal instead of `managedFields` noise.
- `namespace` is optional (omit for all namespaces) on tools that list multiple resources (`list_fleets`, `list_gameservers`, `list_autoscalers`, `fleet_capacity`). It's required everywhere else, because every other tool targets one specific named resource. "All namespaces" isn't a meaningful answer to "which Fleet do you mean."
- `deploy/rbac.yaml` grants exactly the verbs the tools use, nothing more. See [Permissions](#permissions).
- Counter/List changes only happen through `allocate_gameserver`'s `counterActions`/`listActions`, the same allocation-time mechanism Agones's own aggregated allocation API exposes externally. There's deliberately no "set a counter on any running GameServer" tool: a live GameServer's SDK sidecar holds its own in-memory copy of that state and periodically syncs it to `Status`, so an external patch outside of allocation would race that sync and could be silently overwritten a moment later.
- `gameserver_logs` and `gameserver_events` return raw text from inside the cluster (container logs, event messages), which an agent reads as part of its context. Attacker-influenced content there could try to steer the agent toward a write call, and no server can reliably tell real log output from injected instructions. What this one does: caps log and event content independently of what was asked for, marks both outputs as untrusted data before returning them, and refuses to read logs of anything that isn't a GameServer. The real boundary is architectural: your MCP host should require approval on every write-tool call (Claude Code does this by default). That approval gate, not this server, is what stops injected content from causing a write.

## Requirements

- Kubernetes cluster with [Agones installed](https://agones.dev/site/docs/installation/) (supported Kubernetes: 1.33-1.35)
- kubeconfig with access to Agones resources
- Go 1.25+ to install or build
- [Open Match](https://open-match.dev/site/docs/installation/) (optional, only for the `open_match_*` tools) with a match function and director deployed

No cluster yet? [kind](https://kind.sigs.k8s.io/) plus the [Agones install guide](https://agones.dev/site/docs/installation/install-agones/helm/) gets you a local one in a few minutes:

```sh
kind create cluster --name agones-dev
helm repo add agones https://agones.dev/chart/stable
helm install agones agones/agones --namespace agones-system --create-namespace
kubectl apply -f https://raw.githubusercontent.com/googleforgames/agones/release-1.57.0/examples/simple-game-server/fleet.yaml
```

## Install

```sh
go install github.com/nicklesimba/agones-conductor-mcp@latest
```

The binary lands in `$(go env GOPATH)/bin` (add it to your PATH if it isn't already). Or build from source:

```sh
git clone https://github.com/nicklesimba/agones-conductor-mcp
cd agones-conductor-mcp
go build -o agones-conductor-mcp .
```

On Windows, build with `-o agones-conductor-mcp.exe`, and use the full path with escaped backslashes in JSON configs (e.g. `"C:\\tools\\agones-conductor-mcp.exe"`).

## Configure

The server authenticates using your [kubeconfig](https://kubernetes.io/docs/concepts/configuration/organize-cluster-access-kubeconfig/) (the file `kubectl` already uses to talk to your cluster(s), normally `~/.kube/config`), or in-cluster credentials automatically when deployed inside a cluster. A kubeconfig can define multiple **contexts** (named cluster+user+namespace combinations); if yours has more than one and `kubectl config current-context` isn't the right one, set `AGONES_MCP_CONTEXT` to the context name you want.

Claude Code:

```sh
claude mcp add agones -- /path/to/agones-conductor-mcp
```

Claude Desktop / Cursor (`mcpServers` config):

```json
{
  "mcpServers": {
    "agones": {
      "command": "/path/to/agones-conductor-mcp",
      "env": { "AGONES_MCP_CONTEXT": "my-cluster" }
    }
  }
}
```

Running headless (`claude -p`, CI, scripts): the client prompts for per-tool approval on first use, and a non-interactive session has no TTY to answer that prompt, so it just hangs. Pass `--allowedTools` with the specific `mcp__agones__*` tool names you need.

### Multi-cluster

By default the server targets a single cluster (in-cluster config, or the kubeconfig context named by `AGONES_MCP_CONTEXT`), and tool calls never need a `cluster` argument. To manage several clusters from one server, set `AGONES_MCP_CLUSTERS` to a comma-separated list of kubeconfig context names:

```json
{
  "mcpServers": {
    "agones": {
      "command": "/path/to/agones-conductor-mcp",
      "env": { "AGONES_MCP_CLUSTERS": "us-west,us-east,eu-central" }
    }
  }
}
```

The first name listed is the default (override with `AGONES_MCP_CONTEXT`). Every tool then accepts an optional `cluster` argument naming one of these contexts; omitting it targets the default, so single-cluster prompts keep working unchanged. Use `list_clusters` to see what's configured.

### Open Match

Skip this section unless you already run [Open Match](https://open-match.dev). It's a separate matchmaking system, not something Agones needs. The `open_match_*` tools are off by default; without configuration they just return a clear "not configured" error.

If you do run it, these tools connect to its Frontend service over gRPC **without TLS**. The connection is plaintext, matching Open Match's own default in-cluster deployment, so keep the Frontend unexposed to untrusted networks: use in-cluster DNS or a `kubectl port-forward` rather than exposing it publicly. `AGONES_MCP_OPEN_MATCH_FRONTEND` needs a `host:port` this server can actually reach. Concretely, one of:
- **In-cluster DNS name** (e.g. `open-match-frontend.open-match.svc.cluster.local:50504`) - works if `agones-conductor-mcp` itself runs as a Pod in the same cluster.
- **[NodePort or LoadBalancer](https://kubernetes.io/docs/concepts/services-networking/service/#publishing-services-service-types)** - a Kubernetes Service type that exposes something inside the cluster to the outside; use this if `agones-conductor-mcp` runs outside the cluster.
- **A [`kubectl port-forward`](https://kubernetes.io/docs/tasks/access-application-cluster/port-forward-access-application-cluster/)** you've started yourself, forwarding a local port to the Frontend service - the quickest option for local testing.

Set `AGONES_MCP_OPEN_MATCH_FRONTEND` to whichever of these applies:

```json
{
  "mcpServers": {
    "agones": {
      "command": "/path/to/agones-conductor-mcp",
      "env": { "AGONES_MCP_OPEN_MATCH_FRONTEND": "open-match-frontend.open-match.svc.cluster.local:50504" }
    }
  }
}
```

In multi-cluster mode, use `AGONES_MCP_OPEN_MATCH_FRONTENDS` instead: a comma-separated `clusterName=host:port` list, matching the names in `AGONES_MCP_CLUSTERS`. Clusters without an entry simply have Open Match tools disabled for them.

This server doesn't do any matchmaking itself. Open Match's own architecture needs two other pieces to actually turn tickets into matches: a **match function** (your matchmaking logic - which players belong together) and a **director** (polls Open Match for proposed matches and finalizes them). Those are things you write and deploy separately, following [Open Match's own docs](https://open-match.dev/site/docs/). `agones-conductor-mcp` only creates, checks, and cancels tickets against whatever pipeline you've already got running - `allocate_gameserver` has the identical relationship to Agones's own allocator, calling it rather than deciding server placement itself.

### Permissions

Don't run this server with broad cluster-admin-style access. [RBAC](https://kubernetes.io/docs/reference/access-authn-authz/rbac/) (Role-Based Access Control) is how Kubernetes scopes what an identity is allowed to do, and `deploy/rbac.yaml` defines the minimal set this server actually needs (see that file's own comments for the exact list). A `ClusterRole` by itself grants nothing: it's a named bundle of permissions that only takes effect once bound to an identity via a `ClusterRoleBinding`. If this server runs inside the cluster as a Pod, bind it to that Pod's [ServiceAccount](https://kubernetes.io/docs/concepts/security/service-accounts/); if it runs externally via kubeconfig, apply an equivalent grant to whichever identity that kubeconfig authenticates as.

Concretely, for a ServiceAccount named `agones-conductor-mcp` in namespace `default`:

```sh
kubectl apply -f deploy/rbac.yaml
kubectl create serviceaccount agones-conductor-mcp -n default
kubectl create clusterrolebinding agones-conductor-mcp --clusterrole=agones-conductor-mcp --serviceaccount=default:agones-conductor-mcp
```

Every tool has been re-run against a real cluster under this exact role, bound to a fresh ServiceAccount with no other access. Nothing needs more than what's granted here, and a permission matrix confirmed the excess verbs (pod reads, secret access, gameserver writes, and more) are denied.

## Development

`scripts/simulate-churn.sh [fleet]` allocates and tears down GameServers on a timer, simulating matches starting and ending, so there's live state to observe while testing tools interactively. Defaults to context `kind-agones-dev`, namespace `default`, fleet `simple-fleet`; override with `AGONES_CONTEXT`/`AGONES_NAMESPACE` env vars or a fleet name argument.

## Contributing

Issues and PRs welcome. This project aims to be useful to anyone running Agones, regardless of scale or game genre. If a tool here makes an assumption that doesn't fit your setup, that's a bug worth filing.

## License

Apache-2.0. See [LICENSE](LICENSE).

