# APOD Server

一个带缓存、可观测性和容错能力的 NASA APOD 镜像服务。

## 特性

- 多级缓存：内存缓存 + Redis 持久缓存
- 缓存防击穿：singleflight 合并同 key 并发请求
- 图片缓存：本地落盘，支持冷热分层清理
- 定时任务：每日预抓取 APOD，定期清理缓存
- 可观测性：Prometheus 指标、结构化日志、Trace ID
- API 优化：Gzip、ETag、Cache-Control、限流
- 健康探针：healthz / readyz

## 项目结构

- [main.go](main.go): 启动入口
- [app_state.go](app_state.go): 全局常量与运行时状态
- [model.go](model.go): 数据模型
- [cache_memory.go](cache_memory.go): 内存缓存与淘汰策略
- [redis_store.go](redis_store.go): Redis 持久缓存与熔断
- [image_store.go](image_store.go): 图片缓存与清理
- [fetch.go](fetch.go): NASA/Web 抓取与解析
- [utils.go](utils.go): 工具函数与上下文日志
- [server.go](server.go): HTTP 路由、中间件、定时任务
- [Dockerfile](Dockerfile): 生产镜像构建
- [docker-compose.yml](docker-compose.yml): 应用 + Redis 本地编排

## 快速开始

### 1. 安装依赖

```bash
go mod tidy
```

### 2. 运行服务

```bash
go run .
```

默认监听 `:8080`。

### 3. 测试接口

```bash
curl -H 'Authorization: Bearer changeme' 'http://127.0.0.1:8080/v1/apod'
curl 'http://127.0.0.1:8080/v1/apod' # 不带 Authorization 时自动使用 DEMO_KEY
curl 'http://127.0.0.1:8080/healthz'
curl 'http://127.0.0.1:8080/readyz'
curl 'http://127.0.0.1:8080/metrics'
```

## Docker 部署

### 1. 构建镜像

```bash
docker build -t apod-server:latest .
```

### 2. 单容器运行

```bash
docker run --rm -p 8080:8080 \
	-e NASA_API_KEY=your_api_key \
	-e API_AUTH_KEY=your_app_api_key \
	-e REDIS_ADDR=host.docker.internal:6379 \
	--name apod-server apod-server:latest
```

说明：

- 如果 Redis 也在容器中运行，建议使用 Docker Compose。
- Linux 环境如无 `host.docker.internal`，请改为宿主机实际 IP。

### 3. 使用 Docker Compose（推荐）

```bash
docker compose up -d --build
```

查看状态与日志：

```bash
docker compose ps
docker compose logs -f app
docker compose logs -f redis
```

停止并清理：

```bash
docker compose down
```

如果要同时删除 Redis 持久化数据卷：

```bash
docker compose down -v
```

### 4. Compose 环境变量优先级

`docker-compose.yml` 中使用 `${VAR:-default}` 语法，实际优先级为：

- shell 导出的环境变量
- 项目根目录 `.env` 文件
- Compose 文件中的默认值

示例：

```bash
NASA_API_KEY=your_api_key docker compose up -d
```

## 主要接口

- `GET /v1/apod?date=YYYY-MM-DD`（Header: `Authorization: Bearer YOUR_KEY`）
- `GET /v1/apod/image?date=YYYY-MM-DD`（Header: `Authorization: Bearer YOUR_KEY`）
- `GET /metrics`
- `GET /healthz`
- `GET /readyz`

## 环境变量

服务启动时会自动尝试加载项目根目录的 `.env` 文件。
读取优先级为：`系统环境变量 > .env > 代码默认值`。

### 运行与上游

- `APP_ENV`: 运行环境，`development` 或 `production`，默认 `development`
- `LOG_LEVEL`: 日志级别，默认开发环境 `debug`，生产环境 `info`
- `LOG_COLOR`: 控制台日志等级着色开关（`true/false`），默认自动检测终端
- `NASA_API_KEY`: NASA API Key，默认 `DEMO_KEY`
- `API_AUTH_KEY`: 业务 API 访问密钥，默认 `changeme`
- `DEMO_KEY_LIMIT_PER_24H`: 未携带 Authorization 时（自动使用 `DEMO_KEY`）每个 IP 24 小时可调用 `/v1/apod` + `/v1/apod/image` 总次数，默认 `5`
- `API_RATE_LIMIT_RPS`: API 每秒令牌速率，默认 `8`
- `API_RATE_LIMIT_BURST`: API 突发令牌桶容量，默认 `16`

### Redis

- `REDIS_ADDR`: 默认 `127.0.0.1:6379`
- `REDIS_PASSWORD`: 默认空
- `REDIS_DB`: 默认 `0`

### 内存缓存

- `MEMORY_CACHE_TTL_MINUTES`: 今日数据 TTL（分钟），默认 `180`
- `MEMORY_CACHE_MAX_ITEMS`: 最大条目数，默认 `2000`
- `MEMORY_CACHE_CLEANUP_MINUTES`: 清理周期（分钟），默认 `15`

### 图片缓存

- `IMAGE_CACHE_DIR`: 默认 `cache/images`
- `IMAGE_CACHE_MAX_FILES`: 最大文件数，默认 `1000`
- `IMAGE_CACHE_MAX_AGE_HOURS`: 冷数据最大保留时长（小时），默认 `720`（30 天）

## 缓存策略说明

- APOD 今日数据：内存和 Redis 使用 TTL
- APOD 历史数据：默认长期缓存（通过容量控制清理）
- 图片缓存：最近 7 天视为热数据，不参与清理；历史数据按时间和容量策略清理

## 观测建议

日志格式策略：

- 开发环境（`APP_ENV=development`）：Console Encoder，便于本地阅读
- 生产环境（`APP_ENV=production`）：JSON Encoder，便于 ELK/Loki 等系统采集
- 时间字段统一为 ISO8601（例如 `2026-04-14T16:15:02Z`）
- HTTP Access Log 使用统一消息 `http_request`，并包含 `method/path/status/latency/ip/trace_id`
- 开发控制台中的 `latency` 统一为固定宽度毫秒字符串，便于滚动扫描对齐

可以关注以下指标：

- `apod_request_total`
- `apod_request_duration_seconds`
- `apod_source_total`
- `apod_cache_hit_total`
- `apod_cache_hit_ratio`
- `apod_fetch_fail_total`
- `apod_parse_fail_total`
- `apod_image_cache_hit_total`

## 生产部署建议

- 使用真实 `NASA_API_KEY`，避免 `DEMO_KEY` 配额瓶颈
- 生产环境启用 Redis
- 如在容器中运行，镜像可保持轻量，时区数据已通过 `time/tzdata` 内置
- 前置网关可再叠加 IP 级限流
