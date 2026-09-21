<p align="center">
  <img src="argus-logo.png" alt="" width="200">
</p>

<h1 align="center">Argus</h1>

<p align="center">
  Active monitoring for domains. Argus resolves every target itself, probes each
  address behind it separately, and exports the result as a labelled Prometheus
  metric stream.
  <br><br>
  A single static binary. No agent registry, no database, no external dependency
  at runtime.
</p>

## Why per-address

A domain is not a server. `www.google.com` with four A records is four machines,
and when one of them stops answering the site is broken for a quarter of its
visitors while every aggregate check stays green.

Argus does the fan-out in the core: the runner resolves the target, takes every
address, and probes them one at a time, labelling each series with `backend`.

```
probe_up{probe="site",target="https://www.google.com/",backend="142.250.74.46:443",family="ipv4"} 1
probe_up{probe="site",target="https://www.google.com/",backend="142.250.74.78:443",family="ipv4"} 0
target_backends{probe="site",target="https://www.google.com/"}     2
target_backends_up{probe="site",target="https://www.google.com/"}  1
```

Name resolution is measured as its own step, and an HTTP request is broken into
the phases that make up its latency:

```
resolve_last_duration_seconds                        0.00104
probe_phase_last_duration_seconds{phase="connect"}   0.00021
probe_phase_last_duration_seconds{phase="tls"}       0.00208
probe_phase_last_duration_seconds{phase="write"}     0.00014
probe_phase_last_duration_seconds{phase="ttfb"}      0.00057
probe_phase_last_duration_seconds{phase="transfer"}  0.00010
```

Every timing is exported twice: as a histogram for quantiles, and as a
`*_last_duration_seconds` gauge for the value of the most recent run.

## Install

```bash
go install github.com/tonyamdfrost-cmd/Argus/cmd/argus@latest
```

Or build from a checkout — Go 1.24, no code generation, no build tags:

```bash
make build      # ./argus          (also: make test, make race, make lint)
```

## Quick start

The configuration under `examples/` is real: its checks reach the public
internet, so the status page has something on it from the first run.

```bash
make validate   # parse examples/ and print the probes, targets and schedules
make run        # start a node on that configuration
```

`make run` is a shortcut for the full command, which takes a single file just as
happily as a directory:

```bash
./argus run -config examples/argus.yaml -probes examples/probes.d/example.yaml
```

Then:

```
http://localhost:6767/         status page: probes, targets, backends, charts
http://localhost:6767/metrics  Prometheus exposition
```

`-probes` accepts a file or a directory and may be repeated. A directory is the
usual choice in production (`-probes /etc/argus/probes.d`); a single file is
easier while writing one check.

`-state <file>` is the other way to run the node: a configuration accepted
through `PUT /api/config` is written there, and at startup that file wins over
`-probes`. A node driven by a controller therefore comes back from a restart
with what it was pushed rather than with whatever is on disk, and a node that
has never been pushed to starts empty and reports `config_source: none` instead
of refusing to start. A state file that cannot be parsed is logged and left in
place, and the node falls back to `-probes`.

Before running a new check as a daemon, run it once:

```bash
./argus oneshot -probes examples/probes.d/example.yaml
./argus oneshot -probes examples/probes.d/example.yaml -probe example_dns
```

`oneshot` executes the same code as `run` and prints the metrics to stdout,
without daemonising and without the sinks that would ship a debugging run to
production storage.

ICMP checks on Linux need `sysctl net.ipv4.ping_group_range` to cover the
running user, or `icmp.privileged: true` and `CAP_NET_RAW`. On macOS they work
as they are.

## Two configurations

They change on different schedules, so they are separate files.

**`argus.yaml`** — the node: its name and labels, the resolver, scheduler
limits, sinks, logging. Changes at deploy time. See
[examples/argus.yaml](examples/argus.yaml).

**`probes.d/*.yaml`** — the checks. A directory is read as one configuration, so
checks can be split per project or per team.

Inheritance runs `defaults` → `template` → the probe itself. An unknown field is
an error, not a silent ignore.

