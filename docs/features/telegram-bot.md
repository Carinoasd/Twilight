# Telegram Bot 命令

本文说明 Twilight 内置 Telegram Bot 的命令清单、权限边界、绑定流程与可定制文案。Bot 主要承担绑定、查询、统计和通知职责；密码、系统更新、数据库恢复等高风险操作请在 Web 后台完成。

Bot 由二进制子命令 `bot`（或 `all`）启动，轮询逻辑见 `internal/api/telegram_bot.go`，命令注册表见 `internal/api/telegram_commands.go`，群组内联面板见 `internal/api/telegram_inline.go`，配置字段见 `internal/config/config.go` 的 `[Telegram]` 段。

## 扫码绑定与登录

设置、强制换绑及注册页面生成绑定链接后，会同时显示二维码。使用手机扫码进入 Bot，继续现有绑定流程；网页保留直接打开 Telegram 及手动命令。二维码在浏览器本地生成，绑定令牌不会发送给第三方图片服务。链接过期后需重新生成。

扫码登录需要管理员在配置页的 Telegram 区域开启 `login_enabled`，或设置 `[Telegram] login_enabled = true`。默认关闭；环境变量为 `TWILIGHT_TELEGRAM_LOGIN_ENABLED`。API 与独立 Bot 进程需读取同一配置和 PostgreSQL 数据库。原有 Telegram 总开关及 Bot Token 也必须可用。

登录页点击“Telegram 扫码登录”，二维码有效期 3 分钟。仅已经绑定 Telegram 的可用账号可登录。扫码后 Bot 展示站点、浏览器设备描述及核对码；核对网页中的码后点击“确认登录”或“拒绝”。扫码本身不会登录，也不会自动注册、解绑或覆盖绑定。请求页面和设备描述来自浏览器，仅供核对；不要扫描别人发来的登录二维码。

登录请求只允许发起网页凭据兑换一次，并沿用密码登录的设备封禁、设备上限、操作日志及 TG/邮件通知。取消、过期、拒绝、已消费请求均不能重新使用。修改密码、停用账号、改变角色或 TG 绑定、封禁设备和退出全部会话会撤销相应的旧许可；之后恢复原值也不会恢复许可。会话签发失败时必须重新生成二维码。

请求存入独立的 `twilight_telegram_logins` 表，数据库只保存令牌与网页凭据的 SHA-256 摘要，网页凭据仅保留在当前页面内存。逻辑备份不包含请求，恢复或迁移导入时清空请求。新建请求及既有清理任务都会清理过期数据，总量上限为 1000。数据表初始化使用独立事务、版本表 `twilight_telegram_login_schema`（版本 1）及跨进程启动锁；没有历史登录请求需要回填。账号安全字段变化通过主状态表触发器在同一事务撤销请求。

升级前保留正常数据库备份。初始化失败会回滚新增表和触发器；回退程序时保留新增表与触发器，保持已撤销许可的状态，再由数据库管理员按需处理。不要单独删除请求表而保留引用它的触发器。

## 命令与权限

| 规则 | 说明 |
| ---- | ---- |
| 私聊优先 | `/bind`、`/me`、`/emby`、`/resetpwd`、`/cancel`、`/about` 以及管理员命令 `/stats`、`/admin`、`/userinfo`、`/twfind`、`/twishelp` 仅在私聊生效。 |
| 群聊保护 | 在群聊使用上述账号类命令时，Bot 只回复"请私聊使用"提示，不展示账号状态。`/start`、`/help`、`/twihelp` 在群聊会被替换为群聊提示文案。 |
| 管理员判定 | `telegramAdminID` 判定逻辑：Telegram ID 命中 `[Telegram].admin_id` 列表，或该 Telegram ID 已绑定到一个 `Role == admin` 的 Twilight 账号。 |
| 敏感信息边界 | Bot 默认不展示密码、Token、Emby ID、Telegram ID、服务器线路/地址、数据库连接串等敏感信息；管理员自定义 `/twguser` 模板时可显式使用 `{telegram_userid}`。 |
| 写操作边界 | 绑定链接及扫码登录在私聊完成，后者须点击确认按钮；群组 `/twguser` 管理操作每次点击均重新校验管理员身份（详见后文）。 |
| 命令解析 | 命令统一转小写并剥离 `@botname` 后缀（`telegramCommand`），因此在群里写 `/stats@yourbot` 也能识别。 |

## 轮询与性能

- Bot 按 Telegram `getUpdates` 长轮询接收更新。每个批次会按聊天、操作者和成员目标构建连通分组：共享任一身份的更新严格保持原始顺序，完全独立的分组最多由 8 个 worker 并行执行，避免一个慢指令阻塞所有其他会话。
- 启动时会通过 `getMe` 验证 Bot 身份；后续轮询复用该身份，只有 Telegram API 地址或 Bot Token 变化时才重新验证，避免每批更新额外产生一次网络请求。
- 高频更新让长轮询立即返回时，配置文件变更检查最多每秒执行一次；正常低流量长轮询仍会自然检查热重载。
- Bot 关闭或 Token 缺失时仍会定期检查配置恢复，但只在进入等待状态时记录一次运行状态和日志，不会每三秒持续写入运行日志表。
- Telegram API 请求前缀会按当前 API 地址和 Token 缓存。任一配置变化都会自动重新执行地址安全校验并生成新前缀，不会绕过 SSRF 防护。
- `getUpdates`、`getMe`、`getChat`、`getChatMember` 与 `getChatAdministrators` 直接解码为 update / user / chat / member 窄 DTO，不再构造嵌套 `map[string]any`。排序、花名册、成员审查、inline、JS callback 和命令分发都读取强类型字段；批次分组仅保存原切片索引并向 worker 传指针，不复制 update 值。普通回复丢弃无需使用的 Telegram Message 结果，只有需要 `message_id` 的面板/交互发送才解析返回对象。
- 统一协议层仍保持单次 envelope 解码、4 MiB 成功响应上限、调用方 deadline 优先和 HTTP 429 `retry_after` 退避。群组面板与 JS callback data 使用严格的零切片解析，空段、多余段或非法序号会直接拒绝。
- 群成员花名册在专表 `twilight_telegram_roster` 记录首次出现、最近出现和成员状态；最多 4096 项的进程热缓存让普通群消息对未变化成员五分钟内不访问数据库，冷缓存也通过条件 UPSERT 避免近期行物理更新。成员加入、退出、权限状态变化和首次识别为 Bot 仍会立即落库，定时成员检查则整批一条 SQL 合并。
- 所有分组及其副作用完成后才持久化 offset，不会为了并发速度提前确认。进程异常时只可能重取尚未确认的批次；绑定、销号确认、JS waiter 和 inline callback 在共享聊天/用户范围内仍保持顺序。
- `getUpdates` offset 存在单行专用表 `twilight_telegram_runtime`，每批仅执行一次窄行单调更新，不再刷新、复制、序列化和重写整份 `twilight_state`。旧 JSONB 游标会在启动或导入历史 JSON 快照时迁移一次并从主状态清除。
- 普通聊天文本在交互等待状态和私聊绑定码判断后立即结束处理，不再进入命令分词、注册表和自定义指令匹配；无参数命令也不会创建无用的参数切片。
- 自定义指令、禁用指令和配置管理员 ID 会在配置快照上建立只读索引，配置热重载后自动重建，监听与鉴权路径无需逐条扫描配置列表；数据库动态授予的管理员角色仍通过 Telegram ID 专用索引实时判定。
- 自定义 JavaScript 指令首次执行时完成安全校验和 Goja 编译；相同脚本后续复用有上限的编译缓存。脚本内容变化会生成新缓存键，被安全规则拒绝的脚本不会进入缓存。
- `/stats` 直接使用数据库状态层的聚合计数，不再复制并排序完整用户、注册码和邀请码列表。
- `/emby` 首次连通性探测最多等待 1.5 秒；成功结果短期复用 30 秒，失败结果复用 5 秒。Emby 地址或 Token 变化会立即生成新的安全缓存键并重新检测，不会继续展示旧配置结果。
- 管理员用户搜索直接在状态层已有的 UID 顺序索引上匹配，并在达到结果上限后停止，不再为一次 Bot 查询复制完整用户列表。
- 群组管理面板、JS 内联回调、JS 消息等待和销号确认均有独立容量上限。达到上限时优先清理过期状态，再淘汰最早到期项并停止其定时器，避免交互洪泛持续放大内存与 goroutine。

