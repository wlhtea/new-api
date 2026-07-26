# Seed Dance 视频接口测试指南

本文说明如何配置和验证 Type 59「无审核 Seed Dance」渠道，以及如何通过
OpenAI 兼容视频接口完成 T2V、I2V、状态轮询和 MP4 下载。

文中的请求示例只使用以下三个 Apifox 模板变量：

- `{{base_url}}`：New API 对客户端暴露的基础地址，不包含尾部 `/`；
- `{{api_key}}`：专用于测试的 New API 客户端 API Key；
- `{{task_id}}`：New API 返回的公开任务 ID，即提交响应中的 `id`。

供应商渠道 Key、供应商任务 ID、图片 Base64 和视频 Base64 都不应写入本文、
测试日志或提交到 Git。下文的 `TEST_PROMPT`、文件名和域名均为合成测试值。

## 1. 前置条件

测试前确认：

1. New API 已部署包含 Type 59 渠道的版本；
2. 管理员可访问渠道、模型定价和日志页面；
3. 本机安装了 `curl`、`jq`、Python 3；验证媒体时还需 `ffprobe`；
4. 已准备一张合成 JPG 或 PNG，例如 `./reference.png`；
5. 使用隔离的测试用户和测试 API Key，不复用生产凭据；
6. 生产流量的传输门禁已经满足；若尚未满足，渠道保持禁用，只执行 Mock、
   OpenAPI 和本地协议测试。

## 2. 创建禁用的 Type 59 渠道

在管理后台进入「渠道」并创建渠道，保存前使用以下配置：

| 配置项 | 值或要求 |
|---|---|
| 渠道类型 | 「无审核 Seed Dance」，内部类型为 Type 59 |
| 状态 | **禁用** |
| 名称 | 一个不包含凭据的测试名称 |
| Base URL | 填写供应商分配的基础地址 |
| Key | 填写供应商分配的渠道 Key |
| 模型 | `seedance-uncensored` |
| Endpoint Type | `openai-video` |

保存后继续保持禁用状态，并完成以下检查：

1. 渠道详情只配置 `seedance-uncensored`，没有把供应商模型名暴露给客户端；
2. 模型能力列表中存在 `seedance-uncensored`；
3. Base URL、代理和供应商 Key 已通过管理员侧渠道专用连通性测试；
4. 渠道专用连通性测试没有创建任务，也没有产生视频费用；
5. 已配置价格并检查分组倍率；
6. 只有在传输、定价、权限和回滚检查全部完成后，才在受控测试窗口启用渠道。

`{{api_key}}` 是客户端访问 New API 的 Key，与后台 Type 59 渠道保存的供应商
Key 是两个不同的凭据。

## 3. 模型能力和参数

公开模型固定为：

```text
seedance-uncensored
```

`model` 必须是精确、大小写敏感的字符串 `seedance-uncensored`，不能带前后
空白。`prompt` 必须是字符串，经过前后空白移除后至少包含一个非空白字符。
JSON 顶层和 `metadata` 中当前未识别的字段会被接受并忽略，不会转发给供应商。

公开接口为：

```text
POST /v1/videos
GET  /v1/videos/{task_id}
GET  /v1/videos/{task_id}/content
```

### 3.1 时长

- 可通过顶层 `duration`、顶层 `seconds` 或 `metadata.duration` 指定；
- 三个入口都接受 JSON 整数或只包含 ASCII 十进制数字的字符串，例如
  `1`、`"1"` 和 `"01"`；
- 有效范围为 `1–15`；
- 多处同时提供相同值时接受，不同值时返回 `400 invalid_duration`；
- 缺省值为 `15`；为了让测试费用可预测，示例均显式传入 `duration`；
- `0`、负数、小数、指数形式、带正号、含前后空白、空字符串和非数字值均会
  被拒绝。

### 3.2 分辨率

