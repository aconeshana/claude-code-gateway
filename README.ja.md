# Claude Code Gateway

🌐 Language: [English](./README.md) | [中文](./README.zh-CN.md) | **日本語**

リモートクライアント（WebSocket / Feishu IM）とローカルの [Claude Code CLI](https://github.com/anthropics/claude-code) サブプロセスをブリッジする Go 製ゲートウェイ。各チャットセッションが専用の CLI プロセスを 1 つ抱え、stream-json による双方向 stdin/stdout 通信を行う —— スマホ・ブラウザ・チャットアプリから、ノート PC 上で長時間動く Claude Code との会話を、コンテキストを失わずに駆動できます。

---

## クイックスタート

### 方式 A —— Claude Code に自動インストールしてもらう（推奨）

ローカルの `claude` CLI（または IDE プラグイン）に以下の一文を貼り付けてください：

```
请按照这个 QUICK_START.md 帮我部署 Claude Code Gateway，并配置为开机自启守护进程：
https://github.com/aconeshana/claude-code-gateway/blob/main/QUICK_START.md
```

Claude Code が clone・ビルド・`.env` 生成・launchd / systemd 自動起動の登録・ヘルスチェックまで一気通貫で実行してくれます。

### 方式 B —— 手動

前提：`go ≥ 1.22`、`claude` CLI、`jq`（推奨）、`git`。

```bash
git clone https://github.com/aconeshana/claude-code-gateway.git
cd claude-code-gateway
go build -o gateway .

# 最小構成 —— gateway バイナリと同じディレクトリに置く
cat > .env <<'EOF'
GATEWAY_DEFAULT_CWD=~          # メインチャット plain text のフォールバック先
ADMIN_MODEL=claude-sonnet-4-6  # 要約 / セマンティック照合に使うモデル
# Feishu を有効化（任意）
# FEISHU_APP_ID=cli_xxx
# FEISHU_APP_SECRET=xxx
EOF

./gateway.sh start
```

起動後 `http://localhost:8080/health` で動作確認。Feishu を有効化したら、ボット DM に `/help` を送って利用可能なコマンドを確認してください。

---

## 機能

- **マルチセッション管理** —— 各チャットセッション = 1 つの Claude Code CLI サブプロセス。ゲートウェイは active / idle / archived の三状態でプールを管理
- **再起動を跨いだ復帰** —— ゲートウェイ状態は JSON に永続化、セッションは `claude --resume` でコンテキストを保ったまま再開
- **Feishu 統合** —— DM スラッシュコマンド（`/new` `/list` `/switch` `/resume` `/archive` `/branch` `/rename` `/skills` `/cron` `/diff` `/status` `/config` …）、話題（thread）で並行セッションを物理分離、プロジェクトピッカー UI
- **外部セッション検出** —— ターミナル / SDK / IDE から直接起動した Claude Code セッションを自動で見つけ、設定に応じて IM 側にも表示
- **AI 自動要約** —— N 件ごとに小型モデルを呼び、各セッションの一行サマリーを再生成。`/list` を常に見やすく保つ
- **WebSocket プロトコル** —— カスタム UI 向けに IM を経由しない純粋な stream-json チャネルも提供

---

## アーキテクチャ

```
WebSocket / Feishu クライアント               ┌─────────────────────────┐
            │                                │ session.Manager         │
            ▼                                │  · per-owner インデックス│
┌────────────────────────────┐               │  · active / idle /      │
│  channel.Channel アダプタ   │ Inbound/      │    archived 三状態       │
│   (feishu, fake, ws)        │ Outbound      └───────────┬─────────────┘
└──────────────┬─────────────┘                            │
               │                                          │
               ▼                                          ▼
        ┌──────────────┐                        ┌─────────────────────┐
        │ bridge.Bridge│  コマンド・ルーティング│ runtime.Runtime     │
        │ (commands +  │                       │  (claude-code / fake)│
        │  rendering)  │                       └──────────┬──────────┘
        └──────────────┘                                  │
                                                          ▼
                                                ┌────────────────────┐
                                                │ Claude Code CLI    │
                                                │ stream-json プロセス│
                                                └────────────────────┘
```

詳細な設計（状態機械、V2 ルーティング原則、ghost セッション処理、要約 worker、永続化スキーマ）は [`CLAUDE.md`](./CLAUDE.md) と [`docs/state-machine.md`](./docs/state-machine.md) を参照。

---

## 設定項目

環境変数、バイナリと同じディレクトリの `.env`、または Feishu の `/config` コマンドで動的変更可能。

| キー | 既定値 | 用途 |
|---|---|---|
| `GATEWAY_DEFAULT_CWD` | `~` | メインチャット plain text の作業ディレクトリ |
| `GATEWAY_PERMISSION_MODE` | `auto` | ツール呼び出しの権限モード（`auto` / `forward`） |
| `GATEWAY_LISTEN_ADDR` | `:8080` | WebSocket リッスンアドレス |
| `GATEWAY_MAX_SESSIONS` | `10` | 並行 CLI プロセスの上限 |
| `SUMMARY_INTERVAL` | `5` | N 件ごとに要約を再生成（0 = 無効） |
| `ADMIN_MODEL` | `claude-haiku-4-5` | 要約 worker 用モデル（本番では sonnet 推奨） |
| `GATEWAY_SHARE_EXTERNAL_SESSIONS` | `false` | 外部セッション（ターミナル/SDK）を IM に表示 |
| `GATEWAY_DISCOVERY_WINDOW_DAYS` | `7` | 外部セッション scan 期間（日。0 = 全量） |
| `GATEWAY_DISCOVERY_RESCAN_INTERVAL` | `5m` | 再スキャン間隔 |
| `FEISHU_APP_ID` / `FEISHU_APP_SECRET` | — | 両方設定すると Feishu ブリッジが有効化 |
| `FEISHU_ALLOWED_USER_IDS` | — | Feishu ユーザー allow list（コンマ区切り） |

---

## 操作コマンド

```bash
./gateway.sh start          # バックグラウンド起動
./gateway.sh stop
./gateway.sh restart        # rebuild + restart
./gateway.sh status         # デーモン状態
./gateway.sh logs           # ログ tail

go test -race ./...         # race detector 付きテスト
go vet ./...
```

---

## 開発

コードベースは小さく自己完結しています。主要パッケージ：

- `internal/session/` —— セッション管理、ライフサイクル、永続化
- `internal/bridge/` —— コマンドルーティング、カード描画、要約 worker
- `internal/channel/feishu/` —— Feishu IM アダプタ（カード・話題・コールバック）
- `internal/runtime/claude/` —— CLI サブプロセス管理 + stream-json コーデック
- `internal/gateway/` —— WebSocket トランスポート

詳細な規約は [`CLAUDE.md`](./CLAUDE.md) を参照。

---

## License

Apache 2.0、 [`LICENSE`](./LICENSE) を参照。
