# Webhook relay: architecture

Status: agreed design, 12 October 2026. 

## How to use this document

This document describes the architecture of the relay: its parts, the contracts between them and the rules that hold across all of them. It is written for agents that build a part of the relay or add a feature to it.

- Sections marked *(contract)* define behaviour that other parts rely on. Changing a contract needs the maintainer's approval.
- Section 13 lists what is deliberately left to the implementation. Decide those in code, within the contracts.
- Anything this document does not settle is the maintainer's decision. Ask; do not assume.

## 1. Purpose and requirements

The relay delivers webhooks from the internet to services in a private network without exposing that network to the internet.

Requirements:

- Nothing in the private network accepts inbound connections from the internet.
- While the private network is offline, webhooks are buffered, and they are delivered once it is back.
- The relay is generic. GitHub is the first sender, but nothing outside GitHub-specific plugins may assume GitHub.
- One webhook can be delivered to several targets (fan-out).
- Cheap to run and low maintenance. Expected volume: under 100 webhooks a day.
- The cloud side runs on Scaleway today. Use only features Scaleway supports, and keep the cloud-specific parts swappable.
- Configuration is code: no database and no configuration changes at runtime. Changing the configuration means redeploying.
- The relay is standalone software. It does not depend on the tooling of the project that deploys it; that project supplies configuration and secrets at deploy time.
- Any user interface built later is read-only. It may show what the relay is doing; it never changes configuration or messages.

## 2. Terms

| Term | Meaning |
|---|---|
| Relay | The whole system: receiver, queue and forwarder. |
| Sender | An external service that sends webhooks, such as GitHub. |
| Receiver | The public HTTP endpoint, running serverless in the cloud. It checks requests, enqueues them and answers the sender. |
| Queue | The durable message store between receiver and forwarder, including dead-lettering. |
| Forwarder | A long-running process in the private network. It pulls messages from the queue and delivers them to targets. |
| Route | One configured webhook endpoint: its URL IDs, verifier, acceptor and targets. |
| Target | A named destination of a route: an HTTP URL inside the private network. |
| Verifier | A plugin that checks a request is authentic, for example by a sender's signature. Required per route; the verifier `none` states that a route has no authentication. |
| Acceptor | A plugin that decides how the receiver answers the sender and whether the request is enqueued. |
| Envelope | One queue message: a captured request, addressed to one target and signed by the receiver. |
| Delivery ID | The identifier of one incoming request, taken from the sender when it provides one. |
| Envelope key | A secret shared by receiver and forwarder, used to sign envelopes. |

## 3. Overview

```
                 cloud                                        private network
Sender ──HTTPS──▶ Receiver ──send──▶ Queue ◀──receive/ack── Forwarder ──HTTP──▶ Targets
                  verify, accept,    (+ dead-letter)         check signature,
                  sign, enqueue                              dedupe, forward
```

1. A sender sends a request to `https://<receiver-host>/<route-id>`.
2. The receiver finds the route, runs its verifier (if any) and its acceptor. If the acceptor enqueues, the receiver writes one signed envelope per target and then answers the sender.
3. The forwarder long-polls the queue over an outbound connection, checks each envelope's signature and replays the captured request to the target.
4. On a `2xx` from the target, the forwarder acknowledges the message. Otherwise the queue redelivers it after its ack deadline; after a maximum number of deliveries the message is dead-lettered.

## 4. Invariants

These hold everywhere. A change that breaks one needs the maintainer's approval.

1. **The private network is never reachable from the internet.** The forwarder only makes outbound connections, to the queue and to targets.
2. **Envelopes carry names, not addresses.** Envelopes contain route and target *names*, never target URLs or internal hostnames; the forwarder resolves URLs from its own configuration. Both components load the same configuration, so the receiver holds the target URLs too. This is an accepted trade-off: the receiver never uses, logs or exposes them.
3. **The queue is not trusted.** The forwarder delivers only envelopes with a valid receiver signature. Holding a queue send credential is not enough to reach a target.
4. **The request reaches the target unchanged.** Method, query string and body bytes are identical to what the sender sent; kept headers keep all their values.
5. **Configuration is static.** Each component loads its configuration once, at startup. No API, endpoint or UI changes configuration or queue contents.
6. **Secrets live only in the environment at runtime.** A secret is read from an environment variable, or from a file whose path is in the variable of the same name with `_FILE` appended (§10.3). The configuration file contains the *names* of environment variables, never secret values. Secrets never appear in logs, process arguments or error messages.
7. **Logs and anything exposed carry no payload data.** Never log or expose bodies, query strings, header values, route IDs, secrets or target URLs. Use route name, target name, delivery ID, sizes, status codes, delivery counts and durations.
8. **Least privilege.** The receiver can send to the queue but not receive from it. The forwarder can receive and acknowledge but not send, where the backend's permissions allow that split.
9. **Error responses carry no detail.** Every error response to a sender has an empty body.