| `size` 或 `metadata.resolution` 的等价输入 | 规范化分辨率 | 倍率 |
|---|---|---:|
| `854x480`、`480x854`、`480P` | `480P` | `0.5` |
| `1280x720`、`720x1280`、`720P` | `720P` | `1.0` |
| `1920x1080`、`1080x1920`、`1080P` | `1080P` | `2.25` |

`size` 和 `metadata.resolution` 使用同一组九个 alias；匹配前会移除两端空白
并忽略大小写。如果两个入口同时存在，两者必须映射到相同分辨率。T2V 不接受
`480P`；I2V 接受三个分辨率。缺省分辨率为 `720P`。

### 3.3 图片

I2V 只接受一张 JPG 或 PNG。可使用：

- 顶层 `image`；
- 单元素 `images`；
- 顶层 `input_reference`；
- `metadata.image_base64`；
- multipart 文本字段 `image`、`images` 或 `input_reference`；
- multipart 文件字段 `input_reference`。

字符串图片来源可以是纯 Base64、JPG/PNG data URI，或通过平台 SSRF 防护
检查的远程 URL。图片宽高必须在 `240–8000` 之间，宽高比必须在
`1:8–8:1` 之间。

Base64 使用标准字母表和严格 padding，不接受 URL-safe 字母表或内嵌空格、
Tab、CR、LF 等 ASCII 空白。data URI 前缀必须精确为
`data:image/jpeg;base64,` 或 `data:image/png;base64,`。

供应商资料中的 **5 MB 是建议，不是 New API 的 Seed Dance 硬限制**。New
API 不因图片超过该建议值而添加 Seed Dance 专用的 `5 MB` 或 `5 MiB`
提前拒绝；平台已有的通用远程资源、请求体和部署资源配置仍然适用。

### 3.4 供应商扩展字段

`metadata` 支持：

```json
{
  "prompt_optimization": true,
  "multi_shot": false,
  "strict_duration": false,
  "negative_prompt": "SYNTHETIC_NEGATIVE_PROMPT"
}
```

JSON 请求中的布尔字段必须是真正的布尔值。multipart 的 `metadata` 是一个
JSON 编码字符串，其中 `prompt_optimization`、`multi_shot` 和
`strict_duration` 当前必须写成文本 `"true"` 或 `"false"`；适配器会在发送
上游前将其规范化为布尔值。三个布尔字段未提供时不会由适配器注入 `false`，
而是从供应商请求中省略；`strict_duration` 的供应商缺省值为 `false`。
`negative_prompt` 必须是字符串且不会 trim，空字符串会从供应商请求中省略。

## 4. 计费配置与公式

### 4.1 推荐的 ModelPrice

推荐配置：

```text
ModelPrice["seedance-uncensored"] = 0.15
```

`ModelPrice` 表示 720P 的每请求秒基础价格。定义：

```text
P = ModelPrice
D = 规范化后的请求时长
R = 分辨率倍率（480P=0.5，720P=1.0，1080P=2.25）
G = 核心计费逻辑解析出的最终 GroupRatio
Q = QuotaPerUnit（当前为 500000）
```

精确结算分两阶段执行，每一阶段都向零截断：

```text
base_quota = truncate_toward_zero(P × Q × G)
quota      = truncate_toward_zero(base_quota × D × R)
费用       = quota / Q
```

例如 `P=0.15`、`D=1`、`R=1.0`、`G=1` 时：

```text
费用  = 0.15
quota = 75000
```

不要用 `truncate(P × D × R × G × Q)` 代替上述两阶段公式；当第一阶段乘积不是
整数时，一次性公式可能与实际账单相差一个或多个 quota。

### 4.2 ModelRatio 兼容模式

如果没有配置 `ModelPrice`，但配置了：

```text
ModelRatio["seedance-uncensored"] = M
```

则同样分两阶段使用：

```text
base_quota = truncate_toward_zero(M / 2 × Q × G)
quota      = truncate_toward_zero(base_quota × D × R)
```

