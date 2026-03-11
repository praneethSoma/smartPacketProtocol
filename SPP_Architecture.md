# Smart Packet Protocol (SPP) — Architecture & Technical Specification

**Version:** 1.0  
**Author:** Praneeth Soma  
**Date:** March 2026

---

## 1. Protocol Philosophy

Traditional protocols (TCP, UDP, QUIC) treat all data identically. A heart-rate monitor alert and a background OS update compete for the same queue, the same bandwidth, the same path. The network infrastructure (routers, switches) makes all routing decisions — packets are passive cargo.

**SPP inverts this model.** The packet is the intelligence. It carries:
- Its own **identity** (what kind of data it is)
- Its own **map** (what the network looks like right now)
- Its own **engine** (Dijkstra algorithm to calculate the best route)
- Its own **audit trail** (what it observed along the way)

The router's job is reduced to three things:
1. Stamp current conditions into the packet
2. Read the packet's next-hop instruction
3. Forward

---

## 2. System Architecture

```
                              NETWORK PLANE
    ┌─────────────────────────────────────────────────────────┐
    │                                                         │
    │   ┌───────────┐    ┌───────────┐    ┌───────────┐      │
    │   │ ROUTER A  │◄──►│ ROUTER B  │◄──►│ ROUTER C  │      │
    │   │           │    │           │    │           │      │
    │   │ metrics/  │    │ metrics/  │    │ metrics/  │      │
    │   │ gossip/   │    │ gossip/   │    │ gossip/   │      │
    │   │ packet/   │    │ packet/   │    │ packet/   │      │
    │   └─────┬─────┘    └─────┬─────┘    └─────┬─────┘      │
    │         │                │                │             │
    │         │ ◄── gossip ──► │ ◄── gossip ──► │             │
    │         │   (50ms UDP)   │   (50ms UDP)   │             │
    │         │                │                │             │
    └─────────┼────────────────┼────────────────┼─────────────┘
              │                │                │
              │           DATA PLANE            │
              │          (UDP packets)          │
              │                │                │
    ┌─────────┴─────┐                    ┌─────┴───────────┐
    │    SENDER     │                    │    RECEIVER      │
    │               │                    │                  │
    │  packet/      │  ──────────────►   │  packet/         │
    │  (create +    │  smart packets     │  (decode +       │
    │   route)      │                    │   deliver)       │
    └───────────────┘                    └──────────────────┘
```

### Component Deployment

| Node Type | Packages Used | Runs On |
|-----------|--------------|---------|
| **Sender** | `packet/` | Any machine that originates data |
| **Router** | `packet/` + `metrics/` + `gossip/` | Every forwarding node in the network |
| **Receiver** | `packet/` | Any machine that receives data |

---

## 3. The Smart Packet — Anatomy

Every SPP packet on the wire carries this structure:

```
┌─────────────────────────────────────────────────────────────┐
│                    WIRE HEADER (16 bytes)                    │
│  Magic: "SPP" (3B) │ Version (1B) │ Type (1B) │ Flags (1B) │
│  Header Length (2B) │ Payload Length (4B) │ CRC32 (4B)      │
├─────────────────────────────────────────────────────────────┤
│                    INTENT HEADER (4 bytes)                   │
│  Reliability (1B) │ Latency (1B) │ Ordering (1B)           │
│  Priority (1B)                                              │
├─────────────────────────────────────────────────────────────┤
│                    ROUTING STATE                             │
│  Destination     │ CurrentNode    │ HopIndex                │
│  MaxHops (TTL)   │ HopCount      │ Degraded │ Rerouted     │
├─────────────────────────────────────────────────────────────┤
│                    PLANNED PATH                              │
│  [ "sender", "router_a", "router_c", "receiver" ]          │
├─────────────────────────────────────────────────────────────┤
│                    MINI MAP (network topology)               │
│  Link: sender → router_a   lat=1ms    load=5%   loss=0%    │
│  Link: router_a → router_b lat=5ms    load=90%  loss=8%    │
│  Link: router_a → router_c lat=5ms    load=10%  loss=0%    │
│  Link: router_b → receiver lat=100ms  load=90%  loss=8%    │
│  Link: router_c → receiver lat=5ms    load=10%  loss=0%    │
├─────────────────────────────────────────────────────────────┤
│                    CONGESTION LOG (audit trail)              │
│  Hop 1: router_a   load=15.0%  latency=1.2ms               │
│  Hop 2: router_c   load=10.0%  latency=4.8ms               │
├─────────────────────────────────────────────────────────────┤
│                    VISITED NODES (loop detection)            │
│  { "router_a": true, "router_c": true }                    │
├─────────────────────────────────────────────────────────────┤
│                    PAYLOAD                                    │
│  "player_position:x=100,y=200"                              │
└─────────────────────────────────────────────────────────────┘
```

