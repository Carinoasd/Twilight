# Bangumi 记录管理与观看同步

Twilight 通过 Emby / Jellyfin 的播放停止 Webhook 保存观看记录，再由排程或用户手动同步到 Bangumi.tv。用户也可以管理收藏状态、进度、评分、短评、个人标签和私密设置。

## 启用步骤

1. 管理员在「系统配置 → Bangumi 管理与同步」开启需要的功能。
2. 用户在「Bangumi → 设置」填写自己的 [Access Token](https://next.bgm.tv/demo/access-token)，开启同步或收藏管理并保存。Token 只写入后端，页面读取只返回是否已配置。
3. 将媒体服务器的播放停止事件发送到 `POST /api/v2/emby/bangumi/webhook`，配置下述鉴权和事件字段。V1 同路径仍兼容。
4. 运行 Twilight 的 `scheduler` 或 `all` 服务并启用调度器。`sync_bangumi_watching` 默认每 15 分钟执行，也可在任务管理中手动执行或调整间隔。
5. 在用户的「观看记录」查看待同步、待确认、成功、失败和忽略状态；无法唯一匹配的记录由用户填写 Bangumi 条目 ID 和集数，明确确认已看完后再同步。

仅开启同步而没有播放事件来源，不会产生自动完成记录。旧 Emby ActivityLog 记录可供查看和人工确认，但只有活动时长无法证明看完，不会直接写入 Bangumi。

## 配置

同步与收藏管理是独立开关，用户还需开启对应个人模式。两项都关闭时，前端显示功能关闭提示。

```toml
[BangumiSync]
enabled = true
manage_enabled = true
webhook_secret = "replace-with-random-secret"
webhook_allow_legacy_token = false
auto_add_collection = true
private_collection = true
min_progress_percent = 85
block_keywords = []
```

| 键 | 缺省值 | 行为 |
| --- | --- | --- |
| `enabled` | `false` | Webhook、观看记录 API、同步与日志功能 |
| `manage_enabled` | `false` | 收藏查看和修改 |
| `auto_add_collection` | `true` | 同步时创建尚未收藏的条目；关闭时要求用户先收藏，再重试 |
| `private_collection` | `true` | 仅控制自动新建收藏的私密状态，保留已有收藏的隐私设置 |
| `min_progress_percent` | `85` | 播放位置达到总片长的此百分比才视为看完；范围 1–100，非法值回退 85 |
| `block_keywords` | `[]` | 标题或剧集名包含关键词时忽略，不区分大小写 |
| `webhook_secret` | 空 | 为空时拒绝 Webhook |
| `webhook_allow_legacy_token` | `true` | 兼容旧共享 Token 鉴权；推荐完成签名接入后关闭 |

**升级注意：** `auto_add_collection`、`private_collection`、`min_progress_percent`、`block_keywords` 以前只是示例配置，现在真正参与执行。仓库生产示例原有 `min_progress_percent = 80`，保留该值的部署会按 80% 判断完成；不是统一改成 85%。

用户字段：`bgm_mode` 控制同步，`bgm_manage_mode` 控制收藏管理，`bgm_token` 是该用户自己的 Token。全局 `Global.bangumi_token` 不作为个人同步的兜底凭据。`Global.bangumi_api_url` 默认 `https://api.bgm.tv/v0`。

## Webhook 接入

在 Emby / Jellyfin 插件中选择播放停止（Playback Stop）事件，发送 JSON，并保证媒体用户 ID 对应本地账号绑定的 Emby ID。Twilight 接收以下两种结构：

- Emby 嵌套结构：`UserId`，`Item.Id`、`Item.Name`、`Item.Type`、`Item.SeriesName`、`Item.IndexNumber`、`Item.ParentIndexNumber`、`Item.RunTimeTicks`，以及顶层 `PlaybackPositionTicks`。
- Jellyfin 官方 Webhook 平铺结构：`UserId`、`ItemId`、`ItemType`、`Name`、`SeriesName`、`EpisodeNumber`、`SeasonNumber`、`RunTimeTicks`、`PlaybackPositionTicks`、`PlayedToCompletion`。

媒体类型支持 `Episode` 和 `Movie`；Ticks 每秒 10000000。缺少播放位置时不会拿总片长补齐。布尔值 `PlayedToCompletion: true` 也可作为完成证据；字符串 `"true"` 不接受。

### 推荐：签名鉴权

签名发送端或中间转发器应设置：

- `Content-Type: application/json`
- `X-Twilight-Bangumi-Timestamp: <Unix 秒数>`
- `X-Twilight-Bangumi-Signature: sha256=<十六进制 HMAC-SHA256>`

HMAC 密钥是 `webhook_secret`，消息是 `timestamp + "." + 原始请求体字节`。时间允许偏差 ±300 秒；重复签名会被拒绝。发送端必须对实际发送的字节签名，不能在签名后重新序列化 JSON。

不支持动态签名的插件可在兼容开关开启时使用 `X-Twilight-Bangumi-Token` 头；这种方式没有签名的防篡改、防重放能力。不要把密钥放在 URL 查询参数中。此次功能不改变既有鉴权策略或 CORS／CSRF。

### 持久化与完成证据

记录按 `(uid, item_id, played_at)` 幂等写入。用户观看状态另存于 `User.BangumiWatch`，完成证据只会从未完成变成已完成。收到已看完事件后，原先因缺少完成证据而待确认的记录可再次自动尝试。

媒体库重命名或更换媒体 ID 可能形成新的本地记录；仍需检查其对应条目。播放停止的位置比例不等于连续观看时长，跳播到结尾也可能达到比例阈值。

## 同步规则

- 先用个人 Token 验证 Bangumi 账号，再读取最近最多 5000 条本地播放记录并按媒体去重。
- 每用户每轮最多尝试 25 个项目、最长 60 秒；按上次尝试时间轮转，避免较旧的待处理记录被较新的成功记录挡住。
- 搜索名称经大小写和标点归一化后必须唯一匹配。无匹配、同名多条目、第二季及以后或缺少集数时，转为待确认，不猜测条目。
- 剧集只标记这次实际观看的那一集，不补齐前面的集数，不把已有「看过」收藏降级成「在看」。已有短评、标签、评分和私密状态保留。
- 电影确认完成后将对应收藏标为「看过」。
- 未收藏条目按 `auto_add_collection` 决定是否新建；新建剧集为「在看」，电影为「看过」。
- API 与调度进程使用同一 PostgreSQL 用户级锁；收藏编辑也使用此锁，避免与同步同时写入。
- 超时、网络失败、权限错误和限流不会记为成功。未完成项目可重试；待确认或忽略的项目不会在每轮自动重复尝试。

### 日志、检查点与换 Token

成功检查点与可清理的同步日志独立。检查点按已验证的 Bangumi 账号 ID 隔离：

- 同一账号换 Token 后沿用既有成功状态。
- 换到另一个账号后使用该账号的同步状态，保留之前账号的历史。
- 清除同步日志不清除检查点，也不重置同步进度。
- 每用户最多 20000 项证据和检查点；达到上限时拒绝新增并返回错误，不自动删除历史。

远端 API 与本地数据库没有跨系统事务；如果远端已成功而本地保存失败，可能再次提交同一集的「看过」状态。手动收藏编辑也可能只完成部分请求，此时返回失败，需刷新核对后再试。

## 用户界面与 API

首页使用一次摘要读取，显示账号、同步概况、五类收藏入口和近期同步日志。「观看记录」分页并支持状态筛选、确认条目/集数、忽略和重试。收藏页支持服务器分页、当前页名称或个人标签筛选、排序、列表/卡片展示。

| 方法 | V2 路径 | 功能 |
| --- | --- | --- |
| GET | `/api/v2/bangumi/summary` | 本地同步状态、公开账号信息和收藏摘要；不返回 Token |
| GET | `/api/v2/bangumi/records` | 本人记录，`page`、`per_page`（1–50）、`status` 筛选 |
| PUT | `/api/v2/bangumi/records/{key}` | 本人记录 `confirm` / `ignore` / `retry`；确认需 `subject_id`、整数 `episode` |
| POST | `/api/v2/bangumi/sync` | 手动同步，返回 `synced`、`skipped`、`failed`；HTTP 成功不代表所有项目成功 |
| DELETE | `/api/v2/bangumi/sync/history` | 清除本人同步日志，保留检查点 |
| GET | `/api/v2/bangumi/collections` | 按收藏类型分页；`refresh=1` 跳过正常缓存 |
| PATCH | `/api/v2/bangumi/collections/{subject_id}` | 状态、进度、评分、短评、标签、私密设置 |

收藏修改：`type` 为整数 1–5，`rate` 为整数 0–10，`comment` 最多 1000 字，`tags` 最多 10 个无空白标签、每个最多 30 字，`private` 必须为 JSON 布尔值。空短评或空标签数组可清除对应值。

手动设为「看过」（type 2）表示整部完成，后端查询本篇章节并标记全篇，忽略客户端 `ep_status`。手动「在看」（type 3）的 `ep_status` 表示看到第几集；降低它会取消后续本篇集数的看过状态。这与自动同步仅标记单集的语义不同。

前端读取可取消过期请求，弹窗与记录区域有手机/Firefox 滚动边界；操作失败保留编辑内容，刷新后显示服务端实际状态。

## 维护与验证

主要代码：`bangumi_webhook.go`（采集）、`bangumi_sync_service.go`（同步）、`bangumi_watch_handlers.go`（记录与任务）、`bangumi_sync_handlers.go`（收藏）、`internal/store/bangumi_watch.go`（状态与锁）。前端为 `webui/src/app/(main)/bangumi` 与 `bangumi-watch-records.tsx`。

回归覆盖完成比例、Emby/Jellyfin 结构、只标记单集、同名歧义、Token 更换、清日志去重、跨进程锁、失败重试、记录归属、收藏字段和排程用户筛选。使用独立 PostgreSQL，按开发指南设置两个测试数据库环境变量后运行 `go test -p 1 ./...`。

接口依据：[Bangumi 官方 OpenAPI](https://github.com/bangumi/server/blob/master/openapi/v0.yaml)、[Jellyfin 官方 Webhook 插件](https://github.com/jellyfin/jellyfin-plugin-webhook)。