## 5. Routes and targets *(contract)*

A route has:

- a **name**: lowercase letters, digits and hyphens, unique. Used in logs, envelopes and the forwarder's configuration.
- one or more **IDs**: random UUIDs (version 4). The webhook URL is `https://<receiver-host>/<id>`. A route has more than one ID only during an ID rotation.
- the HTTP **methods** it accepts. Default: `POST`.
- a **verifier** with its secrets (§6.3). A route without authentication uses the verifier `none`.
- an optional **acceptor**. Default: the default acceptor (§6.4).
- an optional **delivery ID header**: the request header that carries the sender's delivery ID.
- **headers to keep**, in addition to the default set (§6.5).
- one or more **targets**. Each has a name, unique within the route, and a URL. Only the forwarder uses the URL.

### Route IDs are semi-secret

A route ID is an unguessable address, not authentication. For a route with the verifier `none`, it is the only thing between strangers and the queue. Therefore:

- Generate IDs from a cryptographic random source. Never derive one from the route name.
- An ID appears only in the sender's webhook settings and in the configuration. It is never logged, never used as a metric label, never written into an envelope and never sent to a target.
- The configuration file is private and must not be committed to a public repository. (It also holds target URLs.)
- A route whose sender can sign requests must use a verifier.
- If an ID leaks, rotate it (§10.5).

### Fan-out

Each target of a route gets its own envelope, so each delivery succeeds, retries and dead-letters independently of the others. The receiver writes only target names into envelopes; the forwarder maps route name and target name to a URL.

## 6. Receiver

### 6.1 Request pipeline *(contract)*

| Step | On failure |
|---|---|
| 1. Match the path to a route ID. The path must be exactly `/<id>`. | `404` |
| 2. Check the method against the route's methods. | `405` |
| 3. Read the raw body as bytes before anything parses it. | `413` if it exceeds the body limit; `400` if it cannot be read |
| 4. Run the route's verifier. | `401` |
| 5. Run the acceptor. If it decides not to enqueue, send its reply and stop. | `500` if its decision is invalid (§6.4) |
| 6. Build one envelope per target. Check each against the queue's maximum message size before sending any. | `413`, nothing enqueued |
| 7. Send all envelopes. | `503` |
| 8. Send the acceptor's reply. | — |

- The body limit is the platform's request limit, or the queue's maximum message size where there is no platform limit (`relay receiver`).
- A `405` response carries an `Allow` header listing the route's methods.
- The receiver bounds its queue calls so that it always answers within the sender's deadline (GitHub: 10 seconds).
- If some envelopes of a fan-out were sent and others failed, the receiver answers `503`. If the sender retries with the same delivery ID, the forwarder's dedupe skips the targets that already received it.
- The receiver keeps no state between requests, apart from reusable clients.
- The receiver writes one log line per request: route name (or `unknown`), delivery ID, status, body size, number of envelopes and duration. The delivery ID is logged only once the request passed verification.

### 6.2 Delivery ID

If the route has a delivery ID header and the request carries exactly one value for it that is non-empty, at most 256 bytes and printable ASCII, that value is the delivery ID. Otherwise the receiver generates a random one (a version 4 UUID). The envelope records which of the two it is. All envelopes of one request share the same delivery ID.

### 6.3 Verifiers *(contract)*

A verifier decides whether a request is authentic. Every route has one. The verifier `none` accepts any request sent to the route's ID; a route uses it to state that it has no authentication.

Illustrative Go. The shape is the contract; names may differ.