### Intent Header — What The Packet Needs

| Field | Values | Effect on Routing |
|-------|--------|-------------------|
| **Reliability** | 0=none, 1=best-effort, 2=guaranteed | At level 2: loss penalty ×10 in weight calculation |
| **Latency** | 0=relaxed, 1=normal, 2=low, 3=critical | Higher = latency weighted more heavily in Dijkstra |
| **Ordering** | 0=none, 1=partial, 2=strict | Controls whether packets can take different paths |
| **Priority** | 0=low, 1=medium, 2=high, 3=critical | Determines queue position at routers |

### Packet Types

| Type | Code | Purpose |
|------|------|---------|
| DATA | 0 | Normal data packet |
| GOSSIP | 1 | Topology state exchange between routers |
| PROBE | 2 | Latency measurement ping/pong |
| ACK | 3 | Delivery acknowledgment |

---

## 4. Packet Lifecycle — Step by Step

### Phase A: Creation (at Sender)

```
1. QUERY TOPOLOGY
   Sender contacts nearest router's gossip port → receives live link states
   Fallback: use cached/hardcoded map if no gossip available

2. BUILD PACKET
   SmartPacket {
     Intent:      {Latency: 3, Reliability: 1, Priority: 3}
     MiniMap:     [5 links with live measurements]
     Destination: "receiver"
     Payload:     "player_position:x=100,y=200"
     MaxHops:     16
   }

3. CALCULATE PATH (Dijkstra runs INSIDE the packet)
   Graph edges weighted by intent:
     Critical latency: weight = latency×2.0 + load×1.5
     Relaxed:          weight = load×0.3
   Result: ["sender", "router_a", "router_c", "receiver"]

4. SERIALIZE & SEND
   Encode to SPP wire format → UDP to first hop (router_a)
```

### Phase B: Transit (at each Router)

```
For each router the packet passes through:

1. RECEIVE & DECODE
   Read UDP → Decode wire format → Validate CRC32

2. CHECK TTL
   If HopCount >= MaxHops → DROP packet (prevents infinite loops)

3. STAMP CONGESTION (real measurements from metrics/)
   Read current CPU load from /proc/stat
   Read current network load from /proc/net/dev
   Append HopRecord { NodeName, LoadPct, LatencyMs } to CongestionLog
   Add self to VisitedNodes
   Increment HopCount

4. CHECK FOR REROUTE
   Compare stamped values vs what MiniMap predicted:
     If divergence > 30% threshold:
       a. Get fresh link states from gossip topology
       b. Merge fresh data into packet's MiniMap
       c. Re-run Dijkstra from CURRENT node
       d. Update PlannedPath
       e. Set Rerouted = true

5. DETERMINE NEXT HOP
   Read PlannedPath[HopIndex + 1]

6. LOOP DETECTION
   If next hop is in VisitedNodes:
     → ForceForward: pick least-congested unvisited neighbor
     → Set Degraded = true

7. ENCODE & FORWARD
   Serialize updated packet → UDP to next hop's DataAddr
```

### Phase C: Delivery (at Receiver)

```
1. RECEIVE & DECODE
   Read UDP → Decode → Display payload

2. READ AUDIT TRAIL
   CongestionLog shows what every router observed
   Rerouted flag indicates mid-flight path change
   Degraded flag indicates forced routing (loop recovery)

3. SEND ACK (if AckRequired)
   Create ACK packet → send back toward SourceNode
```

