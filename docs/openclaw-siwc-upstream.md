# OpenClaw SIWC 登录与订阅推理调用完整拆解

> 面向：需要理解、审计或独立实现此协议的开发者和 AI 编程助手。
>
> SIWC = Sign in with ChatGPT。本文专指 OpenClaw 中 `--method siwc` 的应用授权与 token sharing，不把所有 ChatGPT OAuth 登录统称为 SIWC。
>
> 分析日期：2026-10-07。OpenClaw 固定源码基线：`23dae69850132c4d43e1fcc8885396a7f545cebb`。Codex CLI 对照源码基线：`3421c660d043e39d1bff8cee136c0ceeb006177c`。链接均尽量固定到提交，不能据此推断任意已安装版本已经支持这些行为。

## 0. 先给 AI 的核心结论

OpenClaw SIWC 是一条独立的、应用级 OAuth 授权路径：用户在 OpenAI 授权页允许 OpenClaw 使用其 Codex allowance，OpenClaw 获得面向 `https://api.openai.com/v1` 的 access token，再直接调用公共 Responses 端点。

```text
授权入口： https://auth.openai.com/api/accounts/authorize
Token：   https://auth.openai.com/api/accounts/oauth/token
JWKS：    https://auth.openai.com/.well-known/jwks.json
资源：    https://api.openai.com/v1
模型目录： GET  https://api.openai.com/v1/models
推理：    POST https://api.openai.com/v1/responses
回调：    http://localhost:8080/auth/callback
存储类型： type = oauth
推理许可： authFlow = chatgpt-token-sharing
仅身份：   authFlow = chatgpt-identity
```

必须同时记住：

1. **它使用 API，但不是 Platform API Key 计费模式。** URL 与公共 Responses API 相同；凭证、授权范围、额度归属和允许参数不同。
2. **它和原版 Codex CLI 默认 ChatGPT 登录不走同一推理端点。** 后者默认使用 `https://chatgpt.com/backend-api/codex/responses`。
3. **它不是通过第三方服务器反代 Codex backend。** OpenClaw 自有 runtime 可以直接请求 OpenAI；选用 Codex app-server runtime 时，会经过 OpenClaw 自己的本机 relay，最终上游仍是公共 Responses API。
4. **登录成功不等于有推理权限。** 实际返回的 scope 必须含 token sharing 授权；只授予身份权限的凭证必须禁止推理。
5. **不能把任意 Codex CLI token 改个 URL 当 SIWC token 用。** resource、client registration、scope 和服务端授权上下文都不同。
6. **不能从源码承诺任意账号可用。** 注册、workspace 策略、模型权限、额度和当前 Beta 开放情况最终由 OpenAI 决定。

这里的“token sharing”指授权应用使用模型额度，不是把 ChatGPT 浏览器 cookie、网页会话或其他应用的秘密令牌交给第三方。

## 1. 文档证据与边界

本文包含三类内容，阅读和实现时应区分：

| 标记 | 含义 |
| --- | --- |
| 源码行为 | 固定提交的代码、测试或仓库文档中直接可见的行为 |
| 实现建议 | 为独立实现补充的工程要求，不声称 OpenClaw 每一层都按同一方式实现 |
| 示例 | 用于解释协议的脱敏请求或伪代码，不是实际账号返回值，也不是已完成线上验证的 SDK |

验证范围：已核对缓存 OpenClaw 源文件与该提交 Git tree 中的 blob 哈希；已读取固定提交的原版 Codex CLI 授权和 provider 源文件。未使用用户真实账号完成授权、消耗订阅额度或调用付费推理。官方 Codex auth 和 Responses streaming 文档本次访问返回 HTTP 403，因此本文的细节结论以明确列出的源码与仓库文档为依据，不伪称已经获得服务端实测证明。

当前本地 OpenClaw 检出为较早提交 `7098e335bfe96f7999f4ff1de5641bf89393f413`，其中没有本文描述的 `token-sharing` 模块。本文是一份独立研究和实现规格，不能据本地旧目录内容否定固定基线中的新实现，也不能在旧版上照抄命令后保证成功。

推荐阅读路线：

| 目标 | 章节 |
| --- | --- |
| 先判断是不是 Codex 反代 | 0、2、4、15 |
| 独立实现登录和刷新 | 5 至 10、17、19 |
| 已有合法 SIWC 凭证，理解模型调用 | 11 至 14、16、18 |
| 复用 Codex app-server | 14、15，尤其两条路径的区别 |
| 交给另一个 AI 继续开发 | 完整文档，重点 17 的实现规格与 19 的验收清单 |
| 升级版本或追溯结论 | 20 的固定提交源码索引 |

## 2. 与 Codex CLI 登录、API Key 的精确对比

### 2.1 三种模式

| 维度 | OpenClaw SIWC | 原版 Codex CLI 的默认 ChatGPT 浏览器登录 | Platform API Key 调用 Responses |
| --- | --- | --- | --- |
| 身份 | 用户和 workspace 为应用授权 | 用户使用 Codex 产品 | Platform 项目及其 key 权限 |
| OAuth client | 首次动态注册，保存真实 `oaiapp_*` | Codex 自身的 client | 不适用 |
| authorize 路径 | `/api/accounts/authorize` | `/oauth/authorize` | 不适用 |
| token 路径 | `/api/accounts/oauth/token` | `/oauth/token` | 不适用 |
| 显式 OAuth resource | `https://api.openai.com/v1` | 对照的浏览器请求不传 resource | 不适用 |
| 回调 | `localhost:8080/auth/callback` | 默认端口 1455，源码有端口回退机制 | 不适用 |
| 核心推理授权 | `resource.invoke` 与 direct sharing scope | Codex 产品授权 | key 和项目权限 |
| 默认推理 URL | `https://api.openai.com/v1/responses` | `https://chatgpt.com/backend-api/codex/responses` | `https://api.openai.com/v1/responses` |
| 额度归属 | 符合条件的请求使用 Codex allowance | Codex allowance | Platform API 计费 |
| 请求协议 | Responses，HTTP SSE | Codex Responses transport；配置及版本可影响传输 | Responses，具体能力依接口和模型 |
| 凭证刷新 | OpenClaw 管理 SIWC profile | 原版 CLI 管理自己的登录状态 | key 无 OAuth refresh，按项目策略轮换 |
| 是否同一种 token | 独立授权 token | 不可直接假定与 SIWC 互换 | API Key，不是 OAuth access token |

表格中的“默认”限定为未设置自定义 provider/base URL 的相关登录模式。原版 Codex CLI 同样支持 API Key 和自定义 provider；这不改变其默认 ChatGPT 登录的路由结论。[S02][C01][C02]

### 2.2 原版 Codex CLI 的直接证据

在 `codex-rs/model-provider-info/src/lib.rs` 中：

```rust
pub const CHATGPT_CODEX_BASE_URL: &str =
    "https://chatgpt.com/backend-api/codex";
```

`to_api_provider()` 将 `AuthMode::Chatgpt`、`ChatgptAuthTokens` 等指定身份模式的默认 base URL 设为这个 Codex 地址；其他模式默认 `https://api.openai.com/v1`，显式 `base_url` 可以覆盖默认值。

在 `codex-rs/login/src/server.rs` 中，浏览器授权构造器使用：

```text
endpoint = {issuer}/oauth/authorize
scope = openid profile email offline_access api.connectors.read api.connectors.invoke
resource = None
```

这与 SIWC 请求的 scope 和路径不同。这里描述的是**原版 Codex CLI 浏览器登录**，不能套到 OpenClaw 的 Codex 登录实现或 device-code 的 scope 上；OpenClaw 文档明确提醒不同登录入口获得的 connector 权限可能不同。[C01][C02][S02]

### 2.3 避免用变量名判断计费模式

OpenClaw 的共用接口可能把 Bearer 凭证放在 `apiKey`、`discoveryApiKey` 等字段中，SIWC 的 model auth policy 还返回 `authRequirement: "api-key"` 来匹配公共 Responses 调用面。

**这些是内部接口或路由分类，不代表 OAuth token 被转换成 Platform API Key，也不代表切换了计费账户。** 判断应看 profile 的 `type/authFlow`、resource、实际 Authorization token 和目的端点。[S06][S09][S13][S16]

## 3. 最小独立实现需要哪些模块

如果目标只是“脱离 OpenClaw，在自己的应用中登录 SIWC 并调用模型”，无需复制 Gateway、聊天渠道或整个 Codex app-server：

