# Prism / Codex 兼容模式

这是普通 ChatGPT OAuth 账号的独立转发配置，参考 `jushjay/Prism` 的 Codex 请求与传输行为实现。它不是之前撤回的 Prism Web Agent，也不是新的 BPS 服务端：上游仍然是 `https://chatgpt.com/backend-api/codex/responses`。

## 配置

在管理后台添加或编辑普通 OpenAI OAuth 账号时，开启 **Prism / Codex 兼容模式**。批量编辑先勾选“批量修改”，再设置模式开关。账号列表显示 `Prism / Codex` 标识。

- 自动复用已有 `access_token` 和 ChatGPT account ID，继续使用原有 OAuth 刷新链路。
- 不需要 Cookie、`prism_template`、`projectId`、Playwright 或 sidecar。
- 新配置键为 `extra.openai_prism_codex`（布尔值）；旧 `openai_prism_*` Web Agent 设置不会自动启用此模式。
- 与 Excel / Google Sheets BPS 互斥。API 显式同时开启两者会被拒绝；单独开启一个会关闭另一个。
- 开启期间不使用 BPS 自动恢复、质量规则自动启用 BPS、原生 Codex 票据、OAuth 插件转发或原生 WebSocket 连接池。请求仍经过账号准入、代理、RPM、响应解析和计费。
- 模式作用于所选账号，不是全局账号池开关。需要整个分组都使用 Prism 时，应将组内适用账号全部设为此模式。

## 支持范围

| 入口/能力 | 行为 |
| --- | --- |
| `POST /v1/responses` | HTTP JSON 或 SSE 响应 |
| `POST /v1/chat/completions` | 转换为 Responses；支持流式与非流式 |
| `previous_response_id` | 内部通过 WebSocket 发送 `response.create`，不丢弃历史 ID |
| 函数工具及结果 | 保留配对 call ID；异常长度 ID 做确定性归一化 |
| 图片输入 | 保留 URL / data URL，随请求直接交给 Codex；不走 BPS 图片中转 |
| 加密 reasoning | 保留历史；请求 reasoning 时包含 encrypted_content |
| 账号连接测试、模型同步 | 使用同一 Prism 身份配置和当前 OAuth 凭据 |
| 独立 `/images/generations`、`/images/edits` | 网关转换为 Responses `image_generation` 工具，仍走 Prism；不是 Prism 上游的独立 Images 路由 |
| `/responses/compact` | 网关兼容转换到 Prism Responses 流；Prism 上游目前没有独立 compact 路由 |
| `/responses/input_tokens` | 网关本地估算并返回 OpenAI 兼容响应；不请求 Prism 上游不存在的端点 |
| 客户端 WebSocket 入口 | 通过现有 HTTP bridge 接入；首轮使用 Prism HTTP/SSE，带 `previous_response_id` 的续接使用 Prism 内部 Codex WebSocket |

上游请求强制 SSE，非流式客户端由网关聚合为 JSON。普通 HTTP 请求无历史响应 ID 使用 HTTP；带历史 ID 使用内部 WebSocket，将事件转换成 SSE 交给现有响应处理。客户端 WebSocket 入口使用同一套 Prism profile，不会进入 BPS、native 或插件通道。传输身份为 `Codex/1.0 (OpenAI; Linux x86_64)`、`Originator: codex_cli_rs`、`responses_websockets=2026-02-06`，并剥离客户端 Cookie 等无关身份字段。`service_tier` 不发送，以实际上游响应计费信息为准。

选中 Prism 的请求失败时不会自动改用 BPS 或另一个协议账号重放。代理层只允许在证明尚未发送请求时，按已有代理设置尝试备用出口；WebSocket 发送过 `response.create` 后禁止出口重放。响应头 `X-Sub2API-Upstream-Protocol: prism_codex` 可辅助排查。

模型、托管工具（包括 `image_generation`）能否执行仍取决于上游账户和模型权限；保留请求字段不代表上游保证支持。此模式不是权限绕过，不能承诺任何账号都可用。

## 验证与来源

实现基于本项目现有 Go 网关独立编写，没有复制参考仓库的实现代码。参考项目 `https://github.com/jushjay/Prism` 使用 PolyForm Noncommercial 许可证；参考代码版本为 `3424c2565241057726f1990032e15542bb4f9d6a`。

专用 GitHub Actions 工作流 `Prism Codex compatibility` 运行协议/路由测试、WebSocket 生命周期 race 检查、前端设置回归及前后端编译。本机不执行编译或测试。模拟上游测试不等于真实 ChatGPT 账户端到端验证；发布前应使用自有已授权账号分别验证首轮 SSE、续接工具调用和图片输入。