---

## 5. Routing Engine — Intent-Aware Dijkstra

### Weight Calculation

The same link gets **different weights** depending on the packet's intent:

```
Link: router_a → router_b (latency=5ms, load=90%, loss=8%)

Latency-Critical packet (Latency=3):
  weight = 5×2.0 + 90×1.5 = 10 + 135 = 145.0

Relaxed packet (Latency=0):
  weight = 90×0.3 = 27.0

Reliability-Guaranteed (Reliability=2):
  Additional penalty: + 8×10.0 = +80.0
```

This means a latency-critical gaming packet will **avoid** the congested router_b (weight 145), while a relaxed file transfer might **use** it (weight 27) because it doesn't care about latency.

### Rerouting Decision

At each hop, this comparison triggers rerouting:

```
map_says:    router_a → router_b  latency = 5ms,  load = 10%
actual:      router_a → router_b  latency = 50ms, load = 80%

divergence = |50 - 5| / 5 × 100 = 900%
threshold  = 30%

900% > 30% → REROUTE TRIGGERED
```

The packet then re-runs Dijkstra using updated link data from gossip, starting from its current position.

---

## 6. Gossip Protocol — How The Map Stays Alive

### How It Works

```
Every broadcast cycle, each router:
  1. Reads its own metrics (CPU load, network load, neighbor latency)
  2. Creates LinkState entries: { From: "self", To: "neighbor", LatencyMs, LoadPct, LossPct, Sequence, Timestamp }
  3. Broadcasts only CHANGED LinkStates to all neighbors via UDP (delta mode)
  4. Every 5 seconds: broadcasts ALL known LinkStates as a full-sync safety net

Upon receiving gossip from a neighbor:
  1. Compare each incoming LinkState's Sequence number with local copy
  2. If incoming Sequence > local Sequence → update (newer wins)
  3. If incoming Sequence < local Sequence → ignore (stale)

Result: within 100-200ms, ALL routers converge on the same topology view
```

### Delta-Based Gossip (Scaling Optimization)

Instead of broadcasting the entire topology every cycle, routers track what changed since the last broadcast and send only the delta:

```
Full-state mode (old):    Every 50ms → send ALL LinkStates (O(E) bandwidth)
Delta mode (current):     Every 50ms → send only CHANGED LinkStates (O(changed) bandwidth)
Full-sync safety net:     Every 5s   → send ALL LinkStates (prevents drift)

At 500 nodes with ~2000 links:
  Full-state: ~160 KB × 20/sec × 5 neighbors = ~16 MB/s per router
  Delta:      ~400 bytes × 20/sec × 5 neighbors = ~40 KB/s per router (99.7% reduction)
```

The `GossipMessage` carries an `IsDelta` flag so receivers know whether the message is a partial update or a complete state replacement. Both are merged using the same sequence-number-wins logic.

### Adaptive Gossip Frequency

The broadcast interval adjusts automatically based on network stability:

| Network State | Interval | Rationale |
|---|---|---|
| **Changes detected** | 50ms (fast) | Converge quickly when topology shifts |
| **Stable (no changes)** | 500ms (slow) | Reduce overhead 10× when nothing is happening |
| **Full-sync cycle** | Every 5000ms | Safety net to prevent accumulated drift |

This means idle networks produce ~10× less gossip traffic, while actively changing networks still converge in ~100-200ms.

### Staleness Management

| Age of LinkState | Treatment |
|--------------------|-----------|
| 0–300ms | **Fresh** — used as-is |
| 300ms–1000ms | **Unreliable** — 50% weight penalty applied |
| >1000ms | **Pruned** — removed from topology |

### Convergence Example (5-node network)