```go
// Request is the incoming request as the receiver read it. Request and
// Params are shared by verifiers, acceptors and queue backends (§11).
type Request struct {
	Method string
	Query  string      // raw query string, unchanged
	Header http.Header
	Body   []byte      // raw bytes, unparsed
}

type Verifier interface {
	// Verify returns nil when the request is authentic.
	Verify(req *Request) error
	// Headers lists the request headers the scheme uses. The receiver keeps
	// them in the envelope, so a target can verify the request itself.
	Headers() []string
}

// Params holds a plugin's params from the configuration. Decode fills a
// struct defined by the plugin and fails on unknown or mistyped keys.
type Params interface {
	Decode(into any) error
}

// A verifier type is registered by name.
type VerifierType struct {
	Name string
	// NeedsSecrets says whether a route using this type lists at least one
	// secret (true) or none (false). Both components check secrets_env
	// against it without resolving secrets.
	NeedsSecrets bool
	// Validate checks a route's params without secrets. Both components run it
	// at startup, so they accept or reject the same configuration.
	Validate func(params Params) error
	// New builds the verifier for one route. secrets holds every active
	// secret of the route; a match with any one of them is enough.
	New func(params Params, secrets [][]byte) (Verifier, error)
}
```

Rules:

- Verifier types are code, compiled into the binaries and selected by name. Supporting a new scheme means adding a verifier type and releasing. There are no generic or configurable verifiers.
- Each route's verifier is built at startup, so invalid params or missing secrets fail the deploy, not a request.
- Signatures and tokens are compared in constant time.
- Verifiers never log. On failure the receiver logs only the route name and that verification failed.
- Accepting several secrets is what makes rotation possible (§10.5).

The verifier `none` takes no params and no secrets, and `Headers()` returns nothing.

The first verifier for a sender is `github-hmac-sha256`. It computes HMAC-SHA256 over the raw body with the route secret, hex-encodes it and compares it with the value of `X-Hub-Signature-256` after the `sha256=` prefix. A missing or malformed header fails verification. `Headers()` returns `X-Hub-Signature-256`.

### 6.4 Acceptors *(contract)*

An acceptor decides how the receiver answers the sender and whether the request is enqueued. It runs after the verifier.

```go
type Decision struct {
	Enqueue bool        // false: reply without enqueueing, e.g. to a handshake
	Status  int         // must be 2xx when Enqueue is true
	Header  http.Header
	Body    []byte
}

type Acceptor interface {
	Accept(req *Request) Decision
}
```

Rules:

- Acceptor types are registered and validated at startup in the same way as verifier types (`Validate`, `New`), but without secrets.
- When `Enqueue` is true, the reply goes out only after every envelope was sent successfully. A `2xx` to the sender therefore always means the request is durably queued.
- Acceptors cannot override the receiver's failure responses (`400`, `401`, `404`, `405`, `413`, `500`, `503`).
- A decision is invalid if its status is not a final status (200–599), or if `Enqueue` is true and the status is not 2xx. The receiver then answers `500` with an empty body and enqueues nothing.
- A reply that is not 2xx is sent without its body (invariant 9).
- Some senders send handshake requests that they don't sign. Supporting such a sender takes a verifier and an acceptor written for it: the verifier accepts the handshake by that sender's own rules, and the acceptor answers it.

The default acceptor has type `default`. It enqueues the request and then replies with an empty body and the status in `params.status`. The status defaults to `202` and must be a 2xx.

### 6.5 What is captured

The envelope captures the method, the raw query string, the raw body and the kept headers with all their values. The kept headers are the union of:

- the default set: `Content-Type` and `User-Agent`;
- the verifier's `Headers()`;
- the route's `headers`.

All other headers are dropped. Header names starting with `X-Relay-` are reserved for the forwarder and cannot be kept.

## 7. Envelope *(contract)*

One envelope is one queue message, addressed to one target. It contains:

- the format version;
- route name and target name;
- the delivery ID, and whether it came from the sender or was generated;
- the time the receiver accepted the request;
- method, raw query string, kept headers and raw body.

An envelope never contains a route ID, a target URL or a secret.

Rules:

- The receiver signs every envelope with HMAC-SHA256 using the current envelope key. The signature covers every byte of the encoded envelope and travels inside the message body, not as queue metadata, so it works with any queue backend.
- The forwarder accepts an envelope whose signature matches the current envelope key or, during a rotation, the previous one (§10.5).
- Body bytes survive encoding exactly. Compression is allowed.
- The format is versioned. The forwarder rejects versions it does not know. When the format changes, deploy a forwarder that reads both versions before the receiver that writes the new one.
- The exact byte layout is an implementation detail. It is defined in one shared package and covered by round-trip tests.

## 8. Queue *(contract)*

