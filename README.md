# napcat-jm-go

一个面向 NapCat/OneBot 事件的 Go 机器人，提供：
- `JM` 本子下载、检索、发送（PDF/ZIP）
- 识图（soutubot）联动检索并回填 `JM` 号
- 文件加密、随机密码、命名策略、去重与队列控制
- 内置/外置 Cloudflare bypass 对接
- 一键 `systemd` 安装与卸载

## 1. 项目结构

```text
.
├── cmd/napcat-jm-go/        # 标准 Go 入口
├── internal/app/            # 核心实现（HTTP事件、命令、下载、识图、发送）
├── configs/
│   ├── config.example.yml   # 示例配置
│   └── option.yml           # jmcomic 配置
├── docs/
│   └── README_GO.md         # 历史说明（保留）
├── bin/                     # 构建产物
├── config.yml               # 实际运行配置（本地）
├── go.mod / go.sum
└── main.go                  # 兼容入口（支持 `go run .`）
```

说明：
- 推荐入口：`go run ./cmd/napcat-jm-go` 或 `bin/napcat-jm-go`
- 兼容入口：`go run .`（仍可用）

## 2. 核心能力

### 2.1 命令能力
- `/jm <ID>`：下载并发送
- `/jm look <ID>`：查看本子信息
- `/jm search <关键词>`：检索最佳匹配，回复“确认”后下载
- `/jm search` / `识图` / `/jm识图`：进入识图窗口（默认 120 秒）
- `/jm goodluck`：随机本子
- `/jm mode pdf|zip`：发送格式
- `/jm enc on|off`、`/jm passwd <密码>`、`/jm randpwd on|off`
- `/jm fname jm|full`：文件命名
- `/jm regex on|off`：正则提取模式

### 2.2 识图联动
- 识图成功后，会自动提取标题关键词（含中日文片段）并走 `/jm search` 同款检索逻辑
- 命中后自动写入待确认队列，回复“确认”即可下载

### 2.3 发送策略
- 普通消息默认纯文本
- 批量任务通知可按 `reply_as_card` 使用转发卡片（仅批量场景）

## 3. 配置说明

主配置文件：`config.yml`  
示例模板：`configs/config.example.yml`

首次运行若没有 `config.yml`，程序会从示例模板复制生成。

关键配置项（建议优先检查）：
- `http_host` / `http_port`：NapCat 回调监听地址
- `http_port_fallback`：主端口是否自动退避（默认 `false`，建议保持）
- `websocket_url` / `websocket_token`：NapCat WS 发送通道
- `jm_option_path`：默认 `./configs/option.yml`
- `file_dir` / `manga_dir` / `cbz_dir`
- `soutu_*`：识图请求参数
- `cf_bypass_api_url`：外置 bypass 地址（推荐生产使用）
- `embedded_bypass_enabled`：是否启用内置 bypass（需要本机 chrome/chromium）

## 4. 运行方式

### 4.1 开发运行
```bash
go mod tidy
go run .
```

或标准入口：
```bash
go run ./cmd/napcat-jm-go
```

### 4.2 构建运行
```bash
go build -o bin/napcat-jm-go ./cmd/napcat-jm-go
./bin/napcat-jm-go
```

### 4.3 Docker 运行

使用 `Dockerfile`：

```bash
docker build -t napcat-jm-go:latest .
docker run -d --name napcat-jm-go \
  -p 8071:8071 -p 18000:18000 \
  -v $(pwd)/config.yml:/app/config.yml \
  -v $(pwd)/pdf:/app/pdf \
  -v $(pwd)/manga:/app/manga \
  -v $(pwd)/cbz:/app/cbz \
  -v $(pwd)/logs:/app/logs \
  napcat-jm-go:latest
```

查看日志：
```bash
docker logs -f napcat-jm-go
```

停止并删除容器：
```bash
docker rm -f napcat-jm-go
```

### 4.4 Docker Compose（推荐）

项目已提供 `docker-compose.yml`：

```bash
docker compose up -d --build
docker compose logs -f
docker compose down
```

默认挂载：
- `./config.yml -> /app/config.yml`
- `./pdf -> /app/pdf`
- `./manga -> /app/manga`
- `./cbz -> /app/cbz`
- `./logs -> /app/logs`

## 5. systemd 管理

程序内置安装/卸载参数：

```bash
# 安装并启动（需 root）
sudo ./bin/napcat-jm-go --install

# 卸载（停用+删除服务，需 root）
sudo ./bin/napcat-jm-go --uninstall
```

可选参数：
- `--service-name`（默认 `napcat-jm-go`）
- `--service-user`（默认当前登录用户 / `SUDO_USER`）
- `--service-group`（默认用户主组）

## 6. 识图链路说明

识图输入会按以下顺序解析：
1. `image.base64`
2. `image.url`
3. `image.file` -> 调用 OneBot `get_image` 换取 URL
4. CQ / HTML 回退字段提取（兼容场景）

Cloudflare 处理：
- 优先走现有 cookie
- 命中拦截后调用 `cf_bypass_api_url` 轮询获取 `cf_clearance`
- 推荐使用外置 bypass 服务，稳定且无需本机浏览器

## 7. 常见问题

### Q1: 启动报端口占用
报错示例：`bind: address already in use`

处理：
```bash
ss -ltnp | grep ':8071 '
kill <pid>
```
或修改 `config.yml` 中 `http_port`。

### Q2: 识图失败，提示 bypass/chrome 问题
- 若使用内置 bypass：需安装 `chrome/chromium`
- 若不想装浏览器：将 `embedded_bypass_enabled: false`，并配置可用 `cf_bypass_api_url`

### Q3: 回复“确认”未触发下载
- 必须在 `search_timeout` 窗口内回复
- 必须在同一会话范围（当前已按用户级 scope 处理）

### Q4: 发了图但没进入识图
- 先看日志中的 `[soutu-debug]` 行（已内置）
- 重点看是否命中 armed 窗口、是否提取到图片源、`get_image` 是否成功

## 8. 日志与调试

识图相关调试日志前缀：
- `[soutu-debug] recv event`
- `[soutu-debug] armed ...`
- `[soutu-debug] extracted sources ...`
- `[soutu-debug] get_image ...`
- `[soutu-debug] search success ...`

如果需要排障，建议贴出同一时段完整日志片段（含 group/user/scope 行）。

## 9. 安全与运维建议

- `config.yml` 不入库（已在 `.gitignore`）
- `enc_password_*` 建议使用强密码并定期轮换
- 生产建议用 `systemd` + 外置 bypass API
- 主端口建议固定（`http_port_fallback: false`），避免 NapCat 回调漂移