| 模块 | 职责 | 是否必要 |
| --- | --- | --- |
| OAuth coordinator | 首次注册、重连、PKCE、state、nonce、超时 | 必要 |
| Loopback callback | 接收本机回调并严格校验一次性事务 | 必要，按本文的本机授权形态 |
| Identity verifier | JWKS、JWT 签名、issuer、audience、nonce 校验 | 必要 |
| Credential store | 保存注册身份和可刷新的凭证，处理原子写入 | 必要 |
| Refresh coordinator | 同一 profile 串行刷新，防止轮换覆盖和账号串线 | 必要 |
| Model catalog | 用同一个账号的 access token 获取可见模型 | 必要的账号权限发现环节 |
| Responses adapter | SIWC payload 策略、HTTP SSE、多轮上下文 | 必要 |
| Tool loop | 执行本地 function tools 并回传结果 | 需要工具时实现 |
| Local Codex bridge | 启动隔离 app-server、注册私有 provider、relay | 只有要复用 Codex runtime 时需要 |

动态注册可否用于自己的应用、允许什么 redirect URI、应用展示名称和开放资格，由服务端规则决定。源码可说明 OpenClaw 如何请求，不能证明任意第三方复制这些参数就一定能注册。独立应用应明确自己的应用身份和获得的授权，不冒充 OpenClaw。

## 4. 架构总览

```mermaid
flowchart TD
    U[用户浏览器] -->|登录并确认授权| AUTH[auth.openai.com]
    AUTH -->|code + state + 首次注册 client_id| CB[本机 callback :8080]
    CB --> O[OAuth coordinator]
    O -->|code + verifier + real client_id + resource| AUTH
    AUTH -->|access / refresh / id_token / scope| O
    O -->|验签并持久化| STORE[应用凭证存储]
    STORE --> REFRESH[统一刷新协调器]
    REFRESH -->|refresh grant| AUTH
    STORE --> DISC[模型目录客户端]
    DISC -->|GET /v1/models| API[api.openai.com]
    STORE --> DIRECT[OpenClaw runtime 或独立 Responses 客户端]
    DIRECT -->|POST /v1/responses + SSE| API
    CODEX[受管理的本机 Codex app-server] --> RELAY[OpenClaw 本机 relay]
    STORE --> RELAY
    RELAY -->|POST /v1/responses + SIWC Bearer| API
```

有两个不同的本地网络服务：

- `localhost:8080/auth/callback` 是登录期间的一次性 OAuth 回调服务。
- Codex inference relay 使用独立的本机端口和私有随机路径，服务于运行时推理；不是 8080 的 OAuth callback。

SSE 是模型推理结果的数据流；OAuth 浏览器回调不是 SSE。Codex app-server 的控制通信与模型推理传输也应分开理解：模型侧禁用 WebSocket 不等于整个应用完全不能使用 WebSocket。

## 5. 协议常量和数据字典

### 5.1 固定基线中的常量

| 名称 | 值 | 用途 |
| --- | --- | --- |
| `TOKEN_SHARING_AUTH_FLOW` | `chatgpt-token-sharing` | 已获得共享推理许可 |
| `IDENTITY_AUTH_FLOW` | `chatgpt-identity` | 仅身份许可 |
| `TOKEN_SHARING_RESOURCE` | `https://api.openai.com/v1` | OAuth resource 与推理 base URL |
| `TOKEN_SHARING_ISSUER` | `https://auth.openai.com` | JWT issuer 和认证服务器 |
| `TOKEN_SHARING_REDIRECT_URI` | `http://localhost:8080/auth/callback` | 注册/交换使用的回调字符串 |
| `TOKEN_SHARING_CLIENT_ID` | `dynamic_agent_client` | 首次动态注册的入口标记 |
| `TOKEN_SHARING_SCOPE` | `openid email profile resource.invoke chatgpt.tokens.use.direct offline_access` | 新注册请求的权限 |
| `TOKEN_SHARING_LEGACY_SCOPE` | `openid resource.invoke chatgpt.tokens.use.direct offline_access` | 旧注册没有 scope 元数据时的重连请求 |

`dynamic_agent_client` **不是最终可持久化的注册 client ID**。首次回调会给出真实 `oaiapp_*`，授权码交换、ID token audience 校验和 refresh 必须使用这个真实值。[S03][S04]

### 5.2 不同 token 的职责

| 字段 | 用途 | 是否发给推理端 |
| --- | --- | --- |
| authorization code | 一次性交换凭证 | 否 |
| PKCE verifier | 证明发起者持有授权事务的秘密 | 否，只发 token endpoint |
| access token | Bearer 资源访问凭证 | 是 |
| refresh token | 换取下一代 access token | 否，只发 token endpoint |
| ID token | 验证登录身份、client audience 与 nonce | 否 |
| state | 将 callback 绑定到发起的浏览器事务 | 否 |
| nonce | 将 ID token 绑定到本次身份认证 | 否 |
| `accountId` | OpenClaw 内部身份哈希与刷新隔离 | 否 |

## 6. 登录入口和使用者操作

固定基线文档中的 agent 凭证入口：

```bash
openclaw models auth login --provider openai --method siwc
```

个人账号入口：

```bash
openclaw models accounts login openai --method siwc
```

两者凭证所属范围不同。个人账号不能未经选择就覆盖 agent 的共享凭证；服务多个使用者时，授权事务必须绑定到正确的用户、agent 和 profile。登录过程中用于“重连”的已有 profile 列表也只能包含该发起者有权使用的条目。[S02][S04]

浏览器授权时，需要批准 token sharing。只批准身份权限仍可能显示登录成功，但不能完成模型推理。

如果浏览器与 OpenClaw 在不同机器，先在浏览器机器建立隧道：

```bash
ssh -N -L 8080:127.0.0.1:8080 user@gateway-host
```

之后打开 OpenClaw 提供的授权链接。浏览器访问自己的 localhost:8080，通过隧道到运行 OpenClaw 的机器。不能仅在远程 shell 中执行登录，就假定浏览器的 localhost 会指向远程机器。

这里的 SIWC 流程没有展示独立 device-code 实现。`--method device-code` 在该 provider 中走的是 Codex 登录分支，不能把它当作 SIWC 的无回调替代方案。[S01][S09]

## 7. 完整 OAuth 时序

```mermaid
sequenceDiagram
    participant A as 应用
    participant L as 本机回调服务
    participant B as 浏览器
    participant O as OpenAI Auth
    participant S as 凭证存储
    A->>A: 选择新注册或原 profile 重连
    A->>A: 生成 verifier/challenge、state、nonce
    A->>L: 先绑定 127.0.0.1:8080
    A->>B: 打开 authorize URL
    B->>O: 用户登录、选择 workspace、批准授权
    O-->>B: 重定向到 localhost callback
    B->>L: GET code + state + 可选/必需 client_id
    L->>L: 路径、方法、唯一参数、state 校验
    L-->>A: 接受一次有效 callback，暂缓成功页面
    A->>A: 验证返回 client_id
    A->>O: POST token，带 PKCE verifier 与真实 client_id
    O-->>A: access_token / refresh_token / id_token / scope
    A->>O: GET JWKS
    A->>A: 验签、audience、nonce、身份绑定、scope 分类
    A->>L: 完成本次浏览器结果页面
    A-->>S: 返回 credential 给宿主认证流程持久化
    A->>L: finally 关闭 listener
```

注意源码的持久化边界：`loginTokenSharing()` 返回 `ProviderAuthResult` 给上层认证宿主，数据库写入由宿主负责。浏览器 callback 完成不能单独证明宿主持久化已成功。独立实现应向调用者报告“已持久化可用”还是“只完成授权交换”。

### 7.1 创建授权事务

每次登录重新生成：

```text
verifier  = 密码学随机值的 base64url 编码
challenge = BASE64URL(SHA256(ASCII(verifier)))
state     = 独立随机值
nonce     = 另一独立随机值
```

OpenClaw 的 state/nonce 使用 32 字节随机值的 base64url 编码。不要让 nonce 与 state 或 verifier 共用一个变量，也不要复用上一次取消的授权事务。

登录总超时为 5 分钟；取消信号同时管理 listener、等待、token 请求和结果归属。即使网络请求较晚返回，已经取消或被替换的登录也不能覆盖当前 profile。[S04][S18]

### 7.2 首次注册的 authorize 请求

下面是解码后的参数表，实际 URL 必须通过 `URLSearchParams` 等方式编码：