The receiver and the forwarder use the queue only through this contract. Any backend that satisfies it can replace another without changes elsewhere.

| Operation | Used by | Behaviour |
|---|---|---|
| Send(message) | receiver | Stores the message durably, or fails. |
| Max message size | receiver | The largest message the backend accepts, in bytes. |
| Receive | forwarder | Waits (long poll) until messages are available or a timeout passes. Each message comes with its body, delivery count, enqueue time and an ack handle. |
| Ack(handle) | forwarder | Removes the message permanently. |
| Ack deadline | forwarder | The time after which an unacknowledged message is redelivered. |

Guarantees the relay relies on:

- **At least once.** A message is delivered one or more times until it is acknowledged.
- **Fixed redelivery.** An unacknowledged message is redelivered after the backend's ack deadline. The relay never delays, extends or reschedules individual messages, so retries happen at that fixed interval.
- **Dead-lettering.** After a maximum number of deliveries, a message is set aside: it is no longer delivered to the forwarder and is kept until its retention period ends. A backend without native dead-lettering implements this in its queue implementation.
- **No ordering.** Nothing in the relay depends on message order.
- **Bounded buffer.** How long and how much the relay can buffer is limited by the backend's retention period and storage. Fan-out multiplies storage per webhook by the number of targets.
- **Separate credentials** for sending and for receiving and acknowledging, where the backend offers them.

Each backend implements the contract in its own package and is selected by `RELAY_QUEUE_TYPE` (§10.3). A backend type declares the names of its params and credentials, and provides `Validate` (checks its params without credentials and returns the ack deadline, which the forwarder checks its forward timeout against), `NewSender` and `NewReceiver`. Backend implementations are outside this document.

## 9. Forwarder

### 9.1 Processing *(contract)*

The forwarder runs as a single long-running instance and processes one message at a time:

1. **Check the signature.** If it is missing or invalid: log, do not acknowledge. The message is dead-lettered after the maximum number of deliveries.
2. **Decode.** If the version is unknown or the envelope is malformed: log, do not acknowledge.
3. **Find the target** by route name and target name. If it is unknown: log, do not acknowledge.
4. **Dedupe.** If this route, target and delivery ID was delivered within the dedupe window: acknowledge, count a duplicate, stop.
5. **Forward.** Send the captured method, query string, kept headers and body to the target URL, adding:
   - `X-Relay-Delivery-Id`: the delivery ID;
   - `X-Relay-Received-At`: when the receiver accepted the request (RFC 3339);
   - `X-Relay-Attempt`: the queue's delivery count.

   Redirects are not followed.
6. **Handle the result.** A `2xx` acknowledges the message and records the dedupe key. Anything else (any other status, a timeout, a connection error) leaves the message unacknowledged, so it is redelivered.

Rules:

- The forward timeout must be shorter than the queue's ack deadline. The forwarder must not hold more received messages than it can finish within one ack deadline.
- Dedupe is best effort: in memory, bounded in size and age, and lost on restart. Targets must tolerate duplicates.
- On shutdown, the forwarder stops receiving, finishes the current forward and exits. Unacknowledged messages are redelivered later.
- A target URL has no query string of its own; the captured query string is appended to it.
- The forwarder writes one log line per message: route name, target name, delivery ID, delivery count, outcome and duration.

### 9.2 Status and observability

Metrics, health checks and any other status the forwarder exposes are left to the implementation, within invariants 5 and 7: read-only, and no payload data. A future UI reads from that same read-only interface.

## 10. Configuration *(contract)*

### 10.1 One file for both components

A deployment has one configuration file, owned by the project that deploys the relay. That project may generate it, for example from a Terraform template. The file holds the routes and nothing else. Both the receiver and the forwarder load the same file and use only the parts they need. The file is private (§5).

The file contains no secret values. A route names the environment variables that hold its secrets; everything else a component needs comes from environment variables with fixed names (§10.3). Each component is given only the variables it uses:

- **receiver**: the queue settings with its send credentials, the envelope key and the route secrets;
- **forwarder**: the queue settings with its receive credentials, the envelope keys and the forwarder settings.

At startup, each component:

- parses the whole file strictly: unknown keys or missing required fields are errors;
- validates the whole file, including the parts only the other component uses (§10.2), so both accept or reject the same file;
- reads the environment variables it uses, and exits with a clear error if one is missing, empty or invalid. Both run the queue backend's `Validate`; the forwarder also checks its forward timeout against the queue's ack deadline. Variables that only the other component uses are not read.
- never quotes values from the file or the environment in an error, such as route IDs, target URLs or secrets. A TOML syntax error is reported by line, column and last key only.

