# grok-proxy

Grok 多账号反向代理。从目录加载 CPA JSON，自动轮换、自动刷新、429 自动切号。

**对客户端始终是一个端点，一个 api_key，用完不用管。**

## 配置

```json
{
  "listen": "0.0.0.0:5001",
  "api_key": "sk-local-fixed",
  "cpa_dir": "/data/cpa/*.json",
  "refresh_interval": 300
}
```

| 字段 | 说明 |
|------|------|
| `listen` | 监听地址，默认 `:5001` |
| `api_key` | 客户端访问用的固定 key（空则不校验） |
| `cpa_dir` | CPA JSON 通配路径，如 `/data/cpa/*.json` |
| `refresh_interval` | 后台刷新间隔（秒），默认 300 |

## 运行

```bash
go build -o grok-proxy .
./grok-proxy config.json
```

### Docker Compose（NAS）

```bash
mkdir -p cpa
# 把 xai-*.json 丢进 ./cpa/
cp config.json.example config.json   # 改 api_key
docker compose up -d
```

```yaml
# docker-compose.yml
services:
  grok-proxy:
    image: ghcr.io/myflavor/grok-proxy:latest
    container_name: grok-proxy
    restart: unless-stopped
    ports:
      - "5001:5001"
    volumes:
      - ./cpa:/data/cpa
      - ./config.json:/app/config.json:ro
```

## 调用

```bash
curl http://127.0.0.1:5001/v1/chat/completions \
  -H "Authorization: Bearer sk-local-fixed" \
  -H "Content-Type: application/json" \
  -d '{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}]}'
```

```python
from openai import OpenAI
client = OpenAI(api_key="sk-local-fixed", base_url="http://127.0.0.1:5001")
```

## 行为

| 事件 | 处理 |
|------|------|
| 正常请求 | 轮询选号，注入 CPA headers + Bearer |
| access 将过期（5min 内） | 后台 refresh；新 token **写回 CPA 文件** |
| 上游 429 | 该号冷却 65s，换号重试（最多 8 次） |
| 上游 402 | 额度用尽，冷却 1h，换号重试 |
| 上游 401/403 | 先 refresh 再重试；`invalid_grant`/revoked → 标死 |
| refresh 失败（吊销） | 改名为 `xai-xxx.json.dead`，池内跳过 |
| 全死 / 全凉 | 返回 503 / 429 JSON，不返回坏响应 |

- 端点无前缀：`/v1/...` → `https://cli-chat-proxy.grok.com/v1/...`
- 健康检查：`GET /healthz` → `{"ok":true,"live":N,"total":M}`
- 信号 `SIGTERM`/`SIGINT` 优雅退出

## CPA 文件

兼容 [Grok-Register](https://github.com/Charles-0509/Grok-Register) 输出的 `xai-*.json`。

死亡账号：`xai-xxx.json` → `xai-xxx.json.dead`（重启也不会再加载）。
