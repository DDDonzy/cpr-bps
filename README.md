# CPR BPS

独立的 Basis Points 插件，当前版本 **0.11.2**。使用官方 CPR 插件接口，不修改 CPR 程序或原生前端

## 使用

推荐使用官方 **CPR 3.18.2**。导入对应平台的单一插件包后，打开 **插件 → BPS 设置 / BPS 日志**。转换器、设置页和日志页均随包携带，不安装额外服务，不要求配置 Caddy、域名、端口或实例 ID

页面只提供一套配置：

- **启用 BPS**：统一开关；关闭后新请求使用 CPR 原生通道，设置页仍可访问
- **使用账号**：表格按账号、套餐、账号状态和 BPS 状态分列，直接勾选 OpenAI OAuth 账号，支持名称、邮箱和 ID 搜索，以及全选/取消全选；默认不勾选，空选时不走 BPS
- **应用模型**：支持搜索、全选和取消全选；其他模型保持原生处理，空选时不走 BPS
- **失败处理**：回落原生（默认）或拒绝请求
- **请求等待时间**：直接显示，默认 60 分钟

配置只保存在 CPR 插件实例的 configuration 中，页面读取和保存同一份数据。无需旁路 JSON 文件，也无需分别设置请求中间件、上游适配器或内部调度绑定。内部绑定由保存操作生成，不另存一套模型过滤条件

Client Key 继续用于 CPR 鉴权、账号授权范围、配额和计费，但不是 BPS 路由条件。BPS 账号选择只能从 CPR 已授权候选中进行，不能绕过 Key 的权限

首次测试建议使用独立账号范围。核对请求诊断中的 plugin.upstream / basis-points；只收到 HTTP 200 不能证明请求实际经过 BPS

## 失败处理

选择“回落原生”时，发送前失败、没有可用 BPS 账号、上游明确拒收且没有输出或附件副作用时，可以安全回落。选择“拒绝请求”时不创建原生回落子请求，直接返回实际错误

策略保存在同一份 configuration.onFailure 中，旧配置缺少此字段时默认为 native_fallback

已经输出内容、执行状态不明的超时/断链/5xx、附件等外部副作用，或其他请求插件可能造成重复操作时，不盲目重发。此时保留实际错误，避免重复执行工具或重复消费。鉴权/预算等原生限制仍然有效

## 内置日志

日志入口是 **插件 → BPS 日志**，包含使用记录、错误记录、最近 30 天内的时间范围、搜索、分页和详情。直接通过当前 CPR 的公开管理接口读取记录，再按本实例保存的路由事实显示 BPS 通道和回落原因

| 路径 | 端点 | 通道 |
| --- | --- | --- |
| BPS | /basispoints/api/responses | BPS |
| BPS 决定回落原生 | /v1/responses | BPS回落 |
| 无路由标记 | 宿主原值 | 未标记 |

**CPR 原生“使用统计”页面保持原样，不再注入 BPS 标签。** 当前 CPR 没有进入插件前的 HTTP 路径筛选，使用全局钩子会让页面资源占用插件并发。内置日志页避免该问题，也不需要旁路服务

BPS 路由记录最多保留 30 天，列表与详情查询也限制在最近 30 天。清理由 CPR 的维护回调每 30 秒执行，无需 cron 或系统服务；整个插件停用时暂停，重新启用后补清。CPR 原始记录的全局保留策略不由插件修改

未标记不等于未经过 BPS。插件状态分 128 桶，每桶保留最近 64 条路由标记，合计最多 8,192 条，较旧标记会滚动淘汰；不按当前配置猜测补标。宿主仍负责请求记录、传输类型、用量与计费，插件不记录正文、令牌或工具参数

页面直接使用官方 @codex-proxy/ui v0.3.1 组件及 CPR 下发的主题。自身实例 ID、请求上下文和日志访问均由宿主提供，不包含安装机器的域名、IP、端口或绝对目录配置

## 从旧版迁移

仅安装过插件包的用户，升级后即可使用内置日志页，无需其他操作。

如果曾按 v0.10.x 方案安装 bps-log-gateway，应先确认新插件的内置日志页可用，再撤销反向代理中三条日志专用转发并停止旧日志服务，恢复所有请求直接进入 CPR。旧版旁路源码保留在 v0.10.0 标签，不属于新插件或构建流程

