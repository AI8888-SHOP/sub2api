# Sub2API 的 OpenClaw SIWC 接入

这份文档同时面向部署者和接手实现的 AI。新增能力位于 OpenAI 账号入口，凭据仍使用 `platform=openai,type=oauth`，由 `auth_mode=siwc` 区分。SIWC 的授权、换取 token、刷新、密码/TOTP 尝试、重登队列、模型发现和推理请求均在 Go 后端执行。前端编译后嵌入 Go 程序，服务器无需浏览器、Node 或 Python 来执行 SIWC。

项目原有 Codex 账号的外部/本地重登实现保持原样；“纯 Go”在这里指新增 SIWC 的完整服务器执行路径，不代表重写本项目所有原有平台的登录实现。

## 1. 固定基线与真正的 UA

- Sub2API 基线：`5ca3cca21eeaf4ca8a694a7f2f8f0ecd9575c549`，production，v2.10.0。
- OpenClaw 基线：`23dae69850132c4d43e1fcc8885396a7f545cebb`，2026.9.8。
- OpenClaw 此版本依赖 OpenAI JavaScript SDK `7.23.0`。
- SDK `getUserAgent()` 返回 `${this.constructor.name}/JS ${VERSION}`，对应 `OpenAI/JS 7.23.0`。
- OpenClaw SIWC 的 OAuth/目录调用没有显式设置 OpenClaw UA，使用 Node fetch。Go 实现给这些请求设置 `User-Agent: node`。
- 推理给出 `User-Agent: OpenAI/JS 7.23.0`、SDK 语言/包版本/retry-count 头，以及 SIWC preview header。
- `agent_name_hint=OpenClaw` 是应用授权提示，不是 UA。
- 用户手动打开的授权页面使用用户自己的浏览器 UA，这是原版 OpenClaw 浏览器授权流程本来就有的行为。

本实现复制固定版本的应用授权参数和关键请求头。Go TLS/HTTP 实现不会变成 Node/Chromium 的底层网络指纹；未声称完整模拟 TLS 指纹或虚构服务器的 Node 运行版本。这里没有凭空添加 `OpenClaw/xxx`、Codex `originator`、workspace 或组织头。

完整上游拆解见 [openclaw-siwc-upstream.md](openclaw-siwc-upstream.md)。上游源码证明 OAuth 和推理契约；账号密码网页登录适配器是本项目新增的部分，并非 OpenClaw 自身功能。

## 2. 服务器部署与浏览器回调

在“添加账号 → OpenAI → SIWC · OpenClaw”中，选择现有 OpenAI 分组，再点下一步。

### 手动授权

1. 选择“首次在浏览器确认”。
2. 凭据可以留空，只创建可刷新 SIWC 账号；若填一条邮箱/密码/TOTP，授权成功后会加密存储以支持自动重登。
3. 点“开始 SIWC 授权”。服务器生成含 PKCE、state、nonce 的链接。
4. 在自己的电脑或手机浏览器打开链接，使用原账号登录，并允许 OpenClaw 共享额度。
5. 跳转到 `http://localhost:8080/auth/callback?...` 后，复制地址栏的**完整网址**。
6. 即使浏览器报“无法连接”，也可复制网址。这里的 localhost 是用户的电脑，不是服务器；服务器没有启动回调监听，也不需要暴露该端口。
7. 将完整网址粘回 Sub2API，点“完成授权并导入”。

首次动态注册的 callback 必须包含服务端返回的真实 `client_id=oaiapp_...`，不能只粘 code，不能手工补造 client ID。网址里的 code、state、client_id 均参与验证。

服务器已有 HTTP 服务也可能使用 8080，这与用户浏览器地址栏中的 localhost:8080 没有联系。浏览器看到 404 或连接失败不代表授权失败。

### Go 自动尝试

每行输入：

```text
邮箱----密码----TOTP长期密钥
```

TOTP 字段是 Base32 长期密钥，不是当前六位验证码。前端沿用项目已有解析器，支持最多 100 条且拒绝同一批重复邮箱，按账号顺序执行。

自动流程使用独立的内存 cookie jar、固定 auth.openai.com 目标、有限次数的跳转和 HTTP 请求。能处理服务端表单中的邮箱、密码、隐藏 CSRF 字段、可识别的共享额度复选框/确认按钮，以及观察到的密码、MFA challenge/verify API。

自动登录的范围与限制必须明确：

