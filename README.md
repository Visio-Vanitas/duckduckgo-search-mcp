# duckduckgo-search-mcp

一个用 Go 编写的小型 DuckDuckGo HTML 搜索服务，提供 REST 和 HTTP JSON-RPC 工具接口。

无需 Python、浏览器进程或 Docker。项目非 DuckDuckGo 官方服务，也不提供搜索可用性保证。

## 功能

- `GET/POST /v1/search`：返回标题、链接、搜索摘要和来源。
- `POST /mcp`、`POST /mcp/search`：实现 `initialize`、`tools/list`、`tools/call`、`ping` 和通知处理，工具名为 `web_search`。
- 中英文区域选择、时间过滤、广告排除、链接解码和结果去重。
- 可配置 Bearer API Key，或只读验证现有 new-api SQLite Token。
- 可选 HTTP(S)/SOCKS5(H) 出口代理；默认直连。
- 验证码、上游 HTTP 错误或无法识别的页面会明确报错，不伪装成成功空结果。

## 快速开始

需要 Go 1.26 或更新版本。Makefile 默认使用 Go 1.26.0 自动工具链。

```sh
git clone https://github.com/Visio-Vanitas/duckduckgo-search-mcp.git
cd duckduckgo-search-mcp
make build

export SEARCH_API_KEY="$(openssl rand -hex 32)"
./build/duckduckgo-search-mcp
```

默认监听 `127.0.0.1:18085`。除 `/health` 外，搜索和 JSON-RPC 接口都需要 Bearer Token。

在另一个终端配置相同的 `SEARCH_API_KEY` 后调用：

```sh
curl -sS http://127.0.0.1:18085/v1/search \
  -H "Authorization: Bearer $SEARCH_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"query":"Go context cancellation documentation","max_results":5}'
```

查询参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `query` / `q` | 必填 | 最多 499 个字符 |
| `max_results` | `6` | 1–20；第一结果页的数量上限，不保证有足够结果 |
| `language` | `auto` | `auto`：含汉字时用中文区域，否则用英文区域；支持 `en`、`zh-CN` 等，完整映射见 `search.go`；`all` 为全地区 |
| `time_range` | 不限制 | `day`、`week`、`month`、`year` |

结果包含 `query`、`results`、`response_time`、`backend`、`unresponsive_engines`、`suggestions`。
每条结果提供 `title`、`url`、`content`、`snippet` 和 `engines`。`content` 与 `snippet` 都是搜索摘要，不是网页全文；摘要和网页内容未经事实核查。

## HTTP JSON-RPC

```sh
curl -sS http://127.0.0.1:18085/mcp \
  -H "Authorization: Bearer $SEARCH_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"example","version":"1"}}}'

curl -sS http://127.0.0.1:18085/mcp \
  -H "Authorization: Bearer $SEARCH_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"Python asyncio TaskGroup 文档","max_results":5}}}'
```

这是一个小型 HTTP JSON-RPC 实现，协议版本返回 `2024-11-05`。未实现 STDIO、SSE 或完整的 MCP 传输/版本协商机制，不声明与所有 MCP 客户端兼容。调用方需支持这里展示的 HTTP 请求格式。上游搜索失败时，工具结果返回 `isError: true`。

## 配置

```text
-port   监听端口，默认 18085，地址固定为 127.0.0.1
-proxy  可选出口代理，例如 socks5h://127.0.0.1:1080
-db     可选的只读 new-api Token 数据库路径
```

`SEARCH_API_KEY` 与 `--db` 必须且只能配置一个。缺少鉴权配置或同时配置两个时，程序拒绝启动。

使用 API Key 与代理：

```sh
SEARCH_API_KEY="$SEARCH_API_KEY" ./build/duckduckgo-search-mcp \
  -proxy socks5h://127.0.0.1:1080
```

使用已有 new-api 数据库：

```sh
env -u SEARCH_API_KEY ./build/duckduckgo-search-mcp -db /path/to/tokens.db
```

数据库需有 `tokens` 表及代码使用的字段：`key`、`status`、`remain_quota`、`unlimited_quota`、`expired_time`、`deleted_at`。验证状态、额度、过期时间与软删除状态；数据库只读，不扣除额度。数据库出错时请求失败，不回退到其他鉴权方式。

`/health` 仅检查已配置的本地数据库和代理 TCP 连通性，不探测 DuckDuckGo。监听地址固定为回环；如需远程访问，应自行配置受控反向代理及 TLS。

## 构建与测试

```sh
make check        # race 测试与 go vet；不访问真实搜索引擎
make build        # 本机二进制
make build-linux  # Linux amd64 单二进制
```

依赖由 `go.mod` / `go.sum` 固定。CI 执行测试、静态检查与 Linux 构建。systemd 示例见 `deploy/duckduckgo-search-mcp.service`，其环境文件需自行创建并设置 `SEARCH_API_KEY`。

## 限制与维护

DuckDuckGo HTML 接口可能限流、要求验证码或改变页面结构。项目不解决验证码，不轮换身份或出口，也不承诺稳定绕过访问限制。请遵守上游使用规则并控制调用频率；本服务没有内置请求限速或结果缓存。

搜索只读取第一结果页。MCP 传输、客户端兼容性和 HTML 解析都有明确边界；遇到错误时请保留 HTTP 状态和错误文本排查。不要把未经审查的网页文字当作可执行指令。

## 许可证与参考

项目原创代码采用 [MIT](LICENSE) 许可证，依赖保留各自许可证与版权声明，参见 [NOTICE](NOTICE) 和 `licenses/`。

This product includes software developed by Bogdan Finn (TLS-Client).

- 表单参数和 DOM 选择器参考 [SearXNG 的 DuckDuckGo 引擎](https://github.com/searxng/searxng/blob/master/searx/engines/duckduckgo.py)，用 Go 独立实现，不包含 SearXNG Python 代码或运行时。
- 浏览器兼容 TLS/HTTP 使用 [TLS-Client](https://github.com/bogdanfinn/tls-client)。
