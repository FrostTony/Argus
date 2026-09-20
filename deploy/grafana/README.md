# Grafana

[argus-checks.json](argus-checks.json) — one dashboard for everything a node
checks. Eighty-eight panels across eleven rows; the three that matter open by
default and the rest stay folded until a probe type is the question.

## Import

Dashboards → New → Import → upload the file, then pick the Prometheus data
source. Nothing else is required: the data source is a variable, so the same
file works against Prometheus, VictoriaMetrics or Thanos without editing.

Provisioned instead:

```yaml
# /etc/grafana/provisioning/dashboards/argus.yaml
apiVersion: 1
providers:
  - name: argus
    folder: Argus
    type: file
    allowUiUpdates: false
    options:
      path: /var/lib/grafana/dashboards/argus
```

## Variables

| Variable | What |
|---|---|
| `datasource` | the Prometheus-compatible source holding the node's series |
| `node` | `node.name` of the agent that took the measurement |
| `probe_type` | `http`, `tls`, `dns`, `icmp`, `tcp`, `unix`, `udp`, `websocket`, `grpc`, `domain`, `external` |
| `probe` | the check, by the name it has in `probes.d/` |
| `target` | the target; its addresses stay in the `backend` label |

`prefix` is a sixth, hidden variable, empty by default because probe series
carry no prefix. A node configured with `surfacers[].prometheus.prefix` needs
that value set once, in the dashboard's variable list — every probe query reads
it. The node's own counters are `argus_*` whatever the prefix is, so those
queries name it directly.

The scrape has to keep the node's labels, so no `honor_labels: false`
rewriting of `probe`, `target` or `backend`:

```yaml
  - job_name: argus
    static_configs:
      - targets: ["argus-node:6767"]
```

## Rows

| Row | Answers |
|---|---|
| **Overview** | is anything down, how available was the period, how slow is the tail |
| **Targets** | one band per target over time, one line per target with every number it has, and what the `expect` patterns matched |
| **Latency** | quantiles against the probe's timeout, the distribution as a heatmap, and the same time split into `connect`, `tls`, `write`, `ttfb`, `transfer` |
| **Name resolution and addresses** | lookups, TTLs, family fallbacks, and one row per address of a target |
| **HTTP** | status code over time, response classes, TTFB, body size, redirects, validators, document age |
| **TLS** | days left in the leaf and in the chain, validity, revocation over OCSP, and the certificate itself — issuer, subject, cipher, ALPN, SHA-256 |
| **DNS** | queries and failures per resolver, TTL, SOA serial, whether the resolvers agree, the answer set itself, and connect against query for DoT and DoH |
| **ICMP** | loss, RTT spread, jitter, reply hop limit, duplicate replies |
| **Domains** | days until the registration expires, registrar, EPP status, transfer lock |
| **TCP · unix · UDP · WebSocket · gRPC · external** | bytes read, the websocket upgrade and the subprotocol chosen, health status, exit codes |
| **The agent itself** | uptime, heap, series held and dropped, concurrency slots, skipped runs, discovery age, build |

Two annotations sit on the time axis: configuration reloads, on by default,
and rejected reloads, off until wanted.

## Tables

The wide tables — **Targets**, **Addresses**, **Certificates**, **Domains** —
merge several instant queries into one row per target. That means a column is
empty rather than wrong when a probe does not produce the series: a `tcp`
probe has no `TLS days`, and the cell stays blank.

The `*_info` series carry their string in a `val` label, which would collide
between metrics. Each one is read as
`group by (probe, target, <name>) (label_replace(…, "<name>", "$1", "val", "(.*)"))`,
so it arrives as a column named after itself and joins cleanly with the rest.

## Adding a panel

Edit in Grafana, then export with **Export for sharing externally** off and
commit the JSON. Keep `uid: argus-checks`, keep `${datasource}` and
`${prefix}` out of hardcoded queries, and filter on
`{node=~"$node",probe=~"$probe",probe_type=~"$probe_type",target=~"$target"}`
so the new panel follows the same selection as the rest.