- 纯 Go 不执行授权页的 JavaScript。
- 未识别的 SPA 页面、CAPTCHA、Sentinel/浏览器校验、邮件/短信 OTP、额外身份验证、未知 consent 控件、外部身份提供方会转为 `requires_manual`。
- 不生成或绕过上游的安全验证证明。
- 上游页面可能要求用户手动登录，因此不能保证任意账号都能仅凭密码/TOTP 全自动完成。
- 已识别的 OpenClaw 共享额度表单可自动确认；未知授权表单交由浏览器。
- 一个密码或验证码不会因拒绝而无限重试。
- 凭据不会发给项目默认外部重登地址。

这是一条有浏览器授权后备的纯 Go 登录链路，并非已通过真实 SIWC 账号验收的“百分百自动登录”。

## 3. 账号存储与同邮箱并存

SIWC 使用普通账号创建，不经过 `importCodexSession`。已有 Codex 导入索引会忽略 SIWC 账号，即使邮箱相同也不会覆盖。

示意凭据：

```json
{
  "auth_mode": "siwc",
  "auth_flow": "chatgpt-token-sharing",
  "issuer": "https://auth.openai.com",
  "client_id": "oaiapp_example",
  "siwc_subject": "verified-subject",
  "siwc_identity": "sha256-identity-key",
  "authorization_scope": "openid email profile resource.invoke chatgpt.tokens.use.direct offline_access",
  "granted_scope": "openid email profile resource.invoke chatgpt.tokens.use.direct offline_access",
  "access_token": "example-only",
  "refresh_token": "example-only",
  "id_token": "example-only",
  "email": "user@example.test",
  "expires_at": "2026-10-07T12:00:00Z"
}
```

身份散列计算：`SHA256(issuer + NUL + client_id + NUL + verified_sub)`。它用于身份隔离，不是 ChatGPT workspace ID，不发送为推理 header。

密码和 TOTP 通过现有凭证运营加密配置单独存储，不放入账号 token map。自动登录前先检查/启用凭据加密。备份现有加密密钥，遵循项目凭证运营文档。

同邮箱 Codex OAuth 与 SIWC 可以加入同一 OpenAI 分组，获得不同账号 ID、token cache key 和刷新身份。SIWC 独立应用注册的 client ID 不会替换 Codex 固定客户端 ID。

## 4. OAuth 与校验

| 用途 | 实际地址/参数 |
|---|---|
| authorize | `https://auth.openai.com/api/accounts/authorize` |
| token | `https://auth.openai.com/api/accounts/oauth/token` |
| JWKS | `https://auth.openai.com/.well-known/jwks.json` |
| redirect_uri | `http://localhost:8080/auth/callback` |
| resource | `https://api.openai.com/v1` |
| 首次注册 client_id | `dynamic_agent_client` |
| 应用提示 | `agent_name_hint=OpenClaw` |
| 实际 token client_id | callback 返回、经验证的 `oaiapp_...` |
| PKCE | S256，随机 Base64url verifier |
| 会话 | 独立随机 session ID、state、nonce，30 分钟有效、数量限制、一次消费 |

初始 ID token 必须经固定 issuer JWKS 验证 RS256 签名、issuer、audience、sub、iat、exp、nonce、azp 和多 audience 规则。不能沿用项目普通 Codex 的“只解码 JWT”方法。

共享权限依据**实际返回 scope**判断：

```text
resource.invoke AND
(chatgpt.tokens.use.direct OR chatpass.enable.request.direct)
```

身份登录成功但未共享额度标记为 `chatgpt-identity`，不能创建成可推理账号，也不能回退到 Codex backend。

## 5. 刷新和重登

刷新沿用项目已有刷新锁、token version、合并和存储机制，SIWC 的 dispatch 在 Codex token endpoint 之前执行。

SIWC refresh 表单包含 `grant_type=refresh_token`、真实 client ID、refresh token、resource。服务端不返回新的 refresh token、scope 或 ID token 时保留原值；返回新的 ID token 时重新验签，subject 必须保持一致。scope 改成 identity-only 时禁止继续推理。

同进程内按身份和旧 refresh token 去重并发刷新，已验证结果保留最多 5 分钟、最多 64 项；不同身份不会被同一网络刷新锁阻塞。多实例仍依赖项目原有分布式刷新锁和凭据版本控制。

自动重登不依赖 Python Worker：

1. 凭证运营检查 SIWC 模型目录，401 或 `invalid_grant` 表示需要重新授权。
2. 达到失败阈值后复用持久化重登任务、凭据快照和 CAS 机制。
3. 内置 Go consumer 每 5 秒领取 SIWC 任务；数据库行锁隔离多个实例。
4. Go 从加密配置中读取密码/TOTP，用已有注册 client ID、原 subject 和原 scope 重新授权。
5. callback 由 Go 后端换取并验签。
6. 身份一致且凭据未被并发修改时原子写回，完成任务并恢复调度。
7. 额外验证导致失败时，任务提示在账号编辑页选择“SIWC 重新授权”。这里重新生成授权链接并粘贴 callback。