当前 100 条中等文本更新的本地基准中，强类型 DTO 相比上一版单次动态 map 解码，交错执行耗时下降约 17.5%-24%；临时内存从 `322064 B/op` 降到 `184952 B/op`，分配从 `3520 allocs/op` 降到 `431 allocs/op`，分别下降约 42.6% 和 87.8%。旧双重解析路径仍保留为基准参照；绝对耗时会随硬件、睿频和 Go 版本变化，应优先观察交错比率与稳定的分配指标。

群管理员列表的 100 项本地基准中，成员窄 DTO 相比动态 map 的交错耗时比为 `0.6657-0.7581`，临时内存从约 `160704 B/op` 降到 `82328 B/op`，分配从 `2326 allocs/op` 降到 `228 allocs/op`。`/twguser` 占位符替换改为单次扫描后，测试模板由约 `4.6-4.8 us / 3584 B / 39 allocs` 降到约 `0.31 us / 112 B / 1 alloc`；未知占位符仍原样保留。

## 用户命令

下列命令均要求私聊（群聊会被拦截或转为提示）。

| 命令 | 说明 |
| ---- | ---- |
| `/start` | 显示 Bot 入口与常用命令；可被 `bot_start_text` 完整覆盖。 |
| `/help` | 显示帮助；管理员会额外看到管理员命令清单。 |
| `/twihelp` | `/help` 的别名，行为完全一致。 |
| `/about` | 查看服务说明。 |
| `/bind <绑定令牌>` | 手动方式：把 Web 端绑定链接里的令牌发给 Bot 完成绑定；无参数时回复绑定提示文案。 |
| `/start <令牌>` | 点击 Web 端的 `t.me/<bot>?start=<令牌>` 绑定链接后由 Telegram 自动发送，Bot 直接完成绑定；无参数时回复欢迎语。 |
| `<令牌>` | 私聊里直接粘贴 32 位十六进制令牌（不带 `/`）也可完成绑定；其它文本不触发。 |
| `/me` | 查看当前 Telegram 绑定的 Twilight 账号摘要。 |
| `/emby` | 查看账号本地状态、到期、Emby 绑定、服务器是否配置以及连通性（不展示服务器地址）。 |
| `/resetpwd` | 提示前往 Web 端修改密码；Bot 不接收、不生成也不发送密码。 |
| `/cancel` | 回复"已取消当前 Bot 操作"。 |
| `/delAccount <子命令> [原因]` | 删除自己的账号。验证优先级：已绑定邮箱→`/delAccount email [原因]` 发码后 `/delAccount email <验证码>`；已绑定 Emby→`/delAccount emby [原因]` 走 Web 密码+Emby 密码两步验证；无绑定→`/delAccount confirm [原因]` 后再发送 Web 密码二次确认。子命令不分大小写；不认得的参数只回说明，不会执行任何删除。可附带可选删除原因（记录到审计日志）。删除成功后会释放 Telegram 绑定和临时绑定码，同一个 Telegram 可以重新注册新 Web 账号。禁用状态的 Web/Emby 账号默认不可自删；邀请关系中的已到期账号如果 Emby 已被到期任务禁用，可用 Web 密码完成退出清理，执行时会完全删除对应 Emby 账号。示例：`/delAccount emby 不再使用本服务` 或 `/delAccount email 不再使用`。 |

`/emby` 的连通性检测仅在配置了 Emby 地址时进行，结果分为"正常 / 不可用 / 未检测"，不会展示服务器 URL。

## 绑定码残留修补

Telegram 绑定码只是短期运行时票据，真正的账号绑定以用户记录中的 `telegram_id` 为准。历史版本曾把绑定码状态持久化到状态文档，可能留下"绑定码已确认，但没有对应用户记录"的残留。当前版本在系统启动和配置热重载时会自动清理这些旧版持久化绑定码，并清理绑定链接表中的过期或孤立记录（保留有效链接）；这不会解绑已有用户，也不会删除 Telegram 群成员花名册。

## 管理员命令

下列命令要求私聊且通过管理员判定，未授权时统一回复"没有管理员权限。"。

| 命令 | 场景 | 说明 |
| ---- | ---- | ---- |
| `/admin` | 私聊 | 显示管理员只读查询入口列表。 |
| `/stats` | 私聊 | 统计用户总数、活跃用户、Telegram 已绑定、Emby 已绑定、待开通 Emby、注册码数量、邀请码数量。 |
| `/userinfo <关键词>` | 私聊 | 查询单个用户摘要；匹配命中多个时提示缩小关键词。 |
| `/twfind <关键词>` | 私聊 | 搜索用户并返回最多 10 条非敏感摘要列表。 |
| `/twishelp` | 私聊 | 查看管理员帮助文案。 |
| `/banweb <用户> [理由]` | 私聊 | 禁用指定用户的 Web 账号（可选理由，记入操作日志）。`<用户>` 必须精确等于 UID、完整 Web 用户名、Telegram ID 或 @Telegram 用户名，模糊结果只列出候选、不执行。受保护账号不可操作。 |
| `/banemby <用户> [理由]` | 私聊 | 单独禁用指定用户的 Emby 账号，不影响 Web 账号（可选理由，记入操作日志）。目标匹配规则同 `/banweb`。受保护账号及未绑定 Emby 的用户不可操作。 |
| `/twguser <关键词>` | 群聊 / 私聊 | 打开群组用户管理面板（带内联操作按钮）。 |
| `/twguser`（回复目标消息） | 群聊 | 回复某成员消息后发送，按其 Telegram 绑定关系定位对应 Twilight 用户并打开面板。 |

`/userinfo` 与 `/twfind` 的搜索关键词支持用户名、邮箱、UID、Telegram ID、Telegram 用户名、Emby 用户名、Emby ID 进行匹配（`telegramUserMatches`），但返回的摘要不会展示 Telegram ID、Emby ID 等敏感字段。

### 群组用户管理面板（`/twguser` 内联操作）

> 提示：旧文档曾描述群组 `/twguser` 为"只读查询、不提供 inline 写操作按钮"。实际代码（`internal/api/telegram_inline.go`）提供了一组带写操作的内联按钮，下表予以更正。

> 开关：面板受 `[Telegram] enable_tg_panel` 控制，默认 `false`。关闭时 `/twguser` 在群聊中静默忽略、私聊回复“未启用”，已发出面板的按钮也一律拒绝执行。

`/twguser` 命中目标用户后会发送一条带内联键盘的面板消息，可执行以下操作：

| 按钮 | 行为 |
| ---- | ---- |
| 刷新 | 重新拉取并展示用户当前状态。 |
| 启用 / 禁用 Web 账号 | 切换账号 `Active`；如已绑定 Emby 且配置了服务器，会同步调整 Emby 用户启用状态。 |
| 授予 7 天 / 30 天 / 365 天 / 永久 | 为未绑定 Emby 的用户标记 `PendingEmby`，写入 `emby_grant_locked=true`，并设置对应待补建天数；仅在用户未绑定 Emby、未待开通且非受保护账号时出现。 |
| 删除用户 / 确认删除用户 | 两步确认；确认后删除用户并清除其会话。 |
| 移出群组 / 封禁群组 | 对已绑定 Telegram 的目标执行群组踢出 / 封禁，仅在目标已绑定 Telegram 时出现。 |

面板安全约束：

- 受保护账号（`Role == admin`，或其 Telegram ID 命中管理员判定）禁止被禁用、删除、移出或封禁。
- 群内匿名管理员（以群身份发言、`from.id` 为 0 或带 `sender_chat`）发送 `/twguser` 时，必须先点击"验证管理员身份"内联按钮完成真实身份校验，才会展示面板。
- 每次按钮点击都会重新执行 `telegramAdminID` 校验；非管理员点击只会收到弹窗提示（answerCallbackQuery），不会在群里发消息。
- 面板有效期为 1 分钟，无操作自动删除；每次操作会刷新过期时间。
- 非管理员发起的越权指令，连同提示消息会在 30 秒后自动删除；同一成员在同一群 30 秒内只提示一次。匿名身份（sender_chat）发送 `/twguser` 时，同一身份 30 秒内只发一个验证面板。
- 删除 Emby 账号类操作会尊重用户记录上的 `emby_grant_locked`。通过注册码、白名单码、邀请码、后台授予、Telegram 授予或自助创建获得过 Emby 注册资格的账号，不能通过面板删除 Emby 后再次自助注册。