```text
GET https://auth.openai.com/api/accounts/authorize

response_type=code
client_id=dynamic_agent_client
redirect_uri=http://localhost:8080/auth/callback
resource=https://api.openai.com/v1
scope=openid email profile resource.invoke chatgpt.tokens.use.direct offline_access
code_challenge=<本次 PKCE challenge>
code_challenge_method=S256
state=<本次 state>
nonce=<本次 nonce>
agent_name_hint=OpenClaw
```

`agent_name_hint=OpenClaw` 是 OpenClaw 首次注册时的实际行为，重连时不传。注册由授权流程内部处理，本文没有证据支持额外发明一个 `/register` HTTP 调用。

### 7.3 已有注册的重连

已有 profile 重连时：

1. 使用已保存的真实 `clientId`，不再使用入口标记。
2. 优先使用保存的 `authorizationScope`；没有时使用旧版较窄 scope。
3. 请求 scope 中的旧拼写 `chatpass.enable.request.direct` 规范化为 `chatgpt.tokens.use.direct`。
4. 重新生成 state、nonce 和 PKCE。
5. 使用原 ChatGPT 用户与 workspace；要更换其中任何一个，选择新建连接。
6. callback 不得把已知 client ID 替换为另一个注册 ID。
7. token 验证后还要匹配原身份，成功后保留原 profile ID，包括用户自定义的 profile 名称。

这是应用注册和授权的重连，不是无交互 refresh；两者不要混用。

### 7.4 Callback 的精确校验

OpenClaw 在 IPv4 `127.0.0.1:8080` 监听，但 authorize/token 里的注册 redirect URI 仍保持 `http://localhost:8080/auth/callback`，不能自行把它替换成其他字符串。

| 条件 | 处理 |
| --- | --- |
| pathname 不是 `/auth/callback` | 404 |
| 业务 callback 不是 GET | 405；通用服务还支持 OPTIONS |
| state 缺失、重复或不匹配 | 400，不承认该 callback |
| 存在 OAuth error | 在 state 验证后处理，`access_denied` 单独提示用户拒绝 |
| code 空或重复 | 400 |
| 同一个事务已接受 callback | 重复请求 409 |
| 首次注册缺少 client_id | 拒绝 |
| client_id 多于一个 | 拒绝 |
| 首次 client_id 不匹配 `^oaiapp_[A-Za-z0-9_-]+$` | 拒绝 |
| 重连返回 client_id 且不等于保存值 | 拒绝 |
| 重连没有返回 client_id | 使用已保存值 |

示例回调只演示结构：

```text
http://localhost:8080/auth/callback?code=EXAMPLE_CODE&state=EXAMPLE_STATE&client_id=oaiapp_EXAMPLE
```

该 URL 中的 code 不应记录到普通访问日志。callback 使用 `Cache-Control: no-store` 和 `Referrer-Policy: no-referrer`；独立实现应保留这一类防止凭证经页面和日志泄漏的约束。[S04][S07]

## 8. 授权码交换、ID token 与 scope

### 8.1 Authorization code exchange

```http
POST /api/accounts/oauth/token HTTP/1.1
Host: auth.openai.com
Content-Type: application/x-www-form-urlencoded

grant_type=authorization_code
&client_id=oaiapp_EXAMPLE
&code=EXAMPLE_AUTHORIZATION_CODE
&code_verifier=EXAMPLE_PKCE_VERIFIER
&redirect_uri=http%3A%2F%2Flocalhost%3A8080%2Fauth%2Fcallback
&resource=https%3A%2F%2Fapi.openai.com%2Fv1
```

上面换行仅为阅读；实际 body 是标准表单编码。此请求没有 `client_secret`，没有 Platform API Key。使用 callback 得到的真实注册 ID，不能使用 `dynamic_agent_client` 交换。

OAuth 网络层的源码约束：HTTPS、`auth.openai.com` allowlist、零重定向、约 30 秒请求超时、最大 1 MiB 响应读取。错误信息只包含安全的状态分类，不反射服务端原始 body，因为原始 body 可能含凭证。[S04][S08]

### 8.2 Token response 结构

示例，时间和字符串均为虚构：

```json
{
  "access_token": "EXAMPLE_ACCESS_TOKEN",
  "refresh_token": "EXAMPLE_REFRESH_TOKEN",
  "id_token": "EXAMPLE_SIGNED_ID_TOKEN",
  "token_type": "Bearer",
  "expires_in": 3600,
  "scope": "openid email profile resource.invoke chatgpt.tokens.use.direct offline_access"
}
```

初次登录必须获得非空 access、refresh、ID token、有效正数 token lifetime、Bearer token type 和实际 scope。`token_type` 的 Bearer 比较不区分大小写。示例 `3600` 不是服务端固定有效期保证。

`expires_in` 是**秒**；OpenClaw 保存的 `expires` 是**绝对 Unix epoch 毫秒**。调用该解析器时没有设置额外 refresh skew，不要臆造固定“提前多少秒”的源码默认值。独立实现可将提前刷新时间作为内部策略，明确它不是服务器返回字段。

### 8.3 ID token 验证

必须先验证身份，再把 `sub` 用作 profile 归属依据：

1. 从固定 `https://auth.openai.com/.well-known/jwks.json` 取签名公钥。
2. 校验 JWKS 结构。
3. 使用可信 JWT 库验证签名；算法白名单为 RS256。
4. issuer 必须为 `https://auth.openai.com`。
5. audience 必须匹配本次**真实注册 client ID**。
6. 必须有 `iss`、`aud`、`sub`、`iat`、`exp`；由库执行适用时间声明校验。
7. 首次授权/重连，`nonce` 必须等于本次事务 nonce。
8. 若有 `azp`，必须等于 client ID；多 audience 时必须有匹配的 `azp`。
9. subject 不得为空；重连和刷新还要匹配之前已验证的身份。

仅 `decodeJwt()` 或 base64 解码不能验证初次登录。刷新不返回新 ID token 时，OpenClaw 可以解码**以前已经验证并受保护保存**的 ID token 来取稳定 subject；这不等于允许未验签的新 token。[S04]

### 8.4 推理权限只看实际 granted scope

```text
sharing = has("resource.invoke") AND (
    has("chatgpt.tokens.use.direct") OR
    has("chatpass.enable.request.direct")
)

sharing 为真  -> authFlow = chatgpt-token-sharing
sharing 为假  -> authFlow = chatgpt-identity
```

`chatpass.enable.request.direct` 是兼容的旧权限拼写。不能因为客户端“请求了”共享权限，就认定用户“授予了”共享权限。

初次返回没有 scope：拒绝，无法判定许可。返回了身份 scope 但没有共享组合：可以保存身份连接，必须禁止推理，也不能悄悄选用其他付费来源。

## 9. 凭证模型、保存和归属

以下是**概念 JSON**，不是建议手工写入某个固定文件路径；真实 OpenClaw 的持久化由认证 profile 宿主完成。

```json
{
  "type": "oauth",
  "provider": "openai",
  "access": "EXAMPLE_ACCESS_TOKEN",
  "refresh": "EXAMPLE_REFRESH_TOKEN",
  "expires": 1800000000000,
  "idToken": "EXAMPLE_SIGNED_ID_TOKEN",
  "email": "user@example.test",
  "clientId": "oaiapp_EXAMPLE",
  "issuer": "https://auth.openai.com",
  "accountId": "EXAMPLE_SHA256_IDENTITY_HASH",
  "tokenEndpoint": "https://auth.openai.com/api/accounts/oauth/token",
  "authorizationScope": "openid email profile resource.invoke chatgpt.tokens.use.direct offline_access",
  "grantedScope": "openid email profile resource.invoke chatgpt.tokens.use.direct offline_access",
  "authFlow": "chatgpt-token-sharing",
  "displayName": "Sign in with ChatGPT (Beta)"
}
```

| 字段 | 语义 |
| --- | --- |
| `authorizationScope` | 发起授权时实际请求的权限，重连用于保持注册契约 |
| `grantedScope` | 服务端实际授予的权限，判断是否能推理 |
| `clientId` | 用户/workspace 绑定的应用注册 ID |
| `accountId` | `SHA256(issuer + "\0" + clientId + "\0" + verifiedSub)` 的十六进制值 |
| `email` | 可选显示元数据，不作为身份主键 |
| `expires` | access token 的绝对到期时间，毫秒 |

OpenClaw 首次默认 profile 命名使用 `openai:token-sharing` 前缀和 `accountId` 前 24 个字符；重连已有 profile 时保持原 ID。

