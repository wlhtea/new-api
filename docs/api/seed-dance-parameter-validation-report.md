# Seed Dance 全参数验证报告

验证日期：2026-07-26

公开模型：`seedance-uncensored`

渠道类型：Type 59「无审核 Seed Dance」

公开目标：`http://TARGET_HOST`

凭据占位符：`NEW_API_TOKEN`、`SUPPLIER_KEY`

## 1. 结论

Seed Dance MVP 已完成以下四层验证：

1. **请求规范化**：JSON、multipart、别名、类型、默认值、上下界、冲突、
   图片格式与尺寸均有自动化矩阵；
2. **公开 HTTP 契约**：43 个不会提交上游任务的错误用例在部署实例上全部
   通过，认证、错误码和未知任务行为符合契约；
3. **真实供应商链路**：T2V 和 I2V 均完成提交、长轮询、终态转换及 MP4
   下载；
4. **计费**：真实 I2V 任务按 `ModelPrice × GroupRatio × duration ×
   resolutionRatio` 的两阶段向零截断规则精确扣除 `375000` quota。

New API 已正确规范化、转发和计费请求参数。真实媒体同时表明：供应商可能不
严格兑现请求的时长或输出像素；New API 不根据下载后媒体重新计费。

## 2. 验证分层

| 层次 | 目的 | 结果 |
|---|---|---|
| Go 参数矩阵 | 穷举输入类型、边界、别名和冲突 | 17 个顶层测试、298 个子测试 PASS 事件 |
| OpenAPI 合同测试 | 锁定 Apifox 导入结构、示例和错误响应 | 通过 |
| 公网无费用矩阵 | 验证部署实例的 4xx/认证/任务隔离 | 43/43 通过 |
| 真实 T2V | 验证 JSON、轮询、内容下载和 720P 计费 | 通过 |
| 真实 I2V | 验证 multipart、PNG、480P、扩展字段和计费 | 通过 |
| 媒体探测 | 验证 MP4、编解码器、尺寸和实际时长 | 通过 |
| 数据库核账 | 验证任务数、任务 quota 和计费快照 | 精确匹配 |

## 3. 参数合同

### 3.1 基础字段

| 字段 | 接受值 | 默认/规则 |
|---|---|---|
| `model` | 精确字符串 `seedance-uncensored` | 大小写敏感，不接受前后空白 |
| `prompt` | string | trim 后必须至少有一个非空白字符 |
| 未知 JSON/text 字段 | 任意 | 接受但不转发给供应商 |
| 未知 multipart 文件字段 | file | 拒绝 |

### 3.2 时长

时长有三个等价入口：

- 顶层 `duration`；
- 顶层 `seconds`；
- `metadata.duration`。

三个入口都接受 JSON integer 或只含 ASCII 十进制数字的 string。有效范围为
`1–15`，默认值为 `15`。`"01"` 会规范化为 `1`；多入口同时提供时，解析后
必须相等。`null` 视为未提供。

已覆盖的拒绝值包括：

- `0`、`16`、负数；
- 小数、指数形式和整数溢出；
- 空字符串、空白字符串、带正负号字符串、字母字符串；
- boolean、array、object；
- 任意两个或三个入口之间的冲突。

### 3.3 分辨率与倍率

`size` 和 `metadata.resolution` 使用同一组 alias，匹配时 trim 且忽略
大小写：

| 输入 alias | 规范值 | 计费倍率 |
|---|---|---:|
| `854x480`、`480x854`、`480P` | `480P` | 0.5 |
| `1280x720`、`720x1280`、`720P` | `720P` | 1.0 |
| `1920x1080`、`1080x1920`、`1080P` | `1080P` | 2.25 |

默认分辨率为 `720P`。两个入口同时提供时必须规范化为相同值。T2V 拒绝
`480P`；I2V 接受 `480P`、`720P` 和 `1080P`。

### 3.4 图片入口

最终只允许一张图片，可通过以下五类入口提供：

- 顶层 `image`；
- 单元素 `images`；
- 顶层 `input_reference`；
- `metadata.image_base64`；
- multipart 文件字段 `input_reference`。

multipart 还接受 `image`、`images` 或 `input_reference` 文本字段。字符串
图片可为严格标准 Base64、JPG/PNG data URI，或通过平台 SSRF 防护的
HTTP(S) URL。

图片合同：