面板文本可通过 `[Telegram].group_user_panel_template` 自定义，也可在 Web 后台配置页的 Telegram 分组中编辑。留空使用内置模板；未知占位符会原样保留，便于发现拼写错误。面板发在群里、所有群成员可见，因此模板不提供完整邮箱、Emby ID、密码、Token 或服务器线路占位符：`{email}` 只输出遮罩后的邮箱（如 `ab***@example.com`），`{registration_code}` 只输出卡码前 4 位（如 `ABCD***`）；如确需展示 Telegram ID，可显式使用 `{telegram_userid}`。

模板渲染采用单次扫描，仅替换当前文本实际出现的已知占位符；重复占位符保持一致，未知或未闭合占位符原样保留。刷新面板不会为全部占位符重新编译一次性替换器。

常用占位符：

| 占位符 | 含义 |
| ---- | ---- |
| `{server_name}` | 站点名称。 |
| `{username}` / `{uid}` | Web 用户名 / UID。 |
| `{role}` / `{role_id}` | 角色名称 / 角色数字。 |
| `{is_admin}` / `{is_protected}` | 是否管理员 / 是否受保护账号。 |
| `{web_status}` / `{web_active}` | Web 账号启用状态。 |
| `{expire_status}` / `{expired_at}` | 到期摘要 / 具体到期时间。 |
| `{register_time}` / `{created_at}` | 注册时间 / 创建时间。 |
| `{telegram_status}` / `{telegram_username}` / `{telegram_userid}` | Telegram 绑定摘要 / Telegram 用户名 / Telegram 用户 ID。无用户名时 `{telegram_username}` 显示 `None`。 |
| `{emby_status}` / `{emby_username}` | 本地 Emby 绑定摘要 / 本地 Emby 用户名。 |
| `{emby_bound_status}` / `{emby_bound}` | 本地 Emby 绑定状态 / 是否已绑定。 |
| `{emby_unbind_allowed}` | 是否允许用户自助解绑 Emby。 |
| `{pending_emby}` / `{pending_emby_days}` | 是否待补建 Emby / 待补建授权天数。 |
| `{registration_source}` / `{registration_code}` | Emby 注册资格来源 / 对应卡码（仅前 4 位）。 |
| `{emby_remote_block}` | 完整 Emby 远端信息块，包含远端用户名、启用状态、权限、隐藏状态与最近活动。 |
| `{emby_remote_status}` / `{emby_remote_username}` | 远端查询状态 / 远端用户名。 |
| `{emby_remote_enabled}` / `{emby_remote_role}` / `{emby_remote_hidden}` | 远端启用状态 / 远端权限 / 是否隐藏。 |
| `{emby_last_activity}` | 远端最近活动时间。 |
| `{bgm_mode}` / `{bgm_token_status}` / `{bgm_sync_status}` | BGM 同步开关 / Token 是否配置 / 同步可用状态。 |
| `{api_key_status}` | 旧 API Key 开关。 |
| `{panel_ttl}` / `{panel_ttl_seconds}` | 面板有效期文本 / 秒数。 |

## 绑定流程

1. 用户在 Web 端点击“获取绑定链接”。服务端签发有效期 600 秒的链接：`link_id`（观察句柄）、`start_token`（128 位随机熵）、`deep_link`（`https://t.me/<bot>?start=<start_token>`）、`manual_command`（`/bind <start_token>`），注册场景另返回 `link_secret`（256 位随机熵）。数据库只保存摘要。
2. 用户点击“在 Telegram 中打开”，Telegram 打开 Bot 私聊并自动发送 `/start <start_token>`；无法打开链接时可手动发送 `/bind <令牌>` 或直接粘贴令牌。
3. Bot 按 `^[A-Za-z0-9]{8,64}$` 校验令牌形状（`telegramLinkTokenPattern`，大小写不敏感），按摘要定位链接，并检查是否存在、是否过期、是否已被取代，以及该 Telegram 是否已被其它账号占用。
4. 若开启了强制加群 / 订阅频道（`force_bind_group` / `force_bind_channel`），会校验该 Telegram 是否已加入指定群组 / 频道；未满足会列出待加入的目标并把失败原因写回链接（仍为 pending、可重试），用户加群后再次点击链接即可。
5. 通过后写入：已登录场景在同一事务里写入账号绑定与身份历史，链接直接进入 consumed，网页轮询到 `telegram_bound=true`；注册场景链接进入 confirmed，网页轮询到确认后把 `link_id + link_secret` 随注册表单提交，账号创建、注册码权益、身份历史与链接消费在同一事务提交，创建失败保留已确认链接可重试。

绑定确认对同一 Telegram 账号做了幂等处理；另一个 Telegram 不能覆盖已确认的身份。单个 Telegram ID 有每分钟速率限制，避免反复触发群成员校验。群聊内不处理令牌。

已绑定账号不能重新申请绑定链接，也不能用之前签发的链接覆盖现有身份。普通用户需先提交换绑申请，由管理员批准后解绑，再绑定新账号；最终解绑写入会重新检查审批状态及其对应的旧 Telegram ID。身份清除、审批消费、换绑中标志和身份历史在同一个 PostgreSQL 事务内提交，V1/V2 使用同一处理逻辑。Emby 状态同步仍是提交后的外部操作，不属于数据库事务。

“完成换绑”会先检查新 Telegram 的群组/频道资格，再在写入时核对当前身份与换绑开始时间；检查期间账号被另一个请求解绑或修改，会返回状态冲突。资格检查失败不重置换绑开始时间，也不会把其他请求已经完成的状态改回“换绑中”。V1/V2 和设置页共用状态投影，旧身份的审批不会被显示为可用。

换绑审批只对当前这一轮有效。用户解绑、管理员协助解绑或替换 Telegram 身份时，账号原有的已批准申请在同一事务内变为 `used`，待审申请变为 `revoked`；绑定确认和换绑完成也会清理旧流程遗留的许可。管理员原始审核人、时间和备注保持不变。更新同一 Telegram ID 的用户名不会消耗尚未使用的许可。

单条和批量审核只能批准/拒绝 `pending` 申请；批准前还会检查申请对应的 Telegram 身份及当前换绑状态。已批准、已使用、已拒绝或已撤销的申请不能重新批准，单条接口返回 409，批量接口计入失败。需要再次换绑时，用户必须重新提交申请并等待审核。设置页在解绑或绑定成功后立即清除旧操作权限，重新读取状态失败时显示重试入口。

升级不会批量撤销仍未使用的有效审批。若旧版本已经完成换绑却遗留了 `approved` 记录，管理员可使用后台“撤销全部换绑许可”清理；这会同时撤销其他尚未使用的许可，相关用户需重新申请。正在进行的换绑不受此操作影响。

### API / Bot 独立运行

API、Bot、Scheduler 可以继续使用独立 systemd 服务，只要求连接同一个 PostgreSQL 并使用一致的 Telegram 资格策略。Bot 直接把确认结果写入共享的 `twilight_telegram_links`，API 的每个 HTTP 请求都会先刷新主状态、每次状态查询都直接读库，因此没有进程内 hub、长轮询、WebSocket 或回环 HTTP 确认端点。API 重启不会丢失仍在有效期内的链接。

状态机为 `pending → confirmed → consumed`，重新签发会把同一浏览器或账号之前的活动链接置为 `cancelled`。临时网络、加群校验失败保持可重试状态，不要求立刻换链接。数据库故障返回服务不可用，不冒充“链接过期”。

注册必须由签发链接的浏览器继续：只有持有 `link_secret` 的一方才能查询身份或提交注册；把 deep link 或令牌转给别人只会让别人的 Telegram 被记录到这条链接上，并不能替他人注册。所有相关响应禁止缓存，日志不得记录令牌、secret 或 Bot Token。

升级时协调部署后端与 WebUI，再重启 API、Bot、Scheduler。首次启动自动创建链接表并丢弃上一代挑战表；升级前尚未完成的旧绑定码需重新生成，已经绑定的账号无需重新绑定。备份/迁移不导出链接，恢复后需重新生成。

### Telegram 用户名自动刷新