**不要把这个 `accountId` 当 ChatGPT workspace ID，也不要把它放到 `chatgpt-account-id` 推理请求头。** 它是 OpenClaw 自己算的内部身份标识。[S04]

独立实现建议将凭证保存在权限受控的数据库/系统密钥存储中，token、grant、client ID 与版本号原子提交。应用公开状态可只暴露 profile ID、授权类别、到期时间和经脱敏的账号标签。

## 10. Refresh 生命周期和并发

### 10.1 Refresh 请求

```http
POST /api/accounts/oauth/token HTTP/1.1
Host: auth.openai.com
Content-Type: application/x-www-form-urlencoded

grant_type=refresh_token
&client_id=oaiapp_EXAMPLE
&refresh_token=EXAMPLE_REFRESH_TOKEN
&resource=https%3A%2F%2Fapi.openai.com%2Fv1
```

不重新发送授权码、PKCE verifier 或 redirect URI。OpenClaw 在刷新前检查注册信息：accountId/clientId 必须存在、client ID 不能仍是入口标记、issuer/token endpoint 必须匹配预期常量。

### 10.2 响应合并规则

| 字段 | Refresh 处理 |
| --- | --- |
| access token | 必须提供新的非空值 |
| expires_in | 必须提供有效新 lifetime |
| token_type | 必须仍为 Bearer |
| refresh token | 返回新值则替换；缺少时保留旧值 |
| ID token | 返回新值则重新验签；缺少时保留先前已验证值 |
| scope | 返回值则重新分类许可；缺少时保留先前 granted scope |
| subject | 不得切换到另一个用户 |
| authFlow | 依据有效的实际 scope 重新计算，可能降为 identity-only |

新 ID token 的刷新验证不要求最初登录 nonce 再次出现；不能把初次登录的 nonce 校验照搬成“所有 refresh 永远必须带相同 nonce”。身份的 issuer/client/subject 绑定仍必须保留。

### 10.3 为什么必须集中刷新

危险的实现是两个请求同时拿同一个旧 refresh token 发 POST，再让较晚的旧结果覆盖较新的凭证。refresh token 轮换后，这可能导致连接不可恢复。

建议的独立实现逻辑：

```text
读取 profile -> 是否仍有效
  有效：返回当前 generation 的 access token
  需刷新：取得该 profile 的独占刷新权
         -> 重新读取，其他请求可能已刷新
         -> 若仍需刷新，发一次 refresh POST
         -> 验证身份和 scope
         -> 在同一事务内保存 access/refresh/expires/grant/version
         -> 释放刷新权并唤醒等待者
```

多进程部署需要数据库锁、租约或等效的持久化协调，单进程 Promise 缓存不足以保证正确。刷新失败必须保留可判断的状态，不能覆盖为半份凭证。

OpenClaw 源码明确将刷新串行化与原子轮换持久化交给 auth-profile owner。Codex bridge 还检查 profile fingerprint 和 refresh generation，禁止凭证在一个请求等待期间被另一个账号替换。[S04][S13]

### 10.4 失败处理

- `invalid_grant`：表示连接过期、撤销或刷新授权失效，需要重新登录；不要无限重复旧 refresh token。
- 刷新响应换了 subject：拒绝，重新连接。
- scope 缩减为身份权限：停止推理，要求重新获得 sharing consent。
- 请求超时/断网：区分连接未发出与服务器可能已处理；不要假定轮换请求重发一定安全。
- profile 已删除、切换或撤销：丢弃进行中的旧请求结果，不借其他 profile 悄悄恢复。

## 11. 模型发现是账号级权限的一部分

### 11.1 请求和专用响应形状

使用当前已选择 SIWC profile 的 access token：

```http
GET /v1/models HTTP/1.1
Host: api.openai.com
Authorization: Bearer EXAMPLE_SIWC_ACCESS_TOKEN
```

源码期待的顶层结构是 **`{ "models": [...] }`**，并读取 `slug`、`display_name`、`visibility`：

```json
{
  "models": [
    {
      "slug": "example-visible-model",
      "display_name": "Example visible model",
      "visibility": "list"
    },
    {
      "slug": "example-hidden-model",
      "display_name": "Example hidden model",
      "visibility": "hidden"
    }
  ]
}
```

这些模型名是示例。不要套用普通 API Key `/models` 的 `data[].id` 解析器，也不要假定 OpenAI SDK 的 `models.list()` 对当前 SIWC 专用返回结构天然适配。该 catalog 调用处没有额外设置 preview header；不能把推理专用 header 误描述为所有 OAuth 请求都带。

### 11.2 选择规则

1. 只保留 `visibility === "list"` 且 `slug` 非空的记录。
2. `slug` 是发送推理请求时的原始模型 ID。
3. 展示名使用 `display_name`，缺失则使用 slug。
4. 保持服务器返回顺序。
5. 记录为 `api: "openai-responses"`、`baseUrl: "https://api.openai.com/v1"`。
6. 静态元数据可以补全上下文等信息，但不能补造账户未返回的权限。
7. 此层缓存 TTL 为 60 秒；切换 profile 时必须使用新账号对应的发现身份。

OpenClaw 用户侧模型引用可写作 `openai/<slug>`，HTTP JSON 的 `model` 只填 `<slug>`，不加 `openai/`。

### 11.3 错误状态

| 结果 | OpenClaw catalog 行为 | 不能误推的结论 |
| --- | --- | --- |
| 200 且有可见模型 | `ready`，采用账号列表 | 不保证所有未来请求必然成功 |
| 200 且列表为空/无可见项 | `ready`，保持空列表 | 不能回填静态模型冒充权限 |
| 401 | 空列表、`auth-rejected` | 不能回退到别人的 token |
| 403 | 空列表、`auth-rejected`、`rejectionScope: catalog` | catalog 被拒不独自证明 inference token 全面失效 |
| 网络/临时/结构错误 | 保留静态提示，明确 `unavailable` | 静态提示不是账号已获权限 |
| identity-only profile | 无可推理目录、拒绝 | 登录身份成功不是模型授权 |

源码中的默认 `cost: 0`、上下文大小或 `maxTokens` 是客户端元数据，不是官方免费额度、最大上下文或输出上限承诺。尤其 SIWC 发送时会删除 `max_output_tokens`，不要将目录元数据直接变成该请求参数。[S06]

## 12. 独立 Responses 推理请求

### 12.1 基本 HTTP 契约

```http
POST /v1/responses HTTP/1.1
Host: api.openai.com
Authorization: Bearer EXAMPLE_SIWC_ACCESS_TOKEN
Content-Type: application/json
Accept: text/event-stream
x-openai-chatpass-test: codex-direct

{
  "model": "MODEL_SLUG_FROM_THIS_ACCOUNT_CATALOG",
  "input": [
    {
      "role": "user",
      "content": [
        { "type": "input_text", "text": "请只回答：连接正常。" }
      ]
    }
  ],
  "stream": true,
  "store": false
}
```

`Accept: text/event-stream` 是这个手工 HTTP 示例的显式选择。`x-openai-chatpass-test: codex-direct` 是固定基线中的强制 preview header，源码注释说明 OpenAI 退役此 header 后应移除；**不要把它宣传为永久公共 API 契约或获得权限的捷径**。它不能替代合法 SIWC token 和服务端授权。[S05][S14]

不要默认附带 Codex backend 的 `chatgpt-account-id`，也不要把 Platform 项目/组织字段混入 SIWC 身份。Codex relay 路径还会明确删除这些跨模式 header。

### 12.2 请求字段策略

OpenClaw 自有 Responses runtime 在调用方和普通 provider 的 payload transform 完成后，最后执行 SIWC 策略：

| 字段/选项 | SIWC 处理 | 说明 |
| --- | --- | --- |
| `transport` | 强制 `sse` | 忽略调用者试图选择的 WebSocket |
| `stream` | 底层 Responses 构造为 true | 走 HTTP 流式请求 |
| `store` | 强制 false | 调用者设为 true 也会被覆盖 |
| `responsesServerCompaction` | 关闭 | 不发送普通服务端压缩控制 |
| `replayResponsesItemIds` | 关闭 | 构造完整上下文，不靠服务端 item 引用恢复 |
| `context_management` | 删除 | 不能直接沿用普通 API 的 compaction 控制 |
| `metadata` | 删除 | 当前 SIWC 不接受该调用控制 |
| `max_output_tokens` | 删除 | 包括从模型默认最大输出量注入的值 |
| `temperature` | 删除 | 当前兼容策略 |
| `top_p` | 删除 | 当前兼容策略 |
| `prompt_cache_retention` | 删除 | 不等于删除全部 cache 相关字段 |
| `service_tier` | 校验，不在允许列表就报错 | 见下节 |
| `prompt_cache_options` | 本 wrapper 不删除 | 不等于每个模型无条件支持任意值 |
| `text` | 本 wrapper 不删除 | 格式和 verbosity 仍受模型契约约束 |
| `reasoning` | 本 wrapper 不删除 | 支持的 effort 等由模型/其他层判断 |
| `input`、兼容 `tools` | 保留 | 仍需符合 Responses 结构 |

