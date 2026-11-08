# Paigate

**English** | [日本語](README.ja.md)

**An AI gateway small enough to read in one sitting.**

Paigate sits in front of your self-hosted LLMs (vLLM, SGLang, anything OpenAI-compatible) and does what a gateway should do, and nothing more: it checks virtual keys, enforces per-key model access and rate limits, falls back across upstreams, and records who used what. It never records what they said.

```
zero dependencies   ·   no prompts in logs or the database   ·   one static binary
```

## Why

An AI gateway holds the keys to every model you run, and sees every prompt that passes through. It should be the most boring, most auditable piece of your stack.

Most gateways went the other way: hundreds of transitive dependencies, plugin systems, and dozens of provider integrations. Each one is more code to trust. When that supply chain gets compromised, a gateway is exactly where an attacker wants to be.

Paigate goes the other way:

- **Standard library only.** No `go.sum`. Nothing to pin, vendor, or audit beyond Go itself. CI fails if a dependency ever appears.
- **Small on purpose.** The gateway itself is about 1,500 lines of Go; the optional database layer, PostgreSQL client and admin UI bring the total to about 3,800. You can review all of it before you approve it.
- **Metadata, not content.** Logs carry key name, model, upstream, status, token counts and latency. Prompts and completions never touch disk.
- **Fails loud.** Unknown config fields, dangling references, malformed hashes, and missing secrets all stop startup. A typo never silently disables a limit.

| | Paigate | LiteLLM proxy |
|---|---|---|
| Third-party packages | **0** | 202 |
| Lines of code | **3,800** (Go) | 965,000 (Python, the `litellm` package alone) |
| Container image | **12 MB** | 1.3 GB |
| Memory when idle | **3 MB** | 550 MB |
| Start to first healthy response | **0.3 s** | 9–11 s |

Measured 2026-11-08 on an M-series Mac with Docker, both serving one model from Ollama with no database: Paigate at this commit, LiteLLM `main-stable` (1.108.0). LiteLLM does far more, including translating between 100+ providers; the table shows what that costs, not which is better.

## Quick start

### Try it in a minute with Docker