项目现有外部重登/Python 队列的 SQL 会排除 SIWC。即使全局选了 Session Studio，也不会把 SIWC 密码发给它。SIWC 不支持 Mihomo 登录租约模式，使用账号代理或现有 managed proxy。

目录 403 不直接推定 token 被撤销，可能是权限、访问地区或其他上游拒绝，不会因此直接触发密码重登。

## 6. 模型与推理

模型目录使用同账号 access token：

```http
GET https://api.openai.com/v1/models
Authorization: Bearer <SIWC access token>
User-Agent: node
```

解析上游 `models[]` 的 `slug/display_name/visibility`，只保留 `visibility=list`，转换为 Sub2API 的标准 `data[]`。成功空目录保持为空，不编造默认模型。账号测试使用同一目录和同一 SIWC gateway；全 SIWC 分组的普通 `/v1/models` 使用这些真实目录。

混合分组继续沿用项目已有模型公布策略；它不是每个账号权限的交集。推理权限最终仍由上游验证。客户端 `client_version` 的 Codex manifest 是项目既有兼容入口，不等同于 SIWC 上游模型目录；查看 SIWC 权限应使用普通模型列表/账号测试目录。

推理：

```http
POST https://api.openai.com/v1/responses
Authorization: Bearer <SIWC access token>
User-Agent: OpenAI/JS 7.23.0
x-openai-chatpass-test: codex-direct
Content-Type: application/json
Accept: text/event-stream
```

preview header 与固定 OpenClaw 版本绑定，未来上游移除时需同步更新。它不能代替合法共享授权。

最终请求体规范化：

- 上游始终 `stream=true,store=false`。
- 删除 context_management、metadata、max_output_tokens、temperature、top_p、prompt_cache_retention。
- service_tier 只接受 default、priority、ultrafast、slow。
- 支持 function / web_search 工具；拒绝 hosted MCP、tool search、image generation 等工具。
- 拒绝 previous_response_id、conversation、background 和 item_reference 的服务端状态依赖，需要完整上下文。
- 不启用上游 WebSocket、compact、图片生成、embedding、独立音视频端点。
- 拒绝嵌套音频输入、item_reference、已有 file_id 引用；图片 URL/data URL 和内联文件内容仍由具体模型判断是否接受。
- 不注入 Codex account ID、originator、组织/project 头，不走 Codex plugin 或 BPS。

下游 `stream=false` 时聚合上游 SSE 的终止 response 为 JSON；下游 streaming 沿用现有 SSE 转发和计费。`/v1/chat/completions` 沿用本项目的 Chat → Responses → Chat 转换，最终通过同一 SIWC 请求构造器。它不是把 SIWC token 发给 Chat Completions 上游。

## 7. 源码导航

| 文件 | 职责 |
|---|---|
| `backend/internal/pkg/siwc/client.go` | 参数、PKCE、callback、签名校验、刷新、目录、最终 body/header |
| `backend/internal/pkg/siwc/login.go` | 纯 Go cookie jar、表单、密码/TOTP、额外验证后备 |
| `backend/internal/service/openai_siwc.go` | bounded 初次登录任务与凭据转换 |
| `backend/internal/service/openai_siwc_reauth.go` | Go 重登消费者 |
| `backend/internal/service/openai_siwc_gateway.go` | Responses SIWC 入口 |
| `backend/internal/repository/openai_oauth_reauth_repo.go` | SIWC-only 领取与旧消费者隔离 |
| `backend/internal/handler/admin/openai_siwc_handler.go` | 管理 API |
| `frontend/src/components/account/OpenAISiwcLogin.vue` | 自动/浏览器模式、轮询、callback、重试 |
| `CreateAccountModal.vue / EditAccountModal.vue` | 账号创建、分组、重新授权 |

管理 API：

```text
POST   /api/v1/admin/openai/siwc/sessions
GET    /api/v1/admin/openai/siwc/sessions/:session
DELETE /api/v1/admin/openai/siwc/sessions/:session
POST   /api/v1/admin/openai/siwc/exchange
```

POST sessions 支持 `proxy_id`、可选 `login={email,password,totp_secret}`；重新授权附 `account_id`，服务端绑定该账号的 client/subject。exchange 需要 `session_id,callback_url`。