`ModelPrice` 和 `ModelRatio` 同时存在时，`ModelPrice` 优先；值为零沿用平台
已有的免费模型语义。不要同时调整两个 Map 来试图叠加价格。

### 4.3 GroupRatio

Seed Dance 适配器不重复乘分组倍率。核心计费逻辑按以下规则只解析一次：

1. 如果存在 `UserGroup → UsingGroup` 的特殊倍率，使用该特殊倍率；
2. 否则使用 `UsingGroup` 的普通倍率；
3. 最终值保存到任务计费快照的 `BillingContext.GroupRatio`。

验收测试建议使用最终 `GroupRatio=1` 的专用测试用户，以便直接核对公式。

计费依据始终是提交前规范化的请求时长和分辨率。下载后的 MP4 实际时长即使
与请求时长略有差异，也不会重新计算费用或进行二次扣费。

## 5. T2V JSON 请求

```bash
curl -fsS \
  -H "Authorization: Bearer {{api_key}}" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "seedance-uncensored",
    "prompt": "TEST_PROMPT",
    "duration": 1,
    "size": "1280x720",
    "metadata": {
      "prompt_optimization": false,
      "multi_shot": false,
      "strict_duration": true,
      "negative_prompt": ""
    }
  }' \
  "{{base_url}}/v1/videos" | jq .
```

成功响应中的 `id` 和兼容字段 `task_id` 都是 New API 的公开任务 ID，不是
供应商任务 ID：

```json
{
  "id": "{{task_id}}",
  "task_id": "{{task_id}}",
  "object": "video",
  "model": "seedance-uncensored",
  "status": "queued",
  "progress": 10,
  "created_at": 1700000000,
  "seconds": "1",
  "size": "720P"
}
```

## 6. I2V：纯 Base64

下面的命令在运行时从本地合成图片生成纯 Base64；文档中不保存 Base64：

```bash
image_payload="$(base64 < ./reference.png | tr -d '\r\n')"

jq -n \
  --arg image "$image_payload" \
  '{
    model: "seedance-uncensored",
    prompt: "TEST_PROMPT",
    duration: 1,
    size: "1280x720",
    image: $image
  }' |
curl -fsS \
  -H "Authorization: Bearer {{api_key}}" \
  -H "Content-Type: application/json" \
  --data-binary @- \
  "{{base_url}}/v1/videos" | jq .

unset image_payload
```

## 7. I2V：data URI

```bash
image_payload="data:image/png;base64,$(base64 < ./reference.png | tr -d '\r\n')"

jq -n \
  --arg image "$image_payload" \
  '{
    model: "seedance-uncensored",
    prompt: "TEST_PROMPT",
    duration: 1,
    size: "1280x720",
    image: $image
  }' |
curl -fsS \
  -H "Authorization: Bearer {{api_key}}" \
  -H "Content-Type: application/json" \
  --data-binary @- \
  "{{base_url}}/v1/videos" | jq .

unset image_payload
```

只接受 `data:image/jpeg;base64,` 和 `data:image/png;base64,` 前缀。

## 8. I2V：远程 URL

以下域名是合成占位域名。实际执行时，将 URL 字符串换成测试环境中可访问、
可通过 SSRF 防护且返回 JPG/PNG 的地址：

```bash
curl -fsS \
  -H "Authorization: Bearer {{api_key}}" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "seedance-uncensored",
    "prompt": "TEST_PROMPT",
    "duration": 1,
    "size": "1280x720",
    "input_reference": "https://media.example.invalid/reference.png"
  }' \
  "{{base_url}}/v1/videos" | jq .
```

每次重定向都会重新执行 SSRF 校验。不要使用本机回环地址、云元数据地址或内网
管理地址作为测试图片来源。

## 9. I2V：multipart 文件