```
t=0ms    router_a measures: load=15%, latency to router_c=2ms (change detected)
t=50ms   router_a broadcasts DELTA: [router_a→router_c updated] to router_b, router_c
t=50ms   router_b measures: load=90%, latency to receiver=100ms (change detected)
t=50ms   router_b broadcasts DELTA: [router_b→receiver updated] to router_a
t=100ms  router_a now knows about router_b's congestion
t=100ms  router_c receives router_a's delta, merges
t=150ms  All routers have full, consistent topology
t=200ms+ No changes → interval relaxes to 500ms (adaptive)
t=5000ms Full-sync broadcast ensures no drift
```

---

## 7. Metrics Collection — What Gets Measured

### System Load (from `/proc/stat`)

```
CPU Load % = (total_ticks - idle_ticks) / total_ticks × 100

Read twice with interval → compute delta for accuracy
```

### Network Load (from `/proc/net/dev`)

```
Network Load % = (rx_bytes + tx_bytes per second) / link_capacity × 100

link_capacity = 1 Gbps for veth pairs (configurable)
```

### Combined Load (stamped into packets)

```
SystemLoad = CPU × 0.4 + NetworkAvg × 0.6

(Network weighted higher because routers are network-bound, not CPU-bound)
```

### Neighbor Latency (UDP probes)

```
Every 100ms:
  Send: "PING:<nanosecond_timestamp>" to neighbor
  Receive: "PONG:<original_timestamp>" back
  RTT = now - original_timestamp

Loss tracking: sliding window of last 20 probes
  LossPct = (20 - successful) / 20 × 100
```

---

## 8. Wire Format — Binary Specification

```
Offset  Size  Field           Description
─────────────────────────────────────────────────────
0       3     Magic           "SPP" (0x53 0x50 0x50)
3       1     Version         Protocol version (1)
4       1     PacketType      0=DATA 1=GOSSIP 2=PROBE 3=ACK
5       1     Flags           Bit 0: Degraded, Bit 1: Rerouted
6       2     HeaderLen       Length of variable header
8       4     PayloadLen      Length of payload
12      4     Checksum        CRC32 of (header + payload)
16      N     VariableHeader  Intent, routing state, map, log
16+N    M     Payload         Application data
```

Strings are length-prefixed: `[uint16 length][bytes]`

The wire format is ~55% smaller than gob encoding (410 bytes vs 911 bytes for a typical packet in our 5-node testbed).

---

## 9. Deployment Model

### Single Machine (Current Testbed)

```
WSL2 Ubuntu — Linux network namespaces + veth pairs

  ┌─────────┐   veth    ┌──────────┐   veth    ┌──────────┐
  │ ns:     │◄────────►│ ns:      │◄────────►│ ns:      │
  │ sender  │           │ router_a │           │ router_b │
  │ 10.0.1.1│           │ 10.0.1.2 │           │ 10.0.2.2 │
  └─────────┘           │ 10.0.2.1 │           │ 10.0.4.1 │
                        │ 10.0.3.1 │           └─────┬────┘
                        └─────┬────┘                 │
                              │ veth                  │ veth
                        ┌─────┴────┐           ┌─────┴──────┐
                        │ ns:      │           │ ns:        │
                        │ router_c │           │ receiver   │
                        │ 10.0.3.2 │           │ 10.0.4.2   │
                        │ 10.0.5.1 │◄────────►│ 10.0.5.2   │
                        └──────────┘   veth    └────────────┘

Each namespace runs its own binary with its own config.
tc netem applied to simulate congestion on specific links.
```

### Multi-Machine (Production)

```
Each physical machine / VM / container runs:
  - One router binary (with metrics + gossip)
  - Configured with real IP addresses of neighbors
  - Gossip auto-discovers topology

Applications link against the packet/ library:
  - Import "smartpacket/packet"
  - Create SmartPacket with appropriate intent
  - Send to nearest router

No central controller. No route table management.
The network is self-organizing via gossip.
```

### Port Allocation

| Port Range | Purpose |
|------------|---------|
| 8000–8999 | Data plane (smart packet forwarding) |
| 7000–7999 | Gossip plane (topology state sharing) |
| 6000–6999 | Probe plane (latency measurement) |
| 9000 | Receiver (application delivery) |

---

