# Patch: fix cross-node SNAT broken by Flannel MASQUERADE

## Context

This fork of [spidernet-io/egressgateway](https://github.com/spidernet-io/egressgateway)
contains a fix for a bug discovered while deploying site-based egress routing
in a K3s cluster (project Scipio / cluster Nethersphere) with Flannel VXLAN.

**Fix branch**: `fix/cross-node-flannel-masquerade`
**Base**: upstream tag `v0.6.9` (latest release, April 2024)

This is the **second independent bug** found in the same environment. The first
bug (nil-IP route wipe in `keepVXLAN`) is documented in `SCIPIO_PATCH.md` and
fixed in branch `fix/keepvxlan-nil-ip-wipes-routes`.

---

## The Bug

### Location

`pkg/agent/police.go` — `buildNatStaticRule()` function, `POSTROUTING` chain

### Symptom

Cross-node egress SNAT is completely non-functional even after the first bug is
fixed. A pod on a non-gateway node reaches the Internet with the **source node's
IP** instead of the **gateway node's IP (the EIP)**.

**Observable on the source node:**

```text
conntrack -L -s <pod_ip>
# tcp  UNREPLIED src=<pod_ip> dst=<internet> [...]
#                reply src=<internet> dst=172.31.x.x   ← tunnel IP, not EIP
```

The reply path destination is the local VXLAN tunnel address (`172.31.x.x`, not
routable externally), meaning the SNAT on the gateway node is never applied.

**From the pod:**

```bash
curl https://ifconfig.me
# returns source node public IP instead of gateway node public IP
```

### Trigger Sequence

1. A pod on node A (non-gateway) sends a packet toward the Internet.
2. The agent on node A marks the packet with a per-gateway-node mark
   (e.g. `0x26577c9e` for the node B gateway), derived from
   `node.Status.Mark` via `parseMark()`.
3. The packet enters `nat/POSTROUTING`. EgressGateway inserts this chain:

   ```text
   -A POSTROUTING -j EGRESSGATEWAY-SNAT-EIP
   -A POSTROUTING -m mark --mark 0x26000000/0xffffffff -j ACCEPT   ← BUG
   ... (KUBE-POSTROUTING, FLANNEL-POSTRTG, etc.)
   ```

4. The ACCEPT rule uses mask `0xffffffff`, so it only matches when the mark
   equals `0x26000000` **exactly**. The actual mark `0x26577c9e` does not match.
5. The packet reaches `FLANNEL-POSTRTG` (or equivalent CNI masquerade rule) and
   is SNAT'd to the local VXLAN subnet address (e.g. `172.31.0.185`).
6. The packet arrives at the gateway node with `src=172.31.0.185`. The ipset
   `egress-src-v4-*` does not match this address (it expects the pod IP), so
   the EgressGateway SNAT rule in `EGRESSGATEWAY-SNAT-EIP` is skipped.
7. The packet exits the gateway node with the wrong source IP — no EIP applied.

### Root Cause

In `buildNatStaticRule` (`pkg/agent/police.go`, line ~603), the POSTROUTING
ACCEPT rule uses the wrong constant:

```go
// BUGGY — uses Mask = 0xffffffff (exact 32-bit match)
{
    Match:  iptables.MatchCriteria{}.MarkMatchesWithMask(base, Mask),
    Action: iptables.AcceptAction{},
    Comment: []string{"Accept for egress traffic from pod going to EgressTunnel"},
},
```

The constants are defined as:

```go
Mark = 0xff000000  // high-byte mask: matches any mark with the same top byte
Mask = 0xffffffff  // full-word mask: exact match only
```

With `Mask = 0xffffffff`, the rule renders as:

```text
-m mark --mark 0x26000000/0xffffffff
```

This matches **only** if the mark is exactly `0x26000000`. However, the agent
stamps per-gateway-node marks in the format `0x26XXXXXX` (high byte = base
high byte, low 3 bytes from a hash of the node). These per-gateway marks never
equal the base mark exactly, so the rule is effectively dead in cross-node
scenarios.

### Proof of Asymmetry (the same file does it right elsewhere)

The identical function file uses `Mark` (the correct byte mask) in every other
chain:

| Function | Chain | Mask used |
|---|---|---|
| `buildMangleStaticRule` | `PREROUTING` | `Mark` = `0xff000000` ✓ |
| `buildMangleStaticRule` | `PREROUTING` (reply) | `Mark` ✓ |
| `buildFilterStaticRule` | `FORWARD` | `Mark` ✓ |
| `buildNatStaticRule` | `PREROUTING` | `Mark` ✓ |
| **`buildNatStaticRule`** | **`POSTROUTING` ACCEPT** | **`Mask` = `0xffffffff` ✗** |

This is a one-character typo (`Mask` vs `Mark`) never caught because upstream
unit tests only exercise the pod-on-gateway-node path where no cross-node mark
is involved.

---

## The Fix

**File**: `pkg/agent/police.go`
**Change**: 1 line — replace `Mask` with `Mark` in the POSTROUTING ACCEPT rule

```diff
-Match:  iptables.MatchCriteria{}.MarkMatchesWithMask(base, Mask),
+Match:  iptables.MatchCriteria{}.MarkMatchesWithMask(base, Mark),
```

The rule now renders as:

```text
-m mark --mark 0x26000000/0xff000000
```

This matches any packet whose top byte equals `0x26` — which is true for all
per-gateway-node marks regardless of the low 3 bytes.

No other changes are needed. The insertion order in `POSTROUTING` is already
correct: the ACCEPT rule comes **after** `EGRESSGATEWAY-SNAT-EIP` (so the
gateway node's own SNAT fires first) but **before** `KUBE-POSTROUTING` and
`FLANNEL-POSTRTG` (so Flannel's MASQUERADE is bypassed for already-marked
EgressGateway traffic).

---

## Experimental Validation

The fix was validated by manually inserting the equivalent iptables rule on
each node in the Nethersphere cluster before building the patched image:

```bash
iptables -t nat -I POSTROUTING 2 \
  -m mark --mark 0x26000000/0xff000000 \
  -j ACCEPT \
  -m comment --comment "egw-flannel-bypass-workaround"
```

**Results measured live:**

| Metric | Before fix | After fix |
|---|---|---|
| MASQUERADE counter (orange gateway) | 0 packets | 5+ packets |
| Conntrack reply dst | `172.31.0.185` (tunnel) | `10.1.2.30` (gateway IP) |
| Connection state | `UNREPLIED` | `ESTABLISHED [ASSURED]` |
| Pod public IP (5 curls) | source node IP | gateway node IP ✓ |
| ACCEPT workaround rule counter | — | 25+ packets in seconds |

---

## Upstream Status

Related issues (context, not this specific bug):

- [spidernet-io/egressgateway#272](https://github.com/spidernet-io/egressgateway/issues/272)
  — origin PR that introduced the ACCEPT rule (already with the wrong mask)
- [spidernet-io/egressgateway#1797](https://github.com/spidernet-io/egressgateway/issues/1797)
  — identical symptom, fixed via `rp_filter` only (not the mask)
- [spidernet-io/egressgateway#1923](https://github.com/spidernet-io/egressgateway/issues/1923)
  — VXLAN packet drops (Calico, related environment)

**This specific bug (wrong mask in POSTROUTING ACCEPT) has not been reported
upstream before.** A PR should be submitted with title:

> `fix(agent): use byte-mask in nat/POSTROUTING accept rule to match per-gateway marks`

The PR body should reference the asymmetry table above, the cross-node mark
mechanism, and the live validation data, and note that the bug manifests only
with a CNI that applies MASQUERADE (Flannel, some Calico configurations).

---

## Building the Patched Image

```bash
# From the repo root on branch fix/cross-node-flannel-masquerade
IMAGE=ghcr.io/farmvivi/egressgateway-agent:fix-cross-node-flannel-masquerade

docker build \
  --build-arg TARGETARCH=amd64 \
  --build-arg TARGETOS=linux \
  -f ./images/agent/Dockerfile \
  -t $IMAGE .

docker push $IMAGE
```