```bash
curl -fsS \
  -H "Authorization: Bearer {{api_key}}" \
  -F 'model=seedance-uncensored' \
  -F 'prompt=TEST_PROMPT' \
  -F 'duration=1' \
  -F 'size=1280x720' \
  -F 'metadata={"prompt_optimization":"false","multi_shot":"false","strict_duration":"true","negative_prompt":""}' \
  -F 'input_reference=@./reference.png;type=image/png' \
  "{{base_url}}/v1/videos" | jq .
```

不要手动设置 multipart 的 `Content-Type`；`curl` 会生成包含 boundary 的正确
请求头。multipart 也接受文本图片入口：

```bash
-F 'image=IMAGE_REFERENCE'
-F 'images=["IMAGE_REFERENCE"]'
-F 'input_reference=IMAGE_REFERENCE'
```

其中 `IMAGE_REFERENCE` 可以是纯 Base64、受支持的 data URI 或 HTTP(S) URL；
最终仍只能规范化出一张图片。`images` 也可以由一个或多个同名文本 part 表达，
但最终超过一项会返回 `400 invalid_image`。未知文本 part 当前会被忽略，未知
且非空的文件 part 会被拒绝。

## 10. 查询状态

```bash
curl -fsS \
  -H "Authorization: Bearer {{api_key}}" \
  "{{base_url}}/v1/videos/{{task_id}}" | jq .
```

典型处理中响应：

```json
{
  "id": "{{task_id}}",
  "task_id": "{{task_id}}",
  "object": "video",
  "model": "seedance-uncensored",
  "status": "in_progress",
  "progress": 30,
  "created_at": 1700000000,
  "seconds": "1",
  "size": "720P"
}
```

供应商建议轮询间隔至少 5 秒；本文自动化示例统一使用 10 秒，避免高频轮询。
非终态 `queued` 和 `in_progress` 响应不会出现 `completed_at`；该字段只在
`completed` 或 `failed` 终态出现。

## 11. 下载 MP4

任务进入 `completed` 后执行：

```bash
curl -fSL \
  -H "Authorization: Bearer {{api_key}}" \
  -H "Accept: video/mp4" \
  "{{base_url}}/v1/videos/{{task_id}}/content" \
  --output seed-dance-output.mp4

ffprobe \
  -v error \
  -show_streams \
  -show_format \
  -of json \
  seed-dance-output.mp4 | jq .
```

预期响应类型是 `video/mp4`，可验证视频流为 H.264、音频流为 AAC。任务未完成
时下载返回 `400 task_not_completed`；任务不存在或不属于当前用户时返回
`404 task_not_found`。

供应商内容接口的最小成功 JSON 可以只有：

```json
{
  "requestId": "REQUEST_ID",
  "video_base64": "BASE64_MP4"
}
```

`task_id`、`status`、`success` 等字段不是内容下载成功的必填项。New API 校验
严格标准 Base64 和 MP4 `ftyp` 后，向客户端流式返回解码后的 `video/mp4`，
不会把该供应商 JSON 或 Base64 回显给客户端。

## 12. Bash 自动提交、轮询和下载

以下示例保存提交响应中的公开 `id`，每 10 秒轮询一次，在 `completed` 或
`failed` 时退出轮询，并只在成功时下载 MP4：

```bash
set -euo pipefail

task_id="$(
  curl -fsS \
    -H "Authorization: Bearer {{api_key}}" \
    -H "Content-Type: application/json" \
    -d '{"model":"seedance-uncensored","prompt":"TEST_PROMPT","duration":1,"size":"1280x720"}' \
    "{{base_url}}/v1/videos" |
  jq -er '.id'
)"

printf 'public task id: %s\n' "$task_id"

while :; do
  response="$(
    curl -fsS \
      -H "Authorization: Bearer {{api_key}}" \
      "{{base_url}}/v1/videos/${task_id}"
  )"
  status="$(jq -er '.status' <<<"$response")"
  progress="$(jq -r '.progress // 0' <<<"$response")"
  printf 'status=%s progress=%s\n' "$status" "$progress"

  case "$status" in
    completed)
      break
      ;;
    failed)
      jq -r '.error.message // "video task failed"' <<<"$response" >&2
      exit 1
      ;;
    queued|in_progress)
      sleep 10
      ;;
    *)
      printf 'unexpected public status: %s\n' "$status" >&2
      exit 1
      ;;
  esac
done

curl -fSL \
  -H "Authorization: Bearer {{api_key}}" \
  -H "Accept: video/mp4" \
  "{{base_url}}/v1/videos/${task_id}/content" \
  --output seed-dance-output.mp4
```

