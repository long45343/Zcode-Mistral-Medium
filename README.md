# Zcode-Mistral-Medium

一个部署在 AI 网关和上游模型服务之间的窄中间件，用来修正 Responses API 请求体。

它解决两个具体问题：某些客户端发出的消息项缺少 `type` 字段会被上游拒绝（Mistral 报
3051）；某些服务端返回的原始思考事件用的是 `reasoning_text` 频道，而基于 Vercel AI
SDK 的客户端只认 `reasoning_summary_text`。

> 本项目与 AxonHub、ZCode、Mistral、Vercel AI SDK 均无隶属或背书关系，
> 名称仅用于说明使用场景。
>
> `testdata/` 里的请求体和事件流全部是为测试构造的虚构数据，不含任何真实
> 用户内容。

它只做这几件事：

1. 在管理页面配置上游的 BASE URL 和 API KEY
2. 给 `input` 数组里缺少 `type` 的消息项补上 `"type": "message"`
3. 图片超过单请求上限时，把较早的替换成文字占位
4. 转发请求，响应（含 SSE）默认原样透传
5. 可选：把原始思考事件改写成摘要事件（见下）

前两项和图片裁剪直接对应上游的几个硬性拒绝，详见各自的章节。

## License

MIT，见 [LICENSE](LICENSE)。

## 为什么需要补 type

Mistral 的 Responses 接口对没有 `type` 字段的输入项直接报错：

```
400 {"error":{"code":"3051","message":"input: Value error, Input items of type None are not supported."}}
```

从第二轮开始，ZCode 会把上一轮的助手回复作为 `assistant` 消息放进 `input`，
它的 content part 是 `output_text`，项本身却没有 `type`，于是必然触发。

已经带 `type` 的项（`function_call`、`function_call_output` 等）不会被改动。

## 图片数量上限

上游对单个请求的图片总数有硬限制，20 张封顶：

```
400 {"error":{"code":"3051",
     "message":"Total number of images exceeds the maximum allowed of 20."}}
```

计数是**整个请求的总数**，消息里的图片和 `function_call_output` 里的图片一起算。
一个跑久了的会话会不断积累截图，超过 20 张后整条对话就再也发不出去了。

开启「自动裁剪超限图片」后（默认开启），网关保留**最近的** 20 张，更早的替换成
一行文字占位：

```
[image omitted: request exceeded the 20-image limit]
```

文字历史不受影响，模型仍能看到当时发生了什么，只是看不到那张具体的图。上限
可以通过 `max_images` 配置。

## 可选的思考过程改写

Mistral 这类开源模型在 Responses 流里发的是**原始思考过程**：

```
response.reasoning_text.delta
```

而 Vercel AI SDK（ZCode 用的就是它）只实现了**摘要**分支：

```
response.reasoning_summary_text.delta
```

结果就是思考的 token 被消耗、被计费、也统计进了 usage，但客户端一个字都显示不出来。

打开改写开关后，网关会把事件名换掉，让 SDK 认出来：

| 上游发出 | 改写为 | 补充字段 |
|---|---|---|
| `response.reasoning_text.delta` | `response.reasoning_summary_text.delta` | `summary_index` |
| `response.content_part.added`（part 为 reasoning_text） | `response.reasoning_summary_part.added` | `summary_index` |
| `response.content_part.done`（part 为 reasoning_text） | `response.reasoning_summary_part.done` | `summary_index` |

文本内容、item id、事件顺序都不变。`summary_index` 必须补，否则 SDK 拼出的
内部 id 会以 `:undefined` 结尾，多个思考块互相覆盖。

开关方式：

```bash
# 命令行
./gateway -rewrite-reasoning

# 或环境变量
REWRITE_REASONING=1 ./gateway
```

关闭时（默认）走纯字节转发，不解析、不缓冲、不增加延迟。

注意：开启后响应流不再是无状态字节复制，而是按 SSE 帧边界重组。帧被 TCP
切开的情况已处理，但仍属于有状态的路径。

## 部署

```bash
# 1. 交叉编译
export PATH="/c/Program Files/Go/bin:$PATH"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o gateway .

# 2. 上传
scp -C gateway Dockerfile docker-compose.yml root@<host>:/opt/zcode-gateway/

# 3. 构建并启动
ssh root@<host> 'cd /opt/zcode-gateway && \
  DOCKER_BUILDKIT=0 docker build -t zcode-cache-gateway:latest . && \
  docker compose up -d'
```

## 配置

打开 `http://<host>:8787/`，填两个字段：

| 字段 | 说明 |
|---|---|
| BASE URL | 上游地址，例如 `https://api.mistral.ai/v1`，转发目标会自动拼成 `/responses` |
| API KEY | 可选。填写后由网关统一使用；留空则直接沿用 AxonHub 转发的密钥 |

密钥的取值顺序：页面配置的优先；没配置就用请求头 `Authorization` 里的；
两者都没有时返回 503。

保存后立即生效，写入容器的 `/data/settings.json`。

页面用 HTTP Basic 认证保护。浏览器会弹出登录框，输入后同一站点下的请求自动
带上凭据，不用把密码放进 URL。

默认凭据是 `admin` / `admin`，启动参数或环境变量可改初始值：

```bash
./gateway -username admin -password admin
# 或 DASHBOARD_USERNAME / DASHBOARD_PASSWORD
```

部署后可以直接在页面的「登录凭据」区修改，改完立即生效，旧密码随即失效，
浏览器会提示重新登录。密码至少 4 位。

转发接口 `/v1/responses` 另有更严格的限制：只接受来自本机的请求，与密码无关。

注意：Basic 认证的凭据是明文随请求发送的，如果通过公网访问，请在前面挂一层
HTTPS（反向代理即可），否则密码可被链路嗅探。

## 接到 AxonHub

把 Mistral 通道的 `base_url` 改成网关地址：

```
https://api.mistral.ai/v1  ->  http://127.0.0.1:8787/v1
```

通道里的 API Key 可以照常填真实密钥，网关会直接使用它；
如果改由网关统一管理密钥，这里填任意占位值即可。

## 接口

| 路径 | 用途 |
|---|---|
| `POST /v1/responses` | 转发入口 |
| `GET /healthz` | 健康检查，返回当前上游地址与是否已配置 |
| `GET /api/settings` | 读取配置（密钥只返回掩码） |
| `POST /api/settings` | 保存配置 |

## 设计边界

刻意不做的事：不转协议（两边都是 Responses 格式）、不改模型名、不合并消息、
不解析或改写响应内容、不做多通道负载均衡。这些都归 AxonHub。

## 测试

```bash
go test ./...           # 单元 + 回放
go test ./internal/reasoning/ -v
```

`testdata/` 里是脱敏后的真实请求体：

- `body_60306.json` —— 触发 3051 的那一条
- `reasoning_stream.sse` —— 从上游真实抓取的 28KB SSE 流，含 54 个原始思考事件

回放测试会把这股流按 1 字节到 4KB 的随机边界切 30 轮，验证改写结果与单次写入完全一致。