```yaml
defaults:
  interval: 30s
  timeout: 5s

templates:
  web:
    type: http
    http:
      follow_redirects: true
      validators:
        - {name: status, status_code: ["200-399"]}
        - {name: cert, cert_min_days: 14}

probes:
  - name: google
    template: web
    labels: {project: demo, team: sre}
    targets:
      - https://www.google.com/
      - url: https://dns.google/resolve?name=google.com&type=A
        labels: {kind: api}
```

`${VAR}` in a configuration file is replaced from the environment, which is how
secrets stay out of the file. Secrets can also be read from disk on each use —
`password_file`, `token_file`, `client_secret_file` — so rotating one needs no
restart.

## Probe types

`argus kinds` lists them. Every type takes its options in a block named after
itself.

| Type | Checks | Notable options |
|---|---|---|
| `http` | status, body, headers, JSON fields, redirect chain, certificate | `method`, `headers`, `body`, `auth`, `follow_redirects`, `read_body`, `http2`, `compression`, `proxy_url`, `proxy_from_environment`, `no_proxy`, `validators` |
| `dns` | answers, rcode, authority and additional sections, drift between resolvers | `servers`, `proto` (`udp`/`tcp`/`tls`/`https`), `query_type`, `min_answers`, `expect`, `valid_rcodes`, `require_authoritative`, `compare_servers` |
| `tls` | the certificate on the connection, apart from any request | `port`, `min_days_left` |
| `tcp` | the port, or a scripted conversation over it | `port`, `steps` (`send`/`expect`/`starttls`), `tls` |
| `unix` | a unix-domain socket, and the conversation over it | `network` (`unix`/`unixgram`/`unixpacket`), `path`, `steps`, `tls`, `read_bytes` |
| `udp` | a datagram and its reply, or the ICMP refusal from a closed port | `port`, `send`, `expect`, `unreachable_wait` |
| `websocket` | the RFC 6455 upgrade, and optionally a message over the open socket | `path`, `tls`, `subprotocols`, `origin`, `send`+`expect`, `ping` |
| `icmp` | loss and jitter per address | `packets`, `interval`, `max_loss_percent`, `ttl`, `tos`, `privileged` |
| `grpc` | the health service, spoken directly over HTTP/2 | `port`, `service`, `plaintext`, `authority` |
| `domain` | the registration behind the domain over RDAP: expiry, registrar, EPP status | `min_days`, `require_status`, `forbid_status` |
| `external` | anything that exits 0 and prints `name value` lines | `command`, `args`, `env`, `mode`, `metric_prefix` |

HTTP authentication covers basic, bearer and the OAuth2 client-credentials flow.
A `validators:` list replaces the default "2xx is fine" with named assertions,
and each one that fails names itself in the failure message.

Options that apply to any type: `interval`, `timeout`, `labels`, `ip_version`,
`source_ip`, `hostname` (the name presented to the target — Host header and SNI
— when it differs from the target's own), `requests_per_probe`, `negative_test`
(success means being refused), `run_on` (a regular expression over the node
name, so one file can be deployed fleet-wide), and `schedule` — time windows
with a timezone, for checks that should not page anyone at 04:00.

Any probe that speaks TLS takes `check_revoked: true` in its `tls_config`: an
expiry date says nothing about a key that leaked last week. The answer comes
from the stapled OCSP response when the server sends one, and from the
responder named in the certificate otherwise. A responder that cannot be
reached, or one whose answer is past its `nextUpdate`, is reported and not
failed — otherwise every service would depend on a third party's uptime, and a
replayed "good" would read as one. A revocation is not undone by the answer
being old. Responder answers are cached for an hour or until they go stale,
whichever is sooner, and the time spent asking is left out of
`probe_duration_seconds`: the responder is not the target.

[examples/probes.d/example.yaml](examples/probes.d/example.yaml) is a working
file with every option demonstrated and commented.

## Targets

A target is a URL, a `host`/`port` pair, or a shorthand string. Instead of a
list it can name a source:

```yaml
    targets:
      file:
        path: /etc/argus/targets.json
      refresh: 5m
```

The source — a file or an HTTP endpoint — is re-read on a schedule, and a failed
read keeps the previous list rather than emptying the probe. Labels from the
source are merged into the target's own.

## Metrics

Probe series carry no prefix, so `probe_success`, `probe_duration_seconds` and
`probe_up` are the names blackbox_exporter dashboards and alerting rules already
use. `surfacers[].prometheus.prefix` adds one back when several nodes write into
one store through remote-write and their series must not meet. The node's own
counters are `argus_*` whatever that prefix is: the prefix namespaces what the
node measures, and the node is not one of its own targets.

Every series is labelled with `probe`, `probe_type` and `target`; a series that
belongs to one address also carries `backend` and `family`.

### Every probe

| Metric | Shows | How |
|---|---|---|
| `probe_up` | whether the last run of this backend succeeded | 1 or 0, written once per run |
| `probe_total` | attempts | +1 per run per backend |
| `probe_success_total` | attempts that passed | +1 when the run returned no error |
| `probe_failure_total{reason}` | attempts that failed, by class | +1 with `reason` one of `dns`, `connect`, `tls`, `timeout`, `status`, `content`, `protocol`, `internal`, `unknown` |
| `probe_duration_seconds` | run latency, as a histogram | the prober's own time; name resolution excluded |
| `probe_last_duration_seconds` | the latest run's latency | the same measurement as a gauge |
| `probe_phase_duration_seconds{phase}` | latency split by phase: sum and count (the mean); a histogram with `phase_histograms: true` | `connect`, `tls`, `write`, `ttfb`, `transfer`, from the connection trace |
| `probe_phase_last_duration_seconds{phase}` | the latest phase timings | the same as a gauge |
| `probe_timeout_seconds` | the deadline one request runs under | the probe's effective `timeout`, so a latency graph shows its ceiling |
| `probe_expect_info{…}` | what a pattern matched | one label per **named** capture group in an `expect`, `body_regex`, `header_regex` or `answer_regex`; no named groups, no series. A group named after an identity label (`target`, `backend`, `probe`, …) is dropped: the value is the server's text |
| `target_backends` | addresses the target resolved to | count of the answer, capped by `resolver.max_backends` |
| `target_backends_up` | how many of them answered | count of backends whose run passed |
| `resolve_total`, `resolve_failure_total{server}` | resolutions performed and failed | +1 per real lookup; cache hits are not counted |
| `resolve_duration_seconds{server}`, `resolve_last_duration_seconds` | how long resolution took | measured around the lookup, cache hits excluded |
| `resolve_ttl_seconds` | TTL of the answer the backends came from | smallest TTL in the response, clamped by `min_ttl`/`max_ttl` |
| `resolve_fallback_total{family}` | targets answered by the other address family | +1 when `ip_fallback` had to switch families |
| `probe_backends_down` | addresses of this probe not answering right now | backends whose last run failed |
| `probe_runs_skipped` | runs dropped because the previous one was still going | +1 per overlap |
| `probe_runs_paused`, `probe_sleeping` | runs the `schedule` suppressed, and whether it is suppressing now | +1 per skipped tick; the gauge is 1 inside a closed window |
| `probe_panics` | prober calls that panicked | +1, and the run is failed rather than the node |

### `http`

| Metric | Shows | How |
|---|---|---|
| `http_status_code` | status of the last response | the code as a number |
| `http_content_length` | what the server declared | `Content-Length`, absent when the server sent none |
| `http_response_size_bytes` | what was actually read | bytes read from the body, stopping at twice `max_body_bytes` or the largest `max_size_bytes` |
| `http_redirects` | hops followed | count of 3xx responses followed before the final one |
| `http_connection_reused` | whether the socket was already open | 1 when the transport reused a connection |
| `http_validator{validator}` | which assertions passed | 1 or 0 per named entry in `validators:` |
| `http_proto_info{val}` | protocol of the final response | `HTTP/1.1`, `HTTP/2.0` |
| `http_final_url_info{val}` | where the redirect chain ended | only with `expose_final_url: true` |
| `http_last_modified_seconds` | age of the served document | `Last-Modified` as a unix timestamp |
| `http_ssl` | whether the request was over TLS | 1 or 0 |

Over TLS an `http` probe also emits the whole `tls_*` set below, taken from the
handshake it already performed.

### `tls`

The certificate on the connection, without a request. Also emitted by `http`
and by a `tcp` probe that upgrades with `starttls`.

| Metric | Shows | How |
|---|---|---|
| `tls_cert_expiry_days` | life left in the leaf certificate | `notAfter` minus now, in days; negative once expired |
| `tls_chain_expiry_days` | life left in the weakest link | the same for the first certificate in the chain to expire, so an expiring intermediate is visible |
| `tls_cert_valid` | whether it is usable right now | 1 when the chain verifies against the configured name and the clock |
| `tls_chain_length` | certificates the server sent | length of the presented chain |
| `tls_cert_san_count` | names the certificate covers | number of SAN entries |
| `tls_ocsp_stapled` | whether revocation came with the handshake | 1 when the server stapled a response |
| `tls_cert_revoked` | whether the certificate still stands | with `check_revoked: true`, 1 when the responder says it was revoked — and the check fails |
| `tls_ocsp_status_info{val}` | what the responder said | `good`, `revoked`, `unknown`, or `unavailable` when it could not be asked |
| `tls_ocsp_next_update_seconds` | how current that answer is | the responder's `nextUpdate`, as a unix timestamp |
| `tls_cert_fingerprint_info{val}` | the exact certificate | SHA-256 of the leaf, so a change is a diff rather than a guess |
| `tls_cert_issuer_info{val}`, `tls_cert_subject_info{val}` | who issued it and to whom | fields of the leaf, as labels |
| `tls_version_info{val}`, `tls_cipher_info{val}`, `tls_alpn_info{val}` | what was negotiated | `TLS 1.3`, the cipher suite, the ALPN protocol |

### `dns`

Every resolver in `servers:` is queried separately and labelled `resolver`;
`qtype` carries the record type. `proto:` picks the pipe — `udp` (the default),
`tcp`, `tls` for DNS over TLS on :853, or `https` for DNS over HTTPS, where
`servers:` are `https://` endpoints. A resolver behind DoT checked over :53/udp
is a check of a path nobody uses; over TLS the resolver's own certificate is
reported through the `tls_*` series. The round trip is split into the
`connect` and `query` phases.

| Metric | Shows | How |
|---|---|---|
| `dns_queries_total{resolver}`, `dns_query_failures_total{resolver}` | queries sent and failed | +1 per query; a resolver that times out counts as a failure |
| `dns_answers` | records returned | size of the answer section |
| `dns_ttl_seconds` | how long the answer may be cached | smallest TTL in the answer |
| `dns_rcode_info{val}` | the response code | `NOERROR`, `NXDOMAIN`, … |
| `dns_answer_info{val}` | the answer itself | the record set joined into one label, TTLs stripped so it changes only when the data does |
| `dns_authority_rrs`, `dns_additional_rrs` | size of the other sections | record counts, where a delegation's NS set lives |
| `dns_authoritative` | whether the zone answered, not a cache | 1 when the AA flag is set |
| `dns_soa_serial` | the zone's serial | the SOA serial as a number, so a lagging secondary is a comparison in Prometheus |
| `dns_consistent` | whether the resolvers agree | with `compare_servers: true`, 1 when every resolver returned the same record set |
| `dns_resolvers_failed` | how many resolvers did not answer | count, for a probe that tolerates partial failure |

### `icmp`

| Metric | Shows | How |
|---|---|---|
| `icmp_packets_sent`, `icmp_packets_received` | echo requests and replies | `packets:` per run, and how many came back |
| `icmp_packet_loss_percent` | loss over this run | `(sent − received) / sent × 100` |
| `icmp_rtt_min_seconds`, `icmp_rtt_max_seconds`, `icmp_rtt_avg_seconds` | round-trip spread | smallest, largest and mean over the replies received |
| `icmp_rtt_jitter_seconds` | how unsteady the path is | mean absolute deviation from the average RTT |
| `icmp_reply_hop_limit` | hops on the return path | the TTL the reply arrived with |
| `icmp_duplicates` | replies to a packet already answered | +1 per duplicate, which a broadcast address or an anycast set produces |

### `domain`

Registration data over RDAP, answered by the registry rather than the host.

| Metric | Shows | How |
|---|---|---|
| `domain_expiry_days` | life left in the registration | the expiry event minus now, in days |
| `domain_expiry_seconds`, `domain_registered_seconds` | the registration window | RDAP event dates as unix timestamps |
| `domain_age_days` | how long the name has existed | now minus the registration date |
| `domain_locked` | whether the transfer lock is on | 1 when the status list carries a `clientTransferProhibited`-style lock |
| `domain_registrar_info{val}`, `domain_status_info{val}` | who holds it and in what state | registrar name and the EPP status codes, as labels |
| `domain_lookup_duration_seconds` | how long the registry took | measured around the RDAP request |

### `tcp`, `unix`, `udp`, `websocket`, `grpc`, `external`

| Metric | Shows | How |
|---|---|---|
| `tcp_response_size_bytes` | bytes read during the conversation | sum over the `steps:` that read |
| `unix_response_size_bytes` | bytes read over the socket | the same, for a `unix` probe |
| `udp_response_size_bytes` | size of the reply datagram | bytes received, 0 when the probe waited for an ICMP refusal instead |
| `websocket_handshake_status_code` | how the upgrade was answered | 101 is the only success; a proxy that forwards HTTP and drops `Upgrade` answers 200 |
| `websocket_subprotocol_info{val}` | the subprotocol the server chose | the `Sec-WebSocket-Protocol` it echoed back |
| `websocket_response_size_bytes` | size of the message that came back | bytes in the frame, with `send`/`expect` |
| `grpc_serving_status` | the health service's verdict | the `ServingStatus` enum: 1 is `SERVING` |
| `grpc_status_info{val}` | the gRPC status code | the `grpc-status` trailer |
| `external_exit_code` | how the command ended | the process exit status |
| `external_metrics_parsed` | how many lines it produced | count of `name value` lines accepted from stdout |

In `mode: output` the command's own metrics are emitted too, under
`metric_prefix` (`external_` by default) with the name sanitised, so a command
can report whatever it measures.

### The node itself

Always under `argus_`, whatever the exposition prefix is: these belong to the
agent rather than to anything it checks.

`argus_build_info{val}`, `argus_uptime_seconds`, `argus_goroutines`,
`argus_memory_bytes`, `argus_series`, `argus_series_dropped`,
`argus_probes_configured`, `argus_probe_targets`, `argus_probe_slots_used`,
`argus_probe_slots_total`, `argus_config_reloads`,
`argus_config_reload_failures`, `argus_discovery_failures`,
`argus_discovery_age_seconds`.

Cardinality is bounded on purpose. `max_series` caps the store; samples beyond
it are refused and counted in `argus_series_dropped`. Series for a target that
disappears stop being written and are retired after `stale_after`.

### Sinks

`surfacers:` in the node configuration, any number of them at once:

- **`prometheus`** — the `/metrics` endpoint. Default.
- **`file`** — one JSON object per measurement, appended to a file. Reopened on
  SIGHUP, so logrotate works.
- **`remote_write`** — Prometheus remote-write, batched, with retries and
  exponential backoff. A 4xx is permanent and is not retried.

## HTTP surface

| Path | Auth | What |
|---|---|---|
| `GET /metrics` | none | Prometheus exposition |
| `GET /`, `GET /status` | none | status page |
| `GET /status.json`, `GET /status/data` | none | the same state as JSON, plus `version`, `config_source` and `config_hash` |
| `GET /healthz` | none | liveness |
| `GET /api/config` | read | the current check configuration as YAML |
| `PUT\|POST /api/config` | write | replace it, and write it back to disk (`-state`, or the single `-probes` file) |
| `POST /api/reload` | write | re-read the files |
| `POST /api/probes` | write | run a probe now, off its schedule (`?name=` for one) |
| `GET\|POST /api/check` | read / write | run a check once and return the result as JSON |
| `GET /probe?probe=X&target=Y` | read / write | run a check once and return it as a Prometheus exposition |

`/probe` also takes `module=` as a synonym for `probe=` (blackbox's name for the
same thing), `hostname=` to change the Host header and SNI without changing the
address, and `debug=true` to prepend the run's trace as comment lines — the
scrape stays a valid exposition, and whoever is holding curl gets the log.
`target=`, `hostname=` and `debug=` each make the request a write: the first two
send the probe's credentials somewhere the configuration did not, and the third
returns what the target answered.

`/probe` carries no histograms. One run is not a distribution — a histogram
scraped there has a count of one and never moves, so `rate()` over it reads as
nothing happening. The `*_last_duration_seconds` gauge beside it is the
measurement, and it is what `probe_duration_seconds` is aliased from.

The `/api/*` paths exist only when `http.api.enabled` is set. Authentication is
a bearer token, compared in constant time, with a separate read-only token if
one is configured. Naming a target, or sending a probe definition in the body,
makes the request a write — because both send the probe's credentials somewhere
the configuration did not say to send them.

**A write token is equivalent to shell access on the node**: a probe definition
of type `external` runs a command. Treat it accordingly.

`/probe` lets a scrape name the target instead of the configuration, and answers
in the scrape rather than on the next one — a drop-in for blackbox_exporter:

```yaml
  - job_name: argus
    metrics_path: /probe
    params:
      probe: [web]
    static_configs:
      - targets: ["https://www.google.com/", "https://developers.google.com/"]
    relabel_configs:
      - {source_labels: [__address__], target_label: target}
      - {target_label: __address__, replacement: "argus-node:6767"}
```

Alongside its own names, `/probe` publishes blackbox_exporter's for the series
both measure the same way — `probe_duration_seconds`, `probe_http_status_code`,
`probe_http_version`, `probe_http_duration_seconds{phase}`, `probe_http_ssl`,
`probe_ssl_earliest_cert_expiry`, `probe_dns_lookup_time_seconds`,
`probe_dns_answer_rrs`, `probe_ip_protocol`, `probe_tls_version_info{version}`,
`probe_icmp_duration_seconds{phase}` and `probe_failed_due_to_regex` — so a
dashboard written for blackbox works unchanged. Only exact twins are
translated: a familiar name over a different measurement is worse than no
series at all. The aliases never take the node's prefix, since whatever reads
them was written against blackbox. `http.api.probe_aliases: false` turns them
off; `/metrics` never carries them.

## Reloading

SIGHUP re-reads the check configuration and applies it in place: probes that did
not change keep running and keep their history, new ones start, removed ones
stop and their series are retired. Nothing restarts, and no measurement is lost
in the swap. `probing.watch_config` additionally polls the files and reloads
when their content changes.

A change made through `PUT /api/config` is written back to the file it came
from — atomically, keeping its permissions, following a symlink to the real
file — so the API and the file never disagree. When the node was started with a directory rather than a single file,
the API refuses the write instead of guessing which file to edit.

## Deployment

[deploy/argus.service](deploy/argus.service) is a hardened systemd unit;
[deploy/logrotate.conf](deploy/logrotate.conf) covers the file sink. The
[Dockerfile](Dockerfile) builds a static binary into a minimal image running as
an unprivileged user.

[deploy/grafana/argus-checks.json](deploy/grafana/argus-checks.json) is a
dashboard for everything above: the state of every target and of each address
behind it, latency split by phase, and a row per probe type. The data source
and the metric prefix are variables, so it imports as it is —
[deploy/grafana/README.md](deploy/grafana/README.md) has the details.

[docs/swagger.yaml](docs/swagger.yaml) is the full OpenAPI description of the
HTTP surface.