用户在 Telegram 改了 `@username` 后，已绑定账号里存的 `telegram_username` 会过时。`observeTelegramRoster`（`internal/api/telegram_bot.go`）在处理**任意**来自已绑定用户的更新（私聊 / 群消息 / `chat_member` 事件）时，顺手调用 `refreshTelegramUsername` 被动刷新：仅当能解析到绑定账号、且新用户名非空并与现存不同才写库（无额外 API 调用）；用户名为空（对方删了 `@username`）时保留旧值不清空，以免破坏指名注册码的用户名匹配。此外 `/twguser` 面板渲染时也会经 `getChatMember` 做一次按需刷新。

### 指令监听与花名册性能

- Bot 仍按聊天、操作者和成员目标维持有序处理；完全独立的更新分组最多由 8 个 worker 并行，所有副作用完成后才确认本批 offset。
- 群成员历史保存在 `twilight_telegram_roster`，不再把全量历史映射常驻 `twilight_state` 和 Go 堆。普通群消息使用最多 4096 项的进程内热缓存，同成员状态未变时五分钟内不发 SQL；服务重启后的冷缓存也由 PostgreSQL 条件更新避免重复物理写。
- `chat_member` 状态变化、新成员和首次发现 Bot 标记会立即 UPSERT；定时成员检查把整批结果合并成一次 SQL，避免逐成员重写整份主状态。
- 定时群成员巡检会跳过处于换绑流程的用户：待审核、已批准待解绑和正在换绑都不会因旧 Telegram 账号离群/封禁而自动禁用 Web 或 Emby。巡检真正写入禁用前会在 Store 锁内再次确认换绑状态，避免用户刚提交申请就被并发任务误禁用；账号到期检查仍独立执行，不因换绑而延后。
- 旧 JSONB 花名册会在启动时幂等迁移。JSON 备份仍包含 `telegram_roster`，恢复时自动拆回专表，因此性能拆分不会丢失历史。

## 安全边界

- 群聊不处理账号状态、观看记录汇总、绑定码、管理员统计等敏感命令，仅 `/twguser` 在群内可用且受上述面板约束保护。
- 管理员查询摘要仅展示用户名、UID、角色、启用状态、到期状态、Telegram 是否绑定、Emby 是否绑定、是否待开通 Emby。
- Bot 默认模板不展示 Emby ID、Telegram ID、密码、Token、服务线路、数据库连接串；管理员自定义 `/twguser` 模板时可显式使用 `{telegram_userid}`。
- `/emby` 只展示是否配置、是否可连通，不展示服务器地址。
- Bot 处理过程对每条 update 做 panic 隔离，日志中的 panic 文本与敏感内容都会经脱敏处理。

## 文案配置项

下列字段在 `config.toml` 的 `[Telegram]` 段维护，留空时使用 Go 后端内置文案。可在 Web 管理端配置编辑器中修改。

| 配置项 | 对应字段 | 说明 |
| ------ | ------ | ---- |
| `bot_start_text` | `TelegramBotStartText` | 覆盖私聊 `/start` 的完整文案。 |
| `bot_group_start_text` | `TelegramBotGroupStartText` | 覆盖群聊里 `/start`、`/help`、`/twihelp` 的提示文案。 |
| `bot_start_title` | `TelegramBotStartTitle` | 内置 `/start` 文案标题（默认 `Twilight Bot`）。 |
| `bot_start_intro` | `TelegramBotStartIntro` | 内置 `/start` 简介段。 |
| `bot_bind_prompt_text` | `TelegramBotBindPromptText` | `/bind` 无参数时的提示文案。 |
| `bot_help_text` | `TelegramBotHelpText` | 覆盖 `/help` 与 `/twihelp` 的完整文案。 |
| `bot_admin_help_text` | `TelegramBotAdminHelpText` | 覆盖 `/twishelp` 的完整文案。 |
| `bot_help_header` | `TelegramBotHelpHeader` | 追加到内置普通帮助顶部。 |
| `bot_help_footer` | `TelegramBotHelpFooter` | 追加到内置普通帮助底部。 |
| `bot_about` | `TelegramBotAbout` | `/about` 服务说明文案。 |
| `group_user_panel_template` | `TelegramGroupUserPanelTemplate` | 覆盖 `/twguser` 群组用户面板文本，支持上方用户占位符。 |
| `bot_custom_commands` | `TelegramCustomCommands` | 自定义命令应答表（见下）。 |

### 自定义命令

Web 后台的入口是「Telegram 管理」（`/admin/telegram`）。其中的 Bot 指令区域从后端 `GET /admin/telegram/commands/catalog` 读取内置指令目录和开关状态，再编辑 `Telegram.bot_custom_commands` 与 `Telegram.disabled_commands`，不会创建第二套 Bot 指令配置。页面的 Bot 测试、群组策略、Bot 文案和 `/twguser` 模板也统一在该入口维护；Bot Token 与 API 地址仍只在完整配置页修改：

页面首屏并发读取配置 schema、后端权威指令目录和花名册摘要；不会向浏览器发送 Bot Token 或 API 地址。内置/自定义指令列表使用受限 Firefox 滚动区域，避免大量指令撑大移动端页面。占位符按钮会写入最后聚焦的模板/纯文本回复框对应的 React 状态，即使点击按钮后原文本框失焦也不会丢失目标；不直接修改 DOM。Bot 测试由手动请求触发，不使用浏览器轮询或 SSE，失败时只显示通用文案。

- 纯文本类型保存为普通回复，运行时只经过 `telegramRenderText` 的基础占位符替换，例如 `{server_name}`、`{bot_username}`、`{user_name}`。
- 自定义 JS 类型从「开发者模式」保存的 JS 预设中选择，推荐保存为 `js:preset:<id>` 动态引用格式。
- 开发者模式支持新建空白 JS 预设、命名、保存、更新和删除；非空脚本保存前必须通过与沙箱预览一致的安全校验。预检页可模拟真实 `/command`、参数和私聊/群聊上下文，并显示本地静态诊断、后端运行指标、风险 token、耗时、输出和日志。
- Bot 执行时仍只读取 `bot_custom_commands`。使用 `js:preset:<id>` 时会在执行时按预设 ID 读取最新代码；旧格式 `js:<code>` 属于静态代码快照，只有重新保存指令才会改变。
- 内置指令的说明、用法、分类、管理员权限和是否可禁用均由后端注册表生成，前端不再硬编码内置指令清单。
- `disabled_commands` 只保存不带 `/` 的规范化命令名。内置指令被禁用后，Bot 会明确提示该指令已停用，并且不会继续落到同名自定义指令，避免绕过管理员开关。

`bot_custom_commands` 允许配置一组"命令 → 固定回复"的映射，命中后直接返回对应文本（`telegramCustomCommandReply`）。每条形如 `命令 = 回复`，命令会被规范化：转小写、补 `/` 前缀、仅允许字母数字与下划线、长度不超过 32 字符（`normalizeTelegramCommand`），重复命令以首次出现为准。自定义命令在内置命令之后匹配，不会覆盖内置命令。

开发者模式启用后，可把某条回复写成 `js:` 前缀脚本，让 Bot 在受控 Goja（`github.com/dop251/goja`）沙箱中执行。脚本同步执行，单次运行 8 秒墙钟超时；独立页面 `/admin/developer/js-docs` 会通过 `GET /admin/developer/js-docs` 拉取完整接口文档，并以类似 Swagger 的方式展示内置对象、命名空间、函数、配置键、环境变量和示例。

```toml
[Telegram]
bot_custom_commands = [
  "/hello = js:reply('Hello ' + (user.username || 'user'))",
]
```

沙箱只暴露以下绑定：

