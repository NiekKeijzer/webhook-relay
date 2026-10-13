# webhook-relay

Delivers webhooks from the internet to services in a private network, without
exposing that network. A public **receiver** checks and queues each webhook; a
**forwarder** inside the network pulls it from the queue and delivers it. See
[docs/architecture-decisions.md](docs/architecture-decisions.md) for the design.

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