## 10. How SPP Differs From Existing Protocols

| Feature | TCP | UDP | QUIC | **SPP** |
|---------|-----|-----|------|---------|
| Routing decisions by | Router/OS | Router/OS | Router/OS | **The packet itself** |
| Knows data intent | No | No | No | **Yes — per packet** |
| Carries network map | No | No | No | **Yes** |
| Self-reroutes mid-flight | No | No | No | **Yes** |
| Congestion awareness | After-the-fact (backoff) | None | After-the-fact | **Proactive (avoids before hitting)** |
| Recovery from path failure | 30-40s (OSPF reconvergence) | N/A | Connection migration | **~50ms (one stamp, one reroute)** |
| Priority between data types | No (same queue) | No | Stream priorities | **Yes — intent-based queuing** |
| Audit trail per packet | No | No | No | **Yes — full congestion log** |

### What SPP is NOT

- **Not a replacement for TCP/UDP** at the socket layer — SPP runs alongside them
- **Not dependent on SDN controllers** — fully distributed, no central coordinator
- **Not limited to one network type** — works on any IP network with SPP-aware routers

---

## 11. Source Code Map

```
smartpacket/
├── packet/                    ← CORE PROTOCOL
│   ├── packet.go              ← SmartPacket struct, rerouting, loop detection
│   ├── dijkstra.go            ← Intent-aware shortest path algorithm
│   ├── wire.go                ← Binary wire format (encode/decode)
│   ├── serialize.go           ← Gob encoding (backward compat)
│   └── packet_test.go         ← 14 tests
│
├── metrics/                   ← MEASUREMENT INFRASTRUCTURE
│   ├── system.go              ← CPU + network load from /proc/
│   ├── probe.go               ← UDP latency probes to neighbors
│   ├── collector.go           ← Background collection goroutine
│   └── metrics_test.go        ← 4 tests
│
├── gossip/                    ← DISTRIBUTED STATE SHARING
│   ├── state.go               ← Thread-safe topology database
│   ├── gossip.go              ← Broadcast/receive protocol
│   ├── staleness.go           ← TTL expiry and pruning
│   └── gossip_test.go         ← 6 tests
│
├── cmd/                       ← RUNNABLE BINARIES
│   ├── router/main.go         ← Router node (metrics + gossip + forwarding)
│   ├── sender/main.go         ← Packet sender (creates + routes)
│   └── receiver/main.go       ← Packet receiver (delivery + audit)
│
├── configs/                   ← NODE CONFIGURATIONS
│   ├── router_a.json
│   ├── router_b.json
│   ├── router_c.json
│   └── sender.json
│
├── main.go                    ← Demo runner (all features)
├── go.mod                     ← Go module definition
└── SPP_Architecture.md        ← This document
```

---

## 12. Key Algorithms Reference

### Dijkstra Weight Formula

```
weight(link, intent) =
  CASE intent.Latency:
    3 (critical): latency×2.0 + load×1.5
    2 (low):      latency×1.5 + load×1.0
    1 (normal):   latency×1.0 + load×0.5
    0 (relaxed):  load×0.3
  
  IF intent.Reliability >= 2:
    weight += loss×10.0
```

### Reroute Threshold

```
divergence_pct = |actual - map_value| / map_value × 100

IF divergence_pct > 30% for load OR latency:
  → Trigger reroute
  → Merge fresh gossip data into MiniMap
  → Re-run Dijkstra from current node
```

### Gossip Merge Rule

```
IF incoming.Sequence > local.Sequence → ACCEPT (newer data wins)
IF incoming.Sequence < local.Sequence → REJECT (stale data)
IF incoming.Sequence == local.Sequence AND incoming.Timestamp > local.Timestamp → ACCEPT
```

### Loop Detection

```
IF VisitedNodes[nextHop] == true:
  → ForceForward: pick neighbor with lowest (latency + load) that is NOT in VisitedNodes
  → If ALL neighbors visited: pick absolute lowest weight neighbor
  → Set Degraded = true
```

---

> *SPP v1.0 — The packet is the intelligence.*