## 功能

Responses JSON/SSE、客户端 WebSocket、function/custom/namespace/tool_search 中继、工具结果回传、代码参数、JSON 修复、图片、思考等级映射、结构化结果校验、请求内压缩、历史续接和本地预热

CPR 负责账号池、OAuth、代理、租约、重试和账本。Rust 插件使用公开 SDK；Go 转换器不接收 OAuth 凭据，外部模型/附件请求由宿主受管 HTTP 发出

## 0.11.2 工具中继修复

- 普通 function/custom/tool_search 与 raw cmd/code 的规则明确分离；raw metadata 必须是 JSON 对象字符串，无其他参数也需 `{}`
- 冷启动历史回放使用当前工具目录的同一格式，保留原始代码与全部参数
- raw 路由仅对声明了对应 cmd/code 字段的 function 生效；合并后执行完整 schema 校验，不能把普通 create_thread 误当 raw 工具
- 对完整 JSON fence、BOM 和额外一层 JSON 字符串做有界兼容；说明文字、空值、数组、重复 raw 字段及非法 schema 仍拒绝，不猜测参数或盲目重放
- 错误只增加 metadata 类型/长度诊断，不回显命令或参数正文；失败工具批次仍不交付、不进入历史缓存

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

官方 CPR 子模块固定到 v3.18.2，commit e30aad475560b94db2d999e2251d180e45d52671，仅用于公开 SDK 和打包 CLI。不要使用 --remote 跟随其他版本；GitHub 自动生成的源码 ZIP 不包含子模块

## 构建

Linux/WSL，Go 1.26.0、Rust 1.97.0/rustup 和常规链接工具；首次构建需联网下载依赖及 Rust target

```bash
rustup toolchain install 1.97.0
bash Scripts/test.sh
bash Scripts/build.sh amd64
bash Scripts/build.sh arm64
# 在 Apple Silicon macOS 上构建：
bash Scripts/build.sh macos-arm64
```

产物写入 packages，包含 .tar.gz 与 .sha256。静态 musl ELF 的包名沿用官方 CLI 接受的 Linux/architecture 标签。只构建插件、SDK 和打包 CLI，不构建或替换 CPR 服务

页面为 Vue SFC，官方 UI 包固定到 GitHub Release，完整性摘要保存在锁文件中。修改界面后重新生成已提交的静态资源：

```bash
npm ci --prefix frontend
npm run --prefix frontend check
npm run --prefix frontend build
```

BUILD_DIR 可指定构建缓存，CPR_PLUGIN_CLI 可指定同版本官方打包工具。宿主兼容声明为 >=3.18.2、<3.19.0

机器配置无关不等于二进制跨平台通用。当前官方打包器接受 Linux x86_64、Linux ARM64 和 macOS ARM64；本项目发布并实测 Linux 两种架构，macOS 需在对应设备构建验证。Windows 原生目标尚未被该版官方打包器支持，Windows 上运行 Linux 容器时应选择相应 Linux 包

## 边界与维护

- 历史缓存单条 16 MiB、合计 32 MiB、128 条、10 分钟；重启、升级或过期后可能要求完整历史
- 本地预热绑定逻辑会话，单条 8 MiB、合计 16 MiB、5 分钟，不是物理上游连接预热
- BPS 总时限不覆盖客户端/反代等独立期限；原生回落子请求沿用宿主 600 秒默认期限，宿主模型回调正文默认上限 1 MiB
- JSON Schema 输出采用提示约束与终态校验；hosted web_search/file_search/image_generation 等没有全部适配
- 独立压缩端点、模型目录及账本语义由官方 CPR 管理；不承诺任意长思考永不断线
- Caddy 的 flush_interval -1 会在客户端断开后保留后端请求；需要及时取消时可用 100ms，SSE 仍按流刷新
- 停用/卸载整个插件后不再匹配请求，不能依赖已移除插件保持 BPS-only 约束

反馈请提供版本、模型、request_id、客户端协议及脱敏错误，不要公开 Key、OAuth、Cookie 或完整私人会话

## 许可

自有插件清单保留 UNLICENSED，不新增开源许可证。官方子模块保留 Apache-2.0；转换内核来源见 converter/internal/kernel/ORIGIN.md，MIT 许可证见 converter/third_party/CPA_LICENSE
