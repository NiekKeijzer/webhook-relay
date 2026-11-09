# webhook-relay

[![ci](https://img.shields.io/github/actions/workflow/status/NiekKeijzer/webhook-relay/build.yml?branch=main&label=ci&style=for-the-badge&logo=githubactions&labelColor=24292f)](https://github.com/NiekKeijzer/webhook-relay/actions/workflows/build.yml)
[![docker](https://img.shields.io/docker/v/niekkeijzer/webhook-relay?sort=semver&label=docker&style=for-the-badge&logo=docker&labelColor=24292f)](https://hub.docker.com/r/niekkeijzer/webhook-relay)
[![size](https://img.shields.io/docker/image-size/niekkeijzer/webhook-relay?sort=semver&label=size&style=for-the-badge&logo=docker&labelColor=24292f)](https://hub.docker.com/r/niekkeijzer/webhook-relay)
[![go](https://img.shields.io/github/go-mod/go-version/NiekKeijzer/webhook-relay?label=go&style=for-the-badge&logo=go&labelColor=24292f)](go.mod)
[![license](https://img.shields.io/github/license/NiekKeijzer/webhook-relay?label=license&style=for-the-badge&labelColor=24292f)](LICENSE)
[![AI:X I curate](https://img.shields.io/badge/-I%20curate-grey?style=for-the-badge&logo=data%3Aimage%2Fsvg%2Bxml%3Bbase64%2CPHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAxMDAwIDEwMDAiIHdpZHRoPSIxMDAwIiBoZWlnaHQ9IjEwMDAiIGZpbGw9IndoaXRlIj4KICAgIDxjaXJjbGUgY3g9IjUwMCIgY3k9IjUwMCIgcj0iNDUyLjMiIGZpbGw9Im5vbmUiIHN0cm9rZT0id2hpdGUiIHN0cm9rZS13aWR0aD0iNDguNyIgLz4KICAgIDxwYXRoIGQ9Ik0yMzkuMSA0MTUuNkwyMTQuNTMgNTIzLjZMMjYzLjg1IDUyMy42TDIzOS4xIDQxNS42Wk0yMDcuMjkgMzY3Ljg4TDI3MS4xIDM2Ny44OEwzNDIuMzMgNjMxLjc4TDI5MC4xOSA2MzEuNzhMMjczLjkyIDU2Ni41NkwyMDQuMTEgNTY2LjU2TDE4OC4yIDYzMS43OEwxMzYuMDUgNjMxLjc4TDIwNy4yOSAzNjcuODhaTTM3Ny4wMSA0MTMuODRMMzc3LjAxIDM2Ny44OEw1MzQuMTQgMzY3Ljg4TDUzNC4xNCA0MTMuODRMNDgxLjY1IDQxMy44NEw0ODEuNjUgNTg1LjgzTDUzNC4xNCA1ODUuODNMNTM0LjE0IDYzMS43OEwzNzcuMDEgNjMxLjc4TDM3Ny4wMSA1ODUuODNMNDI5LjUgNTg1LjgzTDQyOS41IDQxMy44NEwzNzcuMDEgNDEzLjg0Wk01NjguODggNDA2LjA1TDYyNy43NCA0MDYuMDVMNjI3Ljc0IDQ3MC41N0w1NjguODggNDcwLjU3TDU2OC44OCA0MDYuMDVaTTU2OC44OCA1MjkuMDhMNjI3Ljc0IDUyOS4wOEw2MjcuNzQgNTkzLjk1TDU2OC44OCA1OTMuOTVMNTY4Ljg4IDUyOS4wOFpNODUzLjkgNjMxLjc4TDc5OS45OSA2MzEuNzhMNzQ5LjYyIDU0NC40Nkw2OTkuNDIgNjMxLjc4TDY0NS41MSA2MzEuNzhMNzIyLjkzIDQ5Ny44TDY0Ny42MyAzNjcuODhMNzAxLjU0IDM2Ny44OEw3NDkuNjIgNDUxLjg0TDc5Ny44NyAzNjcuODhMODUxLjc4IDM2Ny44OEw3NzYuODQgNDk3LjhMODUzLjkgNjMxLjc4WiIgLz4KPC9zdmc%2BCg%3D%3D&labelColor=13171f)](https://0rwx.com)

Delivers webhooks from the internet to services in a private network, without
exposing that network. A public **receiver** checks and queues each webhook; a
**forwarder** inside the network pulls it from the queue and delivers it. See
[docs/architecture-decisions.md](docs/architecture-decisions.md) for the design.

## Image

```sh
docker pull ghcr.io/niekkeijzer/webhook-relay:latest
# or
docker pull niekkeijzer/webhook-relay:latest
```

Published tags: `<major>` (from 1.0.0 on), `<major.minor>`,
`<major.minor.patch>` and `latest` for releases (`v*` git tags); `main` and
`sha-<short>` for the main branch.

## Run locally

[compose.yaml](compose.yaml) runs ElasticMQ as the queue, the receiver, the
forwarder and an echo target that logs every request it gets:

```sh
docker compose up --build
curl -i -H 'Content-Type: application/json' -d '{"hello":"world"}' \
  http://localhost:8080/bb10a60e-8f82-4321-b83e-565c0a6d88b4
docker compose logs echo
```

The routes are in [local/relay.toml](local/relay.toml), the queues in
[local/elasticmq.conf](local/elasticmq.conf).

## Build

```sh
go build -o relay ./cmd/relay
```

## Configure

Both components read the same TOML file. It holds only the routes, and names
the environment variables with route secrets, never the secrets themselves.

```toml
version = 1

[[routes]]
name = "github"
ids = ["7c9e6679-7425-40de-944b-e07fc1f90ae7"] # uuidgen | tr '[:upper:]' '[:lower:]'
verifier = { type = "github-hmac-sha256", secrets_env = ["GITHUB_WEBHOOK_SECRET"] }
delivery_id_header = "X-GitHub-Delivery"
headers = ["X-GitHub-Event"]
targets = [{ name = "deploy", url = "http://deploy.internal.example/hooks/github" }]
```

The sender posts to `https://<receiver-host>/<route-id>`. A route without
authentication uses `verifier = { type = "none" }`.
[docs/relay.example.toml](docs/relay.example.toml) shows every field, with
comments.

Everything else comes from the environment:

| Variable | Used by |
|---|---|
| `RELAY_QUEUE_TYPE`, and `RELAY_QUEUE_<NAME>` for the backend's settings | both |
| `RELAY_ENVELOPE_KEY`: secret, at least 32 bytes, the same for both | both |
| `RELAY_ENVELOPE_KEY_PREVIOUS`: secret, only during a key rotation | forwarder |
| `RELAY_FORWARDER_TIMEOUT_SECONDS`, `RELAY_FORWARDER_DEDUPE_WINDOW_SECONDS` | forwarder |
| the names in a route's `secrets_env` | receiver |
| the names in a route's `ids_env`: optional, route IDs kept out of the file | receiver |

Any secret can come from a file instead: set `NAME_FILE` to the file's path
rather than setting `NAME`. Trailing newlines in the file are ignored.

The `sqs` queue type works with Amazon SQS, Scaleway Queues and ElasticMQ. The
queue needs a dead-letter queue (redrive policy).

| Variable | Meaning |
|---|---|
| `RELAY_QUEUE_URL` | the queue URL |
| `RELAY_QUEUE_REGION` | the queue's region |
| `RELAY_QUEUE_VISIBILITY_TIMEOUT_SECONDS` | must equal the queue's visibility timeout |
| `RELAY_QUEUE_MAX_MESSAGE_SIZE` | optional: the queue's maximum message size in bytes, 262144 by default |
| `RELAY_QUEUE_ACCESS_KEY_ID`, `RELAY_QUEUE_SECRET_ACCESS_KEY` | secrets, different for each component |

## Run

```sh
export RELAY_QUEUE_TYPE=sqs
export RELAY_QUEUE_URL="..." RELAY_QUEUE_REGION="..."
export RELAY_QUEUE_VISIBILITY_TIMEOUT_SECONDS=30
export RELAY_QUEUE_ACCESS_KEY_ID="..." RELAY_QUEUE_SECRET_ACCESS_KEY="..."
export RELAY_ENVELOPE_KEY="$(openssl rand -base64 32)"
export GITHUB_WEBHOOK_SECRET="..."                     # or GITHUB_WEBHOOK_SECRET_FILE
export RELAY_FORWARDER_TIMEOUT_SECONDS=10
export RELAY_FORWARDER_DEDUPE_WINDOW_SECONDS=3600

./relay receiver -config relay.toml -listen :8080
./relay forwarder -config relay.toml

# Or pass the configuration itself instead of a path:
RELAY_CONFIG="$(cat relay.toml)" ./relay forwarder
```

## Test

```sh
go test -race ./...

# Also test the SQS backend against the ElasticMQ of `docker compose up`:
RELAY_TEST_SQS_QUEUE_URL=http://localhost:9324/000000000000/relay-test \
  go test -race ./queue/sqs
```

## AI disclaimer

The code, documentation and CI workflow were drafted with AI assistance
(Claude) and reviewed before publishing.