## 13. Python 自动提交、轮询和下载

该示例只使用 Python 标准库：

```python
import json
import sys
import time
import urllib.error
import urllib.request


BASE_URL = "{{base_url}}".rstrip("/")
API_KEY = "{{api_key}}"


def request_json(method, path, payload=None):
    body = None
    headers = {
        "Authorization": f"Bearer {API_KEY}",
        "Accept": "application/json",
    }
    if payload is not None:
        body = json.dumps(payload).encode("utf-8")
        headers["Content-Type"] = "application/json"

    request = urllib.request.Request(
        f"{BASE_URL}{path}",
        data=body,
        headers=headers,
        method=method,
    )
    try:
        with urllib.request.urlopen(request, timeout=130) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        raw = error.read()
        try:
            detail = json.loads(raw)
        except json.JSONDecodeError:
            detail = {"error": {"message": raw.decode("utf-8", "replace")}}
        raise RuntimeError(
            f"HTTP {error.code}: {json.dumps(detail, ensure_ascii=False)}"
        ) from error


created = request_json(
    "POST",
    "/v1/videos",
    {
        "model": "seedance-uncensored",
        "prompt": "TEST_PROMPT",
        "duration": 1,
        "size": "1280x720",
    },
)
task_id = created["id"]
print(f"public task id: {task_id}")

while True:
    task = request_json("GET", f"/v1/videos/{task_id}")
    status = task["status"]
    print(f"status={status} progress={task.get('progress', 0)}")

    if status == "completed":
        break
    if status == "failed":
        message = task.get("error", {}).get("message", "video task failed")
        raise SystemExit(message)
    if status not in {"queued", "in_progress"}:
        raise SystemExit(f"unexpected public status: {status}")
    time.sleep(10)

download = urllib.request.Request(
    f"{BASE_URL}/v1/videos/{task_id}/content",
    headers={
        "Authorization": f"Bearer {API_KEY}",
        "Accept": "video/mp4",
    },
    method="GET",
)
try:
    with urllib.request.urlopen(download, timeout=130) as response:
        with open("seed-dance-output.mp4", "wb") as output:
            while chunk := response.read(1024 * 1024):
                output.write(chunk)
except urllib.error.HTTPError as error:
    sys.stderr.write(f"download failed with HTTP {error.code}\n")
    raise
```

## 14. 公开状态枚举

客户端只应依赖以下四种公开状态：

| 公开状态 | 含义 | 客户端行为 |
|---|---|---|
| `queued` | 已接受或正在排队 | 10 秒后继续轮询 |
| `in_progress` | 正在生成 | 10 秒后继续轮询 |
| `completed` | 已完成 | 下载 MP4 |
| `failed` | 失败终态 | 停止轮询并读取 `error` |

供应商的 `accepted` 和防御兼容的 `queued` 会映射为公开 `queued`；
`running` 和 `processing` 会映射为公开 `in_progress`；`completed`、`failed`
分别映射为同名公开终态。客户端不应依赖供应商状态或供应商任务 ID。

## 15. 嵌套错误响应

三个 OpenAI 视频路由都使用嵌套 `error` 对象，并包含必需的 `message`、
`type` 和 `code`。

参数错误示例：

```http
HTTP/1.1 400 Bad Request
Content-Type: application/json
```

```json
{
  "error": {
    "message": "duration must be between 1 and 15",
    "type": "invalid_request_error",
    "code": "invalid_duration"
  }
}
```

