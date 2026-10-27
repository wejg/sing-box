# Libbox 进程内热更新

这组接口兼容旧 `LibXrayHot` 的 8 个操作，并增加 3 个 endpoint 方法，挂在运行核心的 `LibboxCommandServer` 上。
调用位置是 Network Extension 内持有该对象的进程；不是 `CommandClient` RPC。
不需要调用 `StartOrReloadService`，不重启整个核心。

本次补丁同时包含已暂存、未暂存修改和新增文件。重编译 Libbox 必须使用当前完整工作树，不能只取之前的 staged diff，否则会漏掉 WireGuard 热更新和 sniff 新字段。

## 接口

| Go / gomobile 方法 | 含义 |
| --- | --- |
| `AddOutbound(outboundJSON)` | 新增出站或 WireGuard endpoint，重复 tag 报错 |
| `ReplaceOutbound(outboundJSON)` | 按 tag 替换出站或 WireGuard endpoint；不存在则新增，支持两种类型互换 |
| `RemoveOutbound(tag)` | 删除指定出站或 WireGuard endpoint |
| `AddEndpoint(endpointJSON)` | 新增用户态 WireGuard endpoint，重复 tag 报错 |
| `ReplaceEndpoint(endpointJSON)` | 按 tag 替换成用户态 WireGuard endpoint；不存在则新增 |
| `RemoveEndpoint(tag)` | 删除指定用户态 WireGuard endpoint |
| `AddInbound(inboundJSON)` | 新增监听，重复 tag 报错 |
| `ReplaceInbound(inboundJSON)` | 按 tag 替换；不存在则新增 |
| `RemoveInbound(tag)` | 关闭指定监听 |
| `AddRouteRule(ruleJSON, shouldAppend)` | 追加带标签的路由规则 |
| `RemoveRouteRule(ruleTag)` | 删除指定标签的路由规则 |

所有方法返回 `error`，gomobile 导出为 `NSError`，Swift 使用 `try`。
JSON 必须是**单个 sing-box 对象**，不能传 Xray JSON 或旧 `Invoke` 信封。
路由规则额外支持 `ruleTag`，启动配置中的 `route.rules` 也可设置该字段，方便之后删除。

```swift
// server 是已启动核心所用的 LibboxCommandServer。
try server.addOutbound(#"{"type":"direct","tag":"fp-1-out"}"#)
try server.addRouteRule(
    #"{"ruleTag":"fp-1-route","inbound":["fp-1-in"],"outbound":"fp-1-out"}"#,
    shouldAppend: true
)
try server.addInbound(
    #"{"type":"socks","tag":"fp-1-in","listen":"127.0.0.1","listen_port":21001}"#
)

// 删除时先关闭入口，再删除路由，最后删除出站。
try server.removeInbound("fp-1-in")
try server.removeRouteRule("fp-1-route")
try server.removeOutbound("fp-1-out")
```

`shouldAppend: false` 与旧封装语义相同：用新规则替换**整个路由表**。
普通增删线路应始终传 `true`。追加保持现有顺序，前面的兜底规则仍可能先命中。

## 行为和边界

