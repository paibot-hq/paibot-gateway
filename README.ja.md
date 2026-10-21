# Paigate

[English](README.md) | **日本語**

小さく、監査しやすい、OpenAI互換のAIゲートウェイです。
vLLMなど自前ホストのLLMの前に置き、仮想キーでの認証、モデルの許可、レート制限、フォールバック、利用の記録を行います。

- **外部依存ゼロ**: Goの標準ライブラリだけで書いています(`go.sum`がありません)
- **コードが小さい**: 本体は約850行。全部を読んで承認できます
- **プロンプトを記録しない**: ログに残るのは、キーの名前、モデル、トークン数、時間などのメタデータだけです

## 使い方

```sh
go install paibot.app/paigate/cmd/paigate@latest

paigate genkey                 # 仮想キーとハッシュを作る。キーは利用者に一度だけ渡す
cp paigate.example.json paigate.json   # hash を書き換える
paigate check -config paigate.json
paigate serve -config paigate.json
```

クライアントからは、OpenAIのSDKのbase URLを変えるだけで使えます。

```sh
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $PAIGATE_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"chat","messages":[{"role":"user","content":"こんにちは"}]}'
```

対応しているエンドポイント: `POST /v1/chat/completions`、`POST /v1/completions`、`POST /v1/embeddings`、`GET /v1/models`、`GET /healthz`。それ以外は404を返します。

## 設定

[paigate.example.json](paigate.example.json) を参照してください。

| 項目 | 内容 |
|---|---|
| `upstreams` | 転送先。`base_url`はhttpかhttpsのみ。上流のAPIキーは`api_key_env`で指定した環境変数から読みます |
| `models` | 公開するモデル名と、転送先の一覧。上から順に試し、429と5xx、接続失敗のときだけ次へ進みます |
| `keys` | 仮想キー。`hash`(SHA-256)だけを置きます。`models`で使えるモデルを限定し、`rpm`で1分あたりの回数を制限します |

設定の書き間違い(知らない項目、存在しない転送先、ハッシュの形式違いなど)は、起動時にエラーになります。
制限が書き間違いで無効になったまま動く、ということを防ぐためです。

## 安全のための設計

- 仮想キーは256ビットの乱数で、ハッシュだけを保存します。照合はハッシュで行うので、比較時間からキーを推測できません
- クライアントのヘッダーは転送しません(`Accept`だけは例外)。上流には、新しく作ったリクエストに上流用のキーを付けて送ります
- 上流からのリダイレクトはたどりません。内部の別のアドレスへ誘導されるのを防ぎます
- 環境変数のプロキシ設定は使いません
- 本文の大きさに上限があります(既定は10MB)。ヘッダーの読み込みにも時間の上限があります
- 存在しないモデルと許可されていないモデルには、同じ404を返します。キーを使って他のモデルを探ることはできません
- コンテナは、ダイジェストで固定したdistrolessの上で、root以外のユーザーとして動きます。CIのアクションもコミットのSHAで固定しています

## まだないもの

- レート制限は各プロセスのメモリ内です。複数のレプリカで共有するにはRedisが必要です
- 予算(トークン数や金額の上限)と、利用記録のPostgreSQLへの保存
- TLSの終端(Ingressで行う前提です)
- 管理API。キーの追加と削除は、設定を書き換えて再起動します