任务不存在或不属于当前用户时：

```http
HTTP/1.1 404 Not Found
Content-Type: application/json
```

```json
{
  "error": {
    "message": "video task not found",
    "type": "invalid_request_error",
    "code": "task_not_found"
  }
}
```

常见映射：

| 场景 | HTTP | `error.type` | `error.code` |
|---|---:|---|---|
| malformed JSON、`model` 非字符串或缺失 | 400 | `new_api_error` | 空字符串 |
| 参数缺失、冲突或越界 | 400 | `invalid_request_error` | 对应稳定参数 code |
| 模型价格未配置 | 400 | `invalid_request_error` | `model_price_error` |
| 模型没有可用渠道 | 503 | `new_api_error` | `model_not_found` |
| 客户端 API Key 无效 | 401 | `new_api_error` | 空字符串 |
| 任务不存在或越权 | 404 | `invalid_request_error` | `task_not_found` |
| 任务未完成时下载 | 400 | `invalid_request_error` | `task_not_completed` |
| 提交时供应商返回 401 | 401 | `invalid_request_error` | `upstream_authentication_error` |
| 提交时供应商返回 403 | 403 | `invalid_request_error` | `upstream_authentication_error` |
| 提交时收到可明确安全重试的供应商限流 | 429 | `rate_limit_error` | `upstream_rate_limit_error` |
| 提交结果不确定（超时、连接中断、502/503/504 等） | 502 | `server_error` | `seedance_submit_outcome_unknown` |
| 提交 HTTP 200 但供应商显式业务失败 | 502 | `server_error` | `upstream_error` |
| 查询任务数据库失败 | 500 | `server_error` | `get_task_failed` |
| 下载时供应商认证失败 | 502 | `upstream_error` | `upstream_authentication_error` |
| 视频下载接口收到供应商限流 | 429 | `upstream_rate_limit_error` | `upstream_rate_limit_error` |
| 下载时供应商连接异常 | 502 | `upstream_error` | `upstream_connection_error` |
| 供应商下载超时 | 504 | `upstream_timeout_error` | `upstream_timeout_error` |
| 供应商 JSON、Base64 或 MP4 无效 | 502 | `invalid_upstream_response` | `invalid_upstream_response` |

错误响应不会包含供应商 Key、供应商任务 ID、完整供应商响应、Base64、内部堆栈
或服务器文件路径。

## 16. Apifox 导入

仓库提供两种可导入制品，适用于不同用途：

| 文件 | Apifox 导入类型 | 用途 |
|---|---|---|
| `docs/api/seed-dance-openapi.yaml` | OpenAPI/Swagger | 导入接口定义、请求/响应 schema、示例和错误模型 |
| `docs/api/seed-dance-apifox.postman_collection.json` | Postman | 导入人工验收请求、测试脚本和自动保存 `task_id` 的逻辑 |

### 16.1 导入接口定义

1. 在 Apifox 中选择「导入数据」→「OpenAPI/Swagger」；
2. 导入 `docs/api/seed-dance-openapi.yaml`；
3. 创建环境并配置 `base_url`、`api_key` 和 `task_id`；
4. 对三个接口使用 Bearer Auth，Token 填写 `{{api_key}}`；
5. 如果只需要浏览接口和 schema，到此即可。

### 16.2 导入人工验收集合

1. 在 Apifox 中选择「导入数据」→「Postman」；
2. 导入 `docs/api/seed-dance-apifox.postman_collection.json`；
3. 在当前 Apifox 环境中创建同名的 `base_url` 和 `api_key` 变量；把真实
   Token 只填写到 `api_key` 的本地值，不要写入团队共享远程值，也不要把
   修改后的真实 Token 重新导出或提交；
4. 如需 JSON Base64 I2V，把 `image_base64` 替换为有效 JPG/PNG 的纯
   Base64；
