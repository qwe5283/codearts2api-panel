# CodeArts2API Panel

将华为云 **CodeArts 智能编程 / CodeArts Agent** 账号接入 OpenAI 兼容 API 的自托管网关，附带 Web 管理面板。

项目参考 `references/workbuddy2api-panel` 的页面组织和交互，以 `references/codearts2api-go` 的 CodeArts 协议为后端基础。运行时不依赖参考仓库、腾讯服务或 npm；前端资源直接嵌入 Go 二进制。

## 功能

| 模块 | 支持内容 |
| --- | --- |
| 账号池 | OAuth 浏览器授权、JSON / AK-SK 导入、启用与禁用、清除冷却、删除、热重载 |
| 凭据 | 永久 AK/SK、STS 临时凭据、具备完整 PKCE / DPoP 材料的 OAuth 自动续期 |
| 网关 | `/v1/models`、`/v1/chat/completions`，流式与非流式、工具调用、图片内容块 |
| 多账号 | 并发限制、会话关联、错误冷却、失败换号与排队重试 |
| 模型目录 | AgentCenter、内置模型发现；显示上下文、积分倍率及已知模型标签和图片能力 |
| 额度 | 查询 CodeArts 套餐 CREDIT 积分，未知值不显示为 0 |
| 余额趋势 | 每 30 分钟查询上游，成功采样按 5 分钟去重；多账号折线、每日变化与账号明细，历史落盘并在重启后保留 |
| 活动 | 手动签到 / 新用户活动领取：每日一次、新用户一次，已签按钮显示「已签」且不再请求上游；可配置后台周期执行 |
| 配置 | 表单 / Raw JSON 双模式（切换即重新读取配置文件），保存以当前界面模式为准并热生效；密钥脱敏，仅装配期四项需重启 |
| 可观测 | 进程累计指标、近期请求及 Token 用量（重启后回看 `data/requests` 归档）、最近失败摘要、实时运行日志、脱敏请求 JSONL 归档 |
| 面板 | 账号、余额趋势、模型、用量、配置、日志六页；明暗主题及移动布局 |

腾讯专属成长任务、旅行、抽奖和跨域账号体系没有移植。当前项目已移除历史福利模型目录、Token 福利余额及领取通道。模型目录完全来自账号的上游目录，不再有硬编码兜底项：没有账号时 `/v1/models` 返回空列表，实际可用性以账号套餐及上游响应为准。

## Docker 快速启动

先准备宿主机的配置文件和数据目录。已有本机启动生成的 `config.json` 时可直接复用；否则复制示例，并将其中的 `api_key`、`admin_key` 填成两个不同的随机密钥：

```sh
cp config.example.json config.json
mkdir -p auths data
# 编辑 config.json，设置 api_key 和 admin_key 后启动：
docker compose up -d --build
```

Windows PowerShell 的准备命令：

```powershell
Copy-Item config.example.json config.json
New-Item -ItemType Directory -Force auths, data
# 编辑 config.json，设置两把密钥后：
docker compose up -d --build
```

已有配置时跳过复制步骤，避免覆盖。**启动 Compose 前必须存在 config.json 文件**，否则 Docker 可能创建同名目录，服务会拒绝启动。

Compose 默认只发布到 `127.0.0.1:7866`，直接挂载：

| 宿主机 | 容器内 |
| --- | --- |
| `./config.json` | `/app/config.json` |
| `./auths/` | `/app/auths/` |
| `./data/` | `/app/data/` |

Linux 宿主机需让容器运行用户能写入这些路径。可用自己的 UID/GID 启动：

```sh
PUID="$(id -u)" PGID="$(id -g)" docker compose up -d --build
```

忘记设置时，容器写 `auths/` 或 `data/` 会被拒；这条报错信息里自带上述解法（改 UID 启动 / `chown 10001` / `user: "0:0"`），不必回头翻文档。

也可单独构建：