这份删除列表是**固定版本的 OpenClaw 客户端行为**。未删除某字段，不足以证明服务器支持该字段所有取值；测试里的 JSON 格式和 cache 参数也不是账号授权证明。[S05][S11]

### 12.3 Service tier

此版本 SIWC wrapper 允许：

```text
default
priority
ultrafast
slow
```

可以不传；非空且不在上述集合内则本地拒绝。比如 `auto`、`flex` 被拒绝，而不能套用普通 Responses API 的 tier 列表。

允许通过客户端校验不等于用户账号已开通该档，也不能据此承诺加速、额外计费或固定额度倍率。最小验证请求建议不传 service tier。

### 12.4 Payload 变换顺序

```text
应用消息/工具定义
  -> 普通 Responses 消息转换
  -> 常规 provider 参数处理
  -> 调用者 onPayload
  -> SIWC 最终限制：校验 tier、store=false、删除不允许字段
  -> HTTP SSE 请求
```

独立实现应在最终发送前执行相同的共享授权策略，防止 SDK 默认参数或业务拦截器重新加入 `max_output_tokens`/`store=true`。普通 API Key 和没有 sharing 标记的 OAuth 不自动适用这一套字段清理。

### 12.5 curl 示例

本示例未自动执行。运行前自行设置 `SIWC_ACCESS_TOKEN` 和来自当前账号目录的 `SIWC_MODEL`，不要把真实 token 粘进文档或提交到版本库。

```bash
curl --fail-with-body --no-buffer \
  'https://api.openai.com/v1/responses' \
  -H "Authorization: Bearer ${SIWC_ACCESS_TOKEN:?SIWC access token required}" \
  -H 'Content-Type: application/json' \
  -H 'Accept: text/event-stream' \
  -H 'x-openai-chatpass-test: codex-direct' \
  --data-binary @- <<EOF
{
  "model": "${SIWC_MODEL:?Use a slug returned by this SIWC account}",
  "input": [{"role": "user", "content": [{"type": "input_text", "text": "Reply with OK."}]}],
  "stream": true,
  "store": false
}
EOF
```

这是手工诊断片段；模型变量应来自可信目录且是正常 slug。产品代码应使用 JSON 序列化构造 body，而不是拼接 shell JSON。不要开启会打印 header 的 shell tracing 或 `curl -v` 后公开日志。

## 13. SSE、多轮对话和工具循环

### 13.1 SSE 解析要求

流式 HTTP 响应不是“一次 read 得到一个 JSON”。实现建议：

1. 增量 UTF-8 解码，处理跨 TCP chunk 的多字节字符。
2. 按 SSE 事件边界读取，兼容 LF/CRLF、多行 `data:`、空行和注释。
3. 再解析 event 的 JSON data；事件类型以实际 Responses 数据为准。
4. 区分文本增量、输出 item、function call 参数增量、完成和失败。
5. 处理 abort、网络中断、HTTP 错误，以及 HTTP 200 流中的 error/failure。
6. 收到终止状态后再认定一个响应完成，不能把连接关闭无条件当作成功。
7. 尽量使用兼容的成熟 Responses SDK/事件解析器；未知事件按明确策略处理。

典型 Responses 文本/工具事件名包括 `response.output_text.delta`、`response.output_item.added`、`response.function_call_arguments.delta`、`response.completed`；这里只用于说明解析职责，不是本文已实测的完整服务端事件白名单。

### 13.2 会话状态由客户端维护

当前 SIWC 路径强制 `store=false`、关闭 item ID replay，并要求完整上下文重放。独立实现应保存需要的历史输入、模型输出、工具调用及结果，再构造下一轮 `input`。

不能只传 `previous_response_id` 或 `item_reference` 就假设服务端会恢复之前不存储的内容。本文没有证明任意这种 server state 引用在 SIWC 中可用，因此不要把它作为基本实现依赖。

`store=false` 也不等于关于 OpenAI 一切安全日志、审计和数据保留的全面承诺；这里只说明 Responses 请求参数。

### 13.3 Function tool 回传示例

最小工具声明片段：

```json
{
  "tools": [
    {
      "type": "function",
      "name": "lookup_order",
      "description": "Look up an order the current user is allowed to access.",
      "parameters": {
        "type": "object",
        "properties": {"order_id": {"type": "string"}},
        "required": ["order_id"],
        "additionalProperties": false
      },
      "strict": true
    }
  ]
}
```

模型返回 `function_call` 后，客户端校验 arguments，执行自己有权执行的工具，并使用同一个 `call_id` 回传：

```json
{
  "type": "function_call_output",
  "call_id": "EXAMPLE_CALL_ID_FROM_MODEL",
  "output": "{\"status\":\"shipped\"}"
}
```

下一轮包含所需完整上下文、对应 function_call item 和 function_call_output；不要只留下孤立结果。模型可用 SIWC 生成工具调用，并不意味着模型 token 可以授权读取你的数据库或第三方应用；本地工具权限和外部服务凭证仍由应用自己控制。

### 13.4 上下文压缩

关闭 `context_management` 不等于禁止一切摘要：

- 自有 runtime 可以管理上下文，并通过兼容的模型调用生成摘要。
- Codex runtime 的 SIWC 路径支持自动轮内摘要，采用其本地摘要机制。
- 固定基线文档明确手工 `/compact` 在该 Codex SIWC 路径不可用。
- 本机 OAuth relay 只接收 `/responses`，不能把 `/responses/compact` 当成可用后门。

## 14. 调用路径 A：OpenClaw 自有 runtime

主要职责链：

```text
选中 openai 的 SIWC profile
  -> resolveModelAuthPolicy 检查 sharing 与目的资源
  -> profile owner 解析/刷新当前 access token
  -> buildTokenSharingCatalog 用此身份发现模型
  -> wrapOpenAIResponsesStream 应用 SIWC 参数和 transport 策略
  -> packages/ai 的 openai-responses 构造 Responses payload
  -> OpenAI SDK 通过受控 fetch 发向 api.openai.com/v1/responses
  -> SSE 事件转换为 OpenClaw 消息/工具调用
```

模型授权策略只允许公共 Responses 路径：已指定的 API 类型必须是 `openai-responses`；已指定的 base URL 必须为公共资源 URL（允许源码处理的单个结尾 `/`）。未指定值可以由正常默认解析补全。

若 auth flow 是 `chatgpt-identity`，直接不兼容。若是 SIWC 但 capability 指向独立媒体/embedding 等操作，拒绝该凭证，而不是先刷新再尝试错误 API。[S16]

底层 `createOpenAIProviderClient()` 将当前 Bearer token 交给 SDK 的 `apiKey` 参数，base URL 来自 model，SDK `maxRetries` 设置为 0，并使用 OpenClaw 提供的受控 fetch。这只说明 SDK 自身不会在这一构造中自动重试，不代表整个 runtime 没有其他恢复策略。[S05][S10]

## 15. 调用路径 B：Codex app-server 加本机 relay

### 15.1 为什么需要这层

SIWC 凭证属于 OpenClaw 的应用注册和 auth profile，不是原生 Codex 产品登录。OpenClaw 要复用 Codex 的 agent loop，又要继续拥有 SIWC 的刷新、账号隔离和目的端点控制，所以注入一个私有 provider 和本机 inference relay。

```text
OpenClaw 持有 SIWC profile/refresh token
        |
        | 管理并绑定
        v
本机隔离的 Codex app-server
        |
        | HTTP POST <private-loopback-base>/responses
        v
OpenClaw 本机 relay
        | 验证本轮 owner、profile fingerprint、refresh generation
        | 解析当前 access token，必要时刷新
        | 清理旧身份 headers，注入 SIWC Bearer
        v
https://api.openai.com/v1/responses
```

