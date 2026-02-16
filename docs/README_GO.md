# Go 重构版说明

## 当前状态
项目已新增 Go 入口 `main.go`，可直接替代原 Python 服务接收 NapCat 事件。

已迁移到 Go 的能力：
- HTTP 事件接收（`POST /`）
- NapCat WebSocket 消息发送（群聊/私聊文本、文件）
- `/jm` 命令解析与管理命令
- 下载任务队列与去重窗口
- 配置读写（`config.yml`）
- 文件发送前处理（zip、文件名策略、哈希扰动）

## jmcomic 重构策略
`jmcomic` 没有成熟的 Go 等价库。当前 Go 版本采用“库适配重构”：
- 在 Go 内实现 `JMBridge`（下载/搜索/详情统一接口）
- 通过 `python3 -c` 子进程调用 `jmcomic` 完成核心能力
- 主流程、并发控制、命令分发、传输逻辑全部在 Go

这避免业务层继续依赖 Python 主程序，并且后续可以只替换 `JMBridge` 为纯 Go 实现。

## 运行
```bash
go mod tidy
go run .
```

## 兼容说明
- `enc on/off`、`passwd`、`randpwd on/off` 已支持。
- 加密为标准 PDF 密码保护，主流阅读器可直接输入密码打开。
- 新增可选卡片回复：`reply_as_card: true` 时，优先发送转发卡片，失败自动回退普通文本。
