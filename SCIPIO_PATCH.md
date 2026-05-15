# Patch: fix keepVXLAN nil-IP route wipe

## Context

This fork of [spidernet-io/egressgateway](https://github.com/spidernet-io/egressgateway)
contains a fix for a bug discovered while deploying site-based egress routing
in a k3s cluster (project Scipio / cluster Nethersphere).

**Fix branch**: `fix/keepvxlan-nil-ip-wipes-routes`
**Base**: upstream tag `v0.6.9` (latest release, April 2024)

---

## The Bug

### Location

`pkg/agent/route/route.go` — `Ensure()` function

### Symptom

Cross-node egress routing is completely non-functional. VXLAN tunnel routes
added by `reconcileEgressGateway` are deleted within 10 seconds of creation,
permanently.

**Observable on a non-gateway node:**

```bash
watch -n2 "ip route show table <fwmark_decimal>"
# table is wiped every ~10 seconds
```

**Agent logs show:**

```text
delete route  route="<gateway-vxlan-ip> dev egress.vxlan table <mark>"
```

Routes are added, then deleted within one keepVXLAN cycle.

### Trigger Sequence

1. `keepVXLAN` goroutine (runs every 10s, launched unconditionally at agent
   startup) iterates `peerMap` and calls:

   ```go
   r.ruleRoute.Ensure(r.cfg.FileConfig.VXLAN.Name, val.IPv4, val.IPv6, val.Mark, val.Mark)
   ```

2. If `val.IPv4` is `nil` — which happens when `reconcileEgressTunnel` has not
   yet populated the peer's tunnel IP into `peerMap` (race condition at startup
   or after node restart) — `Ensure()` calls:

   ```go
   EnsureRoute(link, nil, FAMILY_V4, table, log)
   ```

3. Inside `EnsureRoute`, the deletion condition is:

   ```go
   if ip == nil || route.Gw.String() != ip.String() {
       netlink.RouteDel(&route)  // deletes ALL routes when ip==nil
   }
   ```

   With `ip == nil`, this condition is always true, so **every route in the
   policy-routing table is deleted** before the function returns.

4. The cross-node tunnel route (e.g. `default via 172.31.85.199 dev egress.vxlan
   table 0x26577c9e`) is gone. Traffic from non-gateway nodes can no longer be
   forwarded through the VXLAN tunnel to the gateway node.

### Root Cause

`Ensure()` already guards `EnsureRule` behind nil-checks but calls `EnsureRoute`
unconditionally:

```go
// EnsureRule — correctly guarded
if ipv4 != nil {
    err := r.EnsureRule(netlink.FAMILY_V4, table, mark, log)
}

// EnsureRoute — NOT guarded (bug)
err = r.EnsureRoute(link, ipv4, netlink.FAMILY_V4, table, log)  // ipv4 may be nil
err = r.EnsureRoute(link, ipv6, netlink.FAMILY_V6, table, log)  // ipv6 may be nil
```

---

## The Fix

**File**: `pkg/agent/route/route.go`
**Change**: add the same nil-guard pattern already used for `EnsureRule`

```diff
-   err = r.EnsureRoute(link, ipv4, netlink.FAMILY_V4, table, log)
-   if err != nil {
-       return err
-   }
-   err = r.EnsureRoute(link, ipv6, netlink.FAMILY_V6, table, log)
-   if err != nil {
-       return err
+   if ipv4 != nil {
+       err = r.EnsureRoute(link, ipv4, netlink.FAMILY_V4, table, log)
+       if err != nil {
+           return err
+       }
+   }
+   if ipv6 != nil {
+       err = r.EnsureRoute(link, ipv6, netlink.FAMILY_V6, table, log)
+       if err != nil {
+           return err
+       }
    }
```

When `ipv4` or `ipv6` is nil (peer tunnel IP not yet known), `EnsureRoute` is
simply skipped for that address family instead of wiping the routing table.
Existing routes are preserved until the peer IP becomes available.

---

## Upstream Status

- No GitHub issue open for this specific bug in spidernet-io/egressgateway
- No fix in the `main` branch (`pkg/agent/route/route.go` is identical to v0.6.9)
- v0.6.9 is the latest release (April 2024); no v0.7.x exists
- A PR should be submitted upstream using this fix branch as reference

---

## Building the Patched Image

The agent and controller are built from separate Dockerfiles and produce
separate images (`egressgateway-agent` and `egressgateway-controller`).
**Only the agent is affected by this bug** — only the agent image needs to be rebuilt.

```bash
# From the repo root, on branch fix/keepvxlan-nil-ip-wipes-routes
AGENT_IMAGE=ghcr.io/farmvivi/egressgateway-agent:v0.6.9-patched

docker build \
  --platform linux/amd64 \
  -f images/agent/Dockerfile \
  -t $AGENT_IMAGE \
  .

# Authenticate to ghcr.io first if needed:
# echo $GITHUB_TOKEN | docker login ghcr.io -u <github-user> --password-stdin

docker push $AGENT_IMAGE
```

Then override the agent image in the spidernet Helm values
(k3s HelmChart operator in Scipio):

```yaml
# kube/clusters/nethersphere/manifests/platform/egressgateway/helmchart-operator.yaml
agent:
  image:
    registry: ghcr.io
    repository: farmvivi/egressgateway-agent
    tag: v0.6.9-patched
    pullPolicy: IfNotPresent
```

### Reverting to the official image

Once the upstream fix is merged and a new release is available
(check [spidernet-io/egressgateway releases](https://github.com/spidernet-io/egressgateway/releases)):

1. Remove the `image:` block from `agent:` in `helmchart-operator.yaml`
2. Update `version:` to the new release tag
3. The official image will be used automatically

---

## Affected Environment (Scipio / Nethersphere)

| Node | Site | LAN IP | VXLAN IP |
|---|---|---|---|
| nethersphere-server-01 | Trenzalore | 10.1.2.30 | 172.31.85.199 |
| nethersphere-server-02 | Skaro | 10.2.2.30 | 172.31.0.185 |
| nethersphere-server-03 | Telos | 10.4.2.30 | 172.31.160.143 |

EgressGateways: one per site with a dedicated EIP (10.x.2.32).
EgressClusterPolicies: `destSubnet: 10.x.2.0/24` for each site.

**Same-site routing** (pod on server-02 → 10.2.2.x): works — SNAT applied
locally, no VXLAN tunnel needed.

**Cross-site routing** (pod on server-01 → 10.2.2.x): broken without this
fix — VXLAN route is wiped by keepVXLAN before traffic can be forwarded.

**Current workaround**: `nodeSelector` on site-specific apps (Home Assistant,
autodiscover) to schedule pods on their site's node, avoiding cross-node egress.
