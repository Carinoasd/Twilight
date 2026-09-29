# 通知设置

管理员在后台「通知」配置中按事件选择 Telegram 与邮件。个人设置中的「通知偏好」决定用户是否接收对应通知。未绑定 Telegram 或未验证邮箱时不会向该管道发送新增通知。

## 管理员设置

以下字段位于 `[Notification]`，均可从后台保存，也可使用 `TWILIGHT_NOTIFICATION_` 加大写字段名的环境变量覆盖，例如 `TWILIGHT_NOTIFICATION_TICKET_EMAIL_ENABLED=true`。

| 事件 | Telegram 开关及默认值 | 邮件开关及默认值 |
| --- | --- | --- |
| 登录 | `login_telegram_enabled = true` | `login_email_enabled = true` |
| 到期提醒 | `expiry_telegram_enabled = true` | `expiry_email_enabled = false` |
| 工单更新 | `ticket_telegram_enabled = true` | `ticket_email_enabled = false` |
| 排程失败／恢复 | `scheduler_telegram_enabled = true` | `scheduler_email_enabled = false` |

邮件还需要开启 `[Email].enabled` 并正确配置 SMTP。登录通知仍需用户打开原有登录偏好。到期提醒沿用 `[Notification].enabled` 总开关及 `expiry_remind_days`；该总开关不控制登录、工单和排程。排程通知仍受 `[Scheduler].failure_notify` 控制，保留原有失败／恢复判定与节流。

升级不会自动开启新的邮件通知。旧配置未写新字段时沿用上表默认值。认证邮件（验证码、找回密码等）与这些事件开关相互独立。

## 个人偏好

| API 字段 | 含义 | 旧账号默认 |
| --- | --- | --- |
| `notify_on_expiry_telegram` | 到期 Telegram 提醒 | 开启，保留旧行为 |
| `notify_on_expiry_email` | 到期邮件提醒 | 关闭 |
| `notify_on_ticket_email` | 工单邮件通知 | 关闭 |

原有 `notify_on_login_telegram`、`notify_on_login_email`、`notify_on_ticket_telegram` 继续有效。设置接口只接受 JSON boolean；省略字段表示保留原值。`GET /api/v2/settings` 的 `notification_channels` 按个人偏好字段名返回当前服务端管道是否启用且已配置。个人偏好不会绕过管理员开关；管道关闭期间可以保留偏好，待管道启用后生效。

工单 Telegram 的单工单开关仍只静音该工单的 Telegram；邮件由个人工单邮件开关控制。用户邮件不包含管理员内部备注，只含工单基本信息及本次新增的管理员公开回复。管理员邮件发给已验证邮箱、账号启用且打开工单邮件偏好的管理员，并排除当前管理员操作者、对相同邮箱去重。邮件不附带工单图片。

排程邮件发送给启用中的管理员账号已验证邮箱，不使用普通用户收件地址。Telegram 管理员 ID 配置不会被当作邮箱地址。

## 失败与取消

到期提醒分别统计 `telegram_sent`、`email_sent`、`telegram_skipped`，`sent` 表示至少一个管道成功的用户数。`failed` 中带有 `channel`，部分失败仍会令该轮失败。一条 Telegram 发送失败不阻止该用户邮件尝试；连续五次限流后停止后续 Telegram 投递，但继续邮件，且仍响应取消。

工单两管道分别使用有超时的通知协程，失败写运行日志。沿用现有传送方式，不提供持久化通知队列，不保证进程退出后的补发。不会新增通知自动重试，以免重复寄送。