因此“没有任何代理”和“通过第三方反代 Codex backend”都不能准确描述这条路径：它有本机协议适配 relay，但外部目的资源仍是 OpenAI 公共 Responses。

### 15.2 私有 provider 配置

固定基线使用的内部 provider ID：

```text
openclaw_token_sharing
```

它只属于受管理的子进程配置。OpenClaw 对用户公开的 provider 仍为 `openai`，不要新增一个用户模型前缀来代替它。

概念配置：

```json
{
  "model_provider": "openclaw_token_sharing",
  "model_providers": {
    "openclaw_token_sharing": {
      "name": "OpenClaw subscription sharing",
      "base_url": "http://127.0.0.1:LOCAL_PORT/PRIVATE_RANDOM_PATH/v1",
      "wire_api": "responses",
      "requires_openai_auth": true,
      "supports_websockets": false
    }
  }
}
```

此 JSON 解释源码结构，不能把其中 placeholder 当成可直接使用的用户配置。`requires_openai_auth` 也不能推导为“把 SIWC refresh token 写入用户的原生 Codex auth”。真正的上游 SIWC token 由 OpenClaw relay 注入。[S13][S15]

### 15.3 Relay 的上游与入站限制

源码中的关键限制：

- 上游必须是没有用户名、密码、fragment 的 HTTPS URL。
- 存在 SIWC OAuth bridge 时，上游必须匹配 `https://api.openai.com/v1`。
- 本地 route 有不可猜测的随机路径前缀，且与受管理进程 owner 绑定。
- 拒绝带 Origin 的入站请求，避免浏览器滥用本机接口。
- 拒绝路径穿越、编码斜杠等可改变目标解释的路径。
- SIWC 请求只允许 `/responses`，不允许附带 query string。
- HTTP 业务请求只允许 POST。
- 上游出站使用 SSRF 防护、HTTPS 和禁用重定向的受控 fetch。
- 每次实际出网前再次检查请求和凭证 owner，防止等待过程中授权归属改变。

这不是一个开放式代理：客户端不能任意提交要转发的 URL，也不能借 SIWC grant 调用其他公共 API。[S14]

### 15.4 Header 替换

转发 SIWC 上游请求前明确删除：

```text
authorization
chatgpt-account-id
openai-organization
openai-project
```

然后设置：

```http
Authorization: Bearer <OpenClaw 当前解析的 SIWC access token>
x-openai-chatpass-test: codex-direct
```

删除的是入站的旧身份信息，不是禁止最终有 Authorization。不要将原生 Codex 的 token、账号头或另一个 Platform 项目混到 SIWC profile。

### 15.5 401 重试与身份保护

该 relay 的 HTTP 循环在 OAuth 模式最多两次上游尝试：首次正常解析凭证；若上游为 401 且上传可重放，则释放第一次响应，强制刷新后重放一次。第二次仍失败就返回失败，不无限刷新。

`responses-oauth.ts` 的身份 fingerprint 基于 `issuer/clientId/verified sub/authFlow`。解析 profile 时 `allowProfileFallback: false`；请求期间身份变化、授权类别变化或 profile 不再属于本轮，就拒绝。

因此不能把整个逻辑简化为“401 时从环境变量换一个 OPENAI_API_KEY”。这样的静默切换会改变使用者和费用来源。[S13][S14]

### 15.6 不把两条路径的实现混为一谈

第 12 节的 `responses-stream.runtime.ts` 是自有 runtime 的直接 wrapper。Codex app-server 路径由原生 Codex 构造请求、OpenClaw 准备上下文及授权 relay；不能因为两者最终都请求 `/responses`，就声称每个请求必定经过同一个 wrapper 或字节完全一致。

独立直接客户端应实现第 12 节已核实的 SIWC payload 策略；若要完整复制 Codex runtime，还必须核对其请求构造、私有 provider 配置和依赖版本，不宜只拷贝 HTTP relay 文件。

### 15.7 运行条件

固定基线文档要求 managed local process 和 isolated agent home；remote execution、supervised sessions、手工 `/compact` 不支持这一凭证。代理和 trust 配置还必须符合受管理 relay 的环境要求，不能因为某连接地址是 localhost 就绕过 managed owner 检查。[S01][S15]

## 16. SIWC 能力矩阵

以下为固定基线声明的范围，实际还受模型和账号控制：

| 能力 | SIWC | 说明 |
| --- | --- | --- |
| Responses 文本推理 | 支持 | 实际 sharing scope 与额度有效 |
| HTTP SSE 流式返回 | 支持 | 当前模型请求路径 |
| WebSocket 模型推理 | 不支持 | 强制 SSE/禁用原生 WS |
| 本地 function tools | 支持 | 工具执行和权限由应用管理 |
| Web search | 支持条件下可用 | 依赖模型、账号与工具策略 |
| 图片输入 | 模型相关 | 不等于图片生成 |
| 文件内容输入 | 模型相关 | 不等于获得 Files upload API |
| 音频/视频输入与转录 API | 该授权不提供 | 需要单独兼容凭证/路径 |
| 图片生成、hosted image generation | 不支持 | 不应尝试借用 SIWC token 绕过 |
| Text-to-speech | 不支持 | 另配语音凭证 |
| Memory embeddings | 不支持 | 另配 embedding provider/凭证 |
| Realtime voice | 不支持 | 不等于所有 OAuth 都可用相同语音路线 |
| OpenAI-hosted plugins / connected apps | 不支持 | SIWC 不授予 connector invocation |
| Hosted MCP / tool search | 不支持 | 与本地工具集成区分 |
| 导入 ChatGPT 对话/Codex 历史 | 不提供 | 身份及模型额度授权不包含历史读取 |
| 自动上下文摘要 | 支持相应 runtime 实现 | 不代表公共 `/responses/compact` 可调用 |
| Codex 手工 `/compact` | 不支持 | 该 SIWC runtime 限制 |
| OpenClaw 内 SIWC 配额/每应用用量报表 | 不提供 | 查看 ChatGPT Usage；控制项依账号 |