| 绑定 | 说明 |
| ---- | ---- |
| `ctx` | 当前 Telegram 上下文摘要：`private_chat`、`command_time`、`preview`。不向脚本暴露 Telegram ID 或群组 ID。 |
| `args` | 命令参数数组。 |
| `user` | 绑定的 Twilight 用户脱敏摘要：`uid`、`username`、`email`、`email_masked`、`role`、`active`、`has_emby`、`email_verified`、`telegram_bound`、`telegram_id`、`telegram_username`、登录通知开关等。不会注入密码哈希、Token、API Key、BGM Token 明文、Emby 内部 ID 或数据库连接信息。 |
| `constants` | 受控常量：`roles.admin/user/whitelist`、`limits.max_replies/max_logs`。 |
| `users` | 当前 Telegram 绑定用户的受控接口：`current()` / `describe()` 脱敏读取，`hasRole(role)` / `requireActive()` 判断，`setLoginNotify({ telegram?, email? })` 仅修改当前用户登录通知偏好。 |
| `text` | 文本辅助函数：`truncate(value, max)`、`joinLines(values)`、`escape(value)`、`numberLines(values)`。 |
| `arrays` | 数组辅助函数：`first(values)`、`compact(values)`、`unique(values)`、`take(values, count)`。 |
| `time` | 时间辅助函数：`now()`、`formatUnix(ts)`。 |
| `interactions` | Telegram 交互辅助函数：`inline(text, actions)` 发送静态 inline keyboard，`waitText(options)` 等待同一用户在限定时间内发送下一条普通文本。 |
| `reply(text)` | 追加一段回复文本，最多 4 段，最终用换行合并发送。 |
| `exit(text?)` | 正常提前结束脚本；传入文本时会先追加一段回复。 |
| `assert(condition, text?)` | 条件为真时继续，条件为假时追加提示并正常退出。 |
| `log(text)` | 写入本次执行的审计详情，最多 8 条。 |
| `auth(role)` | 角色鉴权辅助函数。`admin` 仅管理员；`whitelist` 包含管理员和白名单；`user` 包含所有有效角色。 |
| `config(key)` | 只读白名单系统配置读取。只返回非敏感键，未允许或敏感键返回空字符串并写入沙箱日志。 |
| `env(key)` | 只读白名单环境变量读取。只返回非敏感 `TWILIGHT_*` 键，未允许或敏感键返回空字符串并写入沙箱日志。 |

安全约束：

- `fetch` 是受限同步能力，仅支持公开 `http/https` 的 `GET` / `POST` / `HEAD`；会阻断 localhost、内网、链路本地目标、跳转和凭据。仍不提供 `require`、文件系统或进程能力；配置与环境变量只能通过白名单函数读取非敏感值。
- Token、Secret、密码、API Key、数据库 URL、服务器线路等敏感信息不会注入沙箱，也不会通过 `config` / `env` 返回。
- 后端会静态拒绝危险 token，并用 8 秒墙钟超时中断长循环或卡死脚本。
- `getUser(uid)` / `users.get(uid)` / `users.byUID(uid)` 只支持按精确 UID 读取脱敏快照：普通用户只能读取自己，读取其他用户必须当前 Telegram 绑定用户为管理员；不会返回密码、Token、API Key、BGM Token 明文、Emby 内部 ID 或数据库连接信息。
- `users.search(query, limit)` / `users.list(options)` / `admin.*` 支持管理员受控搜索、列表和单用户写操作；普通用户只能读取自己。开发者模式预览中 `ctx.preview=true`，写操作只返回 `dry_run=true`，不会写入用户数据。
- `regcodes.generate` / `invites.generate` / `announcements.create`（及对应的 `admin.generateRegcode` / `admin.generateInviteCode` / `admin.createAnnouncement`）只允许管理员调用，预览模式只返回 `dry_run=true` 不写入；成功写入会分别记录 `telegram_js_regcode_generate`、`telegram_js_invite_generate`、`telegram_js_announcement_create` 审计日志，来源标记 `telegram_js`。邀请码生成受 `invite_enabled` 功能开关约束。
- 每次执行都会写入 `telegram_js_command_execute` 审计日志；开发者页面的沙箱预检写入 `developer_js_sandbox_preview`。
- Bot 实际执行 `users.setLoginNotify` 成功写入时，会额外记录 `telegram_js_user_notify_update`。
- `interactions.inline` 的 callback 只接受创建该消息的同一 Telegram 用户、同一 chat、同一 message，默认 2 分钟过期；callback 动作只能使用预定义 `answer` / `edit` / `reply` 静态文本，不会再次执行 JS。
- `interactions.waitText` 只消费同一 chat、同一 Telegram 用户的下一条非 `/` 命令文本；等待窗口限制为 1-60 秒，回复内容会截断、脱敏，并在消费后写入 `telegram_js_interaction_wait_text` 审计日志。
- 纯文本自定义命令保持原行为；只有 `js:` 前缀会启用脚本执行，避免破坏历史配置。

#### JS API 参数速查

完整、实时的函数参数、返回结构和示例以后台独立页面 `/admin/developer/js-docs` 为准；该页面由 `GET /admin/developer/js-docs` 返回结构化文档，管理员鉴权后可查看。下表用于离线阅读时快速定位常用函数：

