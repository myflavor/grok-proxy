# grok-proxy

Grok 多账号反向代理。CPA 轮询 + RT 自动续命 + SSO 复活死号。

**对客户端始终是一个端点，一个 api_key。**

## 配置

```json
{
  "listen": "0.0.0.0:5001",
  "api_key": "sk-local-fixed",
  "cpa_dir": "/data/cpa/*.json",
  "sso_file": "/data/sso/accounts.txt",
  "refresh_interval": 300,
  "revive_enabled": true,
  "revive_interval": 600,
  "revive_concurrency": 2,
  "proxy": ""
}
```

| 字段 | 说明 |
|------|------|
| `listen` | 监听地址，默认 `:5001` |
| `api_key` | 客户端固定 key（空则不校验） |
| `cpa_dir` | CPA 通配路径，如 `/data/cpa/*.json`（也会加载 `*.json.dead`） |
| `sso_file` | 注册机 `SSO/accounts.txt`（`email:pass:sso`），或目录 |
| `refresh_interval` | RT 刷新扫描间隔（秒），默认 300 |
| `revive_enabled` | 是否用 SSO 复活死 RT（有 `sso_file` 时默认开） |
| `revive_interval` | 复活队列扫描间隔（秒），默认 600 |
| `revive_concurrency` | 同时 SSO OAuth 数，默认 2（防 429） |
| `proxy` | 可选出站 HTTP 代理（清障 `http://host:40080`） |

## 生命周期

```
启动
  → 加载 CPA（含 .dead）+ SSO
  → refreshAll：未过期的用 RT 续命并写回 JSON
  → 死号 / invalid_grant → 入 revive 队列
  → 后台 SSO device OAuth（纯 HTTP，无浏览器）→ 新 CPA 写回、重新入池
```

| 事件 | 处理 |
|------|------|
| AT 将过期 | RT refresh，写回文件 |
| RT 吊销 | `*.json.dead` + 排队 SSO 复活 |
| SSO 复活成功 | 去掉 .dead，写新 token，入池 |
| SSO 失效 | 丢掉该 email 的 SSO，不再狂刷 |
| 上游 429 | 冷却 65s 切号 |
| 上游 402 | 冷却 1h 切号（额度，不是 token） |

## Docker

```bash
mkdir -p cpa sso
# CPA
cp /path/to/outputs/*/CPA/xai-*.json cpa/
# SSO（合并去重）
cat /path/to/outputs/*/SSO/accounts.txt | awk -F: '!e[$1]++' > sso/accounts.txt
# 改 api_key
cp config.json.example config.json   # 或编辑仓库里的 config.json

docker compose up -d
curl -s localhost:5001/healthz
```

镜像：`ghcr.io/myflavor/grok-proxy:v0.3.0` / `latest`

## 调用

```bash
curl http://127.0.0.1:5001/v1/chat/completions \
  -H "Authorization: Bearer sk-local-fixed" \
  -H "Content-Type: application/json" \
  -d '{"model":"grok-4","messages":[{"role":"user","content":"hi"}]}'
```

```python
from openai import OpenAI
client = OpenAI(api_key="sk-local-fixed", base_url="http://127.0.0.1:5001")
```

健康检查：`GET /healthz` → `live` / `total` / `sso` / `revive_queue`

## 源码运行

```bash
go build -o grok-proxy .
./grok-proxy config.json
```
