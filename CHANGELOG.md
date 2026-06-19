# Changelog

本文件记录 hexclaw 的用户可见变更，遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。

## [Unreleased]
### Fixed
- matrix 适配器 Stop 幂等（消除二次调用 close(closed channel) panic）。
- knowledge 时间衰减：零值 CreatedAt 不再被衰减清零（修复无时间戳 chunk 永不召回）。
- cron：多副本 job 双跑防护（DB 原子领取 + fencing），fail-open 保纯内存行为。

### Changed
- 媒体/genstore/ssrf/cache/trace/events 迁移到 ai-core/toolkit/hexagon；gateway HMAC 改用 toolkit/crypto/sign。

## [基线]
- 与 ai-core v0.1.3 / hexagon v0.4.8 / toolkit v0.0.6 对齐。