| 函数 | 参数 | 返回 | 说明 |
| ---- | ---- | ---- | ---- |
| `reply(text)` | `text: string` | `void` | 追加一段回复，最多 4 段，发送前会截断和脱敏。 |
| `exit(text?)` | `text?: string` | `never` | 正常提前结束脚本；可选追加一段回复，不会按运行错误处理。 |
| `assert(condition, text?)` | `condition: any`, `text?: string` | `boolean\|never` | 条件为假时追加提示并调用 `exit()`，适合参数和权限前置校验。 |
| `log(text)` | `text: string` | `void` | 追加本次执行日志，最多 8 条，不要写入敏感信息。 |
| `auth(role)` | `role: string\|number` | `boolean` | 检查当前绑定用户角色，支持 `admin`、`whitelist`、`user` 或数字角色。 |
| `authAdmin()` | 无 | `boolean` | 判断当前绑定用户是否管理员。 |
| `getUser(uid)` | `uid: number\|string` | `UserSnapshot\|null` | 按精确 UID 读取脱敏用户快照；跨用户读取需要管理员。 |
| `config(key)` | `key: string` | `string\|number\|boolean` | 读取白名单内非敏感配置。 |
| `env(key)` | `key: string` | `string` | 读取白名单内非敏感 `TWILIGHT_*` 环境变量。 |
| `fetch(url, options)` | `url: string`, `options.method?: GET\|POST\|HEAD` | `{ ok, status, text, error, blocked }` | 受限同步请求；阻断本机、内网、跳转和凭据。 |
| `setTimeout(fn, ms)` / `setInterval(fn, ms)` | `fn: function`, `ms?: number` | `number` | 兼容包装器；回调会在同一次执行中同步运行，不创建异步任务。 |
| `input.arg(index, fallback)` | `index: number`, `fallback?: string` | `string` | 读取指定位置参数。 |
| `input.has(index)` | `index: number` | `boolean` | 判断指定位置参数是否存在且非空。 |
| `input.flag(name)` | `name: string` | `boolean` | 判断是否存在 `--name` 或 `-name`。 |
| `input.named(name, fallback)` | `name: string`, `fallback?: string` | `string` | 读取 `--name=value`、`--name value`、`-name=value` 或 `-name value`。 |
| `db.schema()` | 无 | `object` | 返回受控集合结构和允许字段，不暴露原始 state。 |
| `db.collections()` | 无 | `string[]` | 返回受控集合名。 |
| `db.count(name)` | `name: string` | `number` | 返回允许的集合计数；无权限返回 `-1`。 |
| `db.currentUser()` / `users.current()` | 无 | `UserSnapshot` | 返回当前绑定用户脱敏快照。 |
| `db.getUser(uid)` / `users.get(uid)` / `users.byUID(uid)` | `uid: number\|string` | `UserSnapshot\|null` | 按精确 UID 读取脱敏快照。 |
| `db.findUsers(query, limit)` / `users.search(query, limit)` / `users.find(query, limit)` | `query: string`, `limit?: number` | `UserSnapshot[]` | 管理员搜索，最多 50 条；`find` 为 `search` 简化别名。 |
| `users.exists(uid)` | `uid: number\|string` | `boolean` | 该 UID 是否存在；跨用户查询需管理员，否则 `false`。 |
| `db.listUsers(options)` / `users.list(options)` | `limit?`, `offset?`, `role?`, `active?` | `UserSnapshot[]` | 管理员可分页/筛选；普通用户仅返回自己。 |
| `db.listRegcodes(options)` | `limit?`, `offset?` | `RegCodeSnapshot[]` | 管理员专用，脱敏注册码快照，不含用户密钥。 |
| `db.listInviteCodes(options)` | `limit?`, `offset?` | `InviteCodeSnapshot[]` | 管理员看全部；普通用户只看自己拥有的邀请码。 |
| `db.listMediaRequests(options)` | `limit?`, `offset?` | `MediaRequestSnapshot[]` | 管理员看全部；普通用户只看自己的求片记录。 |
| `db.listAnnouncements(options)` | `limit?`, `offset?` | `AnnouncementSnapshot[]` | 可见公告快照，不含正文。 |
| `db.listTickets(options)` | `limit?`, `offset?` | `TicketSnapshot[]` | 管理员看全部；普通用户只看自己的工单，不含正文。 |
| `db.listPresets(options)` | `limit?`, `offset?` | `PresetSnapshot[]` | 管理员专用，开发者 JS 预设元数据，仅含 `code_length`。 |
| `db.updateCurrentUser(patch)` | `notify_on_login_telegram?`, `notify_on_login_email?`, `telegram?`, `email?` | `{ ok, dry_run?, user?, error? }` | 只修改当前用户登录通知偏好。 |
| `users.setLoginNotify(options)` | `telegram?: boolean`, `email?: boolean` | `{ ok, dry_run?, user?, error? }` | `db.updateCurrentUser` 的便捷形式。 |
| `users.setActive(uid, active)` / `admin.setActive(uid, active)` | `uid`, `active: boolean` | `{ ok, dry_run?, user?, error? }` | 管理员启停 Web 账号，带最后管理员保护。 |
| `users.enable(uid)` / `users.disable(uid)` | `uid` | `{ ok, dry_run?, user?, error? }` | `setActive(uid, true/false)` 的简化别名。 |
| `users.setRole(uid, role)` / `admin.setRole(uid, role)` | `uid`, `role: number` | `{ ok, dry_run?, user?, error? }` | 管理员修改角色。 |
| `users.setExpiry(uid, expiredAt)` / `admin.setExpiry(uid, expiredAt)` | `uid`, `expiredAt: number` | `{ ok, dry_run?, user?, error? }` | 管理员修改到期时间，`-1` 表示永久。 |
| `users.extend(uid, days)` | `uid`, `days: number` | `{ ok, dry_run?, uid?, expired_at?, note?, error? }` | 在当前到期（或现在，取较晚者）上顺延 `days` 天；永久用户原样返回。 |
| `users.update(uid, patch)` / `admin.updateUser(uid, patch)` | `uid`, `patch` | `{ ok, dry_run?, user?, error? }` | 管理员组合更新受控字段。 |
| `admin.ok()` / `admin.ensure()` | 无 | `boolean` | 管理员快捷判断；`ensure()` 会在失败时写沙箱日志。 |
| `admin.searchUsers(query, limit)` / `admin.listUsers(options)` | 同 `users.search/list` | `UserSnapshot[]` | 管理员快捷读接口。 |
| `regcodes.list(options)` | `limit?`, `offset?` | `RegCodeSnapshot[]` | 管理员专用，等价 `db.listRegcodes`。 |
| `regcodes.get(code)` | `code: string` | `RegCodeSnapshot\|null` | 管理员按精确码值查询脱敏快照。 |
| `regcodes.generate(options)` / `admin.generateRegcode(options)` | `count?`, `type?`, `days?`, `use_count_limit?`, `validity_time?`, `note?`, `target_username?`, `decoy?` | `{ ok, dry_run?, codes?, count?, type?, days?, error? }` | 管理员批量生成注册/续期/白名单码，写审计日志。 |
| `regcodes.quick(days?, count?, type?)` | `days?`, `count?`, `type?` | 同 `regcodes.generate` | `generate` 的位置参数简化别名。 |
| `invites.list(options)` | `limit?`, `offset?` | `InviteCodeSnapshot[]` | 管理员看全部；普通用户看自己的邀请码。 |
| `invites.generate(options)` / `admin.generateInviteCode(options)` | `days?`, `expires_at?`, `note?`, `target_username?` | `{ ok, dry_run?, code?, invite?, days?, error? }` | 管理员生成邀请码（需 `invite_enabled`），写审计日志。 |
| `invites.quick(days?)` | `days?` | 同 `invites.generate` | `generate` 的位置参数简化别名（需 `invite_enabled`）。 |
| `announcements.list(options)` | `limit?`, `offset?` | `AnnouncementSnapshot[]` | 可见公告快照，不含正文。 |
| `announcements.create(options)` / `admin.createAnnouncement(options)` | `title?`, `content?`, `level?`, `render_mode?`, `visible?`, `pinned?`, `expires_at?` | `{ ok, dry_run?, announcement?, error? }` | 管理员创建公告，写审计日志。 |
| `announcements.post(title, content, level?)` | `title`, `content`, `level?` | 同 `announcements.create` | `create` 的位置参数简化别名。 |
| `admin.stats()` / `system.stats()` | 无 | `object` | 返回安全聚合统计；管理员可看到更多计数。 |
| `system.info()` | 无 | `object` | 返回安全系统元信息、功能开关和限制。 |
| `system.feature(key)` | `key: string` | `boolean` | 读取一个安全功能开关。 |
| `text.truncate(value, max)` | `value`, `max?: number` | `string` | 按字符数截断。 |
| `text.joinLines(values)` | `values: any[]` | `string` | 数组转多行文本。 |
| `text.escape(value)` | `value` | `string` | 转义基础 HTML 敏感字符。 |
| `text.numberLines(values)` | `values: any[]` | `string` | 数组转编号列表。 |
| `text.trim/lower/upper(value)` | `value` | `string` | 常用字符串整理。 |
| `text.contains(value, needle)` | `value`, `needle` | `boolean` | 大小写不敏感包含判断。 |
| `text.split(value, separator)` | `value`, `separator?: string` | `string[]` | 拆分字符串。 |
| `text.maskEmail(email)` | `email: string` | `string` | 按后端规则脱敏邮箱。 |
| `text.template(template, data)` | `template: string`, `data: object` | `string` | 替换 `{key}` 占位符。 |
| `arrays.first/last(values)` | `values: any[]` | `any\|undefined` | 取首项或末项。 |
| `arrays.compact(values)` | `values: any[]` | `any[]` | 移除 `null` 和空字符串。 |
| `arrays.unique(values)` | `values: any[]` | `string[]` | 字符串化后去重。 |
| `arrays.take(values, count)` | `values`, `count` | `any[]` | 截取前 N 项。 |
| `arrays.join(values, separator)` | `values`, `separator?: string` | `string` | 数组转字符串。 |
| `arrays.includes(values, value)` | `values`, `value` | `boolean` | 精确字符串包含判断。 |
| `arrays.sortStrings(values)` | `values` | `string[]` | 返回排序后的字符串数组副本。 |
| `time.now()` | 无 | `number` | 当前 Unix 秒。 |
| `time.formatUnix(ts)` | `ts: number` | `string` | Unix 秒转 UTC RFC3339。 |
| `time.fromNow(seconds)` | `seconds: number` | `number` | 当前时间偏移秒数。 |
| `time.addDays(ts, days)` | `ts`, `days` | `number` | 给时间戳增加天数，`ts<=0` 时以当前时间为基准。 |
| `time.duration(seconds)` / `format.duration(seconds)` | `seconds` | `string` | 格式化时长。 |
| `format.bool(value, yes, no)` | `value`, `yes?`, `no?` | `string` | 布尔值转文本。 |
| `format.role(role)` | `role: number` | `string` | 角色 ID 转角色名。 |
| `format.date(ts)` | `ts: number` | `string` | 时间戳转日期文本。 |
| `format.expiry(expiredAt)` | `expiredAt: number` | `string` | 用户到期时间转状态文本。 |
| `format.user(user)` | `UserSnapshot` | `string` | 用户快照转一行摘要。 |
| `format.json(value)` | `value` | `string` | 返回受限长度字符串；结构化 JSON 优先用 `JSON.stringify`。 |
| `interactions.inline(text, actions)` | `text`, `actions[]` | `{ ok, dry_run?, message_id?, actions?, error? }` | 发送静态 inline keyboard。 |
| `interactions.waitText(options)` | `seconds?`, `prompt?`, `reply_prefix?`, `timeout_reply?`, `max_chars?`, `numbered?` | `{ ok, dry_run?, seconds?, error? }` | 等待同一用户下一条普通文本。 |

`UserSnapshot` 主要字段：`uid`、`username`、`email`、`email_masked`、`has_email`、`role`、`role_name`、`active`、`expired_at`、`expire_status`、`created_at`、`register_time`、`has_emby`、`emby_username`、`emby_disabled`、`avatar`、`background`、`bgm_mode`、`bgm_token_set`、`email_verified`、`email_verified_at`、`telegram_bound`、`telegram_id`、`telegram_username`、`notify_on_login_telegram`、`notify_on_login_email`、`legacy_api_key_enabled`、`rebinding_in_progress`、`rebinding_since`。不会包含密码、Token、API Key、BGM Token 明文、Emby 内部 ID、原始数据库状态或数据库连接信息。