A component reads its configuration from a file or from one environment variable holding the same content; the platform adapter uses whichever the platform supports. The CLI takes `-config <path>`, or reads `RELAY_CONFIG` when `-config` is not given; setting both is an error.

### 10.2 Format

The configuration file is TOML, read with `github.com/BurntSushi/toml`. Example with invented values:

```toml
version = 1

[[routes]]
name = "github-infra"
ids = ["7c9e6679-7425-40de-944b-e07fc1f90ae7"]
methods = ["POST"]
verifier = { type = "github-hmac-sha256", params = {}, secrets_env = ["ROUTE_GITHUB_INFRA_SECRET"] }
acceptor = { type = "default", params = { status = 202 } }
delivery_id_header = "X-GitHub-Delivery"
headers = ["X-GitHub-Event", "X-GitHub-Delivery", "X-GitHub-Hook-ID"]
targets = [
  { name = "deploy", url = "http://deploy.internal.example/hooks/github" },
  { name = "notify", url = "http://notify.internal.example/github" },
]

[[routes]]
name = "status-page"
ids = ["0f8fad5b-d9cb-469f-a165-70867728950e"]
verifier = { type = "none" }
targets = [{ name = "dashboard", url = "http://dashboard.internal.example/hook" }]
```

| Field | Required | Meaning |
|---|---|---|
| `version` | yes | Configuration format version: `1`. |
| `routes[].name` | yes | `[a-z0-9-]+`, unique. |
| `routes[].ids` | yes | One or more version 4 UUIDs in lowercase canonical form, unique across all routes. The receiver matches the path exactly, so case matters. |
| `routes[].methods` | no | Default `["POST"]`. If present, one or more of `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS`, each once. |
| `routes[].verifier` | yes | `type`, `params`, and `secrets_env`: names of the environment variables holding the route's active secrets (§10.3). `secrets_env` has at least one entry if the type needs secrets, and none otherwise. Each name is a valid environment variable name (`[A-Za-z_][A-Za-z0-9_]*`), listed once, and does not end in `_FILE`. Use `{ type = "none" }` for a route without authentication. |
| `routes[].acceptor` | no | `type` and `params`. Default `{ type = "default" }`. |
| `routes[].delivery_id_header` | no | Header carrying the sender's delivery ID. The same name rules as `headers` apply. |
| `routes[].headers` | no | Extra headers to keep. Valid header names, matched regardless of case. Names starting with `X-Relay-` are rejected, and so are headers the forwarder cannot replay: `Connection`, `Content-Length`, `Host`, `Keep-Alive`, `Proxy-*`, `TE`, `Trailer`, `Transfer-Encoding` and `Upgrade`. |
| `routes[].targets` | yes, at least one | An inline array of tables, each with `name` (`[a-z0-9-]+`, unique within the route) and `url`, an absolute `http` or `https` URL without user info, query string or fragment. |

Validation at startup also verifies that every verifier and acceptor type exists and accepts its params, and that names and IDs are unique.

Parsing rules:

- **Unknown keys are errors**, in the configuration file and in plugin params. Keys are lowercase and must match exactly; the library matches keys to struct fields regardless of case, so the parser also rejects keys that differ only in case.
- **Plugin params are decoded per route.** The library's `MetaData` keys carry no array index, so `Undecoded()` and `Type()` report one result for a key path across all routes. If one route's plugin decodes `params.x`, another route's unknown `params.x` would no longer count as undecoded. Each route's params are therefore decoded on their own, and a plugin that does not decode non-empty params fails validation.
- **Each route's `verifier`, `acceptor` and `targets` are written inline**, inside the route. A `[[routes.targets]]` or `[routes.verifier]` block belongs to whichever `[[routes]]` comes before it, so a misplaced block silently moves it to another route. Components reject targets written as `[[routes.targets]]`. Decoded into `map[string]any`, the library returns blocks as `[]map[string]any` and an inline array as `[]any`, which tells them apart per route. (`MetaData.Type` reports `ArrayHash` and `Array`, but only once per key path, so a later route with inline targets hides an earlier block.) The library reports an inline `verifier` or `acceptor` and a `[routes.verifier]` block as the same type, so for those two the rule is a convention the parser cannot enforce.

