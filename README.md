# CPR BPS

独立的 Basis Points 插件，当前版本 **0.7.0**。使用官方 CPR 插件接口，不修改 CPR 程序或原生前端

## 使用

推荐使用官方 **CPR 3.18.2**。安装对应服务器架构的插件包后，打开 **插件 → BPS 设置**

页面只提供一套配置：

- **启用 BPS**：统一开关；关闭后新请求使用 CPR 原生通道，设置页仍可访问
- **使用账号**：选择全部可用 OpenAI OAuth 账号，或指定账号
- **应用模型**：只在此处选择一次，其他模型保持原生处理
- **更多设置**：请求等待时间，默认 60 分钟

配置只保存在 CPR 插件实例的 configuration 中，页面读取和保存同一份数据。无需旁路 JSON 文件，也无需分别设置请求中间件、上游适配器或内部调度绑定。内部绑定由保存操作生成，不另存一套模型过滤条件

Client Key 继续用于 CPR 鉴权、账号授权范围、配额和计费，但不是 BPS 路由条件。BPS 账号选择只能从 CPR 已授权候选中进行，不能绕过 Key 的权限

首次测试建议使用独立账号范围。核对请求诊断中的 plugin.upstream / basis-points；只收到 HTTP 200 不能证明请求实际经过 BPS

## 默认回落

默认自动尝试原生通道，不再提供“拒绝或回落”两套用户配置。发送前失败、没有可用 BPS 账号、上游明确拒收且没有输出或附件副作用时，可以安全回落

已经输出内容、执行状态不明的超时/断链/5xx、附件等外部副作用，或其他请求插件可能造成重复操作时，不盲目重发。此时保留实际错误，避免重复执行工具或重复消费。鉴权/预算等原生限制仍然有效

## 功能

Responses JSON/SSE、客户端 WebSocket、function/custom/namespace/tool_search 中继、工具结果回传、代码参数、JSON 修复、图片、思考等级映射、结构化结果校验、请求内压缩、历史续接和本地预热

CPR 负责账号池、OAuth、代理、租约、重试和账本。Rust 插件使用公开 SDK；Go 转换器不接收 OAuth 凭据，外部模型/附件请求由宿主受管 HTTP 发出

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
```

产物写入 packages，包含 .tar.gz 与 .sha256。静态 musl ELF 的包名沿用官方 CLI 接受的 Linux/architecture 标签。只构建插件、SDK 和打包 CLI，不构建或替换 CPR 服务

修改界面后重新生成已提交的静态资源：

```bash
npm ci --prefix frontend
npm run --prefix frontend build
```

BUILD_DIR 可指定构建缓存，CPR_PLUGIN_CLI 可指定同版本官方打包工具。宿主兼容声明为 >=3.18.2、<3.19.0

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