5. 如需 multipart I2V，打开对应请求并重新选择本机 JPG/PNG 文件，不要
   手动添加 multipart `Content-Type`；
6. 先执行「服务状态」，确认服务可访问；
7. 从三个创建请求中只选择一个手工执行。成功脚本会校验创建响应，并把
   `id`、计费时长和计费分辨率快照同时保存到 Collection 和当前
   Environment；
8. 每约 10 秒手工重复执行「查询任务状态」，直到状态为 `completed` 或
   `failed`；脚本会确认 `seconds` 和 `size` 在长任务轮询期间没有漂移；
9. 只有状态为 `completed` 时执行「下载完成视频」，确认响应是非空
   `video/mp4`，并检查下载文件名、缓存策略和 `nosniff` 响应头。

「01 - 创建视频任务（会产生费用）」中的三个请求都会创建真实供应商任务。
不要对整个集合或该目录使用 Runner；一次人工验收只执行需要的一个创建请求。

「03 - 无费用参数拒绝用例」应在本地参数验证阶段返回 `400`，不存在任务查询
应返回 `404`。请逐个手工执行；如果任何拒绝用例返回 `2xx`，立即停止后续
POST 验证并检查渠道路由与参数验证配置。

| Apifox 变量名 | 模板值 | 用途 |
|---|---|---|
| `base_url` | `http://TARGET_HOST` | New API 基础地址，不包含尾部 `/` |
| `api_key` | `NEW_API_TOKEN` | New API 客户端 API Key，不是供应商渠道 Key |
| `task_id` | `TASK_ID` | 创建成功后自动保存的 New API 公开任务 ID |
| `model` | `seedance-uncensored` | 渠道模型名 |
| `prompt` | 合成测试提示词 | T2V/I2V 测试提示词 |
| `negative_prompt` | 合成负面提示词 | `metadata.negative_prompt` |
| `duration` | `1` | 测试时长，允许范围为 1–15 秒 |
| `size` | `1280x720` | T2V 测试分辨率 |
| `image_base64` | `IMAGE_BASE64` | JSON I2V 使用的 JPG/PNG 纯 Base64 |
| `input_image_path` | `/ABSOLUTE/PATH/TO/INPUT.png` | multipart I2V 本机图片路径；导入后重新选文件 |
| `expected_seconds` | 自动保存 | 创建响应中的计费时长快照 |
| `expected_size` | 自动保存 | 创建响应中的计费分辨率快照 |
| `task_status` | 自动保存 | 最近一次有效状态查询返回的状态 |

Collection 中的脚本会验证创建、状态和下载响应合同，但不会自动递归轮询，也
不会自动执行任何创建请求。导入成功只验证发布制品可被 Apifox 使用，不能
替代 Redocly lint、Postman Collection schema 验证和 Go 合同测试。

## 17. 合成 fixtures

`docs/api/fixtures/seed-dance/` 中包含：

| 文件 | 内容 |
|---|---|
| `generate-response.json` | 合成的提交成功响应 |
| `status-accepted.json` | 合成的 `accepted` 状态 |
| `status-processing.json` | 合成的 `processing` 状态 |
| `status-completed.json` | 合成的 `completed` 状态 |
| `status-business-error.json` | HTTP 200 业务错误 |
| `video-response-minimal.json` | 仅含 `requestId` 和合成最小 MP4 Base64 的内容成功响应 |
| `ffprobe-output.json` | 不含绝对路径的合成 H.264/AAC 媒体描述 |

这些文件只用于确定性 Mock 和文档复核，不包含请求头、凭据、真实任务 ID、
图片 Base64 或真实视频。`video-response-minimal.json` 中的 `video_base64`
只编码一个确定性的合成 `ftyp` box，用于锁定供应商最小响应契约。

可执行：

```bash
for fixture in docs/api/fixtures/seed-dance/*.json; do
  jq -e . "$fixture" >/dev/null
done
```

## 18. 响应大小与费用边界