### 10.3 Environment

Everything outside the routes reaches the components as environment variables, set by the deploying project at deploy time. How that project stores and injects them is up to it. An empty variable counts as unset.

| Variable | Read by | Secret | Meaning |
|---|---|---|---|
| `RELAY_QUEUE_TYPE` | both | no | Queue backend name. |
| `RELAY_QUEUE_<NAME>` | both | per backend | The backend's params (not secret) and credentials (secret), under names the backend declares: uppercase letters, digits and underscores, other than `TYPE` and not ending in `_FILE`. Unset params are left out; every credential is required. The receiver and the forwarder are each given their own credential values. |
| `RELAY_ENVELOPE_KEY` | both | yes | The current envelope key, at least 32 bytes. The receiver signs with it. |
| `RELAY_ENVELOPE_KEY_PREVIOUS` | forwarder | yes | Optional. The previous envelope key during a rotation (§10.5), at least 32 bytes. The forwarder accepts it as well. |
| `RELAY_FORWARDER_TIMEOUT_SECONDS` | forwarder | no | Forward timeout in whole seconds, at least 1. Must be shorter than the queue's ack deadline. |
| `RELAY_FORWARDER_DEDUPE_WINDOW_SECONDS` | forwarder | no | How long delivered dedupe keys are remembered, in whole seconds, at least 1. |
| the names in a route's `secrets_env` | receiver | yes | The route's active secrets. |

The values of the forwarder settings are a deploy decision, not part of this design.

Secrets:

- A secret `NAME` is read from the variable `NAME`, or from the file whose path is in `NAME_FILE`. Setting both is an error.
- A secret from a variable is the raw bytes of its value. A secret from a file is the file's content without trailing line feeds and carriage returns, so a file written with `echo` holds the intended value.
- A missing or empty secret and a file that cannot be read are errors. Errors name the variables, never the value or the file's path, which may hold a misplaced secret.

Route IDs and target URLs are not secrets, but they are private, so they live in the private configuration file.

### 10.4 Change order

The receiver and the forwarder are deployed separately. Both load the same file, but a change takes effect in a component only once that component is redeployed with it. Deploy in this order:

| Change | Order |
|---|---|
| Add a route or target | forwarder, then receiver, then register the URL at the sender |
| Remove a route or target | unregister at the sender, then receiver, then forwarder once no messages for it remain in the queue (otherwise they are dead-lettered) |
| Change a target URL | forwarder only; the receiver does not use target URLs |
| Change the envelope format | forwarder that reads both versions, then receiver |

### 10.5 Rotation

Each arrow is a deploy of the components named. A component that is not redeployed keeps its previous file and environment; that is safe here, because it does not use the parts that change.

- **Route secret:** add the new secret to `secrets_env` → receiver. Change the secret at the sender. Remove the old secret → receiver.
- **Route ID:** add the new ID to `ids` → receiver. Change the URL at the sender. Remove the old ID → receiver.
- **Envelope key:**
  1. Set `RELAY_ENVELOPE_KEY` to the new key and `RELAY_ENVELOPE_KEY_PREVIOUS` to the old one → forwarder. It now accepts both.
  2. Set `RELAY_ENVELOPE_KEY` to the new key → receiver. The receiver now signs with it.
  3. Remove `RELAY_ENVELOPE_KEY_PREVIOUS` → forwarder. Do this only once no message signed with the old key can remain: after the queue's retention period, or once the queue and its dead-letter store are empty.

  The forwarder accepts at most two keys, so a rotation finishes before the next one starts.

## 11. Code structure

**Language:** Go, for both components and the CLI. One static binary suits the forwarder, the standard library covers HTTP, HMAC and compression, typed interfaces make the plugin contracts explicit, and Scaleway Functions supports Go.

**Dependencies:** standard library first. `github.com/BurntSushi/toml` is approved for reading configuration, in the `config` package only. A queue backend or platform adapter may use its provider's client library, confined to its own package. Propose any other dependency to the maintainer before adding it.

**Packages** (illustrative names). A package may import packages listed below it, never above it:

```
cmd/relay              CLI: receiver, forwarder
platform/<name>        entry-point adapters for serverless platforms
queue/<backend>        queue backend implementations
receiver               request pipeline, a plain http.Handler
forwarder              receive, check, dedupe, forward
verify, verify/<type>  Verifier contract, registry, implementations
accept, accept/<type>  Acceptor contract, registry, implementations
envelope               encode, decode, sign, check
queue                  queue contract, interfaces only
config                 file schema, strict parsing, validation, environment
plugin                 Request and Params, shared by verifiers and acceptors
```