- 仅 JPEG/PNG；
- 宽高均为 `240–8000`；
- 宽高比为 `1:8–8:1`；
- 多入口包含相同解码字节时去重；
- 多入口内容不同或 `images` 超过一个元素时拒绝；
- 根据实际图片字节识别格式，不信任文件名或 multipart 声明 MIME；
- Base64 拒绝 URL-safe alphabet、非法 padding/pad bits 以及内嵌
  space、tab、CR、LF、CRLF。

### 3.5 供应商扩展字段

| `metadata` 字段 | JSON | multipart metadata | 省略行为 |
|---|---|---|---|
| `prompt_optimization` | boolean | string `"true"` / `"false"` | 不注入默认值 |
| `multi_shot` | boolean | string `"true"` / `"false"` | 不注入默认值 |
| `strict_duration` | boolean | string `"true"` / `"false"` | 不注入默认值 |
| `negative_prompt` | string | string | 空字符串不发给供应商 |

JSON 中三个布尔字段的 string、number、array 和 object 形式均拒绝。
multipart metadata 中真正的 JSON boolean 也拒绝，必须使用文本
`"true"` 或 `"false"`。

## 4. 公网无费用矩阵

执行前 Type 59 数据库基线：

```text
task_count=6
max_task_id=6
quota_sum=4500000
```

执行结果：

```text
passed=43
failed=0
unexpected_success=false
```

执行后数据库：

```text
task_count=6
max_task_id=6
quota_sum=4500000
```

因此 43 个失败用例全部在提交供应商前结束，没有创建任务或产生费用。覆盖：

- malformed JSON、metadata 类型、prompt 缺失/空白/错误类型；
- duration 三入口的边界、类型和冲突；
- resolution 类型、alias 冲突与 T2V 480P；
- metadata 布尔值和 negative prompt 类型；
- 非法 Base64/data URI、图片尺寸、图片容器和多入口冲突；
- multipart 重复字段、非法 metadata、空文件、未知文件字段和多个文件；
- 未知任务、缺失 Token 和无效 Token。

验证脚本在首个不符合预期的 POST 响应处立即停止整个 POST 矩阵；该规则同时
适用于意外 2xx 和提交结果可能不确定的 5xx，避免在异常状态下继续发送可能
产生费用的请求。

脱敏原始结果保存在本机：

```text
/tmp/seedance-invalid-live-report.json
```

## 5. 真实 T2V

复用已完成的 1 秒、720P JSON T2V 任务进行状态和内容验证：

```text
requested duration=1
requested size=1280x720
strict_duration=true
public status=completed
content HTTP=200
Content-Type=video/mp4
bytes=1013854
MP4 ftyp=true
video codec=h264
video width=1280
video height=720
audio codec=aac
actual duration=2.020000
```

提交后的公开生命周期实测为：

```text
queued(10) -> in_progress(30) -> completed(100)
```

请求为 `duration=1` 且 `strict_duration=true`，供应商实际输出约
`2.02` 秒。该偏差发生在供应商生成结果，不改变 New API 的请求计费快照。

## 6. 真实 multipart I2V

唯一新增的付费 I2V 请求合并验证了尽可能多的有效参数：

```text
model=seedance-uncensored
duration="1"
seconds="1"
size="854x480"
metadata.duration="01"
metadata.resolution="480p"
metadata.prompt_optimization="true"
metadata.multi_shot="false"
metadata.strict_duration="false"
metadata.negative_prompt="blur"
input_reference=240x240 synthetic PNG
```

提交响应：

```text
HTTP=200
status=queued
progress=10
seconds="1"
size="480P"
completed_at absent
```

轮询结果：

```text
queued(10) -> in_progress(30) -> completed(100)
terminal elapsed=220.4 seconds
all non-terminal responses omitted completed_at
terminal response included completed_at
```

内容响应：

```text
HTTP=200
Content-Type=video/mp4
Content-Length=47077
actual bytes=47077
MP4 ftyp=true
SHA-256=99c2848022cc15d3336cbb706a172cf1ebd00fd6f4b401aedf90591cacc99ba4
```

`ffprobe`：

```text
format=mov,mp4,m4a,3gp,3g2,mj2
video codec=h264
video width=240
video height=240
audio stream=absent
actual duration=5.062012
```

供应商接受了规范化为 `480P`、`1` 秒的请求，但实际输出保持输入图片的
`240×240` 尺寸且约为 `5.06` 秒。这里记录真实上游行为，不在 New API 侧
伪造媒体尺寸或按实际媒体时长二次扣费。

脱敏原始结果和媒体保存在本机：

```text
/tmp/seedance-i2v-live-report.json
/tmp/seedance-i2v-TASK_ID_I2V.mp4
```

## 7. 精确计费验证