- 截至当前供应商资料版本，没有文档化的供应商 JSON 响应、视频 Base64 或
  解码后 MP4 最大值；
- New API 因此不增加 Seed Dance 专用的 JSON、Base64 或 MP4 大小预限制；
- 这不取消平台通用的并发、磁盘、请求期限和部署资源保护；
- 下载实现按流处理供应商 JSON 字段、Base64 解码和临时 MP4，不要求把完整
  JSON、完整 Base64 和完整视频同时保存在内存中；
- 实际 MP4 时长不重新计算费用；费用只使用提交前规范化并写入任务计费快照的
  请求时长、分辨率和 GroupRatio。

## 19. 故障排查

### 渠道保持不可用

- 确认 Type 59 渠道仍处于预期状态；禁用渠道不会被调度；
- 确认 `seedance-uncensored` 已加入渠道模型和模型能力；
- 确认 Base URL、渠道 Key 和渠道代理已保存；
- 先运行 Type 59 专用连通性测试；该测试不应创建任务或产生费用；
- 未满足生产传输门禁时，维持禁用并使用 Mock 验证，不发送真实媒体。

### 返回 `model_price_error`

- 配置 `ModelPrice["seedance-uncensored"]`，或在没有 `ModelPrice` 时配置
  `ModelRatio["seedance-uncensored"]`；
- 两者同时存在时按 `ModelPrice` 排查；
- 检查测试用户的最终 GroupRatio，特殊分组倍率可能覆盖普通倍率。

### 返回 `invalid_duration` 或 `invalid_resolution`

- 时长必须是 `1–15` 的整数或十进制数字字符串；
- 不要让 `duration`、`seconds` 与 `metadata.duration` 互相冲突；
- 不要让 `size` 与 `metadata.resolution` 互相冲突；
- T2V 使用 `720P` 或 `1080P`，`480P` 仅用于 I2V。

### 图片请求失败

- 确认格式为 JPG/PNG、宽高为 `240–8000`、比例为 `1:8–8:1`；
- 确认只提供一张图片；多个来源只有在解码后字节完全相同时才会去重；
- data URI 只能使用 JPG/PNG 前缀；
- 远程 URL 的每次重定向都必须通过 SSRF 防护；
- 5 MB 只是供应商建议，不要据此诊断为 New API 的 Seed Dance 专用硬限制。

### 长时间停留在 `queued` 或 `in_progress`

- 保持至少 5 秒的轮询间隔，推荐 10 秒；
- 客户端 GET 只读取已持久化的任务快照，不会同步触发供应商查询；默认
  `async_task_poll` 调度与执行周期约为 15 秒，提交后数秒仍为 `queued` 属于
  正常现象；
- 超过 30–60 秒完全不变化时，检查 `UPDATE_TASK`、系统任务
  `async_task_poll`、供应商 `/status/{task_id}` 请求和 durable ledger；
- 查看管理员日志中的安全错误 code，不记录完整供应商响应；
- 瞬时网络错误会保留原状态并在后续调度周期重试；
- 不要使用供应商任务 ID 查询公开接口，也不要重新 POST 来代替轮询，否则会
  创建新的付费任务。

### 下载失败

- 只在 `completed` 后请求内容；
- `404 task_not_found` 时确认使用同一用户的公开任务 ID；
- `502 invalid_upstream_response` 表示供应商 JSON、Base64 或 MP4 校验失败；
- `504 upstream_timeout_error` 表示内容阶段超时；
- 下载成功后用 `ffprobe` 验证 MP4、H.264、AAC 和分辨率；
- 不要添加供应商资料未声明的 Seed Dance 专用 JSON/Base64/MP4 阈值来掩盖
  上游或部署资源问题。

### 费用与实际媒体时长不一致

按请求时长、分辨率倍率、最终 GroupRatio 和提交时价格快照复算。最终 MP4
时长不是结算输入，因此不会触发补扣或退款。
