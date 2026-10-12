# webhook-relay

Delivers webhooks from the internet to services in a private network, without
exposing that network. A public **receiver** checks and queues each webhook; a
**forwarder** inside the network pulls it from the queue and delivers it. See
[docs/architecture-decisions.md](docs/architecture-decisions.md) for the design.

> No queue backend exists yet, so both commands stop with
> `unknown queue type` after validating the configuration.

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

## Run

```sh
export RELAY_QUEUE_TYPE=example-backend                # no backend exists yet
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
```