Paigate, PostgreSQL and the admin UI, sending requests to [Ollama](https://ollama.com) on your machine:

```sh
ollama pull qwen3:0.6b
git clone https://github.com/paibot-hq/paigate && cd paigate/examples/quickstart
echo "PAIGATE_ADMIN_TOKEN=$(openssl rand -hex 32)" > .env
docker compose up -d --build
docker compose run --rm cli key create -models chat alice   # prints a key
```

Call it with that key, then sign in at http://127.0.0.1:8081 with the token in `.env` to see the key and its usage. To use OpenAI or vLLM instead, edit the upstream in [examples/quickstart/paigate.json](examples/quickstart/paigate.json). On Linux, start Ollama with `OLLAMA_HOST=0.0.0.0` so the container can reach it.

### Install

```sh
go install paibot.app/paigate/cmd/paigate@latest

paigate genkey                          # mint a virtual key; hand it out once, keep only the hash
cp paigate.example.json paigate.json    # paste the hash in
paigate check -config paigate.json
paigate serve -config paigate.json
```

Point any OpenAI SDK at it by changing the base URL:

```sh
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $PAIGATE_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"chat","messages":[{"role":"user","content":"Hello"}]}'
```

Endpoints: `POST /v1/chat/completions`, `POST /v1/completions`, `POST /v1/embeddings`, `POST /v1/responses` (plus `/responses/compact` and `/responses/input_tokens`), `GET /v1/models`, `GET /healthz`, and for Anthropic clients `POST /v1/messages` and `POST /v1/messages/count_tokens`. Everything else is a 404.

### Claude and other Anthropic-format upstreams

Mark the upstream with `"api": "anthropic"` and point the official Anthropic SDK at Paigate. The virtual key can go in `x-api-key`, as the SDK sends it, or in `Authorization: Bearer`.

```json
"upstreams": {
  "claude": { "base_url": "https://api.anthropic.com/v1", "api": "anthropic", "api_key_env": "ANTHROPIC_API_KEY" }
},
"models": {
  "sonnet": { "targets": [{ "upstream": "claude", "model": "claude-sonnet-4-5" }] }
}
```

```python
client = anthropic.Anthropic(api_key=PAIGATE_KEY, base_url="http://localhost:8080")
client.messages.create(model="sonnet", max_tokens=1024, messages=[...])
```

Requests are passed through in their own format, never translated, so new Anthropic features work without changes to Paigate. That is also why a model's targets must all use the same `api`, and why an Anthropic model is refused on the OpenAI endpoints and the other way round. Errors on these endpoints come back in Anthropic's shape, and a 529 (overloaded) moves on to the next target like a 5xx. Logged `prompt_tokens` include cached input, as OpenAI's do.

### Responses API (Codex, Agents SDK)

`POST /v1/responses` goes to the same OpenAI-format upstreams as chat completions: OpenAI itself, or vLLM and Ollama, which serve it too. Point Codex at Paigate with a provider in `~/.codex/config.toml`:

```toml
model_provider = "paigate"
model = "chat"

[model_providers.paigate]
name = "Paigate"
base_url = "http://localhost:8080/v1"
env_key = "PAIGATE_KEY"
wire_api = "responses"
```

The Responses API stores conversations upstream unless told not to. Paigate tells it not to: it sends `"store": false`, and refuses `"store": true`, `previous_response_id` and `conversation`, which need a stored conversation. Clients then send the whole conversation each time, as Codex does. Set `allow_response_storage` to let upstreams keep them; a request that continues a stored conversation then never falls back to another target, which would not have it. Fetching or deleting a stored response by id is not served, since Paigate cannot tell which upstream holds it.

### Keys in a database, usage and budgets

Without a database, keys live in the config file and changing them means editing it and sending `SIGHUP` (`kill -HUP <pid>`), which reloads the file without dropping a request. With PostgreSQL, keys can be created and revoked at run time, and Paigate tracks usage and enforces a monthly token budget per key:

```json
"database_url_env": "PAIGATE_DATABASE_URL",
"timezone": "Asia/Tokyo"
```

```sh
export PAIGATE_DATABASE_URL='postgres://paigate:secret@db.internal/paigate'   # sslmode=verify-full is the default
paigate db migrate
paigate key create -name tanaka -owner tanaka@example.com -models chat,sonnet -rpm 60 -budget 2000000 -expires 2027-03-31
paigate key list
paigate key revoke -name tanaka
paigate usage -month 2026-11
```

The key is printed once and only its hash is stored. Paigate reloads keys every 5 seconds, so a revoked key stops working within that time, and requests never wait on the database: keys are served from memory, and usage is summed in memory and written in batches. If the database goes away, the last keys loaded keep working and usage is written once it is back. `timezone` decides where calendar months, and so budgets, begin.

The database holds keys and one row per key, model and hour with request and token counts. It never holds prompts, responses, or a record of individual requests. The PostgreSQL client is part of Paigate and, like the rest, uses only the standard library: TLS with certificate checks, SCRAM-SHA-256 authentication, parameterized queries.

### Admin UI

Set `admin_token_env` as well and Paigate serves a small web UI for keys and usage, by default on `127.0.0.1:8081`, reachable only from the machine itself (use `ssh -L` or `kubectl port-forward`, or set `admin_listen`). Make the token with `paigate genkey`.

```json
"admin_token_env": "PAIGATE_ADMIN_TOKEN"
```

There are no admin accounts or passwords: signing in takes the token. Pages are rendered on the server with a strict Content-Security-Policy, forms are protected against cross-site requests, and failed sign-ins are throttled.

## Architecture

```mermaid
flowchart LR
  C["Client<br/>(any OpenAI SDK)"] -- "virtual key" --> A

  subgraph P["Paigate (one process, stdlib only)"]
    direction LR
    A["Authenticate<br/>hash lookup"] --> R["Rate limit<br/>token bucket per key"]
    R --> V["Validate<br/>body cap, model allowlist"]
    V --> W["Rewrite<br/>public → real model,<br/>client → upstream key"]
  end

  W -- "1st target" --> U1["vLLM A"]
  W -. "on 429 / 5xx / conn fail" .-> U2["vLLM B"]
  W --> L[("stdout JSON log<br/>metadata only")]
```

Each request goes through four checks in order. The first one that fails ends the request with an OpenAI-shaped error, so SDKs surface it normally. Responses stream back unbuffered, and token usage is read as the response passes through.

| File | Role |
|---|---|
| [`cmd/paigate/main.go`](cmd/paigate/main.go) | CLI, HTTP server timeouts, SIGHUP reload, graceful shutdown |
| [`internal/gateway/config.go`](internal/gateway/config.go) | Strict JSON config loading and validation |
| [`internal/gateway/keys.go`](internal/gateway/keys.go) | Key generation, hashing, token-bucket limiter |
| [`internal/gateway/gateway.go`](internal/gateway/gateway.go) | Routing, auth, proxying with fallback, SSE relay, usage logging |
| [`internal/gateway/responses.go`](internal/gateway/responses.go) | Responses API: conversation storage rule, usage parsing |
| [`internal/gateway/keystore.go`](internal/gateway/keystore.go) | Database keys in memory, usage batching, budgets |
| [`internal/store/store.go`](internal/store/store.go) | Tables and queries for keys and hourly usage |
| [`internal/pg/`](internal/pg/) | PostgreSQL wire protocol: TLS, SCRAM-SHA-256, queries, a small pool |
| [`internal/admin/`](internal/admin/) | Admin UI: sign-in, keys, usage |

Without a database, state lives in memory only: the parsed config and one rate-limit bucket per key. With one, a background loop reloads keys and writes usage every 5 seconds; nothing in the request path waits on it.

## Configuration

One JSON file. See [paigate.example.json](paigate.example.json).

| Field | What it does |
|---|---|
| `upstreams` | Where requests go. `base_url` must be http or https with no embedded credentials. Upstream API keys come from the environment variable named in `api_key_env`, never from the file. `api` is `openai` (the default) or `anthropic`. |
| `models` | Public model names mapped to an ordered list of targets. Paigate moves to the next target only on 429, 5xx, or a connection failure, and only to targets you listed. |
| `keys` | Virtual keys, stored as SHA-256 hashes. `models` limits what each key may call; `rpm` caps requests per minute. |
| `database_url_env` | Optional. The environment variable holding a `postgres://` URL for keys, usage and budgets. |
| `admin_token_env`, `admin_listen` | Optional. Turn on the admin UI; it needs the database. |
| `timezone` | Where months begin for budgets and usage reports. Default `UTC`. |
| `allow_response_storage` | Let upstreams store Responses API conversations. Default `false`. |

## Security model

- **Keys.** 256-bit random keys, stored only as hashes. Lookup is by hash, so comparison timing reveals nothing about a guess.
- **Clean upstream requests.** Every upstream request is built from scratch. The client's credentials, cookies, and forwarding headers are dropped. Only `Accept` passes through, plus `anthropic-version` and `anthropic-beta` to Anthropic upstreams. The upstream's own key is attached server-side.
- **No redirects.** Upstream redirects are returned as-is, never followed, so an upstream cannot bounce Paigate into your internal network.
- **No ambient proxies.** `HTTP_PROXY` and friends are ignored.
- **Bounded input.** Request bodies are size-capped (10 MB by default). Header reads are time-boxed.
- **No model discovery.** A model that doesn't exist and a model the key can't use return the same 404.
- **Hardened delivery.** Distroless, non-root container with base images pinned by digest. CI actions are pinned by commit SHA, run with read-only permissions, and get no secrets.

## Not yet

- Rate limits are per process. Budgets are shared through the database, but may run over by up to a few seconds of traffic across replicas.
- Spend caps in money, and teams. Budgets are in tokens, per key.
- Signing in to the admin UI through SSO, and more than one administrator.
- TLS termination. Run Paigate behind your ingress.

Small is the feature. Additions will be judged by what they cost to audit.