常用示例：

```js
// 查看用户输入指令时可读取的全部非敏感上下文。
// 不提供 chat ID、message ID、群组 ID、Emby 内部 ID、Token、API Key 或密码。
const me = users.current();
const lines = [
  "private_chat=" + ctx.private_chat,
  "preview=" + ctx.preview,
  "command_time=" + time.formatUnix(ctx.command_time),
  "args=" + JSON.stringify(args),
  "uid=" + me.uid,
  "username=" + (me.username || "unbound"),
  "role=" + me.role,
  "active=" + me.active,
  "has_emby=" + me.has_emby,
  "email_verified=" + me.email_verified,
  "telegram_bound=" + me.telegram_bound,
  "notify_tg=" + me.notify_on_login_telegram,
  "notify_email=" + me.notify_on_login_email
];
reply(text.truncate(text.joinLines(lines), 1200));
```

```js
// 使用 assert/exit 做参数守卫和正常提前退出。
assert(input.has(0), "Usage: /lookup <uid>");
const uid = Number(input.arg(0));
if (!uid) {
  exit("UID must be a number");
}
const target = getUser(uid);
if (!target) {
  exit("User not found or permission denied");
}
reply(format.user(target));
```

```js
// 查看当前绑定用户摘要（可读取邮箱/Telegram 脱敏联系信息；不会返回 Emby 内部 ID、Token、API Key 或密码）
const me = users.current();
reply("User: " + (me.username || "unbound") + "\nActive: " + me.active);
```

```js
// 管理员按精确 UID 查看脱敏用户摘要；非管理员不能跨用户读取
if (!auth("admin")) {
  reply("Admin only");
  return;
}
const target = getUser(Number(args[0] || 0));
if (!target) {
  reply("User not found or permission denied");
  return;
}
reply([
  "UID: " + target.uid,
  "Username: " + target.username,
  "Active: " + target.active,
  "Has Emby: " + target.has_emby,
  "Email verified: " + target.email_verified
].join("\n"));
```

```js
// 开启当前绑定用户的 Telegram 登录通知；预览模式只 dry-run
const result = users.setLoginNotify({ telegram: true });
reply(result.dry_run ? "Preview only" : "Telegram login notifications enabled");
```

```js
// 清理参数并输出
const values = arrays.unique(arrays.compact(args));
reply(text.truncate(text.joinLines(values), 120));
```

```js
// 发送静态 inline 操作。点击后只执行预设 answer/edit/reply，不会再次运行 JS。
interactions.inline("Choose an action", [
  { text: "Status", answer: "OK", edit: "Status acknowledged" },
  { text: "Help", reply: "Use /help for commands" }
]);
```

```js
// 等待同一用户 30 秒内发送下一条普通文本，并以编号形式回复前 120 个字符
interactions.waitText({
  seconds: 30,
  prompt: "Send one line in 30 seconds",
  reply_prefix: "Received:",
  max_chars: 120,
  numbered: true
});
```

更多示例：

```js
// 回显参数、flag 和命名选项。示例命令：/tool ping --uid 10001 --force
const lines = [
  "command=" + input.command,
  "first=" + input.arg(0, "none"),
  "has_second=" + input.has(1),
  "force=" + input.flag("force"),
  "uid=" + input.named("uid", "missing"),
  "text=" + input.text
];
reply(text.joinLines(lines));
```

```js
// 使用模板生成当前用户摘要。
reply(text.template("Hi {name}\nUID: {uid}\nEmail: {email}\nRole: {role}\nExpiry: {expiry}", {
  name: user.username || "unbound",
  uid: user.uid,
  email: user.email_masked || "none",
  role: user.role_name,
  expiry: user.expire_status
}));
```

```js
// 查询当前用户邮箱和登录通知状态。
const me = users.current();
reply([
  "email=" + (me.email_masked || "none"),
  "verified=" + format.bool(me.email_verified, "yes", "no"),
  "notify_email=" + format.bool(me.notify_on_login_email, "on", "off"),
  "notify_tg=" + format.bool(me.notify_on_login_telegram, "on", "off")
].join("\n"));
```

```js
// 用 on/off 参数开关当前用户登录通知。
const enable = input.flag("on") || text.lower(input.first) === "on";
const disable = input.flag("off") || text.lower(input.first) === "off";
if (!enable && !disable) {
  reply("Usage: /notify on|off");
  return;
}
const result = users.setLoginNotify({ telegram: enable, email: enable });
reply(result.dry_run ? "Preview only" : ("Notifications " + (enable ? "enabled" : "disabled")));
```

```js
// 管理员按关键词搜索用户，输出前 5 条摘要。
if (!admin.ensure()) return;
const query = input.named("q", input.text);
const rows = admin.searchUsers(query, 5);
if (!rows.length) {
  reply("No users matched: " + query);
  return;
}
reply(text.numberLines(rows.map(function(u) {
  return format.user(u);
})));
```

```js
// 管理员分页列出启用中的普通用户。
if (!admin.ensure()) return;
const rows = admin.listUsers({
  limit: 10,
  offset: Number(input.named("offset", 0)),
  role: roles.user,
  active: true
});
reply(rows.length ? text.numberLines(rows.map(format.user)) : "No users");
```

```js
// 管理员按天数设置用户到期时间。示例：/setexp --uid 10001 --days 30
if (!admin.ensure()) return;
const uid = Number(input.named("uid", 0));
const days = Number(input.named("days", 7));
if (!uid || days < 1 || days > 3650) {
  reply("Usage: /setexp --uid 10001 --days 30");
  return;
}
const result = admin.setExpiry(uid, time.addDays(time.now(), days));
reply(result.ok ? ("New expiry: " + format.expiry(result.user.expired_at)) : ("Failed: " + result.error));
```

```js
// 管理员禁用用户前要求显式 --confirm，避免误触。
if (!admin.ensure()) return;
const uid = Number(input.named("uid", 0));
if (!uid) {
  reply("Usage: /disable --uid 10001 --confirm");
  return;
}
if (!input.flag("confirm")) {
  const target = users.get(uid);
  reply("Preview: would disable " + (target ? format.user(target) : ("#" + uid)) + "\nAdd --confirm to execute.");
  return;
}
const result = admin.setActive(uid, false);
reply(result.ok ? ("Disabled #" + uid) : ("Failed: " + result.error));
```

```js
// 受限 fetch 读取公开 JSON。注意：内网、localhost、跳转和凭据都会被阻断。
const res = fetch("https://example.com/status.json");
if (!res.ok) {
  reply("fetch failed: " + (res.error || res.status));
  return;
}
try {
  const data = JSON.parse(res.text);
  reply("status=" + (data.status || "unknown"));
} catch (e) {
  reply("invalid json: " + text.truncate(res.text, 120));
}
```

```js
// 复杂示例：搜索用户、统计子集并输出有界摘要。
if (!admin.ensure()) return;
const query = input.named("q", input.text);
const rows = admin.searchUsers(query, 20);
const active = rows.filter(function(u) { return u.active; }).length;
const withEmail = rows.filter(function(u) { return u.has_email; }).length;
const preview = arrays.take(rows.map(function(u) {
  return "#" + u.uid + " " + (u.username || "unknown") + " " + format.bool(u.active, "on", "off") + " " + (u.email_masked || "no-email");
}), 10);
reply(text.truncate(text.joinLines([
  "query=" + query,
  "matched=" + rows.length,
  "active=" + active,
  "email_bound=" + withEmail,
  "---",
  text.numberLines(preview)
]), 1200));
```

### 文案占位符

自定义文案支持以下占位符（`telegramRenderText`）：

| 占位符 | 当前替换值 |
| ------ | ---- |
| `{server_name}` | 应用名称（`AppName`）。 |
| `{bot_username}` | 当前替换为空字符串。 |
| `{user_name}` | 当前替换为空字符串。 |

Go Bot 使用纯文本发送消息，不依赖 Markdown 转义。

## 相关配置与扩展

群成员巡检（调度任务 `enforce_group_membership`）的安全约束：

