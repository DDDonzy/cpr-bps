# CPR BPS

独立的 Basis Points 受管上游插件，当前版本 **0.5.1**。使用官方 CPR 插件接口，不需要修改 CPR 源码、二进制或原生前端

## 功能

- Responses JSON/SSE，客户端 WebSocket 与 BPS HTTP/SSE 适配
- Function、custom、namespace、tool_search 转换及工具结果回传
- 原始代码参数中继、JSON 修复和参数 Schema 校验
- 图片附件、思考等级映射（如 max → xhigh）、JSON Schema 结果校验
- 请求内压缩、作用域隔离的历史续接、本地预热、取消和受控原生回落

CPR 负责鉴权、选账号、OAuth、账号代理、租约、配额、重试和账本。Rust 插件只依赖公开 SDK；Go 转换器不接收 OAuth 凭据，当前运行路径中的外部请求全部通过宿主受管 HTTP 发出

## 获取源码

```bash
git clone --recurse-submodules https://github.com/DDDonzy/cpr-bps.git
cd cpr-bps
```

已有克隆：

```bash
git pull --ff-only
git submodule update --init --recursive
```

官方 CPR 源码作为未修改的子模块固定到 **v3.18.2**，commit `e30aad475560b94db2d999e2251d180e45d52671`，仅用于 SDK 和打包 CLI。不要使用 `git submodule update --remote` 跟随其他版本

GitHub 自动生成的源码 ZIP 不包含子模块内容；建议使用上面的克隆命令。第三方代码保持在 vendor 中，插件本身位于 plugin 和 converter

## 构建与检查

Linux 或 WSL，Go 1.26.0、Rust 1.97.0/rustup，以及常规本机链接工具。首次构建需联网下载 Go/Cargo 依赖和 Rust target

```bash
rustup toolchain install 1.97.0
bash Scripts/test.sh
bash Scripts/build.sh amd64
bash Scripts/build.sh arm64
```

产物写入 packages，包含安装用 .tar.gz 和 .sha256。BUILD_DIR 可指定独立构建缓存；CPR_PLUGIN_CLI 可指定匹配版本的官方 cpr-plugin 工具，否则脚本从固定子模块构建打包 CLI

只构建插件、SDK 和打包 CLI，不构建或替换 CPR 服务。ELF 使用静态 musl 链接；文件名中的 gnu 是官方打包 CLI 接受的 Linux/architecture 标签。安装成品不需要编译工具链

## 安装与配置

推荐使用官方 **CPR 3.18.2**。插件声明的宿主兼容范围为 >=3.18.2、<3.19.0，实际线上验收基线是 3.18.2

1. 备份自己的配置和数据库，使用自己的 OpenAI OAuth 账号与独立测试 Client Key
2. 用 uname -m 确认服务器架构，在 CPR 插件管理页上传对应 .tar.gz；不要把源码 ZIP 当作插件包
3. 接受 trustedProcess 的信任提示。插件是可执行进程，不是操作系统沙箱
4. 参考 configuration.example.json 配置；模型须存在于自己的 CPR 有效目录和账号授权范围内
5. 配置下表两条绑定，使用相同模型和测试 Key 范围；验证后再扩大使用范围

| contribution | stage | failurePolicy | provider |
|---|---|---|---|
| donzy.excel-bps.upstream-adapter | upstream | reject | openai |
| donzy.excel-bps.middleware | request | reject | 按宿主匹配范围配置 |

```json
{
  "models": ["gpt-6-luna", "gpt-6-astra", "gpt-6-sol"],
  "timeoutMs": 3600000,
  "onFailure": "native_fallback"
}
```

只测试 BPS、不允许回落时将 onFailure 设为 reject。native_fallback 是插件检查完整历史、未发送及无副作用条件后的原生调用，不等于宿主绑定策略 delegate

检查 running、actualRevision、actualArtifactSha256，并核对实际请求日志中的 plugin.upstream / basis-points。仅收到 HTTP 200 不能证明请求走了 BPS

## 测试建议

依次验证文本 JSON/SSE、真实工具结果回传、图片、多轮续接、WebSocket 重连、请求内 compaction、思考等级映射、取消后再次请求和安全原生回落

反馈时提供版本、模型、时间、request_id、客户端协议与脱敏错误；不要公开 Key、OAuth、Cookie 或完整私人会话。不要使用在线低并发 Key 做压力测试

## 边界

- 历史缓存仅在进程内：单条 16 MiB、总量 32 MiB、128 条、10 分钟。失效后要求完整历史，不静默截断
- generate:false/store:false 为本地逻辑会话预热，单条 8 MiB、总量 16 MiB、5 分钟；不是物理上游连接预热
- timeoutMs 只覆盖 BPS 模型总时限；客户端、HTTP、RPC 等期限独立。原生回落子请求仍采用 CPR 默认 600 秒，宿主模型回调正文默认上限 1 MiB
- JSON Schema 输出采用提示约束和终态校验，并非上游原生受限解码
- Hosted web_search、file_search、image_generation 等未全部适配；客户端工具中继不代表这些服务端能力
- 独立压缩端点、模型目录和账本语义由官方 CPR 管理；旧宿主原生页面的定制徽标/配色不在本插件中
- 上游错误由官方 CPR 协议层分类，外层 HTTP 状态不保证与上游逐字节一致；不承诺任意长思考永不断线
- 停用/卸载后插件不再匹配请求，不能依赖已移除插件保持 BPS-only 约束

Caddy 的 flush_interval -1 会在客户端断开后保留后端请求。需要及时取消时可用 100ms，text/event-stream 仍按流即时刷新

## 代码与许可

plugin 是 Rust 宿主适配层，converter 是 Go 协议内核，Scripts 是构建和测试入口。仓库不保存部署账号、密钥、运行数据或安装产物

自有插件清单保留 UNLICENSED，不新增开源许可证。官方子模块保留 Apache-2.0；转换内核来源见 converter/internal/kernel/ORIGIN.md，MIT 许可证见 converter/third_party/CPA_LICENSE。复用转换代码不涉及修改 CPA 服务