部署实例的真实配置和任务快照：

```text
ModelPrice["seedance-uncensored"]=1.5
GroupRatio=1
duration ratio=1
resolution ratio=0.5
QuotaPerUnit=500000
```

实现分两阶段向零截断：

```text
base_quota = truncate_toward_zero(1.5 × 500000 × 1) = 750000
quota      = truncate_toward_zero(750000 × 1 × 0.5) = 375000
```

真实 I2V 任务记录：

```text
status=SUCCESS
progress=100%
quota=375000
billing_context.model_price=1.5
billing_context.group_ratio=1
billing_context.other_ratios={"seconds":1,"resolution":0.5}
```

数据库变化：

```text
before: task_count=6, max_task_id=6, quota_sum=4500000
after:  task_count=7, max_task_id=7, quota_sum=4875000
delta:  task_count=1, quota_sum=375000
```

当 `ModelPrice` 未配置而存在 `ModelRatio=M` 时，兼容模式先计算
`truncate(M / 2 × QuotaPerUnit × GroupRatio)`，再应用 duration 和
resolution 倍率。`ModelPrice` 与 `ModelRatio` 同时存在时前者优先。

## 8. 任务与内容响应

公开状态映射：

| 供应商状态 | 公开状态 | progress |
|---|---|---:|
| `accepted` | `queued` | 10 |
| `queued` | `queued` | 20 |
| `running` / `processing` | `in_progress` | 30 |
| `completed` | `completed` | 100 |
| `failed` | `failed` | 100 |

`completed_at` 只在 `completed` 或 `failed` 终态输出。失败终态包含：

```json
{
  "error": {
    "code": "task_failed",
    "message": "SANITIZED_FAILURE_REASON"
  }
}
```

内容下载接受供应商最小成功 envelope：

```json
{
  "requestId": "REQUEST_ID",
  "video_base64": "BASE64_MP4"
}
```

`success` 缺失是合法成功形式；显式 `success:false`、`status:"failed"` 或
非零 `errCode` 仍会拒绝。内容代理继续执行严格 Base64 和 MP4 `ftyp` 校验。

## 9. 官方资料边界

用户提供的供应商 DOCX 明确记录：

- 图片最小/最大宽高 `240–8000`，宽高比 `1:8–8:1`；
- JPG/PNG，5 MB 仅为“建议”；
- 时长表为 `1–15`；
- `480P` 仅用于 I2V，另有 `720P`、`1080P`；
- `prompt_optimization`、`multi_shot`、`strict_duration` 和
  `negative_prompt`；
- 状态建议每 `5–10` 秒轮询；
- 完成后通过 `/video/{task_id}` 获取 `video_base64`。

资料没有规定以下 Seed Dance 专用硬限制：

```text
最大 JSON 响应 256 MiB
最大解码视频 192 MiB
```

因此实现没有添加这两个预判上限。图片 5 MB 同样保留为建议，不转成渠道专用
硬限制；平台通用请求体、远程资源和部署资源限制仍照常生效。

## 10. 验证中发现并修复的问题

1. **内容 envelope 兼容性**：真实供应商成功响应没有 `success:true`，旧逻辑
   错误返回 502；现允许 `success` 缺失，同时保留显式失败和媒体校验。
2. **非终态 `completed_at`**：提交结算更新 `updated_at` 曾被误当完成时间；
   现仅终态输出。
3. **严格 Base64**：Go `base64.StdEncoding.Strict()` 仍会忽略 CR/LF；现先
   规范化字符串两端空白，再显式拒绝 Base64 载荷内部的全部 ASCII 空白，并
   新增 CR、LF、CRLF 回归矩阵。
4. **OpenAPI 漂移**：duration string、九个 resolution alias、multipart 文本
   图片入口、错误状态、进度示例和终态字段均已与运行时对齐。

## 11. 可复现工件

仓库内：

```text
docs/api/seed-dance-openapi.yaml
docs/api/seed-dance-testing.md
docs/api/seed-dance-parameter-validation.py
docs/api/openapi_contract_test.go
docs/api/fixtures/seed-dance/video-response-minimal.json
relay/channel/task/seedance/parameter_matrix_test.go
```

OpenAPI 文件可直接导入 Apifox。导入后设置：

```text
base_url=http://TARGET_HOST
api_key=NEW_API_TOKEN
task_id=PUBLIC_TASK_ID
```

所有报告、fixture、测试输出和 Git 变更均不保存 SSH 密码、New API Token、
供应商 Key、真实 Authorization header 或供应商任务 ID。