查看共享额度的入口见 [ChatGPT Usage](https://chatgpt.com/settings/usage)。不能根据某个模型的客户端 cost 元数据或接口域名推断“无限使用”“免费”或具体每周额度。[S01][S02]

## 17. 给 AI 的实现规格与参考片段

### 17.1 可直接作为开发任务输入的规格

```text
目标：实现一个 SIWC OAuth + 公共 Responses HTTP SSE 客户端。

认证类型必须区分：
  - Platform API Key
  - Codex 产品 OAuth
  - SIWC identity-only
  - SIWC token-sharing

只实现 SIWC 时：
  1. 固定 issuer/resource/authorize/token/JWKS/redirect 常量。
  2. 首次使用 dynamic_agent_client 发起注册；回调必须返回真实 oaiapp_*。
  3. 已注册 profile 重连使用真实 client ID，禁止身份或 registration 偷换。
  4. 生成独立 PKCE/state/nonce；先启动 listener，再打开浏览器。
  5. callback 校验唯一 state/code、client ID、取消和一次性消费。
  6. 用真实 client ID + verifier + resource 交换授权码。
  7. 用 JWKS 验证 ID token；不把 decode 当验签。
  8. 保存 requested authorizationScope 和 returned grantedScope。
  9. 只有 resource.invoke + direct-sharing scope 才能推理。
 10. 原子保存可刷新凭证，expires 使用毫秒，集中串行 refresh。
 11. 使用同一 profile token GET /v1/models，解析 models[].slug/visibility。
 12. POST /v1/responses，Bearer access token，preview header，SSE，store=false。
 13. 最终删除 context_management/metadata/max_output_tokens/temperature/
     top_p/prompt_cache_retention，校验 service_tier。
 14. 客户端维护完整上下文和工具循环，不依赖存储的 response/item 引用。
 15. 区分 token 无效、scope 不足、catalog 拒绝、额度不足和网络失败。
 16. 未明确选择其他凭证时，不自动切换账号或 Platform API Key。

不要引入：
  - 第三方反代服务或 chatgpt.com/backend-api/codex 作为 SIWC 上游
  - ChatGPT 网页 cookie 抓取
  - 原版 Codex token 冒充 SIWC token
  - 以 identity-only 登录成功作为推理成功
  - 从静态模型表推断账号可用性
  - 将 refresh token 发到 inference endpoint
```

### 17.2 TypeScript：协议纯函数

下面是便于审查和移植的片段，不包含完整 HTTP server、JWT verifier 或数据库实现。省略的部分必须按前文规格补齐，不能把它当成可直接上线的完整认证程序。

```typescript
import { createHash, randomBytes } from "node:crypto";

const SIWC = {
  issuer: "https://auth.openai.com",
  authorize: "https://auth.openai.com/api/accounts/authorize",
  token: "https://auth.openai.com/api/accounts/oauth/token",
  jwks: "https://auth.openai.com/.well-known/jwks.json",
  resource: "https://api.openai.com/v1",
  redirect: "http://localhost:8080/auth/callback",
  entryClientId: "dynamic_agent_client",
  scope: "openid email profile resource.invoke chatgpt.tokens.use.direct offline_access",
} as const;

function createAuthorizationTransaction(
  registered?: { clientId: string; authorizationScope: string },
) {
  const verifier = randomBytes(32).toString("base64url");
  const challenge = createHash("sha256").update(verifier).digest("base64url");
  const state = randomBytes(32).toString("base64url");
  const nonce = randomBytes(32).toString("base64url");
  const clientId = registered?.clientId ?? SIWC.entryClientId;
  const scope = (registered?.authorizationScope ?? SIWC.scope)
    .split(/\s+/u)
    .map(s => s === "chatpass.enable.request.direct" ? "chatgpt.tokens.use.direct" : s)
    .join(" ");
  const url = new URL(SIWC.authorize);
  url.search = new URLSearchParams({
    response_type: "code",
    client_id: clientId,
    redirect_uri: SIWC.redirect,
    resource: SIWC.resource,
    scope,
    code_challenge: challenge,
    code_challenge_method: "S256",
    state,
    nonce,
    ...(!registered ? { agent_name_hint: "OpenClaw" } : {}),
  }).toString();
  // 复现 OpenClaw 的 hint；自己的应用应明确自己的注册身份和开放资格。
  return { url, verifier, state, nonce, clientId, authorizationScope: scope };
}

function resolveCallbackClientId(params: URLSearchParams, requestedId: string) {
  const ids = params.getAll("client_id");
  const id = ids[0];
  const registering = requestedId === SIWC.entryClientId;
  if (
    ids.length > 1 ||
    (registering
      ? !id || !/^oaiapp_[A-Za-z0-9_-]+$/u.test(id)
      : id !== undefined && id !== requestedId)
  ) throw new Error("Invalid SIWC registration callback");
  return id ?? requestedId;
}

function classifyGrant(scope: string) {
  const scopes = new Set(scope.split(/\s+/u));
  return scopes.has("resource.invoke") &&
    (scopes.has("chatgpt.tokens.use.direct") ||
     scopes.has("chatpass.enable.request.direct"))
    ? "chatgpt-token-sharing"
    : "chatgpt-identity";
}

function applySiwcPayloadPolicy(payload: Record<string, unknown>) {
  const body = { ...payload };
  const tiers = new Set<unknown>(["default", "priority", "ultrafast", "slow"]);
  if (body.service_tier != null && !tiers.has(body.service_tier)) {
    throw new Error("Unsupported SIWC service tier");
  }
  body.store = false;
  for (const key of [
    "context_management", "metadata", "max_output_tokens",
    "temperature", "top_p", "prompt_cache_retention",
  ]) delete body[key];
  return body;
}
```

调用者必须在 `resolveCallbackClientId()` 之前校验 callback 路径、唯一 state、唯一非空 code 和 OAuth error；这个函数只负责 client ID 部分。`classifyGrant()` 只能处理通过可信 token response 取得的实际 scope。

### 17.3 TypeScript：凭证和存储接口建议

```typescript
type SiwcFlow = "chatgpt-token-sharing" | "chatgpt-identity";

type SiwcCredential = {
  type: "oauth";
  provider: "openai";
  clientId: string;
  issuer: "https://auth.openai.com";
  tokenEndpoint: "https://auth.openai.com/api/accounts/oauth/token";
  accountId: string;
  access: string;
  refresh: string;
  idToken: string;
  expires: number; // epoch milliseconds
  authorizationScope: string;
  grantedScope: string;
  authFlow: SiwcFlow;
  email?: string;
};

// 以下接口是独立实现建议，不是 OpenClaw 原始接口签名。
interface CredentialStore {
  read(profileId: string): Promise<{ version: number; credential: SiwcCredential }>;
  withRefreshLock<T>(profileId: string, fn: () => Promise<T>): Promise<T>;
  compareAndSwap(
    profileId: string,
    expectedVersion: number,
    next: SiwcCredential,
  ): Promise<boolean>;
}
```

数据库的 compare-and-swap 失败时不能强行覆盖，应重新读取并核对当前归属。锁的作用域要覆盖实际进程部署边界；不要把 token 值用作日志中的锁名称。

### 17.4 推理前的伪代码

```text
infer(profileId, messages, options):
    owner = bindAuthorizedProfile(profileId)
    credential = getOrRefreshUnderOwner(owner)
    assert credential.authFlow == chatgpt-token-sharing
    assert owner still current

    catalog = discoverForSameProfile(credential)
    choose model from verified visible catalog
    # catalog unavailable 与成功空列表/拒绝必须区别处理

    payload = buildResponsesInputWithCompleteContext(messages, options)
    payload = applyNormalCallerTransforms(payload)
    payload = applySiwcPayloadPolicy(payload)
    payload.stream = true

    response = POST fixed public responses endpoint:
        Authorization = Bearer credential.access
        x-openai-chatpass-test = codex-direct
        Content-Type = application/json
        redirect policy = reject

    if response is HTTP error:
        classify safely; do not log raw secrets
    else:
        parse SSE incrementally
        aggregate output and tool calls
        execute authorized local tools if requested
        append tool outputs with matching call_id
        repeat as needed
```

这是推荐控制顺序。不能把其中 catalog 步骤解读为“每次推理必须额外访问服务器一次”；可以使用按账号隔离且状态明确的短期缓存。

## 18. 错误定位与恢复矩阵

下表把源码明确行为和独立实现建议合并展示，未证明固定服务端错误码的部分只按错误类别描述。

| 阶段/现象 | 优先检查 | 恢复方式 |
| --- | --- | --- |
| 旧版 CLI 不认识 `siwc` | 安装版本是否包含本文基线实现 | 核对版本，不能改成普通 oauth 后仍称 SIWC |
| 授权页提示注册不可用 | 账号/workspace 的 SIWC 开放资格 | 按服务端资格和管理员策略处理 |
| 本机 8080 绑定失败 | 端口占用、进程未退出 | 释放冲突后新建登录事务 |
| 远程浏览器 callback 失败 | localhost 指向、SSH 转发、IPv4 bind | 在浏览器机器建隧道后重新登录 |
| Invalid state / 重复 code | 过期页面、事务混用、重复 callback | 不绕过校验，重启新事务 |
| 首次回调缺 client_id | 注册分支、服务端资格、请求形态 | 不能继续用 dynamic_agent_client 交换 |
| ID token audience/nonce 不匹配 | 用错 client ID、事务、签名验证参数 | 拒绝身份，重新登录 |
| 登录成功但提示 token sharing disabled | 实际 grantedScope | 重新授予共享权限，或明确选择其他凭证 |
| token endpoint `invalid_grant` | 被撤销、旧 refresh、轮换竞争 | 停止旧值重试，重新登录并修复刷新协调 |
| catalog 401 | 当前 profile token 与刷新状态 | 有界恢复凭证，不回填静态权限 |
| catalog 403 | 目录许可单独被拒 | 保留 catalog-only 拒绝语义，不能直接判所有推理失效 |
| catalog 200 空数组 | 账号没有可展示模型 | 保持空，不伪造授权模型 |
| 推理 400/参数错误 | `max_output_tokens` 等字段、tier、模型 ID | 检查最终序列化 body 是否符合 SIWC 策略 |
| 推理 401 | Bearer 来源、过期、撤销 | 有界刷新；relay 可重放条件下最多刷新重试一次 |
| 推理 403/权限错误 | sharing grant、模型/账号/workspace 权限 | 不把刷新当所有授权错误的万能恢复 |
| 429/额度类拒绝 | 服务端错误类型与重试时间 | 区分速率限制和额度耗尽；刷新不会补额度 |
| 网络/5xx | 连接状态和响应是否已经部分输出 | 有界退避；不要重执行有副作用的本地工具 |
| 流中途中断 | 是否收到终止事件、是否已执行工具 | 标为未完成，按会话恢复策略处理 |
| 调用图片/embedding 被拒 | capability 与 credential 类型 | 另配兼容凭证，而非更换 SIWC URL |
| relay 报公共端点要求 | 自定义 base URL 或错误 upstream | 恢复固定公共 Responses 资源 |
| relay identity changed | profile 在等待中被换账号或撤销 | 停止该 turn，用明确选中的新身份重建 |
| Codex remote/supervised 失败 | 是否使用 managed local process | 改用支持的运行形态或其他凭证 |

建议记录的诊断元数据：阶段名、profile 的非敏感标识、固定端点类别、HTTP 状态、脱敏错误分类、是否刷新、请求关联 ID（若服务端提供）。不要记录 Authorization、完整 OAuth callback、refresh token、原始 token response、私有 relay URL 或模型正文作为默认日志。

## 19. 验收清单

### 19.1 登录和权限

- [ ] 首次注册收到唯一合法 `oaiapp_*`，exchange 使用真实 ID。
- [ ] 重连不能更换 client ID、用户或 workspace 归属。
- [ ] PKCE、state、nonce 每次独立生成并验证。
- [ ] callback path/method/重复参数/一次性消费都验证。
- [ ] 取消或超时后 listener 关闭，迟到结果不能写入当前 profile。
- [ ] ID token 验签、issuer/audience/azp/nonce 约束完整。
- [ ] identity-only 可以表示登录状态，但不能推理或触发静默付款来源切换。
- [ ] 首次缺 scope 拒绝；实际 grant 与请求 scope 分开保存。

### 19.2 Refresh

- [ ] refresh 使用真实 client ID 和正确 resource。
- [ ] expires_in 秒和 expires 毫秒无混淆。
- [ ] 新 refresh token 原子保存，没有新值才保留旧值。
- [ ] refresh 可省略 ID token/scope，但不能省略有效 access/lifetime/Bearer。
- [ ] 新 ID token 验证且 subject 不变。
- [ ] 多请求、多进程刷新不会互相覆盖。
- [ ] `invalid_grant` 不被无限重试。
- [ ] profile 删除、重连或改身份时，旧请求不能继续使用旧归属。

### 19.3 Catalog 和推理

- [ ] 解析 `{ models: [...] }` 而不是普通 key 的 `data` 结构。
- [ ] 只展示 `visibility=list`，HTTP model 使用原始 slug。
- [ ] 成功空目录不回填静态模型；403 与临时失败分开表达。
- [ ] catalog 与 inference 使用同一个已选择 profile。
- [ ] 只发到 `https://api.openai.com/v1/responses`。
- [ ] 最终 payload 强制 store=false 并移除六个不兼容字段。
- [ ] transport SSE，完整上下文重放，service tier 按当前集合校验。
- [ ] preview header 与版本绑定，不作为权限来源。
- [ ] SSE 能处理分片、失败、取消和终止事件。
- [ ] 本地工具结果匹配 call_id，工具权限独立校验。

### 19.4 如果实现 Codex bridge

- [ ] 受管理的本机进程、隔离 home、私有 provider。
- [ ] 私有 relay 目的地址固定，不能开放为通用转发器。
- [ ] 删除旧身份 headers，再注入 SIWC Bearer。
- [ ] 保留 profile owner/fingerprint/generation 检查。
- [ ] 401 只有满足重放条件才有界刷新重试。
- [ ] 不把 remote execution、supervised sessions、手工 `/compact` 误报为已支持。

本次文档已做源码核对和文档片段检查；上述包含真实账号、服务端行为、并发故障注入的验收项仍应由实现者在授权测试环境执行，不能因本文列出清单就认为已经通过。

## 20. 源码索引与后续升级核对

OpenClaw 固定提交：[`23dae69850132c4d43e1fcc8885396a7f545cebb`](https://github.com/openclaw/openclaw/tree/23dae69850132c4d43e1fcc8885396a7f545cebb)。

| 编号 | 文件 | 本文所依赖内容 |
| --- | --- | --- |
| S01 | [setup.md][S01] | CLI、回调、能力限制、账户条件、额度说明 |
| S02 | [authentication.md][S02] | 三种登录的对照、端点、个人与 agent 账号 |
| S03 | [token-sharing.ts][S03] | 常量、scope、动态注册标记 |
| S04 | [token-sharing-oauth.runtime.ts][S04] | 登录、client 校验、ID token、scope、refresh |
| S05 | [responses-stream.runtime.ts][S05] | header、SSE、payload 清理、tier |
| S06 | [token-sharing-catalog.ts][S06] | 目录 schema、账号可见性与错误状态 |
| S07 | [oauth-loopback-callback.ts][S07] | callback 状态机、state/code 校验和清理 |
| S08 | [openai-oauth-http.runtime.ts][S08] | form 编码与受控 fetch 生命周期 |
| S09 | [openai-provider.ts][S09] | siwc/oauth/device-code 分流、catalog 和 refresh 路由 |
| S10 | [openai-responses.ts][S10]、[openai-provider-client.ts][S10B] | SDK 请求构造、base URL、retry 配置 |
| S11 | [token-sharing-stream.test.ts][S11] | 强制策略、正常 credential 不受影响的测试证据 |
| S12 | [token-sharing-oauth.runtime.test.ts][S12] | 注册、重连、identity-only、nonce、刷新测试 |
| S13 | [responses-oauth.ts][S13] | host-owned OAuth、fingerprint、refresh generation |
| S14 | [inference-proxy.ts][S14] | 固定上游、header 替换、401 重试与 relay 边界 |
| S15 | [inference-routing.ts][S15] | managed ownership、私有 provider、禁用 WS |
| S16 | [model-auth-policy.ts][S16] | 身份-only、resource 与 capability 兼容策略 |
| S17 | [token-sharing-catalog.test.ts][S17] | 目录可见性、空结果、临时失败和拒绝测试 |
| S18 | [provider-oauth-runtime.ts][S18] | state 随机值、expires 时间单位和取消工具 |

Codex CLI 固定提交：[`3421c660d043e39d1bff8cee136c0ceeb006177c`](https://github.com/openai/codex/tree/3421c660d043e39d1bff8cee136c0ceeb006177c)。

| 编号 | 文件及位置 | 本文所依赖内容 |
| --- | --- | --- |
| C01 | [model-provider-info/src/lib.rs:428][C01] | ChatGPT 模式默认 Codex backend 与 base URL 覆盖 |
| C02 | [login/src/server.rs:586][C02] | 原版浏览器 authorize 路径、scope、resource=None；同文件 token exchange 使用 `/oauth/token` |

升级时优先重新核对：authorize/token 路径、scope 拼写、动态注册 contract、callback URI、preview header 是否退役、模型目录结构、允许的 tier/字段、hosted tools 开放情况，以及 Codex 私有 provider/本地 relay 的依赖行为。不要只更新日期而保留旧的协议常量。

[S01]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/docs/providers/openai/setup.md#sign-in-with-chatgpt-beta
[S02]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/docs/providers/openai/authentication.md
[S03]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/extensions/openai/token-sharing.ts
[S04]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/extensions/openai/token-sharing-oauth.runtime.ts
[S05]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/extensions/openai/responses-stream.runtime.ts
[S06]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/extensions/openai/token-sharing-catalog.ts
[S07]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/src/infra/oauth-loopback-callback.ts
[S08]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/extensions/openai/openai-oauth-http.runtime.ts
[S09]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/extensions/openai/openai-provider.ts
[S10]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/packages/ai/src/providers/openai-responses.ts
[S10B]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/packages/ai/src/providers/openai-provider-client.ts
[S11]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/extensions/openai/token-sharing-stream.test.ts
[S12]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/extensions/openai/token-sharing-oauth.runtime.test.ts
[S13]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/extensions/codex/src/app-server/responses-oauth.ts
[S14]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/extensions/codex/src/app-server/inference-proxy.ts
[S15]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/extensions/codex/src/app-server/inference-routing.ts
[S16]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/extensions/openai/model-auth-policy.ts
[S17]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/extensions/openai/token-sharing-catalog.test.ts
[S18]: https://github.com/openclaw/openclaw/blob/23dae69850132c4d43e1fcc8885396a7f545cebb/src/plugin-sdk/provider-oauth-runtime.ts
[C01]: https://github.com/openai/codex/blob/3421c660d043e39d1bff8cee136c0ceeb006177c/codex-rs/model-provider-info/src/lib.rs#L428
[C02]: https://github.com/openai/codex/blob/3421c660d043e39d1bff8cee136c0ceeb006177c/codex-rs/login/src/server.rs#L586
