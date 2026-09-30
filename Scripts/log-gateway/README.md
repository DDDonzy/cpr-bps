# BPS 日志转发服务

仅用于在 CPR 原生日志页显示 BPS 路径，不参与模型请求或账号调度。BPS 插件本身不再注册全局 HTTP 中间件

## 数据路径

- Caddy 将三个日志 GET 接口转发到本服务，其他路径直接交给 CPR
- 本服务先把请求交给 CPR 鉴权和查询，成功后使用同一浏览器身份调用插件的只读 `decorate-logs` 管理接口
- 不保存 Cookie、Key 或日志正文，不使用额外管理员凭据，不连接 Docker 或数据库
- 转换接口每次发现当前插件版本，不固定制品摘要或 revision
- 转换失败、插件停用、日志响应超出 4 MiB 或显示转换并行槽位用满时，原样返回 CPR 日志；4 MiB 只限制可选显示转换，不截断日志、不限制模型历史
- 转换最多并行 4 个，最多等待 2 秒；CPR 查询不占用这套插件全局 HTTP 槽位
- 本服务停止时 Caddy 自动退回 CPR，暂时不显示 BPS 标签

## 构建

```bash
bash Scripts/log-gateway/build.sh arm64
bash Scripts/log-gateway/build.sh amd64
```

产物为 packages/bps-log-gateway-版本-linux-架构.gz。测试：

```bash
cd Scripts/log-gateway
go test -race ./...
go vet ./...
```

## 部署

将解压后的二进制安装到 `/opt/cpr-bps-log-gateway/bps-log-gateway`，使用随附 systemd unit。unit 中的公网 Origin、插件实例 ID 和 CPR 回环端口须与安装环境一致；监听必须是回环地址，systemd 使用 DynamicUser，不需要 root、数据库或 Docker 权限

Caddy 对应站点配置：

```caddyfile
@bps_logs {
    method GET
    path /api/admin/usage/records /api/admin/usage/records/detail /api/admin/operations/errors
}
handle @bps_logs {
    reverse_proxy 127.0.0.1:18901 127.0.0.1:8080 {
        lb_policy first
        fail_duration 10s
        max_fails 1
        lb_try_duration 1s
    }
}
handle {
    reverse_proxy 127.0.0.1:8080 {
        flush_interval 100ms
    }
}
```

不要再配置固定版本的 `/assets/*` 文件目录。HTML、JS、CSS 均由当前 CPR 提供，升级和回滚时无需另行同步静态文件

迁移旧版：先启动回环服务并验证原样转发，安装不含 HTTP 绑定的插件，验证日志管理接口，再切换 Caddy。保留旧 Caddy 配置及插件制品以便回滚；全程使用实例 revision 比较更新，避免覆盖用户改动

如果以后 CPR 提供进入插件前的路径筛选，可用该正式接口替代本转发服务。此方案不修改 CPR 程序，也不改变宿主的业务并发限制