- 只有明确的用户层级错误（`user not found`、`PARTICIPANT_ID_INVALID` 等）或成员状态为 `left` / `kicked` 才算不在群。`chat not found`、Bot 被踢出群、没有权限等群组层级错误会让整轮中止并标记为失败，不停用任何人。
- 熔断：单轮拟停用人数超过扫描人数的 `Telegram.membership_breaker_percent`（默认 20%，且至少 3 人），或超过 `Telegram.membership_breaker_max`（默认 50 人）时整轮中止，不停用任何人；摘要里 `circuit_breaker_tripped=true` 并列出 `would_disable_uids`。两项都可设 0 关闭，也可在任务参数里用 `breaker_percent` / `breaker_max` 临时覆盖。
- 支持 `dry_run`：只回报会停用的名单（`would_disable_uids`），不做任何写入。
- 巡检停用会在用户上记 `disabled_reason = "telegram_membership"`；回群自动启用（`auto_enable_rejoined`）和人工复核名单都只处理带这个原因的账号，管理员手动停权的账号不会被放出来。任何其他路径改动启用状态都会清掉这个原因。升级前已被巡检停用的旧账号没有该标记，需要管理员手动启用。
- 回群自动启用时，若 Emby 当初是随 Web 一起被系统停用（`emby_auto_disabled`），会一并重新启用 Emby；管理员单独封禁的 Emby 保持不动。启用失败由「Emby 状态对账」任务收敛。
- 退群停用写系统稽核 `disable_telegram_users_on_leave`，回群自动启用写 `enable_telegram_users_on_rejoin`，都附 uid 清单。

强制加群 / 订阅、退群封禁（`ban_on_leave`）、重新入群自动恢复（`auto_enable_rejoined`）、群成员校验并发度等行为属于 Bot 运行策略而非命令，配置字段集中在 `[Telegram]` 段，详见 [Go 后端架构与配置](../reference/backend.md)。其它功能文档参见 [文档导航](../README.md)。
### 开发者 JS 指令补充

- 开发者模式由仪表盘输入 `DEBUGMODE` 后二次验证管理员密码开启；再次输入 `DEBUGMODE` 并验证会关闭。关闭后服务端会阻断所有 `js:` / `js:preset:<id>` 指令、inline callback 和 waitText 等 JS 交互，但不会删除 JS 预设或 Telegram 指令配置。纯文本自定义命令继续可用。
- 推荐在 Telegram 管理的 Bot 指令中保存 `js:preset:<id>`。该格式动态引用开发者模式中保存的预设，预设更新后已绑定指令会读取最新代码；旧格式 `js:<code>` 仍兼容，但属于静态代码快照。
- JS 运行时会自动注入 `ctx`、`command`、`args`、`user`、`constants`、`users`、`db`、`text`、`arrays`、`time`、`interactions`、`reply()`、`exit()`、`assert()`、`log()`、`auth()`、`authAdmin()`、`getUser()`、`config()`、`env()`、`fetch()`、`setTimeout()`、`setInterval()`。
- `db.*` 是受控数据库接口，提供 `schema()`、`collections()`、`count(name)`、`currentUser()`、`getUser(uid)`、`findUsers(query, limit)`、`listUsers(options)`、`updateCurrentUser(patch)`、`updateUser(uid, patch)`。用户快照可包含邮箱、Telegram 用户名/ID 等联系信息；但不暴露原始 state、SQL、密码、Token、API Key、BGM Token 明文、Emby 内部 ID 或数据库连接信息。跨用户搜索和写入仅限管理员，写操作会写入审计日志；预览模式为 dry-run。
- `fetch()` 为受限同步能力，仅支持 `http/https` 的 `GET` / `POST` / `HEAD`，阻断 localhost、内网、链路本地目标，禁用跳转和凭据，响应体限长并脱敏。`eval`、`Function`、`globalThis`、`fetch`、`setTimeout`、`setInterval` 会被标记为高风险但不再静态拒绝；`require`、`process`、浏览器对象、本地存储、cookie、`constructor.constructor` 等仍会被阻断。

## Developer JS expanded user APIs

JS runtime now exposes a richer but still controlled user and system API surface:

- `user` / `users.current()` / `getUser(uid)` include user profile and contact metadata: `email`, `email_masked`, `has_email`, `telegram_id`, `telegram_username`, `emby_username`, `role_name`, `expire_status`, `avatar`, `background`, `bgm_mode`, `bgm_token_set`, `pending_emby`, `pending_emby_days`, `legacy_api_key_enabled`, login notification flags, and rebinding state.
- Sensitive implementation fields are still never injected: password hashes, raw tokens, API key hashes or full keys, BGM token values, Emby internal IDs, database URLs, and config secrets.
- Admin-only read helpers:
  - `users.search(query, limit)` / `db.findUsers(query, limit)` search by UID, username, email, Telegram username/ID, or Emby username. Results are capped at 50.
  - `users.list(options)` / `db.listUsers(options)` list sanitized users. Non-admin users only receive themselves; admins may pass `limit`, `offset`, `role`, and `active`.
- Admin-only write helpers:
  - `users.setActive(uid, active)`
  - `users.setRole(uid, role)`
  - `users.setExpiry(uid, expiredAt)`
  - `users.update(uid, patch)` / `db.updateUser(uid, patch)`
  - Accepted combined patch fields are `active`, `role`, `expired_at`, `notify_on_login_telegram`, `notify_on_login_email`, and the `telegram` / `email` aliases. Runtime writes audit logs and enforces last-admin protection.
- `system.info()` returns safe site metadata, feature flags, and limits. `system.feature(key)` reads one safe boolean feature flag. Neither helper returns raw secret config values.
- Convenience aliases and helpers:
  - `me` is an alias of `user`; `roles` is an alias of `constants.roles`.
  - `input` exposes `text`, `first`, `rest`, `count`, plus `arg(index, fallback)`, `has(index)`, `flag(name)`, and `named(name, fallback)` for commands such as `/lookup --q alice --force`.
  - `admin.*` provides shorter admin helpers: `ok()`, `ensure()`, `searchUsers()`, `listUsers()`, `updateUser()`, `setActive()`, `setRole()`, `setExpiry()`, and `stats()`.
  - `format.*` provides common output helpers: `bool()`, `role()`, `date()`, `expiry()`, `duration()`, `user()`, and `json()`.
  - `text.*`, `arrays.*`, and `time.*` include extra helpers for trimming/case conversion/templates, joining/sorting arrays, and calculating timestamps.
- Scripts execute inside a function scope, so a top-level `return` can be used to stop a command early.

## Telegram 绑定同步与 `/twguser` 搜索

### 绑定状态

Telegram 绑定关系以 PostgreSQL 中的用户记录为准，`TelegramID` 是唯一身份键，`TelegramUsername` 只是从 Telegram update 被动刷新的人类可读字段。`api`、`bot`、`scheduler` 分进程运行时，Bot 会在每个 update 批次开始前按版本刷新用户快照和 Telegram ID 索引；刷新失败会暂停该批处理并保留 offset，避免使用过期的管理员权限或绑定状态。

### 统一状态读取

跨进程状态刷新不限于绑定：所有已匹配的 HTTP 请求会在鉴权前刷新一次 Store，定时任务会在每次执行前刷新，Telegram 则在每个非空更新批次开始时刷新。版本未变化时 PostgreSQL 只返回版本号，不会传输或反序列化整份业务 JSONB；任一边界刷新失败都会停止当前请求、任务或 update 批次，避免用旧的用户、邀请码、工单、邀请关系或权限状态继续读取、鉴权和操作。

Web 解绑、管理员解绑和删除账号会同时撤销该用户的短期 Telegram 绑定票据，避免 Bot、Web 和历史绑定码在短时间内显示不同状态。Telegram 用户删除用户名时不会用空值覆盖历史用户名，但重新绑定或管理员修改绑定会以新的 Telegram ID 为准。

### `/twguser`

该命令仅允许已鉴权的 Telegram 管理员使用。可直接模糊搜索 UID、Web 用户名、Telegram ID 和 Telegram 用户名：

```text
/twguser 2345
/twguser testuser
```

也可以限定搜索字段，避免同名或数字产生歧义：

```text
/twguser 2345 uid
/twguser testuser username
/twguser testuser tgid
/twguser testuser tgname
```

匹配到多个用户时，Bot 返回候选按钮。每次点击候选或面板操作都会重新验证操作者的管理员身份，并校验候选 UID 仍属于当前搜索面板；候选过期、账号删除或权限撤销后不会继续执行操作。回复群成员消息时仍可省略关键词，Bot 会按回复消息中的 Telegram ID 查找绑定账号。