这些接口沿用管理员认证；敏感响应设置 no-store，session ID 不代表公共匿名登录接口。会话在内存中，服务器重启或多实例路由到其他实例后应重新生成；多实例部署对授权请求使用粘性路由。

审计中间件对 sessions 和 exchange 两个 POST 接口整体省略请求体，保留操作、时间、结果等审计信息；不会把登录密码、TOTP 密钥或完整 callback 存入审计表。关闭服务时取消并等待 Go 自动登录任务，取消后的回调结果不得继续导入账号。

## 8. 构建

构建机需要 Go 1.27 和 Node/pnpm；部署机只需编译好的程序与项目已有数据库/Redis 等运行设施。

```bash
# 仓库根目录
cd frontend
corepack pnpm@9.15.9 install --frozen-lockfile
corepack pnpm@9.15.9 run check:i18n
NODE_OPTIONS=--max-old-space-size=8192 corepack pnpm@9.15.9 exec vue-tsc -b
NODE_OPTIONS=--max-old-space-size=8192 corepack pnpm@9.15.9 exec vite build
cd ../backend
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags embed -trimpath \
  -ldflags='-s -w -X main.Version=2.10.0-siwc -X main.BuildType=release' \
  -o ../artifacts/sub2api-siwc-linux-amd64 ./cmd/server
```

2026-10-08 用户要求将本次源码覆盖到自有仓库 `AI8888-SHOP/sub2api` 的 `production` 分支并编译发版，发布版本为 `v2.10.1`。发布使用仓库 Release 工作流，构建多平台安装包和镜像；发布安装包不等于已部署到业务服务器。

## 9. 验证边界

离线验证包括 mock JWKS/token、动态注册 callback、nonce/audience/issuer/azp/expiry、refresh 省略字段和身份变化、目录空值/可见性、Go 密码/TOTP/consent 流程、跨站跳转拒绝、SSE/JSON、Chat Completions 桥接、同邮箱导入隔离及前端重试。

2026-10-07 已获得用户提供的临时账号授权，进行了实际测试：

1. 纯 Go 访问授权入口返回 HTTP 403，在提交密码/TOTP 前停止。
2. 用户在自己浏览器打开后备链接，返回包含 code、state、真实动态 client ID 和共享 scope 的完整 callback。这里只确认收到回调，不以回调参数代替 token 验证。
3. Go 用同一内存会话的 PKCE verifier 交换授权码，token 端点返回 HTTP 403，没有获取到 token。
4. 随后对相同 token 端点进行不含账号秘密的诊断请求，响应为 `unsupported_country_region_territory`（Country, region, or territory not supported），说明当前服务器访问出口受到地区限制。
5. 未执行真实 refresh、目录查询或推理，因为没有可用 token；没有保存账号密码、TOTP、callback 或 token 到仓库与文档。

当前交付包含离线验证的 Go 实现，不是已验收的全自动真实登录服务。真实密码/TOTP 页面、共享 consent UI、刷新、重登和推理仍需在上游支持地区的部署环境用隔离账号验收。授权码交换一旦发起，会话即一次消费；失败后必须重新生成链接，不要反复粘贴本次旧 callback。

### 2026-10-08 最终本地检查

- `go test -race ./internal/pkg/siwc -count=1 -timeout=120s`：通过，包含并发刷新去重、不同身份独立刷新、密码/TOTP/共享表单模拟、JWT 验证、请求规范化和错误脱敏。
- service/repository 中 SIWC、OpenAI OAuth 重登、AccountTokenGuardV2 相关回归：通过。
- admin handler、公共模型读取及审计中间件的相关回归：通过。
- 前端 SIWC、创建账号弹窗、OpenAI 账号徽标的组件回归：89 项通过；包含取消后丢弃迟到回调结果。
- `vue-tsc -b` 与 Vite production build：通过。构建机 Node 内存上限设为 8 GiB。原项目仍有大 chunk、混合动态/静态导入和 Browserslist 数据过旧提示。
- 最终产物：`artifacts/sub2api-siwc-linux-amd64`，版本 `2.10.0-siwc`，包含编译后的前端；`file` 确认为静态链接的 Linux x86-64 ELF。
- 同目录 `.sha256` 文件记录最终产物校验值。在全新临时 DATA_DIR、清空继承环境变量并指定独立配置路径后启动：安装状态接口、嵌入首页及 JS 静态文件均返回 HTTP 200。检查后已关闭临时进程，没有连接生产数据库。
- 未进行已登录管理页的浏览器端到端验收，也未执行整个项目的全部测试。以上结果不替代真实 SIWC 上游验收。
