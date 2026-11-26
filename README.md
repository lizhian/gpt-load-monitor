# GPT-Load 监控面板

只读面板，包含订阅额度与重置倒计时、AccessKey 统计、网关与分组健康，以及整体用量趋势。每 60 秒刷新一次，也可手动刷新。

## 本地启动

1. `cp .env.example .env`
2. 编辑 `.env`，填写 GPT-Load 地址、管理员 Key、自选的共享查看密码和会话密钥。可用 `openssl rand -hex 32` 生成会话密钥。
3. 使用 Go 1.25 或更新版本，运行下列命令并打开 `http://127.0.0.1:8080`。

   ```sh
   set -a
   . ./.env
   set +a
   go run .
   ```

## Docker

复制并填写 `.env` 后，运行 `docker compose up -d --pull always --no-build` 拉取并启动 GHCR 镜像。要从当前源码构建，运行 `docker compose up -d --build`。

Compose 默认绑定 `127.0.0.1:8080`，可用 `DASHBOARD_PORT` 调整宿主机端口。通过 HTTPS 反向代理提供访问时，请在 `.env` 设置 `COOKIE_SECURE=true`，并让代理保留原始 `Host` 请求头。

每次推送到 `main` 都会构建并发布 `ghcr.io/lizhian/gpt-load-monitor` 的 Linux amd64 和 arm64 镜像。发布会更新 `latest` 和北京时间的 `YYYYMMDD` 标签。同日再次发布会覆盖同一日期标签。

## 运行配置

| 变量 | 用途 |
| --- | --- |
| `GPT_LOAD_BASE_URL` | GPT-Load 服务地址，例如 `https://gpt-load.example.com` |
| `GPT_LOAD_AUTH_KEY` | 仅由 Go 服务读取，用于调用 GPT-Load 管理接口 |
| `VIEWER_PASSWORD` | 看板的共享登录密码 |
| `SESSION_SECRET` | 保存查看会话的签名密钥，至少 32 个字符 |
| `COOKIE_SECURE` | HTTPS 部署设为 `true` |
| `ADDR` | Go 服务监听地址，本地默认 `127.0.0.1:8080`，Docker 内为 `:8080` |

AccessKey 和整体用量共用顶部时间筛选，支持近 6 小时、近 12 小时、近 24 小时、今天、昨天、近 7 天和近 30 天，默认近 24 小时。今天和昨天按北京时间零点划分，其余范围滚动计算。

AccessKey 统计包含当前启用和停用的 Key，以及 GPT-Load 在所选范围的用量分布中返回的已删除 Key。

AccessKey 用量趋势默认显示估算费用，也可切换到请求数、Token 用量和缓存率。点击图例可显示或隐藏曲线，分桶明细会标记计价不全的费用。

订阅额度以 GPT-Load 最近一次观测的快照为准。美元额度按同一周期内、可归属于账号的已计价消耗估算，计价缺失会在页面提示。倒计时到期时，剩余额度仍以上游下一次观测为准。

运行 `go test ./...` 验证登录保护、上游部分失败处理、北京时间日界和额度及 AccessKey 计算。
