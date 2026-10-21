# Paigate

**English** | [日本語](README.ja.md)

**An AI gateway small enough to read in one sitting.**

Paigate sits in front of your self-hosted LLMs (vLLM, SGLang, anything OpenAI-compatible) and does what a gateway should do, and nothing more: it checks virtual keys, enforces per-key model access and rate limits, falls back across upstreams, and records who used what. It never records what they said.

```
zero dependencies   ·   ~850 lines   ·   no prompts in logs   ·   one static binary
```

## Why

An AI gateway holds the keys to every model you run, and sees every prompt that passes through. It should be the most boring, most auditable piece of your stack.

Most gateways went the other way: hundreds of transitive dependencies, plugin systems, admin UIs, and dozens of provider integrations. Each one is more code to trust. When that supply chain gets compromised, a gateway is exactly where an attacker wants to be.

Paigate goes the other way:

- **Standard library only.** No `go.sum`. Nothing to pin, vendor, or audit beyond Go itself. CI fails if a dependency ever appears.
- **Small on purpose.** The whole gateway is about 850 lines. You can review all of it before you approve it.
- **Metadata, not content.** Logs carry key name, model, upstream, status, token counts and latency. Prompts and completions never touch disk.
- **Fails loud.** Unknown config fields, dangling references, malformed hashes, and missing secrets all stop startup. A typo never silently disables a limit.

## Quick start

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

Endpoints: `POST /v1/chat/completions`, `POST /v1/completions`, `POST /v1/embeddings`, `GET /v1/models`, `GET /healthz`. Everything else is a 404.

## Configuration

One JSON file. See [paigate.example.json](paigate.example.json).

| Field | What it does |
|---|---|
| `upstreams` | Where requests go. `base_url` must be http or https with no embedded credentials. Upstream API keys come from the environment variable named in `api_key_env`, never from the file. |
| `models` | Public model names mapped to an ordered list of targets. Paigate moves to the next target only on 429, 5xx, or a connection failure, and only to targets you listed. |
| `keys` | Virtual keys, stored as SHA-256 hashes. `models` limits what each key may call; `rpm` caps requests per minute. |

## Security model

- **Keys.** 256-bit random keys, stored only as hashes. Lookup is by hash, so comparison timing reveals nothing about a guess.
- **Clean upstream requests.** Every upstream request is built from scratch. The client's `Authorization`, cookies, and forwarding headers are dropped, and only `Accept` passes through. The upstream's own key is attached server-side.
- **No redirects.** Upstream redirects are returned as-is, never followed, so an upstream cannot bounce Paigate into your internal network.
- **No ambient proxies.** `HTTP_PROXY` and friends are ignored.
- **Bounded input.** Request bodies are size-capped (10 MB by default). Header reads are time-boxed.
- **No model discovery.** A model that doesn't exist and a model the key can't use return the same 404.
- **Hardened delivery.** Distroless, non-root container with base images pinned by digest. CI actions are pinned by commit SHA, run with read-only permissions, and get no secrets.

## Not yet

- Rate limits are per process. Sharing them across replicas needs Redis.
- Budgets (token or spend caps) and persisting usage to PostgreSQL.
- TLS termination. Run Paigate behind your ingress.
- An admin API. To change keys, edit the config and restart.

Small is the feature. Additions will be judged by what they cost to audit.