```sh
docker build -t codearts2api-panel .
docker run --rm -p 127.0.0.1:7866:7866 \
  --mount type=bind,src="$(pwd)/config.json",dst=/app/config.json \
  --mount type=bind,src="$(pwd)/auths",dst=/app/auths \
  --mount type=bind,src="$(pwd)/data",dst=/app/data \
  codearts2api-panel
```

需要远程访问时，通过自己的 HTTPS 反向代理接入。浏览器授权遇到本机回调不可达时，使用面板的回调地址提交功能。

## CI/CD

GitHub Actions 测试构建、GHCR 镜像发布与 Debian SSH 自动更新，见 [部署指南](docs/deployment.md)（部署默认关闭）。

## 下载发布版

从 [GitHub Releases](https://github.com/qwe5283/codearts2api-panel/releases) 下载对应系统和架构的压缩包：Windows x64 使用 `.zip`，Linux x64 / ARM64 使用 `.tar.gz`。解压后进入目录，运行 `codearts2api-panel.exe`（Windows）或 `./codearts2api-panel`（Linux），无需安装 Go 或 Node.js。可用随发布提供的 `SHA256SUMS` 校验下载文件。

首次运行会生成 `config.json`，打开 `http://127.0.0.1:7866/panel` 并使用其中的 `admin_key` 登录。升级前停止旧进程、备份 `config.json`、`auths/` 和 `data/`，只替换程序，不覆盖已有配置和账号数据。面板显示当前程序版本。

版本格式为 `vYY.MM.DD.N`，日期按 `20YY` 解释，`N` 是当日从 1 开始的发布序号，例如 `v26.11.01.1`；序号不使用前导零。维护者在通过测试的提交上创建并推送该标签后，`Release binaries` 工作流会重新测试并发布三个平台的压缩包及校验和：

```sh
git tag v26.11.01.1
git push origin v26.11.01.1
```

每个新版本使用新标签。可在 `docs/releases/<版本>.md` 编写发布说明；未提供时自动生成变更记录。工作流重跑会补传同一 Release 的附件，保留现有发布说明。

## 快速启动

需要 **Go 1.22 或更新版本**。业务代码仅依赖 Go 标准库；Node.js 仅用于可选的前端测试。

```sh
go run ./cmd/server
```

首次启动自动生成 `config.json`，包含独立的随机 `api_key` 和 `admin_key`，不会覆盖已有配置。打开：

[http://127.0.0.1:7866/panel](http://127.0.0.1:7866/panel)

使用 `config.json` 中的 **admin_key** 登录面板。初始账号池为空，通过「添加账户」完成接入。

| 密钥 | 用途 |
| --- | --- |
| `admin_key` | 管理面板及 `/admin/api/*` |
| `api_key` | OpenAI 客户端调用 `/v1/*` |

已有配置未设置 admin_key 时，为兼容旧配置会使用 api_key；新生成的配置两者独立。管理密钥不应分发给 API 调用方。

也可以先编译再运行：

```sh
go build -trimpath -o bin/codearts2api-panel ./cmd/server
./bin/codearts2api-panel
```

Windows：

```powershell
go build -trimpath -o bin/codearts2api-panel.exe ./cmd/server
.\bin\codearts2api-panel.exe
```

指定配置文件：

```sh
go run ./cmd/server -config /path/to/config.json
```

`auth_dir`、`state_file` 的相对路径以**配置文件所在目录**为基准。将配置放到独立目录即可隔离账号和状态。

## 添加 CodeArts 账号

### 浏览器 OAuth

1. 面板点击「添加账户 → 浏览器授权 → 生成授权链接」。
2. 打开授权链接，完成华为云登录。
3. 返回面板等待结果，或点击检查状态。

服务会保存临时 AK/SK、Security Token、最新 refresh_token、原 PKCE verifier、DPoP 私钥及 client_id。续期需要复用原始绑定材料，并保存轮转后的 refresh_token。

**远程部署：** 华为门户仍可能回调浏览器所在机器的 `127.0.0.1`。若页面打不开，在添加账户窗口的「提交浏览器回调地址」区域粘贴完整回调 URL。生成授权链接后，可使用右侧按钮复制链接或在新标签页打开授权页。若返回下一步授权链接，继续打开并按提示提交后续回调。不要把这些回调地址发给其他人。

原生门户回调端口与网关监听端口可以不同；服务会为需要的登录会话准备高位回调端口。无需把 `oauth_callback_host` 当成任意公网 redirect_uri。

### 永久 AK/SK

选择「AK / SK」，输入本人的访问密钥。永久密钥可不填 Security Token 和到期时间；它没有 OAuth 自动续期流程。

也可导入 JSON：

```json
{
  "user_id": "my-codearts-account",
  "user_name": "我的 CodeArts",
  "access_key_id": "<AK>",
  "secret_access_key": "<SK>"
}
```

### 临时凭据或已有导出

支持单个对象、对象数组，以及 STS 响应中的 `credentials` 结构。例如：

```json
{
  "user_id": "my-codearts-account",
  "credentials": {
    "access_key_id": "<TEMP_AK>",
    "secret_access_key": "<TEMP_SK>",
    "security_token": "<SECURITY_TOKEN>",
    "expiration": "2099-01-01T00:00:00Z"
  }
}
```

示例日期仅表达 RFC3339 格式，必须替换为真实到期时间。导入已有 OAuth 记录时，请同时保留 `refresh_token`、`code_verifier`、`client_id` 和 `dpop_private_key`；兼容 `dpop_private_key_jwk` 字段。

仅有临时 AK/SK/Token 时，可在有效期内使用，但不能据此自动生成续期材料。导入会校验凭据并拒绝无效数据；删除账户会同时删除本服务保存的对应凭据文件。

## 调用 OpenAI 兼容 API

客户端配置：

```text
Base URL: http://127.0.0.1:7866/v1
API Key:  config.json 中的 api_key
Model:    从 /v1/models 或面板模型目录复制
```

模型 ID 必须在模型目录里：目录已经发现过时，目录之外的 ID 由网关直接回 `404 model_not_available`，**不会**带着这个 ID 去请求上游 —— 上游对未注册模型只会回 `InferHub.002002009.404`，还白占一次并发会话与轮转名额。目录只存在内存里（有效期 1 小时，重启即失效），由 `GET /v1/models` 或面板（账号页、模型页）触发拉取；发现之前没有判断依据，仍交给上游回答。因此**只调 `/v1/chat/completions`、从不查模型目录也不打开面板的客户端，这道拦截不会生效**。大小写与旧别名按目录归一，`glm-5.2` 与 `GLM-5.2` 等价：`/v1/models` 列出的 ID 统一小写，同一模型不会因为大小写出现两条，发往上游时再还原成该账号注册的写法（`GLM-5.2` 这类模型用小写直接打上游会被回 `InferHub.002002009.404`）。

查询模型：

```sh
curl http://127.0.0.1:7866/v1/models \
  -H "Authorization: Bearer <API_KEY>"
```

对话：

```sh
curl http://127.0.0.1:7866/v1/chat/completions \
  -H "Authorization: Bearer <API_KEY>" \
  -H "Content-Type: application/json" \
  -d '{"model":"GLM-5.2","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

`GLM-5.2` 仅为示例，请替换为账号可用模型。设 `stream:false` 可请求聚合后的普通 JSON 响应；工具使用 OpenAI 风格的 `tools` / `tool_calls` / `tool_call_id`。

图片使用支持多模态的模型，消息形状为：

```json
{
  "model": "<支持图片的模型ID>",
  "messages": [{
    "role": "user",
    "content": [
      {"type": "text", "text": "描述图片"},
      {"type": "image_url", "image_url": {"url": "data:image/png;base64,<BASE64>"}}
    ]
  }],
  "stream": true
}
```

当前网关实现 Chat Completions，不提供 `/v1/responses`。上游的采样参数、工具选择和多模态支持仍可能因模型而异。

## 积分、签到与定时行为

- **套餐积分**来自 SNAP 的账户统计接口，保留小数；总额和基础/赠送等分类不会重复相加。
- 「刷新额度」查询真实积分余额；查询失败会保留已有缓存并显示错误，未查询时显示未知。成功完成下游对话请求后，如果所用账号的积分缓存已超过 5 分钟未查询，会异步刷新一次；同一账号的刷新任务不会重复并发，不增加下游响应等待时间。
- 页面普通刷新读取余额缓存，不会每次轮询都领取积分。模型页「重新获取」可主动刷新模型目录。
- **余额趋势**在启动后立即刷新启用账号，之后每 30 分钟刷新一次；手动「刷新额度」也走同一采样入口。只有真实查询成功且上游返回 CREDIT 指标时才记录，同一账号 5 分钟内的重复采样会被去重，失败缓存与未知余额不会写成 0。历史保存到 `state_file` 同目录的 `balance_history.json`，近 30 天保留原始点，更早折叠为日点并最多保留 365 天。每日消耗/到账是相邻采样差分的下界估算，不是精确账单；趋势页读取历史不会额外请求上游。
- 「签到」处理 CodeArts 的活动领取与确认：按活动 `type` 选每日签到（`USER_LOGIN`）和新手（`NEW_USER_REGISTER`），不按 `campaignId` —— 活动 ID 会随投放批次变化；活动资格和金额来自当前账号的活动列表。
- **领取次数由本地记账兜住**：每日签到按自然日只领一次（跨零点自动失效），新手活动每个账号只领一次。已经了结的账号不再向上游发请求——`data/state.json` 里的 `last_checkin_day` / `checkin_claimed` / `new_user_settled` 就是这份账本，重启后仍然生效。只有上游真的给了答复（领到、已领取、没有该活动）才算「了结」；网络失败、待确认（`CLAIMED` 未 `CONFIRMED`）会留到下一轮继续。
- **签到按本地时点执行**：`credits_auto_claim:true` 时，进程在 `checkin_hours`（默认 `[9, 21]`，本地时间整点）各领一次每日签到和新手活动，与 `watch.poll_minutes` 的 token 轮询互不相干。启动时若今天已经过了最近一个时点却还没签，会立刻补一次（已签的账号由账本短路，零上游请求）。
- `watch.enabled:true` 时，后台校验登录态并按条件续期；关闭后仍可手动操作。

自动生成的默认配置开启活动积分领取与凭据看护；如希望只手动操作，将 `credits_auto_claim` 或 `watch.enabled` 设为 false 并保存即可（热生效，无需重启）。

## 配置

[config.example.json](config.example.json) 是字段示例，密钥留空。直接首次启动会生成可用配置；手工复制示例时应填入自己的密钥，或设置对应环境变量。

| 配置项 | 默认值 | 说明 |
| --- | --- | --- |
| `listen` | `:7866` | HTTP 监听地址；仅本机访问可设 `127.0.0.1:7866` |
| `api_key` / `admin_key` | 首次随机生成 | API 与管理鉴权 |
| `auth_dir` | `./auths` | CodeArts 凭据目录 |
| `state_file` | `./data/state.json` | 账号池状态 |
| `default_model` | `glm-5.2` | 请求省略模型时的默认值 |
| `account_strategy` | `first` | 无会话绑定时选号：`first` 第一个可用账号、`credits` 已缓存积分最多、`round_robin` 账号池轮询、`least_active` 在途请求最少（平局按游标轮转，多账号池推荐）；积分未知不视为 0，全部未知时取第一个 |
| `max_concurrent` | `2` | 单账号并发上限（上游单账号上限 3，留一个槽位余量） |
| `cooldown.soft_rate` | `60s` | 临时限流冷却 |
| `cooldown.err_threshold` | `3` | 连续错误阈值 |
| `cooldown.err_cooldown` | `10m` | 错误冷却 |
| `queue_retry_seconds` | `10` | 排队重试间隔；缺省/0 使用内部默认值 |
| `queue_max_attempts` | `30` | 排队重试上限；缺省/0 使用内部默认值 |
| `upstream.timeout_seconds` | `120` | 上游客户端超时配置 |
| `credits_auto_claim` | `true` | 后台活动积分领取（按 `checkin_hours` 时点） |
| `checkin_hours` | `[9, 21]` | 每日签到时点（本地小时 0-23）；空数组/非法值回落默认 |
| `request_log_retention_days` | `30` | `data/requests/` 归档保留天数，按文件修改时间清理；0/负数回落默认 |
| `max_body_mb` | `32` | 聊天请求体上限，单位 MB；凭据导入仍独立限制为 8MB |
| `image_shrink.enabled` | `true` | 按需压缩内联 PNG/JPEG；参数及默认值见 `config.example.json` |
| `vision_bridge.enabled` | `true` | 纯文本模型的内联图片先交视觉模型转述；默认使用 `Qwen3-VL-235B` |
| `watch.enabled` | `true` | 后台凭据看护 |
| `watch.poll_minutes` | `30` | 后台轮询间隔 |
| `watch.refresh_skew_minutes` | `30` | 临近到期续期窗口 |

环境变量优先于配置文件。支持 `CA2A_API_KEY`、`CA2A_ADMIN_KEY`、`CA2A_LISTEN`、`CA2A_AUTH_DIR`、`CA2A_STATE_FILE`、`CA2A_DEFAULT_MODEL`、`CA2A_WATCH_ENABLED`、`CA2A_CREDITS_AUTO_CLAIM`、`CA2A_CHECKIN_HOURS`（如 `9,21`）、`CA2A_REQUEST_LOG_RETENTION_DAYS`、`CA2A_MAX_BODY_MB` 等；完整映射见 `cmd/server/config.go` 的 `applyEnv`。

重试与冷却五项（`cooldown.soft_rate` / `cooldown.err_threshold` / `cooldown.err_cooldown` / `queue_retry_seconds` / `queue_max_attempts`）的非法值——`0`、负数、解析不了的时长——一律按「没配置」处理：启动时按默认值跑，面板保存时直接把默认值写回配置文件，面板显示的也就是运行期真正生效的值。

多模态参数可在表单的「多模态请求」「图片压缩」「视觉转述」分组或 Raw JSON 中编辑，保存后热生效。`max_body_mb`、`image_shrink` 和 `vision_bridge` 中需要归一化的参数也会在保存时回写实际默认值。缩图默认在请求体超过 4MB 或图片超预算时触发，目标为单张 2MB、合计 4MB、最长边 2048 像素、JPEG 质量 85；小图保持原样，WebP/GIF 和远程图片不参与缩图。

视觉转述给纯文本模型的是图片的文字描述，精细颜色和布局仍应直接使用视觉模型。每张未命中缓存的图片会额外消耗一次视觉模型调用额度，并增加等待时间；默认最多处理 8 张图，总预算 90 秒。超出图片数量或任一转述失败时保留全部原图，由上游按模型能力判定请求。可用 `vision_bridge.enabled=false` 单独关闭转述，或用 `image_shrink.enabled=false` 关闭缩图。

面板「服务配置」提供表单与 Raw JSON 两种编辑模式：顶部 Segmented Control 切换，切换时重新读取配置文件（未保存的修改会丢弃），点「保存配置」以当前界面模式为准——表单模式以最近读到的配置为底只覆盖表单字段，Raw 模式提交编辑框里的完整 JSON。表单不覆盖的字段（含未知扩展字段）在表单模式下原样保留。面板不会回显密钥，空密钥输入表示保留原值；保存前会校验配置，并保留未知字段。普通配置文件采用原子替换，Docker 单文件挂载使用原地写入。**保存后立即热生效**：请求期读取的字段走运行期快照（密钥、默认模型、选号策略、冷却、排队重试、OAuth 回调与 client_id），`max_concurrent` / `keepalive_window` 直接下发给账号池，`watch.*` / `checkin_hours` / `credits_auto_claim` 直接重配调度器，`request_log_retention_days` 直接换掉归档清理口径（后台清理每秒一轮）。只有四项装配期依赖必须重启：`listen`、`auth_dir`、`state_file`、`upstream.timeout_seconds`，表单里这四项的标签后会标注「需重启」，其余字段保存即生效。环境变量覆盖的配置仍需修改部署环境后重启。

## 数据与日志

```text
config.json                 配置与网关/管理密钥
auths/codearts-*.json        CodeArts 凭据及续期材料
data/state.json             账号池持久状态
data/state.json.chats.json   会话关联状态
data/requests/              脱敏请求元数据 JSONL
```

用量页按时间窗口向服务端要聚合结果（`/admin/api/usage?window=1`）：服务端扫 `data/requests/` 归档、并上进程内存里还没落盘的那几条，按请求 ID 去重后只回合计、按模型和按小时的汇总，既不传逐条记录，也不需要把整段窗口读进内存。所以右上角的时间范围（今天 / 近 24 小时 / 近 3 天 / 近 7 天 / 近 30 天 / 自定义）里的每个数字都是整段窗口的全量统计，不是页面上几百条样本的统计；默认「近 30 天」与归档保留期一致。顶部卡片除完成数、成功率和平均耗时外，还给平均首字时间（TTFT，只平均真正返回过首字的请求）、平均缓存命中率（按已上报缓存的 token 加权，即总命中 / 总 prompt 缓存 token，卡片下方同时给出命中与未命中的 token 数；上游没报缓存的请求不参与）以及输入 / 输出 / 总计 Token 三个合计。样本不足时这些卡片显示 `—` 而不是 0。请求归档默认保留 30 天（`request_log_retention_days`，按文件修改时间清理，可在面板「日志与归档」里改，保存后一秒内生效），只记录请求 ID、模型、状态、耗时、用量及失败错误摘要等元数据，不记录提示词、模型正文或鉴权头。“按模型汇总”右侧可切“按模型 / 按账号”两种口径：按账号看请求数、成功数、输入/输出/总 token 与缓存命中率；按模型在此基础上还给出平均 TTFT 和输出 TPS（输出 token ÷ (耗时 − 首字时间)，按样本加权，没有首字样本时显示 `—`）。两种口径来自同一次窗口聚合，切换不会重新查询。运行日志页走的是另一条路径：显示最近的逐条记录（进程 100 条 + 归档 200 条，合并去重，重启后由归档补上），每 5 秒刷新一次，不受用量页时间范围影响；失败项会显示脱敏、截断后的最终错误摘要，运行日志本身为有界内存快照。

凭据文件含敏感材料，应与配置一并备份并限制访问。`auths/`、`data/`、`runtime/`、真实 `config.json` 和 `references/` 均已加入 Git/Docker 忽略规则。

## 开发与测试

```sh
go test ./...
go vet ./...
node --check internal/server/panel.js
node --test internal/server/panel_frontend.test.cjs
```

具备受支持 C 编译器的环境可运行：

```sh
go test -race ./...
```

主要目录：

```text
cmd/server/          启动、配置校验及原子保存
cmd/login/           CodeArts 命令行登录工具
internal/auth/       凭据解析、校验和持久化
internal/pool/       账号调度、刷新、余额缓存
internal/upstream/   华为云 HMAC / DPoP、模型、聊天、余额与活动协议
internal/server/     OpenAI 接口、管理接口及嵌入式面板
internal/scheduler/  后台凭据看护与活动调度
internal/reqlog/     请求指标与脱敏归档
docs/                协议提取文档
references/          本地参考仓库，运行与构建均不依赖它
```

协议出处、历史接口和仓库间差异见 [CodeArts API 文档](docs/codearts-api.md)。该文档保留参考仓库的历史福利接口，不代表当前项目仍支持这些入口。源码与面板测试不替代真实账号联调；账号套餐、模型清单及活动规则仍由华为云决定。

## 开源来源

采用 MIT 许可证，保留上游版权声明。来源与版本说明见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)，许可证见 [LICENSE](LICENSE)。