- 操作与核心启动、停止、重载共用生命周期锁；停止状态下返回错误。
- 修改 B 不重建 A。规则变更影响后续连接，已建立的连接不会重新匹配规则。
- 同端口替换入站需要短暂关闭该监听；新配置启动失败时尝试恢复旧配置，恢复失败也会返回错误。其他监听保持运行。
- 删除入站关闭监听，不强制关闭已经建立的 TCP 连接。替换出站可能关闭该出站自身的连接，不承诺被替换线路不断流。
- 默认出站和仍被路由引用的出站/endpoint 不能直接删除。selector、其他出站/endpoint 的 detour、HTTP 客户端、入站、服务和 NTP 仍引用的节点不能替换或删除；需要先解除依赖，复杂依赖修改使用完整重载。
- DNS detour 引用允许同 tag 替换，后续新建连接会按 tag 重新查找节点。现有 DNS 连接池里的连接可能继续使用原路径，直到连接关闭或重建；不会主动打断查询。仍被 DNS 引用的节点不能删除。
- 新增/替换时校验配置和依赖；新对象完全启动后才切换。解析、依赖检查或启动失败会释放新对象，保留原节点。节点替换和移除一旦提交，旧对象的关闭错误记入日志，操作返回成功，让调用方可以准确记录撤销步骤。关闭错误可能留下尚未释放的资源，完整重载可用于恢复。入站同端口替换仍按前述关闭/恢复流程处理。
- 更新成功后同步核心的连接保活引用，避免新增线路被误判为未使用。
- 本接口不热改 TUN、DNS 规则、rule-set 文件、系统 WireGuard 接口或其他类型的 endpoint。已有 geo 规则保持原样；热加含 `rule_set` 的规则返回错误，因为未使用的集合可能已经被核心清理。
- WireGuard 必须使用当前 endpoint JSON（`address`、`private_key`、`peers`），不能使用旧 outbound 的 `local_address`、`peer_public_key` 等字段。`AddOutbound` / `ReplaceOutbound` 遇到 `type: "wireguard"` 自动分派到 endpoint 管理器，调用方可以继续使用原 8 个导出方法。运行库需开启 `with_wireguard,with_gvisor`。
- WireGuard 热更新支持 `system: false`。新旧 endpoint 使用相同非零 `listen_port` 会因占用而拒绝替换，保留旧 endpoint；客户端应使用默认随机端口，固定端口变更使用完整重载。同密钥立即替换后，新握手可能等待 WireGuard 自身重试，当前被替换的线路不保证不断流。
- WireGuard 目的域名解析遵循 `dns.rules` / `dns.final`；endpoint 的 `domain_resolver` 用于解析 peer 地址。隧道内 DNS 应通过 DNS 规则指向带对应 endpoint detour 的 DNS server，并为 peer 域名单独配置不依赖隧道的引导解析器。
- 热加规则如需要启动时未启用的进程、邻居或 Wi-Fi 元数据监控，返回错误，需通过完整配置重载启用。
- 这些修改只在当前实例内生效，不自动保存到磁盘。调用方要维护完整配置供下次启动使用。多次方法调用不是事务，失败后由调用方撤销此前成功的步骤。

## 嗅探目标覆盖

为恢复原 Xray `routeOnly: false` 的行为，路由 sniff action 新增两个字段：

```json
{
  "action": "sniff",
  "sniffer": ["tls", "http", "quic"],
  "override_destination": true,
  "override_excluded_domains": ["courier.push.apple.com", "Mijia Cloud", "dlg.io.mi.com"]
}
```

`override_destination` 默认关闭。开启后，用嗅探得到的有效域名替换实际目标地址，保持原端口。排除列表继承 Xray 无前缀 `domainsExcluded` 的大小写不敏感子串匹配；命中时仅跳过地址覆盖，域名仍可用于后续路由。该列表不解析 `full:`、`domain:` 或 `regexp:` 前缀。

TCP、UDP 及 UDP 预匹配的覆盖分支使用同一判断。新字段可用于启动配置或热添加路由规则。旧 framework 不认识这些字段，App 开始发送前必须重新编译并接入完整工作树生成的新库。

## 已执行的验证

本地回环集成测试覆盖实际 SOCKS/TCP 转发、A 长连接保持、B 新增/替换/删除、同端口认证替换、端口冲突恢复、隐式默认出站替换、路由增删/替换失败保护、依赖拒绝、保活引用更新，以及并发连接和停止/启动竞争。补充覆盖 DNS detour 新拨号跟随替换、WireGuard 增删替换及同 tag 类型互换、默认出口更新、启动/端口冲突失败保留原节点，以及两个真实用户态 WireGuard endpoint 之间的握手与 TCP 回显。额外注入资源关闭错误，验证入站替换和 inbound/outbound/endpoint 移除提交后仍返回成功，重复删除仍返回错误。嗅探回归覆盖 JSON 往返、排除项大小写/子串匹配，以及真实本地 SOCKS → HTTP 流量的目标覆盖、默认关闭、排除保留 IP、端口和原 HTTP 请求保持。

```sh
go test -race -mod=readonly ./experimental/libbox ./daemon ./route/... ./option ./adapter/inbound ./adapter/outbound ./adapter/endpoint -count=1 -timeout 120s
go test -race -mod=readonly -tags with_quic,with_wireguard,with_utls ./experimental/libbox -run '^TestHot' -count=1 -timeout 120s
go test -race -mod=readonly -tags with_quic,with_wireguard,with_utls,with_gvisor ./experimental/libbox -run '^TestHot' -count=1 -timeout 120s
```

另用 `gobind -lang=objc` 生成临时头文件，确认原 8 个方法及新增 3 个 endpoint 方法均可导出。
没有打包 Libbox/XCFramework，也未做 macOS Network Extension、远端 HY2、自签证书、远端 WireGuard 或 TikTok 的实机联调；这些需要接入新库后验证。