Rules:

- `receiver` and `forwarder` depend only on the `queue`, `verify` and `accept` contracts, never on a backend, a plugin implementation or a platform. Only `cmd/relay` and `platform/<name>` wire implementations in.
- `config` cannot import the registries above it, so the caller passes plugin and queue checks in.
- `internal/queuefake` is an in-memory queue for tests. Only `_test.go` files import it, and no queue type refers to it.
- The receiver is a plain `http.Handler`. `relay receiver` serves it as an ordinary HTTP server, for local testing and container platforms. A platform adapter wraps the same handler for a serverless platform and follows that platform's packaging rules.
- Only `queue/<backend>` and `platform/<name>` touch a cloud provider's API.

### Extension points

| To add | Do this | Leave untouched |
|---|---|---|
| A sender's signature scheme | a new `verify/<type>`, registered by name, tested against the sender's published test vectors where they exist | receiver, forwarder, envelope |
| A handshake or a custom reply | a new `accept/<type>` | receiver, forwarder, envelope |
| A queue backend | a new `queue/<backend>` that meets §8, including dead-lettering and its maximum message size | everything else |
| A cloud platform | a new `platform/<name>` | the receiver pipeline |
| Bodies larger than one message | a pointer-based queue backend that stores the body elsewhere and enqueues a reference; the receiver's size limit follows the backend's declared maximum | receiver, forwarder |

## 12. Constraints from today's platform and sender

These facts were verified on 12 October 2026 and shape the contracts above. Re-check them before relying on exact figures.

**Scaleway Queues** (current backend):

- A message's visibility timeout (the ack deadline) can only be changed to `0` or to the queue's own value, and `DelaySeconds` is not supported. Hence fixed-interval retries without backoff (§8).
- Dead-lettering works through a redrive policy. `ApproximateReceiveCount` and `SentTimestamp` are available on received messages.
- Limits: 256 KB per message; 100 MB of storage across all queues in a Scaleway Project, dead-letter queues included; retention up to 14 days; visibility timeout up to 12 hours.
- Sending needs the publish permission; receiving and deleting need the receive permission. Invariant 8 therefore holds.

**Scaleway NATS** (a possible alternative backend):

- Includes JetStream. Message payloads up to 512 KB; stream storage up to 300 MB per NATS account, or 100 MB with three-way replication.

**GitHub** (first sender):

- Expects a `2xx` within 10 seconds and otherwise counts the delivery as failed. Hence the receiver answers before delivery happens.
- Signs the raw body with HMAC-SHA256 in `X-Hub-Signature-256` (`sha256=<hex>`). Identifies each delivery in `X-GitHub-Delivery`; a requested redelivery keeps the same value, which is what makes dedupe work.
- Redelivers failed deliveries only when asked to (manually or via its API).
- Advises against putting sensitive information in the webhook URL. Hence route IDs are addresses, not authentication, and GitHub routes always use a verifier.

## 13. Left to the implementation

- The envelope's byte layout and compression.
- Queue backend implementations and the names of their params and credentials.
- Metrics, health checks, canaries and any status endpoint, within invariants 5 and 7.
- Batching of queue calls, and how the forwarder bounds messages in flight.
- The dedupe data structure and its limits.
- Logging library and log format.
- Timeouts for queue calls, within the sender's deadline.
- Packaging and build of platform adapters and the forwarder image.

## 14. Out of scope

- Synchronous replies: returning a target's response to the sender.
- Bodies larger than one queue message (see the extension points).
- Tools to inspect or replay dead-lettered messages.
- Configuration through a UI or API. Any UI is read-only.
- Generic or configurable verifiers.
- More than one forwarder instance.
- Message ordering.
- Infrastructure as code, deployment and monitoring setup. These belong to the deploying project.

## Sources

- Scaleway Queues supported actions: https://www.scaleway.com/en/docs/queues/reference-content/queues-support/
- Scaleway Organization quotas (Queues, NATS): https://www.scaleway.com/en/docs/organizations-and-projects/organization/organization-quotas/
- GitHub, best practices for webhooks: https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks
- GitHub, validating webhook deliveries: https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries