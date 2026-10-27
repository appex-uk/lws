# lws — lightweight web server

A small, fast static file server built only on the Go standard library (no third-party dependencies).

- Serves `GET`/`HEAD` from a single content directory (ranges, `If-Modified-Since`, MIME types, `sendfile`).
- Content is confined with `os.Root`: paths and symlinks cannot escape the root.
- No directory listings: a directory is served only if it has an `index.html`.
- Dotfiles and dot-directories (`.env`, `.git/`) are never served; `.well-known/` is allowed.
- Per-path headers, SPA fallback, custom 404 page, and precompressed `.br`/`.gz` files.
- Kubernetes liveness and readiness endpoints, Prometheus metrics on a separate port, and an optional access log.
- Graceful shutdown on `SIGINT`/`SIGTERM`, with a readiness drain period first.

## Configuration (JSON)

Pass a file with `-config path/to/config.json`. Any field you leave out keeps its default. Unknown fields are rejected.

| Field                | Default    | Description                                        |
|----------------------|------------|----------------------------------------------------|
| `address`            | `:8080`    | Listen address                                     |
| `root`               | `/srv/www` | Content directory                                  |
| `probes.liveness_path`  | `/livez`  | Liveness probe; set `""` to disable             |
| `probes.readiness_path` | `/readyz` | Readiness probe; set `""` to disable            |
| `metrics.address`    | `:9090`    | Metrics listener; set `""` to disable              |
| `metrics.path`       | `/metrics` | Prometheus scrape path                             |
| `access_log`         | `false`    | Log one JSON line per request                      |
| `headers`            | `{}`       | Headers added to every file response               |
| `header_rules`       | `[]`       | Per-path headers, see below                        |
| `fallback`           | `""`       | File served with 200 for missing routes (SPAs), e.g. `/index.html` |
| `not_found_page`     | `""`       | File served with 404 for missing files, e.g. `/404.html` |
| `precompressed`      | `false`    | Serve `<file>.br` / `<file>.gz` when accepted      |
| `timeouts.read_header` | `5s`     | Go duration strings                                |
| `timeouts.read`      | `10s`      |                                                    |
| `timeouts.write`     | `60s`      | Raise this if you serve very large files           |
| `timeouts.idle`      | `120s`     |                                                    |
| `timeouts.shutdown`  | `15s`      | Grace period for in-flight requests                |
| `timeouts.drain`     | `5s`       | Time readiness reports 503 before shutdown starts  |

See [configs/config.json](configs/config.json). Configured `fallback` and `not_found_page` files must exist at startup.

### Header rules

Each rule has a `match` pattern (Go `path.Match` syntax) and the `headers` to set. Rules are applied in order on top of `headers`, and later rules win.

- A pattern without `/` matches the file name, e.g. `*.html`.
- A pattern with `/` matches the full path, e.g. `/assets/*`. `*` does not cross `/`.
- Directory URLs match their `index.html`, so `/` matches `*.html`.

The shipped config sends `no-cache` for HTML and a one-year `immutable` cache for `/assets/*`. Put fingerprinted build output (e.g. `app.3f9a.js`) there so new deploys show up immediately. Error responses never carry `Cache-Control` from your config.

### SPA fallback and 404 page

- With `fallback` set, a missing path with **no file extension** (e.g. `/users/42`) gets the fallback file with status 200.
- A missing path **with** an extension (e.g. `/app.js`) still returns 404, so broken asset links aren't masked by HTML.
- `not_found_page` replaces the plain-text 404 body and is sent with `Cache-Control: no-store`.

### Precompressed files

Compress at build time and ship the results next to the originals, for example:

```sh
find www -type f \( -name '*.html' -o -name '*.css' -o -name '*.js' -o -name '*.svg' -o -name '*.json' \) \
  -exec gzip -9 -k {} \; -exec brotli -q 11 -k {} \;
```

With `precompressed: true`, clients that accept `br` (preferred) or `gzip` get the compressed sibling with the original `Content-Type`. All file responses include `Vary: Accept-Encoding`. Files without a compressed sibling are served as normal.

## Metrics

Prometheus text format, served on `metrics.address`. It's a separate port so metrics are never public. Request metrics cover file requests only; probe traffic is excluded.

| Metric | Type | Labels | Notes |
|--------|------|--------|-------|
| `lws_http_requests_total` | counter | `method` (`GET`/`HEAD`/`OTHER`), `code` | |
| `lws_http_request_duration_seconds` | histogram | | 0.5ms to 10s buckets; time until the handler returns |
| `lws_http_requests_in_flight` | gauge | | |
| `lws_http_response_bytes_total` | counter | | Body bytes, excluding `HEAD` |
| `lws_build_info` | gauge | `version`, `go_version` | Version set with `make build VERSION=1.2.3` |
| `go_goroutines`, `go_gomaxprocs`, `go_memlimit_bytes`, `go_memory_mapped_bytes`, `go_heap_objects_bytes`, `go_gc_cycles_total` | | | Go runtime |
| `process_start_time_seconds`, `process_cpu_seconds_total`, `process_open_fds`, `process_resident_memory_bytes` | | | The last two are Linux only |

All labels have a fixed, small set of values; paths are never used as labels.

Useful queries:

```promql
sum(rate(lws_http_requests_total[5m]))                                        # traffic
sum(rate(lws_http_requests_total{code=~"5.."}[5m])) / sum(rate(lws_http_requests_total[5m]))  # error ratio
histogram_quantile(0.99, sum by (le) (rate(lws_http_request_duration_seconds_bucket[5m])))      # p99 latency
sum(rate(lws_http_response_bytes_total[5m]))                                  # bytes/s
```

Instrumentation costs about 170 ns per request (`make bench`).

## Kubernetes probes

- **Liveness** (`/livez`) returns `200 ok` whenever the process can answer HTTP. It never checks dependencies, so a problem with the content volume won't cause restart loops.
- **Readiness** (`/readyz`) returns `200 ok`, or `503` when:
  - the server is draining after `SIGTERM`, or
  - the content root can't be stat'ed.

On `SIGTERM` the server:

1. starts failing readiness,
2. keeps serving traffic for `timeouts.drain` so Kubernetes can remove the pod from Service endpoints,
3. then shuts down gracefully within `timeouts.shutdown`.

Keep `terminationGracePeriodSeconds` greater than `drain + shutdown`. Set `GOMEMLIMIT` to about 90% of the container memory limit, as the example Deployment does. Go already sizes `GOMAXPROCS` from the CPU limit.

Probe requests are excluded from metrics and the access log. An example Deployment and Service are in [infra/k8s/deployment.yaml](../infra/k8s/deployment.yaml).

## Usage

```sh
make test          # run tests
make run           # run locally with configs/config.local.json (serves ./www)
make bench         # run benchmarks
make docker        # build image (scratch base, runs as nobody, ports 8080 + 9090)

# serve your own content and config
docker run --rm -p 8080:8080 -p 9090:9090 \
  -v "$PWD/site:/srv/www:ro" \
  -v "$PWD/config.json:/etc/lws/config.json:ro" \
  lws:latest
```

The image ships a sample page from [www/](www/). Replace it by mounting a volume, or by changing the `COPY www/` line in the Dockerfile.
