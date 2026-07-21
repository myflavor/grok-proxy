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
| `cpa_dir` | CPA JSON 目录路径，支持通配符如 `/data/cpa/*.json` |
| `refresh_interval` | 后台刷新间隔（秒），默认 300（5分钟） |

## 运行

```bash
go build -o grok-proxy .
./grok-proxy config.json
```

### Docker

```bash
docker run -d --name grok-proxy --restart unless-stopped \
  -p 5001:5001 \
  -v /path/to/cpa:/data/cpa \
  -v ./config.json:/app/config.json:ro \
  ghcr.io/myflavor/grok-proxy:latest
```

## 调用

```bash
curl http://127.0.0.1:5001/v1/chat/completions \
  -H "Authorization: Bearer sk-local-fixed" \
  -H "Content-Type: application/json" \
  -d '{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}]}'
```

兼容 OpenAI SDK：

```python
client = OpenAI(
    api_key="sk-local-fixed",
    base_url="http://127.0.0.1:5001"
)
```

## 工作模式

- 从 `cpa_dir` 加载所有 `xai-*.json` CPA 凭证
- 请求轮询分配账号
- 账号 6 小时过期前自动刷新 token
- 429/403 自动冷却 65s 并切下一个账号
- 全部冷却则返回 429

端点不带前缀，直接 `/v1/...` 转发到 `https://cli-chat-proxy.grok.com/v1/...`。
