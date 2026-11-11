<div align="center">

# Paibot Gateway

### 一気に読み切れる大きさの AI ゲートウェイ

仮想キー · モデルの許可 · レート制限 · フォールバック · トークンの予算。プロンプトは一切ディスクに残しません。

<a href="https://github.com/paibot-hq/paibot-gateway/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/paibot-hq/paibot-gateway/actions/workflows/ci.yml/badge.svg"></a>
<img alt="Go" src="https://img.shields.io/badge/Go-1.27-00ADD8?logo=go&logoColor=white">
<img alt="Dependencies" src="https://img.shields.io/badge/dependencies-0-2ea043">
<img alt="Code" src="https://img.shields.io/badge/code-~3.8k%20lines-555">
<img alt="Image" src="https://img.shields.io/badge/image-12%20MB-555">
<a href="LICENSE"><img alt="License" src="https://img.shields.io/badge/license-Apache--2.0-blue"></a>

[English](README.md) · **日本語**

<br>

| **0** | **約3,800** | **12 MB** | **2 MB** | **0.3秒** |
|:---:|:---:|:---:|:---:|:---:|
| 外部のパッケージ | Go の行数 | コンテナイメージ | 待機中のメモリ | 起動から最初の応答まで |

<br>

[使い方](#使い方) · [アーキテクチャ](#アーキテクチャ) · [設定](#設定) · [安全のための設計](#安全のための設計) · [まだないもの](#まだないもの)

</div>

---

小さく、監査しやすい、OpenAI互換のAIゲートウェイです。
vLLMなど自前ホストのLLMの前に置き、仮想キーでの認証、モデルの許可、レート制限、フォールバック、利用の記録を行います。

- 📦 **外部依存ゼロ**: Goの標準ライブラリだけで書いています(`go.sum`がありません)
- 🔍 **コードが小さい**: ゲートウェイ本体は約1,500行です。必要なときだけ使うDB、PostgreSQLのクライアント、管理画面を含めても約3,800行です。全部を読んで承認できます
- 🙈 **プロンプトを記録しない**: ログとDBに残るのは、キーの名前、モデル、トークン数、時間などのメタデータだけです

| | Paibot Gateway | agentgateway | Bifrost | LiteLLM proxy |
|---|---|---|---|---|
| 外部のパッケージ | **0** | `Cargo.lock`にRustのクレート816個 | Goのモジュール225個 | Pythonのパッケージ202個 |
| コードの行数 | **3,800** | Rust 271,000 + TypeScript 42,000 | Go 614,000 + TypeScript 203,000 | 965,000(`litellm`パッケージだけで) |
| コンテナイメージ | **12 MB** | 102 MB | 293 MB | 1.3 GB |
| 待機中のメモリ | **2 MB** | 11 MB | 140〜200 MB | 570 MB |
| 起動から最初の応答まで | **0.3秒** | 0.3〜0.4秒 | 5.5〜6.7秒 | 9〜11秒 |

すぐに返事を返す転送先に、ゲートウェイを通して送ったときの時間(中央値)です。それぞれ2回ずつ測り、1回につき2万リクエスト(LiteLLMは3,000)、ゲートウェイはCPU 2つに制限しました。

| 同時リクエスト数 | Paibot Gateway | agentgateway | Bifrost | LiteLLM proxy |
|---|---|---|---|---|
| 1 | **0.08〜0.09 ms** | 0.13 ms | 0.22〜0.23 ms | 4.2 ms |
| 10 | 0.32〜0.37 ms | 0.32〜0.33 ms | 1.1〜1.2 ms | 35〜37 ms |
| 50 | 1.7〜1.8 ms | **1.6 ms** | 5.7〜5.9 ms | 195 ms |
| 同時50での毎秒リクエスト数 | 23,000〜24,000 | **26,000〜27,000** | 7,200〜7,700 | 250 |

2026-11-08に、M系のMacのDockerで測りました。どれも仮想キー1つ、DBなしの設定です。Paibot Gatewayはこのコミット、agentgatewayは1.7.0(Rust、単体で動かす設定)、Bifrostは2.5.0(リクエストの記録はオフ)、LiteLLMは`main-stable`(1.108.0、標準のワーカー1つ)です。ほかの3つは、プロバイダー間の形式の変換、MCPやエージェント同士の通信、金額での予算など、はるかに多くのことをします。この表は、その代わりに何がかかっているかを示すもので、どれが優れているかを示すものではありません。

## 使い方

### Dockerで1分で試す

Paibot Gateway、PostgreSQL、管理画面を起動し、手元の[Ollama](https://ollama.com)にリクエストを送ります。

```sh
ollama pull qwen3:0.6b
git clone https://github.com/paibot-hq/paibot-gateway && cd paibot-gateway/examples/quickstart
echo "PAIBOT_GATEWAY_ADMIN_TOKEN=$(openssl rand -hex 32)" > .env
docker compose up -d --build
docker compose run --rm cli key create -models chat alice   # キーが表示されます
```

表示されたキーで呼び出したあと、http://127.0.0.1:8081 に`.env`のトークンでログインすると、キーと利用量が見られます。OpenAIやvLLMを使うときは、[examples/quickstart/paibot-gateway.json](examples/quickstart/paibot-gateway.json)の転送先を書き換えてください。Linuxでは、コンテナから届くように`OLLAMA_HOST=0.0.0.0`でOllamaを起動してください。

### インストール

```sh
go install github.com/paibot-hq/paibot-gateway/cmd/paibot-gateway@latest

paibot-gateway genkey                 # 仮想キーとハッシュを作る。キーは利用者に一度だけ渡す
cp paibot-gateway.example.json paibot-gateway.json   # hash を書き換える
paibot-gateway check -config paibot-gateway.json
paibot-gateway serve -config paibot-gateway.json
```

クライアントからは、OpenAIのSDKのbase URLを変えるだけで使えます。

```sh
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $PAIBOT_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"chat","messages":[{"role":"user","content":"こんにちは"}]}'
```

対応しているエンドポイント: `POST /v1/chat/completions`、`POST /v1/completions`、`POST /v1/embeddings`、`POST /v1/responses`(と`/responses/compact`、`/responses/input_tokens`)、`GET /v1/models`、`GET /healthz`。Anthropic のクライアント向けに `POST /v1/messages` と `POST /v1/messages/count_tokens`。それ以外は404を返します。

<details>
<summary><b>Claude など Anthropic 形式の上流</b></summary>


上流に `"api": "anthropic"` と書き、Anthropic の公式SDKの接続先をPaibot Gatewayにします。仮想キーは、SDKが送る `x-api-key` でも、`Authorization: Bearer` でも受け付けます。

```json
"upstreams": {
  "claude": { "base_url": "https://api.anthropic.com/v1", "api": "anthropic", "api_key_env": "ANTHROPIC_API_KEY" }
},
"models": {
  "sonnet": { "targets": [{ "upstream": "claude", "model": "claude-sonnet-5-5" }] }
}
```

```python
client = anthropic.Anthropic(api_key=PAIBOT_GATEWAY_KEY, base_url="http://localhost:8080")
client.messages.create(model="sonnet", max_tokens=1024, messages=[...])
```

リクエストは形式を変換せず、そのまま中継します。そのため、Anthropicの新しい機能もPaibot Gatewayを直さずに使えます。同じ理由で、1つのモデルの転送先はすべて同じ `api` にそろえる必要があります。また、AnthropicのモデルをOpenAIのエンドポイントで呼ぶことはできず、その逆もできません。このエンドポイントのエラーはAnthropicと同じ形で返します。529(混雑)のときは、5xxと同じく次の転送先に進みます。ログの `prompt_tokens` には、OpenAIと同じくキャッシュから読んだ入力も含めます。

Claude Code も同じように使えます。仮想キーを `ANTHROPIC_AUTH_TOKEN` に、Paibot Gateway のモデル名を `ANTHROPIC_MODEL` などに指定します。

```sh
ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_AUTH_TOKEN=$PAIBOT_GATEWAY_KEY \
ANTHROPIC_MODEL=sonnet ANTHROPIC_DEFAULT_HAIKU_MODEL=sonnet claude
```

Anthropic 形式の転送先に自前のモデルを置く場合（vLLM も Ollama も `/v1/messages` に対応しています）は、`CLAUDE_CODE_MAX_OUTPUT_TOKENS` をモデルの扱える長さより小さくしてください。Claude Code は初期設定で3万2千トークンの返事を求めるので、収まらないリクエストを vLLM は断ります。

</details>

<details>
<summary><b>Responses API(Codex、Agents SDK)</b></summary>


`POST /v1/responses`は、chat completionsと同じOpenAI形式の転送先に送ります。OpenAI本体のほか、vLLMやOllamaもこのAPIに対応しています。Codexから使うときは、`~/.codex/config.toml`にPaibot Gatewayを登録します。

```toml
model_provider = "paibot-gateway"
model = "chat"

[model_providers.paibot-gateway]
name = "Paibot Gateway"
base_url = "http://localhost:8080/v1"
env_key = "PAIBOT_GATEWAY_KEY"
wire_api = "responses"
```

Responses APIは、指定しなければ会話を転送先に保存します。Paibot Gatewayは保存させません。`"store": false`を付けて送り、保存した会話を前提にする`"store": true`、`previous_response_id`、`conversation`は断ります。クライアントは毎回会話の全体を送ることになります(Codexはもともとそうしています)。保存を許すときは`allow_response_storage`を設定します。その場合、保存した会話の続きのリクエストは、ほかの転送先に切り替えません。切り替え先にはその会話がないためです。保存した返事をIDで取り出したり消したりするエンドポイントは中継しません。どの転送先が持っているか分からないためです。

</details>

<details>
<summary><b>キーをDBに置く、利用量と予算</b></summary>


DBなしの場合、キーは設定ファイルに書きます。キーを変えたら`SIGHUP`(`kill -HUP <pid>`)を送ると、リクエストを止めずに設定を読み直します。PostgreSQLを使うと、動かしたままキーを発行・無効化でき、キーごとの利用量の記録と、月ごとのトークン数の予算が使えます。

```json
"database_url_env": "PAIBOT_GATEWAY_DATABASE_URL",
"timezone": "Asia/Tokyo"
```

```sh
export PAIBOT_GATEWAY_DATABASE_URL='postgres://paibot_gateway:secret@db.internal/paibot_gateway'   # 既定はsslmode=verify-full
paibot-gateway db migrate
paibot-gateway key create -name tanaka -owner tanaka@example.com -models chat,sonnet -rpm 60 -budget 2000000 -expires 2027-03-31
paibot-gateway key list
paibot-gateway key revoke -name tanaka
paibot-gateway usage -month 2026-11
```

キーは発行したときに一度だけ表示し、保存するのはハッシュだけです。Paibot Gatewayは5秒ごとにキーを読み直すので、無効にしたキーは5秒以内に使えなくなります。リクエストの処理中にDBを待つことはありません。キーはメモリから引き、利用量もメモリで合計してからまとめて書き込みます。DBが止まっても、最後に読んだキーで動き続け、利用量はDBが戻ってから書き込みます。`timezone`は、月の区切り(予算と利用量の集計の区切り)を決めます。

DBに入るのは、キーと、キー×モデル×1時間ごとのリクエスト数とトークン数だけです。プロンプト、応答、1件ずつのリクエストの記録は入りません。PostgreSQLのクライアントもPaibot Gatewayの一部で、標準ライブラリだけで書いています。TLSでの証明書の確認、SCRAM-SHA-256の認証、パラメータ付きのクエリに対応しています。

</details>

<details>
<summary><b>管理画面</b></summary>


`admin_token_env`も設定すると、キーと利用量を扱う小さな管理画面が使えます。既定の待ち受けは`127.0.0.1:8081`で、そのマシンの中からしか開けません(`ssh -L`や`kubectl port-forward`でつなぐか、`admin_listen`を変えます)。トークンは`paibot-gateway genkey`で作ってください。

```json
"admin_token_env": "PAIBOT_GATEWAY_ADMIN_TOKEN"
```

管理者のアカウントやパスワードはなく、トークンでログインします。画面はサーバー側で作り、厳しいContent-Security-Policyを付けています。フォームは別サイトからの送信を防ぎ、ログインの失敗が続くと待たせます。

</details>

## アーキテクチャ

```mermaid
flowchart LR
  C["クライアント<br/>(OpenAIのSDK)"] -- "仮想キー" --> A

  subgraph P["Paibot Gateway(1プロセス、標準ライブラリのみ)"]
    direction LR
    A["認証<br/>ハッシュで照合"] --> R["レート制限<br/>キーごとのトークンバケット"]
    R --> V["検査<br/>本文の上限、モデルの許可"]
    V --> W["書き換え<br/>公開名 → 実際のモデル名、<br/>利用者のキー → 上流のキー"]
  end

  W -- "1番目の転送先" --> U1["vLLM A"]
  W -. "429・5xx・接続失敗のとき" .-> U2["vLLM B"]
  W --> L[("標準出力のJSONログ<br/>メタデータのみ")]
```

リクエストは、この4段階を順に通ります。どこかで止まったら、その場でOpenAIと同じ形のエラーを返すので、SDKでは普通のエラーとして扱えます。応答は溜めずにそのまま流し、流れる途中でトークン数を読み取ります。

| ファイル | 役割 |
|---|---|
| [`cmd/paibot-gateway/main.go`](cmd/paibot-gateway/main.go) | コマンド(`serve`、`check`、`genkey`、`hash`)、HTTPサーバーの時間制限、安全な停止 |
| [`internal/gateway/config.go`](internal/gateway/config.go) | 設定のJSONの読み込みと、厳しめの検査 |
| [`internal/gateway/keys.go`](internal/gateway/keys.go) | キーの生成とハッシュ、トークンバケットによる制限 |
| [`internal/gateway/gateway.go`](internal/gateway/gateway.go) | 振り分け、認証、フォールバックつきの転送、SSEの中継、利用の記録 |
| [`internal/gateway/responses.go`](internal/gateway/responses.go) | Responses API: 会話を保存させないルール、利用量の読み取り |
| [`internal/gateway/keystore.go`](internal/gateway/keystore.go) | DBのキーをメモリに持つ、利用量のまとめ書き、予算 |
| [`internal/store/store.go`](internal/store/store.go) | キーと1時間ごとの利用量の表とクエリ |
| [`internal/pg/`](internal/pg/) | PostgreSQLの通信。TLS、SCRAM-SHA-256、クエリ、小さな接続プール |
| [`internal/admin/`](internal/admin/) | 管理画面。ログイン、キー、利用量 |

DBなしの場合、状態はメモリの中だけにあります。読み込んだ設定と、キーごとのレート制限のバケットです。DBを使う場合は、5秒ごとにキーを読み直して利用量を書き込む処理が裏で動きます。リクエストの処理がそれを待つことはありません。

## 設定

[paibot-gateway.example.json](paibot-gateway.example.json) を参照してください。

| 項目 | 内容 |
|---|---|
| `upstreams` | 転送先。`base_url`はhttpかhttpsのみ。上流のAPIキーは`api_key_env`で指定した環境変数から読みます。`api`は`openai`(既定)か`anthropic` |
| `models` | 公開するモデル名と、転送先の一覧。上から順に試し、429と5xx、接続失敗のときだけ次へ進みます |
| `keys` | 仮想キー。`hash`(SHA-256)だけを置きます。`models`で使えるモデルを限定し、`rpm`で1分あたりの回数を制限します |
| `database_url_env` | 任意。キー、利用量、予算に使う`postgres://`のURLを入れた環境変数の名前 |
| `admin_token_env`、`admin_listen` | 任意。管理画面を使うときに設定します。DBが必要です |
| `timezone` | 予算と利用量の集計で、月の区切りに使うタイムゾーン。既定は`UTC` |
| `allow_response_storage` | Responses APIの会話を転送先に保存させるかどうか。既定は`false` |

設定の書き間違い(知らない項目、存在しない転送先、ハッシュの形式違いなど)は、起動時にエラーになります。
制限が書き間違いで無効になったまま動く、ということを防ぐためです。

## 安全のための設計

|  |  |
|:-:|---|
| 🔑 | 仮想キーは256ビットの乱数で、ハッシュだけを保存します。照合はハッシュで行うので、比較時間からキーを推測できません |
| 🧼 | クライアントのヘッダーは転送しません(例外は`Accept`と、Anthropicの上流への`anthropic-version`と`anthropic-beta`)。上流には、新しく作ったリクエストに上流用のキーを付けて送ります |
| ↪️ | 上流からのリダイレクトはたどりません。内部の別のアドレスへ誘導されるのを防ぎます |
| 🧭 | 環境変数のプロキシ設定は使いません |
| 📏 | 本文の大きさに上限があります(既定は10MB)。ヘッダーの読み込みにも時間の上限があります |
| 🙈 | 存在しないモデルと許可されていないモデルには、同じ404を返します。キーを使って他のモデルを探ることはできません |
| 📦 | コンテナは、ダイジェストで固定したdistrolessの上で、root以外のユーザーとして動きます。CIのアクションもコミットのSHAで固定しています |

## まだないもの

- レート制限は各プロセスのメモリ内です。予算はDBで共有しますが、複数のレプリカでは数秒分の利用だけ超えることがあります
- 金額での上限と、チーム。予算はキーごとのトークン数です
- SSOでの管理画面へのログインと、複数の管理者
- TLSの終端(Ingressで行う前提です)

## テスト

`go test ./...` でユニットテストを流します。`PG_TEST_DSN` に PostgreSQL のデータベースを指定すると、DB のテストも流れます。[e2e/run.sh](e2e/run.sh) は、動いている Paibot Gateway を本物のモデルにつないで確かめます。チャット、ストリーミング、Responses API と Messages API、予算、キーの無効化、利用量の記録を試したあと、Codex と Claude Code から Paibot Gateway 経由で答えさせます。CI では、PR のたびに両方を流します。通しのテストの相手は、0.5B のモデルを動かす Ollama です。

## ライセンス

[Apache License 2.0](LICENSE)
