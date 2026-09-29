# Bangumi 观看与记录管理计划

署名：Carinoasd

1. 仅确认看完的记录进入自动写入，逐集标记、精确匹配；歧义由用户指定条目和集数。保留原有收藏状态、短评与评分。
2. 用户级持久检查点与日志独立，按 Bangumi 账号分开保存。换 Token、清日志均保留历史；同一账号换 Token 可沿用状态。PostgreSQL 锁协调 API 与排程，每用户每轮最多处理 25 条，按上次尝试时间轮转。
3. 加入用户观看记录分页、状态筛选、手动确认和忽略；每 15 分钟调度已开启同步的用户。
4. 收藏补齐短评、个人标签、私密设置；首页一次摘要读取，列表支持窄屏、取消过期请求、明确保存失败和多语言。
5. 模拟 Bangumi/Emby 回归测试、真实 PostgreSQL 全套测试、前端检查与 Firefox 验证后，推送独立 PR。

## 数据与边界

沿用 PlaybackRecords 与用户 JSONB 存储。Webhook 播放位置达到配置比例（未配置时为 85%）才提供完成证据；旧记录或仅活动时长不推断看完，用户可以明确确认。原有 auto_add_collection、private_collection、block_keywords 和 min_progress_percent 配置接入后端与管理页面。检查点每用户上限 20000 项，达到上限时明确报错，不自动删除旧数据。外部 API 无分布式事务，远端成功而本地写入失败时可能重试幂等点格子。

保留 CORS／CSRF。官方接口依据：https://github.com/bangumi/server/blob/master/openapi/v0.yaml 。
