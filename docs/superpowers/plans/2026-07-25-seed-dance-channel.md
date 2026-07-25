# Seed Dance Channel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 New API 增加 Type 59“无审核 Seed Dance”异步视频渠道，完整支持请求规范化、计费、任务轮询、MP4 内容下载、前端配置、Apifox 文档和可回滚部署。

**Architecture:** 使用独立 `relay/channel/task/seedance` 适配供应商的 `/generate`、`/status/{id}`、`/video/{id}`，核心层只增加向后兼容的延迟成功响应、提交失败分类、context-aware polling 和通用视频内容获取接口。任务公开 ID、供应商 ID、选中 Key 和计费快照分层保存；内容响应先流式落入 `0600` 临时文件并完成 JSON、Base64、MP4 全量验证，再发送 200。

**Tech Stack:** Go 1.25.1、Gin、GORM v2、SQLite/MySQL/PostgreSQL、React 19、TypeScript、React Hook Form、Zod、Bun、OpenAPI 3.0.3/3.0.1、Docker Compose。

## Global Constraints

- 实现基线为 `84a79b6807ac1a679ca86f34c8c6f39175c294d8`，工作分支固定为 `codex/seed-dance-channel`。
- 渠道类型固定为 `ChannelTypeSeedDance = 59`，`ChannelTypeDummy` 顺延到 60 并继续作为最后一个枚举。
- 后端渠道名固定为 `seed-dance`，前端 i18n key 固定为 `Uncensored Seed Dance`，公开模型固定为 `seedance-uncensored`。
- 客户端接口固定为 `POST /v1/videos`、`GET /v1/videos/{task_id}`、`GET /v1/videos/{task_id}/content`。
- 供应商接口固定为 `POST /generate`、`GET /status/{upstream_task_id}`、`GET /video/{upstream_task_id}`。
- 所有 JSON 编解码必须调用 `common.Marshal`、`common.Unmarshal`、`common.UnmarshalBodyReusable` 或 `common.DecodeJson`；业务代码只可引用 `json.RawMessage` 等类型，不直接调用标准库 JSON 编解码函数。
- 所有数据库改动必须同时兼容 SQLite、MySQL 5.7.8+ 和 PostgreSQL 9.6+；本功能不新增表、列或迁移。
- `ModelPrice` 优先于 `ModelRatio`；初始运行时价格为 `ModelPrice["seedance-uncensored"]=0.15`，不得写入项目默认价格表，不得加入 `TASK_PRICE_PATCH`。
- 固定价格 quota 公式为 `ModelPrice × duration × resolutionRatio × resolvedGroupRatio × 500000`；ModelRatio 兼容公式为 `ModelRatio / 2 × duration × resolutionRatio × resolvedGroupRatio × 500000`。
- 分辨率倍率固定为 `480P=0.5`、`720P=1.0`、`1080P=2.25`；时长必须在计费前验证为整数 `1–15`。
- 供应商图片 `5 MB` 只作为文档建议，不增加 Seed Dance 专用 5 MB/5 MiB 输入拒绝逻辑；继续服从平台通用远程资源配置。
- 不增加 Seed Dance 专用 JSON、Base64 或 MP4 大小阈值，不恢复此前被否决的猜测阈值；通过流式磁盘处理避免完整视频响应进入内存。
- 提交、状态、内容 deadline 分别为 60 秒、30 秒、120 秒，连接建立 deadline 为 10 秒；必须保留渠道 HTTP/SOCKS 代理行为。
- `POST /generate` 没有幂等键；结果不确定时绝不自动重试。只有明确 429、无任务 ID 且业务响应确认未创建任务时才允许切换渠道。
- 客户端只看到 `task_xxx`；供应商任务 ID 只保存到 `Task.PrivateData.UpstreamTaskID`，提交实际选中的单 Key 只保存到 `Task.PrivateData.Key`。
- OpenAI 三个视频路由使用嵌套 `{error:{message,type,code}}`；旧 `/v1/video/generations/...` 保持现有扁平错误和 400 兼容行为。
- 真实生产流量必须满足 HTTPS、双方互信私网、VPN/等价加密隧道或可信边界内 TLS 终止代理中的至少一项；门禁不满足时 Type 59 保持禁用，不通过普通公网 HTTP 发送真实 Key 或媒体。
- 代码、测试、文档、fixture、Git 历史和命令不得包含真实 SSH 密码、供应商 Key、New API Token、可复用供应商任务 ID 或真实 Base64。
- 新增或大幅修改的 Go 测试使用 `testify/require` 和 `testify/assert`；前端测试保护用户可见行为，不断言脆弱源码字符串。

## Execution Preflight

The local Darwin/arm64 workstation has a user-scoped Go 1.25.1 toolchain and Bun 1.3.13
exposed through `/opt/homebrew/bin`. Before Task 1, run:

```bash
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:$PATH"
test "$(command -v go)" = /opt/homebrew/bin/go
test "$(command -v gofmt)" = /opt/homebrew/bin/gofmt
test "$(command -v bun)" = /opt/homebrew/bin/bun
test "$(go version)" = "go version go1.25.1 darwin/arm64"
test "$(bun --version)" = "1.3.13"
command -v docker
command -v gh
command -v ssh
command -v curl
```

If the user-scoped Go link has been removed, restore the already-provisioned toolchain:

```bash
GO_ROOT="$HOME/.cache/codex-toolchains/go1.25.1"
test -x "$GO_ROOT/bin/go"
test -x "$GO_ROOT/bin/gofmt"
ln -sfn "$GO_ROOT/bin/go" /opt/homebrew/bin/go
ln -sfn "$GO_ROOT/bin/gofmt" /opt/homebrew/bin/gofmt
```

If the Bun link has been removed, restore it from the installed local runtime:

```bash
BUN_SOURCE="$HOME/Library/Application Support/kiro-cli/bun"
test -x "$BUN_SOURCE"
ln -sfn "$BUN_SOURCE" /opt/homebrew/bin/bun
```

Use `bun x`, not a separately installed wrapper, for Redocly. Stop before changing code if
any preflight assertion fails; do not silently downgrade the Go version or skip a gate.

---

### Task 1: Channel Constants and Backward-Compatible Contracts

**Files:**
- Modify: `constant/channel.go`
- Modify: `common/endpoint_type.go`
- Modify: `relay/channel/adapter.go`
- Modify: `dto/task.go`
- Create: `constant/channel_test.go`
- Create: `common/endpoint_type_test.go`
- Create: `dto/task_retry_test.go`

**Interfaces:**
- Consumes: 现有 `constant.EndpointTypeOpenAIVideo`、`model.TaskStatus`、`relaycommon.RelayInfo`、`dto.TaskError`。
- Produces: `constant.ChannelTypeSeedDance`; `channel.FullPrepaidTaskSubmitter`; `channel.DurableTaskSubmitter`; `channel.DeferredTaskSubmitResponder`; `channel.TaskSubmitFailureClassification`; `channel.TaskSubmitFailureClassifier`; `channel.VideoContentFetcher`; `channel.VideoContent`; `channel.VideoContentError`; `dto.TaskError.Retryable`.

- [ ] **Step 1: Write failing channel and endpoint contract tests**

```go
// constant/channel_test.go
package constant

import (
    "testing"

    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestSeedDanceChannelConstantContract(t *testing.T) {
    require.Equal(t, 59, ChannelTypeSeedDance)
    assert.Equal(t, 60, ChannelTypeDummy)
    require.Len(t, ChannelBaseURLs, ChannelTypeDummy)
    assert.Equal(t,
        "http://alb-o13xqj8f2cpjsa67ym.ap-northeast-1.alb.aliyuncsslbintl.com/v1/public_api/m-predict/polar4ai-i2v",
        ChannelBaseURLs[ChannelTypeSeedDance],
    )
    assert.Equal(t, "SeedDance", GetChannelTypeName(ChannelTypeSeedDance))
}
```

```go
// common/endpoint_type_test.go
package common

import (
    "testing"

    "github.com/QuantumNous/new-api/constant"
    "github.com/stretchr/testify/assert"
)

func TestSeedDanceOnlyAdvertisesOpenAIVideo(t *testing.T) {
    assert.Equal(t,
        []constant.EndpointType{constant.EndpointTypeOpenAIVideo},
        GetEndpointTypesByChannelType(
            constant.ChannelTypeSeedDance,
            "seedance-uncensored",
        ),
    )
}
```

- [ ] **Step 2: Run the tests and verify the new symbols fail to compile**

Run:

```bash
go test ./constant ./common -run 'TestSeedDance' -count=1
```

Expected: FAIL because `ChannelTypeSeedDance` is undefined and Type 59 has no endpoint mapping.

- [ ] **Step 3: Add Type 59 and the endpoint mapping**

Add the actual constant immediately after Type 58:

```go
ChannelTypeAdvancedCustom = 58
ChannelTypeSeedDance      = 59
ChannelTypeDummy               // count sentinel; keep last
```

Append index 59 to `ChannelBaseURLs`:

```go
"http://alb-o13xqj8f2cpjsa67ym.ap-northeast-1.alb.aliyuncsslbintl.com/v1/public_api/m-predict/polar4ai-i2v", // 59
```

Add the name:

```go
ChannelTypeSeedDance: "SeedDance",
```

Add the endpoint switch branch:

```go
case constant.ChannelTypeSeedDance:
    endpointTypes = []constant.EndpointType{
        constant.EndpointTypeOpenAIVideo,
    }
```

- [ ] **Step 4: Write failing JSON-hiding and optional-interface tests**

```go
// dto/task_retry_test.go
package dto

import (
    "testing"

    "github.com/QuantumNous/new-api/common"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func TestTaskErrorRetryableIsInternalOnly(t *testing.T) {
    retryable := false
    body, err := common.Marshal(TaskError{
        Code:       "seedance_submit_outcome_unknown",
        Message:    "submit result is unknown",
        StatusCode: 502,
        Retryable:  &retryable,
    })
    require.NoError(t, err)
    assert.JSONEq(t,
        `{"code":"seedance_submit_outcome_unknown","message":"submit result is unknown","data":null}`,
        string(body),
    )
}
```

Run:

```bash
go test ./dto -run TestTaskErrorRetryableIsInternalOnly -count=1
```

Expected: FAIL because `TaskError.Retryable` is undefined.

- [ ] **Step 5: Add the optional contracts and structured content error**

Add `context` to `relay/channel/adapter.go` imports, then add:

```go
type TaskSubmitHTTPResponse struct {
    StatusCode      int
    Body            any
    InitialStatus   model.TaskStatus
    InitialProgress string
}

type DeferredTaskSubmitResponder interface {
    BuildTaskSubmitResponse(
        info *relaycommon.RelayInfo,
        taskData []byte,
    ) (*TaskSubmitHTTPResponse, error)
}

type FullPrepaidTaskSubmitter interface {
    RequiresFullPrepaidBilling() bool
}

type DurableTaskSubmitter interface {
    RequiresDurableTaskBeforeSubmit() bool
}

type TaskSubmitFailureClassification struct {
    TaskError      *dto.TaskError
    UpstreamTaskID string
    TaskData       []byte
}

type TaskSubmitFailureClassifier interface {
    ClassifyTaskSubmitFailure(
        resp *http.Response,
        requestErr error,
    ) *TaskSubmitFailureClassification
}

type VideoContent struct {
    ContentType   string
    ContentLength int64
    Body          io.ReadCloser
}

type VideoContentFetcher interface {
    FetchVideoContent(
        ctx context.Context,
        baseURL string,
        key string,
        upstreamTaskID string,
        proxy string,
    ) (*VideoContent, error)
}

type VideoContentError struct {
    StatusCode int
    Type       string
    Code       string
    Message    string
    Cause      error
}

func (e *VideoContentError) Error() string {
    if e == nil {
        return ""
    }
    if e.Message != "" {
        return e.Message
    }
    if e.Cause != nil {
        return e.Cause.Error()
    }
    return e.Code
}

func (e *VideoContentError) Unwrap() error {
    if e == nil {
        return nil
    }
    return e.Cause
}
```

Add the internal field to `dto.TaskError`:

```go
Retryable *bool `json:"-"`
```

- [ ] **Step 6: Run focused tests and formatting**

Run:

```bash
gofmt -w constant/channel.go constant/channel_test.go \
  common/endpoint_type.go common/endpoint_type_test.go \
  relay/channel/adapter.go dto/task.go dto/task_retry_test.go
go test ./constant ./common ./dto -run 'TestSeedDance|TestTaskErrorRetryable' -count=1
go test ./relay/channel -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add constant/channel.go constant/channel_test.go \
  common/endpoint_type.go common/endpoint_type_test.go \
  relay/channel/adapter.go dto/task.go dto/task_retry_test.go
git commit -m "feat: add Seed Dance channel contracts"
```

---

### Task 2: Raw JSON Scalars, Duration, Resolution, and Metadata

**Files:**
- Create: `relay/channel/task/seedance/constants.go`
- Create: `relay/channel/task/seedance/dto.go`
- Create: `relay/channel/task/seedance/normalize.go`
- Create: `relay/channel/task/seedance/normalize_test.go`

**Interfaces:**
- Consumes: `common.UnmarshalBodyReusable`; `common.Unmarshal`; `json.RawMessage` as a type; `relaycommon.MaxTaskDurationSeconds`.
- Produces: `seedance.NormalizedRequest`; `parseJSONRequest(*gin.Context) (*requestInput, *dto.TaskError)`; `normalizeScalars(requestInput) (*NormalizedRequest, *dto.TaskError)`; stable `invalid_duration`, `invalid_resolution`, `invalid_request` codes.

- [ ] **Step 1: Write failing table tests for duration presence and conflicts**

```go
package seedance

import (
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"

    "github.com/gin-gonic/gin"
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
)

func jsonContext(t *testing.T, body string) *gin.Context {
    t.Helper()
    recorder := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(recorder)
    c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(body))
    c.Request.Header.Set("Content-Type", "application/json")
    return c
}

func TestNormalizeJSONDurationContract(t *testing.T) {
    tests := []struct {
        name     string
        body     string
        duration int
        code     string
    }{
        {"missing defaults to fifteen", `{"prompt":"p"}`, 15, ""},
        {"null defaults to fifteen", `{"prompt":"p","duration":null}`, 15, ""},
        {"integer", `{"prompt":"p","duration":10}`, 10, ""},
        {"decimal string integer", `{"prompt":"p","seconds":"10"}`, 10, ""},
        {"same sources", `{"prompt":"p","duration":10,"seconds":"10","metadata":{"duration":10}}`, 10, ""},
        {"zero", `{"prompt":"p","duration":0}`, 0, "invalid_duration"},
        {"empty", `{"prompt":"p","duration":""}`, 0, "invalid_duration"},
        {"negative", `{"prompt":"p","duration":-1}`, 0, "invalid_duration"},
        {"fraction", `{"prompt":"p","duration":1.5}`, 0, "invalid_duration"},
        {"exponent", `{"prompt":"p","duration":1e1}`, 0, "invalid_duration"},
        {"nonnumeric", `{"prompt":"p","duration":"abc"}`, 0, "invalid_duration"},
        {"over provider maximum", `{"prompt":"p","duration":16}`, 0, "invalid_duration"},
        {"conflicting sources", `{"prompt":"p","duration":9,"seconds":10}`, 0, "invalid_duration"},
    }

    for _, test := range tests {
        t.Run(test.name, func(t *testing.T) {
            input, taskErr := parseJSONRequest(jsonContext(t, test.body))
            require.Nil(t, taskErr)
            got, taskErr := normalizeScalars(input)
            if test.code != "" {
                require.NotNil(t, taskErr)
                assert.Equal(t, test.code, taskErr.Code)
                return
            }
            require.Nil(t, taskErr)
            assert.Equal(t, test.duration, got.Duration)
        })
    }
}
```

- [ ] **Step 2: Write failing resolution, prompt, and optional-metadata tests**

```go
func TestNormalizeJSONResolutionAndMetadata(t *testing.T) {
    input, taskErr := parseJSONRequest(jsonContext(t, `{
      "prompt":"  flower  ",
      "size":"1920x1080",
      "metadata":{
        "resolution":"1080p",
        "prompt_optimization":false,
        "multi_shot":true,
        "strict_duration":false,
        "negative_prompt":"blur"
      }
    }`))
    require.Nil(t, taskErr)

    got, taskErr := normalizeScalars(input)
    require.Nil(t, taskErr)
    assert.Equal(t, "flower", got.Prompt)
    assert.Equal(t, "1080P", got.Resolution)
    require.NotNil(t, got.PromptOptimization)
    assert.False(t, *got.PromptOptimization)
    require.NotNil(t, got.MultiShot)
    assert.True(t, *got.MultiShot)
    require.NotNil(t, got.StrictDuration)
    assert.False(t, *got.StrictDuration)
    assert.Equal(t, "blur", got.NegativePrompt)
}

func TestNormalizeJSONRejectsResolutionConflict(t *testing.T) {
    input, taskErr := parseJSONRequest(jsonContext(t,
        `{"prompt":"p","size":"1280x720","metadata":{"resolution":"1080P"}}`,
    ))
    require.Nil(t, taskErr)
    _, taskErr = normalizeScalars(input)
    require.NotNil(t, taskErr)
    assert.Equal(t, "invalid_resolution", taskErr.Code)
}

func TestNormalizeJSONRejectsStringBoolean(t *testing.T) {
    input, taskErr := parseJSONRequest(jsonContext(t,
        `{"prompt":"p","metadata":{"strict_duration":"false"}}`,
    ))
    require.Nil(t, taskErr)
    _, taskErr = normalizeScalars(input)
    require.NotNil(t, taskErr)
    assert.Equal(t, "invalid_request", taskErr.Code)
}
```

- [ ] **Step 3: Run tests and verify missing implementation**

Run:

```bash
go test ./relay/channel/task/seedance -run 'TestNormalizeJSON' -count=1
```

Expected: FAIL because the package functions and types do not exist.

- [ ] **Step 4: Add constants and raw DTOs without decoding away field presence**

```go
// constants.go
package seedance

import "time"

const (
    ModelName                  = "seedance-uncensored"
    ChannelName                = "seed-dance"
    defaultDuration            = 15
    defaultResolution          = "720P"
    normalizedRequestContextKey = "seedance_normalized_request"
    submitTimeout              = 60 * time.Second
    statusTimeout              = 30 * time.Second
    contentTimeout             = 120 * time.Second
    connectTimeout             = 10 * time.Second
)

var resolutionRatios = map[string]float64{
    "480P":  0.5,
    "720P":  1.0,
    "1080P": 2.25,
}
```

```go
// dto.go
package seedance

import "encoding/json"

type rawJSONRequest struct {
    Model          json.RawMessage `json:"model"`
    Prompt         json.RawMessage `json:"prompt"`
    Duration       json.RawMessage `json:"duration"`
    Seconds        json.RawMessage `json:"seconds"`
    Size           json.RawMessage `json:"size"`
    Image          json.RawMessage `json:"image"`
    Images         json.RawMessage `json:"images"`
    InputReference json.RawMessage `json:"input_reference"`
    Metadata       json.RawMessage `json:"metadata"`
}

type rawMetadata struct {
    Duration           json.RawMessage `json:"duration"`
    Resolution         json.RawMessage `json:"resolution"`
    ImageBase64        json.RawMessage `json:"image_base64"`
    PromptOptimization json.RawMessage `json:"prompt_optimization"`
    MultiShot          json.RawMessage `json:"multi_shot"`
    StrictDuration     json.RawMessage `json:"strict_duration"`
    NegativePrompt     json.RawMessage `json:"negative_prompt"`
}

type requestInput struct {
    Raw      rawJSONRequest
    Metadata rawMetadata
}

type NormalizedRequest struct {
    Prompt             string
    ImageBase64        string
    Resolution         string
    Duration           int
    PromptOptimization *bool
    MultiShot          *bool
    StrictDuration     *bool
    NegativePrompt     string
}
```

- [ ] **Step 5: Implement strict scalar normalization**

Implement `parseJSONRequest` with the project wrapper:

```go
func parseJSONRequest(c *gin.Context) (*requestInput, *dto.TaskError) {
    var raw rawJSONRequest
    if err := common.UnmarshalBodyReusable(c, &raw); err != nil {
        return nil, service.TaskErrorWrapperLocal(
            err, "invalid_request", http.StatusBadRequest,
        )
    }
    input := &requestInput{Raw: raw}
    metadata := bytes.TrimSpace(raw.Metadata)
    if len(metadata) != 0 && !bytes.Equal(metadata, []byte("null")) {
        if err := common.Unmarshal(metadata, &input.Metadata); err != nil {
            return nil, service.TaskErrorWrapperLocal(
                err, "invalid_request", http.StatusBadRequest,
            )
        }
    }
    return input, nil
}
```

Implement duration without float conversion:

```go
func parseDurationValue(raw json.RawMessage) (int, bool, error) {
    trimmed := bytes.TrimSpace(raw)
    if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
        return 0, false, nil
    }

    text := string(trimmed)
    if trimmed[0] == '"' {
        var value string
        if err := common.Unmarshal(trimmed, &value); err != nil {
            return 0, true, err
        }
        text = value
    }
    if text == "" {
        return 0, true, errors.New("duration must not be empty")
    }
    for _, char := range text {
        if char < '0' || char > '9' {
            return 0, true, errors.New("duration must be a decimal integer")
        }
    }
    value, err := strconv.ParseInt(text, 10, 32)
    if err != nil {
        return 0, true, err
    }
    if value < 1 || value > 15 || value > relaycommon.MaxTaskDurationSeconds {
        return 0, true, errors.New("duration must be between 1 and 15")
    }
    return int(value), true, nil
}
```

Normalize all three duration sources and reject conflicts:

```go
func normalizeDuration(input *requestInput) (int, *dto.TaskError) {
    sources := []json.RawMessage{
        input.Raw.Duration,
        input.Raw.Seconds,
        input.Metadata.Duration,
    }
    value := defaultDuration
    found := false
    for _, source := range sources {
        candidate, present, err := parseDurationValue(source)
        if err != nil {
            return 0, service.TaskErrorWrapperLocal(
                err, "invalid_duration", http.StatusBadRequest,
            )
        }
        if !present {
            continue
        }
        if found && candidate != value {
            return 0, service.TaskErrorWrapperLocal(
                errors.New("duration fields conflict"),
                "invalid_duration",
                http.StatusBadRequest,
            )
        }
        value = candidate
        found = true
    }
    return value, nil
}
```

Implement resolution mapping exactly:

```go
var resolutionAliases = map[string]string{
    "854X480":   "480P",
    "480X854":   "480P",
    "480P":      "480P",
    "1280X720":  "720P",
    "720X1280":  "720P",
    "720P":      "720P",
    "1920X1080": "1080P",
    "1080X1920": "1080P",
    "1080P":     "1080P",
}
```

`normalizeScalars` must trim prompt, require it non-empty, call `normalizeDuration`,
map `size` and `metadata.resolution`, require equal mapped values when both exist,
default to `720P`, decode JSON optional booleans only as booleans, and decode
`negative_prompt` only as a string.

- [ ] **Step 6: Run focused tests**

Run:

```bash
gofmt -w relay/channel/task/seedance
go test ./relay/channel/task/seedance -run 'TestNormalizeJSON' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add relay/channel/task/seedance/constants.go \
  relay/channel/task/seedance/dto.go \
  relay/channel/task/seedance/normalize.go \
  relay/channel/task/seedance/normalize_test.go
git commit -m "feat: normalize Seed Dance scalar requests"
```

---

### Task 3: Image Sources, Multipart Requests, and One-Time Normalization

**Files:**
- Create: `relay/channel/task/seedance/image.go`
- Modify: `relay/channel/task/seedance/normalize.go`
- Modify: `relay/channel/task/seedance/normalize_test.go`
- Create: `relay/channel/task/seedance/image_test.go`

**Interfaces:**
- Consumes: Task 2 `requestInput`, `NormalizedRequest`, scalar normalization; `common.ParseMultipartFormReusable`; `service.GetImageFromUrl`.
- Produces: `normalizeRequest(*gin.Context) (*NormalizedRequest, *dto.TaskError)`; `getNormalizedRequest(*gin.Context) (*NormalizedRequest, error)`; canonical pure Base64 `NormalizedRequest.ImageBase64`.

- [ ] **Step 1: Write failing image validation and deduplication tests**

Use deterministic 240×240 in-memory PNG/JPEG fixtures and assert observable request
behavior:

```go
func TestNormalizeImagesDeduplicatesDecodedBytes(t *testing.T) {
    pngBytes := testPNG(t, 240, 240)
    encoded := base64.StdEncoding.EncodeToString(pngBytes)
    body := fmt.Sprintf(`{
      "prompt":"p",
      "image":%q,
      "input_reference":%q,
      "metadata":{"image_base64":%q}
    }`, encoded, "data:image/png;base64,"+encoded, encoded)

    got, taskErr := normalizeRequest(jsonContext(t, body))
    require.Nil(t, taskErr)
    assert.Equal(t, encoded, got.ImageBase64)
}

func TestNormalizeImagesRejectsDifferentDecodedBytes(t *testing.T) {
    first := base64.StdEncoding.EncodeToString(testPNG(t, 240, 240))
    second := base64.StdEncoding.EncodeToString(testPNG(t, 241, 240))
    body := fmt.Sprintf(`{"prompt":"p","image":%q,"images":[%q]}`, first, second)

    _, taskErr := normalizeRequest(jsonContext(t, body))
    require.NotNil(t, taskErr)
    assert.Equal(t, "invalid_image", taskErr.Code)
}

func TestNormalizeImagesRejectsT2V480P(t *testing.T) {
    _, taskErr := normalizeRequest(jsonContext(t,
        `{"prompt":"p","size":"854x480"}`,
    ))
    require.NotNil(t, taskErr)
    assert.Equal(t, "invalid_resolution", taskErr.Code)
}
```

Also add table rows for:

```text
JPG and PNG success
GIF/WebP failure
239×240 failure
8001×240 failure
ratio wider than 8:1 failure
ratio taller than 1:8 failure
invalid Base64 failure
unsupported data URI MIME failure
images array with more than one item failure
I2V 480P success
```

- [ ] **Step 2: Write failing multipart cleanup and cache-reuse tests**

```go
func TestNormalizeMultipartRemovesTemporaryFilesAndCachesResult(t *testing.T) {
    tempDir := t.TempDir()
    t.Setenv("TMPDIR", tempDir)

    body, contentType := multipartRequest(t, map[string]string{
        "prompt":   "flower",
        "duration": "10",
        "metadata": `{"strict_duration":"false","multi_shot":"true"}`,
    }, "input_reference", "input.png", testPNG(t, 240, 240))
    c := multipartContext(t, body, contentType)

    first, taskErr := normalizeRequest(c)
    require.Nil(t, taskErr)
    second, taskErr := normalizeRequest(c)
    require.Nil(t, taskErr)
    assert.Same(t, first, second)

    entries, err := os.ReadDir(tempDir)
    require.NoError(t, err)
    assert.Empty(t, entries)
}
```

For remote URL reuse, inject a loader into `normalizeRequestWithLoader`, count calls,
invoke the function twice on the same Gin context, and assert exactly one download.

- [ ] **Step 3: Run tests and verify the image/multipart behavior is missing**

Run:

```bash
go test ./relay/channel/task/seedance \
  -run 'TestNormalizeImages|TestNormalizeMultipart' -count=1
```

Expected: FAIL.

- [ ] **Step 4: Implement strict image decoding and validation**

Implement the durable image candidate boundary:

```go
type imageCandidate struct {
    source string
    bytes  []byte
    mime   string
}

type remoteImageLoader func(string) (mimeType string, data string, err error)
```

For string sources:

```go
func loadImageCandidate(source string, loadRemote remoteImageLoader) (imageCandidate, error) {
    source = strings.TrimSpace(source)
    if source == "" {
        return imageCandidate{}, nil
    }

    mimeType := ""
    encoded := source
    switch {
    case strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://"):
        var err error
        mimeType, encoded, err = loadRemote(source)
        if err != nil {
            return imageCandidate{}, err
        }
    case strings.HasPrefix(source, "data:image/jpeg;base64,"):
        mimeType = "image/jpeg"
        encoded = strings.TrimPrefix(source, "data:image/jpeg;base64,")
    case strings.HasPrefix(source, "data:image/png;base64,"):
        mimeType = "image/png"
        encoded = strings.TrimPrefix(source, "data:image/png;base64,")
    case strings.HasPrefix(source, "data:"):
        return imageCandidate{}, errors.New("unsupported image data URI")
    }

    decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
    if err != nil {
        return imageCandidate{}, fmt.Errorf("decode image: %w", err)
    }
    config, format, err := image.DecodeConfig(bytes.NewReader(decoded))
    if err != nil {
        return imageCandidate{}, fmt.Errorf("decode image config: %w", err)
    }
    detected := map[string]string{"jpeg": "image/jpeg", "png": "image/png"}[format]
    if detected == "" || (mimeType != "" && mimeType != detected) {
        return imageCandidate{}, errors.New("image must be JPEG or PNG")
    }
    if config.Width < 240 || config.Height < 240 ||
        config.Width > 8000 || config.Height > 8000 {
        return imageCandidate{}, errors.New("image dimensions must be 240–8000")
    }
    ratio := float64(config.Width) / float64(config.Height)
    if ratio < 1.0/8.0 || ratio > 8.0 {
        return imageCandidate{}, errors.New("image ratio must be between 1:8 and 8:1")
    }
    return imageCandidate{source: source, bytes: decoded, mime: detected}, nil
}
```

Import JPEG and PNG decoders for registration:

```go
import (
    _ "image/jpeg"
    _ "image/png"
)
```

Compare every non-empty candidate by `sha256.Sum256(candidate.bytes)`. Encode the
single canonical byte slice with `base64.StdEncoding.EncodeToString`; do not retain
source URLs or multipart readers.

- [ ] **Step 5: Implement multipart parsing and exactly-once request caching**

`normalizeRequest` must first return a cached pointer:

```go
func normalizeRequest(c *gin.Context) (*NormalizedRequest, *dto.TaskError) {
    if cached, ok := c.Get(normalizedRequestContextKey); ok {
        normalized, valid := cached.(*NormalizedRequest)
        if !valid || normalized == nil {
            return nil, service.TaskErrorWrapperLocal(
                errors.New("invalid normalized request cache"),
                "invalid_request",
                http.StatusBadRequest,
            )
        }
        return normalized, nil
    }
    normalized, taskErr := normalizeRequestWithLoader(c, service.GetImageFromUrl)
    if taskErr != nil {
        return nil, taskErr
    }
    c.Set(normalizedRequestContextKey, normalized)
    return normalized, nil
}

func getNormalizedRequest(c *gin.Context) (*NormalizedRequest, error) {
    cached, ok := c.Get(normalizedRequestContextKey)
    if !ok {
        return nil, errors.New("Seed Dance request was not normalized")
    }
    normalized, ok := cached.(*NormalizedRequest)
    if !ok || normalized == nil {
        return nil, errors.New("invalid Seed Dance normalized request")
    }
    return normalized, nil
}
```

For multipart:

```go
form, err := common.ParseMultipartFormReusable(c)
if err != nil {
    return nil, service.TaskErrorWrapperLocal(
        err, "invalid_request", http.StatusBadRequest,
    )
}
defer form.RemoveAll()
```

Convert multipart decimal fields into quoted `json.RawMessage`, decode its `metadata`
string with `common.Unmarshal`, convert only `"true"`/`"false"` multipart booleans,
and read exactly one `form.File["input_reference"]`. Reject multiple uploaded files.
After the file is read into validated bytes, close it immediately. Call scalar
normalization, then image normalization, then reject `Resolution=="480P"` when
`ImageBase64==""`.

- [ ] **Step 6: Prove the supplier 5 MB recommendation is not a Seed Dance limit**

Generate a deterministic 3000×3000 noisy PNG in memory whose encoded payload exceeds
the supplier recommendation, pass it through the local Base64 path, and assert successful
normalization. Do not perform a network request or add the generated binary to Git.

- [ ] **Step 7: Run focused tests**

Run:

```bash
gofmt -w relay/channel/task/seedance
go test ./relay/channel/task/seedance \
  -run 'TestNormalizeImages|TestNormalizeMultipart|TestNormalizeRemote|TestImageOverSupplierRecommendation' \
  -count=1
```

Expected: PASS; no temporary file remains; remote loader count is one.

- [ ] **Step 8: Commit**

```bash
git add relay/channel/task/seedance/image.go \
  relay/channel/task/seedance/image_test.go \
  relay/channel/task/seedance/normalize.go \
  relay/channel/task/seedance/normalize_test.go
git commit -m "feat: normalize Seed Dance image inputs"
```

---

### Task 4: Seed Dance HTTP Adaptor, Submit Classification, and Billing Ratios

**Files:**
- Create: `relay/channel/task/seedance/http.go`
- Create: `relay/channel/task/seedance/http_test.go`
- Modify: `relay/channel/task/seedance/constants.go`
- Modify: `relay/channel/task/seedance/dto.go`
- Create: `relay/channel/task/seedance/billing.go`
- Create: `relay/channel/task/seedance/billing_test.go`
- Create: `relay/channel/task/seedance/adaptor.go`
- Create: `relay/channel/task/seedance/adaptor_test.go`
- Modify: `relay/relay_adaptor.go`
- Create: `relay/relay_adaptor_test.go`

**Interfaces:**
- Consumes: `NormalizedRequest`; `normalizeRequest`; `getNormalizedRequest`; `channel.FullPrepaidTaskSubmitter`; `channel.DurableTaskSubmitter`; `channel.DeferredTaskSubmitResponder`; `channel.TaskSubmitFailureClassifier`; `taskcommon.BaseBilling`.
- Produces: `seedance.TaskAdaptor`; `newStageClient(base *http.Client, proxy string, connectTimeout time.Duration) (*http.Client, error)`; submit/status provider DTOs; `TaskAdaptor.FetchTaskWithContext`; Type 59 registration in `relay.GetTaskAdaptor`.

- [ ] **Step 1: Write failing adaptor, error-classification, deadline, and ratio tests**

Create table-driven tests with these concrete cases:

```go
func TestEstimateBillingUsesNormalizedDurationAndResolution(t *testing.T) {
    c, _ := gin.CreateTestContext(httptest.NewRecorder())
    c.Set(normalizedRequestContextKey, &NormalizedRequest{
        Duration:   10,
        Resolution: "1080P",
    })
    a := &TaskAdaptor{}
    ratios := a.EstimateBilling(c, &relaycommon.RelayInfo{})
    assert.Equal(t, map[string]float64{
        "seconds":    10,
        "resolution": 2.25,
    }, ratios)
}

func TestDoResponseTreatsAmbiguousHTTP200AsUnknown(t *testing.T) {
    cases := []struct {
        name string
        body io.ReadCloser
    }{
        {"truncated", io.NopCloser(strings.NewReader(`{"requestId":"R"`))},
        {"invalid", io.NopCloser(strings.NewReader(`{not-json}`))},
        {"missing task id", io.NopCloser(strings.NewReader(
            `{"requestId":"R","status":"accepted"}`,
        ))},
        {"empty task id", io.NopCloser(strings.NewReader(
            `{"requestId":"R","task_id":"","status":"accepted"}`,
        ))},
        {"read interrupted", &errorReadCloser{err: io.ErrUnexpectedEOF}},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            retryable := true
            a := &TaskAdaptor{}
            _, _, taskErr := a.DoResponse(
                &gin.Context{},
                &http.Response{StatusCode: http.StatusOK, Body: tc.body},
                &relaycommon.RelayInfo{},
            )
            require.NotNil(t, taskErr)
            require.Equal(t, "seedance_submit_outcome_unknown", taskErr.Code)
            require.NotNil(t, taskErr.Retryable)
            retryable = *taskErr.Retryable
            assert.False(t, retryable)
        })
    }
}

func TestClassifySubmitFailureRetryMatrix(t *testing.T) {
    cases := []struct {
        name       string
        status     int
        body       string
        requestErr error
        code       string
        retryable  bool
        upstreamID string
    }{
        {"auth 401", 401, `{"success":false,"errCode":"401"}`, nil,
            "upstream_authentication_error", false, ""},
        {"auth 403", 403, `{"success":false,"errCode":"403"}`, nil,
            "upstream_authentication_error", false, ""},
        {"confirmed empty 429", 429,
            `{"success":false,"errCode":"429","errMessage":"rate limited","data":null}`,
            nil, "upstream_rate_limit_error", true, ""},
        {"429 with task", 429,
            `{"success":false,"errCode":"429","task_id":"UPSTREAM_TASK_ID"}`,
            nil, "seedance_submit_outcome_unknown", false, "UPSTREAM_TASK_ID"},
        {"gateway", 503, ``, nil,
            "seedance_submit_outcome_unknown", false, ""},
        {"timeout", 0, ``, context.DeadlineExceeded,
            "seedance_submit_outcome_unknown", false, ""},
        {"eof", 0, ``, io.EOF,
            "seedance_submit_outcome_unknown", false, ""},
    }
    _ = cases
}
```

Add tests that assert:

```go
var _ channel.TaskAdaptor = (*TaskAdaptor)(nil)
var _ channel.FullPrepaidTaskSubmitter = (*TaskAdaptor)(nil)
var _ channel.DurableTaskSubmitter = (*TaskAdaptor)(nil)
var _ channel.DeferredTaskSubmitResponder = (*TaskAdaptor)(nil)
var _ channel.TaskSubmitFailureClassifier = (*TaskAdaptor)(nil)
var _ interface {
    FetchTaskWithContext(
        context.Context,
        string,
        string,
        map[string]any,
        string,
    ) (*http.Response, error)
} = (*TaskAdaptor)(nil)
```

Task 6 declares the service-local named context-aware interface; this structural assertion
protects the same method set without introducing a package cycle.

- [ ] **Step 2: Run the focused package and confirm it fails**

Run:

```bash
go test ./relay/channel/task/seedance ./relay \
  -run 'TestEstimateBilling|TestDoResponse|TestClassifySubmit|TestSeedDanceTaskAdaptor' \
  -count=1
```

Expected: FAIL because the adaptor, provider DTOs, and Type 59 registration do not exist.

- [ ] **Step 3: Extend the existing constants and DTO file with provider contracts**

Keep Task 2's existing public/default/timeout constants unchanged and add the provider
request/response DTOs:

```go
type generateRequest struct {
    Prompt             string `json:"prompt"`
    ImageBase64        string `json:"image_base64,omitempty"`
    Duration           int    `json:"duration"`
    Resolution         string `json:"resolution"`
    PromptOptimization *bool  `json:"prompt_optimization,omitempty"`
    MultiShot          *bool  `json:"multi_shot,omitempty"`
    StrictDuration     *bool  `json:"strict_duration,omitempty"`
    NegativePrompt     string `json:"negative_prompt,omitempty"`
}

type providerEnvelope struct {
    RequestID string          `json:"requestId,omitempty"`
    TaskID    string          `json:"task_id,omitempty"`
    Status    string          `json:"status,omitempty"`
    Success   *bool           `json:"success,omitempty"`
    ErrCode   json.RawMessage `json:"errCode,omitempty"`
    ErrMessage string         `json:"errMessage,omitempty"`
    Message   string          `json:"message,omitempty"`
    Data      json.RawMessage `json:"data,omitempty"`
}

type cleanedTaskData struct {
    RequestID string `json:"requestId,omitempty"`
    Success   *bool  `json:"success,omitempty"`
    ErrCode   string `json:"errCode,omitempty"`
    ErrMessage string `json:"errMessage,omitempty"`
    Status    string `json:"status,omitempty"`
    Message   string `json:"message,omitempty"`
    Model     string `json:"model,omitempty"`
    Seconds   string `json:"seconds,omitempty"`
    Size      string `json:"size,omitempty"`
}
```

Import the standard library only for types such as `json.RawMessage`; call
`common.Marshal` and `common.Unmarshal` for all encoding and decoding.

- [ ] **Step 4: Implement timeout-safe HTTP clients without breaking proxy transports**

Add:

```go
type cancelOnCloseReadCloser struct {
    io.ReadCloser
    cancel context.CancelFunc
    once   sync.Once
}

func (r *cancelOnCloseReadCloser) Close() error {
    err := r.ReadCloser.Close()
    r.once.Do(r.cancel)
    return err
}

func bindCancelToBody(
    resp *http.Response,
    cancel context.CancelFunc,
) *http.Response {
    resp.Body = &cancelOnCloseReadCloser{
        ReadCloser: resp.Body,
        cancel:     cancel,
    }
    return resp
}
```

Clone the client and its `*http.Transport`. Preserve `Proxy`, TLS settings,
connection pools, and the existing `DialContext`; wrap the existing dial function rather
than replacing it:

```go
originalDial := transport.DialContext
transport.DialContext = func(
    ctx context.Context,
    network string,
    address string,
) (net.Conn, error) {
    dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
    defer cancel()
    return originalDial(dialCtx, network, address)
}
```

For submit and status requests, cancel immediately only when request creation or
`client.Do` fails. On a successful `*http.Response`, wrap the Body with
`bindCancelToBody`; otherwise returning from `DoRequest` would cancel the context before
`DoResponse` reads it.

Tests must use an HTTP proxy server and a fake `DialContext` to prove:

```text
submit parent cancellation closes /generate
status scheduler cancellation closes /status
connect attempt sees a deadline no later than 10 seconds
HTTP/SOCKS proxy selection is preserved
closing resp.Body calls cancel exactly once
```

- [ ] **Step 5: Implement the adaptor and the explicit submit policy**

Implement:

```go
type TaskAdaptor struct {
    taskcommon.BaseBilling
    apiKey  string
    baseURL string
}

func (a *TaskAdaptor) Init(info *relaycommon.RelayInfo) {
    a.apiKey = info.ApiKey
    a.baseURL = strings.TrimRight(info.ChannelBaseUrl, "/")
}

func (*TaskAdaptor) RequiresFullPrepaidBilling() bool { return true }
func (*TaskAdaptor) RequiresDurableTaskBeforeSubmit() bool { return true }
func (*TaskAdaptor) GetModelList() []string { return []string{ModelName} }
func (*TaskAdaptor) GetChannelName() string { return ChannelName }

func (a *TaskAdaptor) ValidateRequestAndSetAction(
    c *gin.Context,
    info *relaycommon.RelayInfo,
) *dto.TaskError {
    normalized, taskErr := normalizeRequest(c)
    if taskErr != nil {
        return taskErr
    }
    if info.OriginModelName != ModelName {
        return service.TaskErrorWrapperLocal(
            fmt.Errorf("unsupported model %q", info.OriginModelName),
            "model_not_supported",
            http.StatusBadRequest,
        )
    }
    info.Action = constant.TaskActionGenerate
    c.Set(normalizedRequestContextKey, normalized)
    return nil
}

func (a *TaskAdaptor) EstimateBilling(
    c *gin.Context,
    _ *relaycommon.RelayInfo,
) map[string]float64 {
    normalized, err := getNormalizedRequest(c)
    if err != nil {
        return nil
    }
    resolution := map[string]float64{
        "480P": 0.5,
        "720P": 1,
        "1080P": 2.25,
    }[normalized.Resolution]
    return map[string]float64{
        "seconds": float64(normalized.Duration),
        "resolution": resolution,
    }
}
```

`BuildRequestBody` must fetch the cached `NormalizedRequest`, map every supported field
into `generateRequest`, call `common.Marshal`, set
`info.UpstreamRequestBodySize = int64(len(body))`, and return `bytes.NewReader(body)`.
`BuildRequestHeader` sets only:

```go
req.Header.Set("Authorization", "Bearer "+a.apiKey)
req.Header.Set("Content-Type", "application/json")
req.Header.Set("Accept", "application/json")
```

`BuildRequestURL` returns `a.baseURL + "/generate"`.

Implement the complete `TaskAdaptor.DoRequest` method; a successful response owns the
derived context until its Body is closed:

```go
func (a *TaskAdaptor) DoRequest(
    c *gin.Context,
    info *relaycommon.RelayInfo,
    requestBody io.Reader,
) (*http.Response, error) {
    ctx, cancel := context.WithTimeout(
        c.Request.Context(),
        submitTimeout,
    )
    requestURL, err := a.BuildRequestURL(info)
    if err != nil {
        cancel()
        return nil, err
    }
    req, err := http.NewRequestWithContext(
        ctx,
        http.MethodPost,
        requestURL,
        requestBody,
    )
    if err != nil {
        cancel()
        return nil, err
    }
    if info.UpstreamRequestBodySize > 0 {
        req.ContentLength = info.UpstreamRequestBodySize
    }
    if err := a.BuildRequestHeader(c, req, info); err != nil {
        cancel()
        return nil, err
    }

    proxy := info.ChannelSetting.Proxy
    baseClient, err := service.GetHttpClientWithProxy(proxy)
    if err != nil {
        cancel()
        return nil, err
    }
    client, err := newStageClient(baseClient, proxy, connectTimeout)
    if err != nil {
        cancel()
        return nil, err
    }
    resp, err := client.Do(req)
    if err != nil {
        cancel()
        return nil, err
    }
    return bindCancelToBody(resp, cancel), nil
}
```

Tests assert successful submit, parent cancellation, the 60-second deadline, retained
HTTP/SOCKS proxy selection, and exactly-once cancellation when `resp.Body.Close` runs.

- [ ] **Step 6: Implement response parsing and stable retry classifications**

Use this internal constructor for ambiguous submit outcomes:

```go
func submitOutcomeUnknown(cause error) *dto.TaskError {
    retryable := false
    return &dto.TaskError{
        Code:       "seedance_submit_outcome_unknown",
        Message:    "upstream submission result is unknown",
        StatusCode: http.StatusBadGateway,
        LocalError: false,
        Error:      cause,
        Retryable:  &retryable,
    }
}
```

`DoResponse` must:

```text
defer resp.Body.Close()
read Body
decode providerEnvelope
if the complete object is an explicit business failure:
    return any reliable task_id + cleaned data + non-retryable business error
if task_id is empty:
    return outcome unknown, not a generic 500
return upstream task_id + cleaned data + nil
```

Never put `TaskID`, prompt, image data, or Base64 in `cleanedTaskData`. Construct the
deferred public response with:

```go
func (a *TaskAdaptor) BuildTaskSubmitResponse(
    info *relaycommon.RelayInfo,
    taskData []byte,
) (*channel.TaskSubmitHTTPResponse, error) {
    body := dto.NewOpenAIVideo()
    body.ID = info.PublicTaskID
    body.TaskID = info.PublicTaskID
    body.Model = ModelName
    body.Status = dto.VideoStatusQueued
    body.Progress = 10
    body.CreatedAt = time.Now().Unix()
    var cleaned cleanedTaskData
    if err := common.Unmarshal(taskData, &cleaned); err != nil {
        return nil, fmt.Errorf("decode cleaned task data: %w", err)
    }
    body.Seconds = cleaned.Seconds
    body.Size = cleaned.Size
    return &channel.TaskSubmitHTTPResponse{
        StatusCode:      http.StatusOK,
        Body:            body,
        InitialStatus:   model.TaskStatusSubmitted,
        InitialProgress: taskcommon.ProgressSubmitted,
    }, nil
}
```

`DoResponse` builds `cleanedTaskData.Seconds` and `Size` from the already-normalized
request values retained in the adaptor for this request. It never re-reads the client
Body.

`ClassifyTaskSubmitFailure` returns
`*channel.TaskSubmitFailureClassification`. Only this complete shape is retryable:

```text
HTTP 429
success is explicitly false
errCode identifies rate limiting
task_id is empty
data is null or empty
the complete JSON decoded without error
```

Every timeout, EOF, 502/503/504, incomplete error Body, 429 with task ID, and uncertain
business shape returns `seedance_submit_outcome_unknown` with `Retryable=false`.
401/403 returns `upstream_authentication_error` with `Retryable=false`.

- [ ] **Step 7: Add status parsing and OpenAI conversion**

Add `FetchTaskWithContext`:

```go
func (a *TaskAdaptor) FetchTaskWithContext(
    parent context.Context,
    baseURL string,
    key string,
    body map[string]any,
    proxy string,
) (*http.Response, error) {
    taskID, _ := body["task_id"].(string)
    if taskID == "" {
        return nil, errors.New("task_id is required")
    }
    ctx, cancel := context.WithTimeout(parent, statusTimeout)
    req, err := http.NewRequestWithContext(
        ctx,
        http.MethodGet,
        strings.TrimRight(baseURL, "/")+"/status/"+url.PathEscape(taskID),
        nil,
    )
    if err != nil {
        cancel()
        return nil, err
    }
    req.Header.Set("Authorization", "Bearer "+key)
    baseClient, err := service.GetHttpClientWithProxy(proxy)
    if err != nil {
        cancel()
        return nil, err
    }
    client, err := newStageClient(baseClient, proxy, connectTimeout)
    if err != nil {
        cancel()
        return nil, err
    }
    resp, err := client.Do(req)
    if err != nil {
        cancel()
        return nil, err
    }
    return bindCancelToBody(resp, cancel), nil
}
```

Keep the legacy method:

```go
func (a *TaskAdaptor) FetchTask(
    baseURL string,
    key string,
    body map[string]any,
    proxy string,
) (*http.Response, error) {
    return a.FetchTaskWithContext(
        context.Background(), baseURL, key, body, proxy,
    )
}
```

`ParseTaskResult` maps:

```go
switch strings.ToLower(provider.Status) {
case "accepted":
    info.Status, info.Progress = model.TaskStatusSubmitted, "10%"
case "queued":
    info.Status, info.Progress = model.TaskStatusQueued, "20%"
case "running", "processing":
    info.Status, info.Progress = model.TaskStatusInProgress, "30%"
case "completed":
    info.Status, info.Progress = model.TaskStatusSuccess, "100%"
case "failed":
    info.Status, info.Progress = model.TaskStatusFailure, "100%"
default:
    return nil, fmt.Errorf("unknown Seed Dance status %q", provider.Status)
}
```

Preserve only `requestId`, `success`, `errCode`, `errMessage`, `status`, and `message`
in the returned `TaskInfo.Data`. `ConvertToOpenAIVideo` constructs a new
`dto.OpenAIVideo` exclusively from public Task fields and billing snapshot ratios.

- [ ] **Step 8: Register Type 59 and run focused tests**

Add:

```go
import taskseedance "github.com/QuantumNous/new-api/relay/channel/task/seedance"

case constant.ChannelTypeSeedDance:
    return &taskseedance.TaskAdaptor{}
```

Run:

```bash
gofmt -w relay/channel/task/seedance relay/relay_adaptor.go relay/relay_adaptor_test.go
go test ./relay/channel/task/seedance ./relay \
  -run 'TestEstimateBilling|TestDoResponse|TestClassifySubmit|TestFetchTask|TestSeedDanceTaskAdaptor' \
  -count=1
```

Expected: PASS. No test contains a real API key or real upstream task ID.

- [ ] **Step 9: Commit**

```bash
git add relay/channel/task/seedance \
  relay/relay_adaptor.go relay/relay_adaptor_test.go
git commit -m "feat: add Seed Dance task adaptor"
```

---

### Task 5: Durable Submission Lifecycle, Pre-Submit Billing Gate, and Refund Ownership

> **Supersession notice:** Do not execute this Task 5 section as a standalone
> source of truth. The conflicting signatures, refund algorithm, transaction
> boundaries, test matrix, and file scope below are superseded by
> [`../specs/2026-07-25-seed-dance-task5-corrective-design.md`](../specs/2026-07-25-seed-dance-task5-corrective-design.md).
> The original Task 5 Steps 1–12 below are retained only as historical context and
> must not be executed directly. The amendment and generated Task 5A/5B briefs are
> the sole execution instructions; they incorporate every surviving non-conflicting
> requirement and require **Task 5A → Task 5B** as two independent commits.
> In particular, the amendment's RequestID-keyed billing-attempt-before-preconsume
> sequence replaces every request-refund and TaskID-keyed ledger sequence below.
> Marker semantics are also authoritative throughout the remaining steps:
> Task 5B writes `SubmissionSettledAt` after Task Attach+Commit, full-prepaid
> revalidation, and successful zero-delta settle, before consumption log/HTTP 200.
> Task 5B never writes `SucceededAt`; Task 6 alone writes it after a locked narrow
> `SUCCESS/100%` transition and final settlement.

**Files:**
- Modify: `model/task.go`
- Create: `model/task_submission.go`
- Create: `model/task_submission_test.go`
- Modify: `model/system_task.go`
- Modify: `model/system_task_test.go`
- Modify: `relay/common/relay_info.go`
- Modify: `relay/relay_task.go`
- Create: `relay/relay_task_submission_test.go`
- Modify: `controller/relay.go`
- Create: `controller/relay_task_submission_test.go`
- Create: `service/task_submission.go`
- Create: `service/task_submission_test.go`
- Modify: `controller/system_task_handlers.go`

**Interfaces:**
- Consumes: `channel.FullPrepaidTaskSubmitter`; `channel.DurableTaskSubmitter`; `channel.TaskSubmitFailureClassification`; `channel.DeferredTaskSubmitResponder`; `BillingSettler.GetPreConsumedQuota`; `model.Task.UpdateWithStatus`; `service.RefundTaskQuota`.
- Produces: `model.TaskStatusSubmitting`; `model.GetTaskByPrimaryID`; `model.PrepareTaskSubmissionAttempt`; `model.AttachTaskUpstreamResult`; `model.CommitTaskSubmission`; `model.CreateSystemTaskWithActiveKey`; `model.GetActiveSystemTaskByActiveKey`; `service.FailAndRefundTaskSubmission`; `SystemTaskTypeSeedDanceSubmitReconciliation`; `TaskRelayInfo.PersistentTaskID`; `TaskRelayInfo.RefundOwnedByTask`; `TaskSubmitResult.HTTPResponse`.

- [ ] **Step 1: Write failing model lifecycle tests**

Protect the exact two-phase ordering:

```go
func TestPrepareAttachAndCommitTaskSubmission(t *testing.T) {
    setupTaskDB(t)
    candidate := &Task{
        TaskID:    "task_public",
        UserId:    7,
        ChannelId: 59,
        Quota:     75000,
        Status:    TaskStatusSubmitting,
        Progress:  "0%",
        PrivateData: TaskPrivateData{
            Key: "KEY_A",
            BillingContext: &TaskBillingContext{
                GroupRatio: 1,
                OtherRatios: map[string]float64{
                    "seconds": 1,
                    "resolution": 1,
                },
            },
        },
    }
    prepared, err := PrepareTaskSubmissionAttempt(candidate, 0)
    require.NoError(t, err)
    require.NotZero(t, prepared.ID)
    assert.Equal(t, TaskStatusSubmitting, prepared.Status)
    assert.Empty(t, prepared.PrivateData.UpstreamTaskID)

    attached, err := AttachTaskUpstreamResult(
        prepared.ID,
        "task_public",
        "UPSTREAM_TASK_ID",
        []byte(`{"requestId":"REQUEST_ID","status":"accepted"}`),
    )
    require.NoError(t, err)
    assert.Equal(t, "UPSTREAM_TASK_ID", attached.PrivateData.UpstreamTaskID)
    assert.Equal(t, TaskStatusSubmitting, attached.Status)
    assert.Equal(t, "0%", attached.Progress)

    committed, err := CommitTaskSubmission(prepared.ID, "task_public")
    require.NoError(t, err)
    assert.Equal(t, TaskStatusSubmitted, committed.Status)
    assert.Equal(t, "10%", committed.Progress)
    assert.Equal(t, "UPSTREAM_TASK_ID", committed.PrivateData.UpstreamTaskID)
}
```

Add separate tests for:

```text
Prepare first call inserts exactly one row
Prepare retry updates channel/key but keeps public ID and row ID
Prepare retry is rejected after an upstream ID is attached
Attach is idempotent for the same upstream ID
Attach rejects a different upstream ID without overwriting
Attach write-error followed by read-after-error recognizes committed data
Commit is idempotent
Commit cannot revive FAILURE
SUBMITTING is excluded from normal polling batches but counted by HasTaskPollingWork
timed-out SUBMITTING remains eligible for timeout sweep
```

- [ ] **Step 2: Run model tests and verify the symbols are missing**

Run:

```bash
go test ./model \
  -run 'TestPrepareAttachAndCommitTaskSubmission|TestSubmittingTask' \
  -count=1
```

Expected: FAIL because `TaskStatusSubmitting` and lifecycle methods are undefined.

- [ ] **Step 3: Add the internal status and transactional lifecycle methods**

Add:

```go
const (
    TaskStatusNotStart   TaskStatus = "NOT_START"
    TaskStatusSubmitting TaskStatus = "SUBMITTING"
    TaskStatusSubmitted  TaskStatus = "SUBMITTED"
    TaskStatusQueued     TaskStatus = "QUEUED"
    TaskStatusInProgress TaskStatus = "IN_PROGRESS"
    TaskStatusFailure    TaskStatus = "FAILURE"
    TaskStatusSuccess    TaskStatus = "SUCCESS"
    TaskStatusUnknown    TaskStatus = "UNKNOWN"
)

var (
    ErrTaskSubmissionStateConflict = errors.New("task submission state conflict")
    ErrTaskUpstreamIDConflict      = errors.New("task upstream id conflict")
)
```

`PrepareTaskSubmissionAttempt(candidate, persistentID)` inserts when
`persistentID == 0`. On retry, use `DB.Transaction` and `lockForUpdate(tx)` to load:

```go
Where(
    "id = ? AND user_id = ? AND task_id = ?",
    persistentID,
    candidate.UserId,
    candidate.TaskID,
)
```

Reject unless the current status is `SUBMITTING` and
`PrivateData.UpstreamTaskID == ""`. Update only:

```text
Group
ChannelId
Quota
Properties
PrivateData.Key
PrivateData.BillingSource
PrivateData.SubscriptionId
PrivateData.TokenId
PrivateData.NodeName
PrivateData.BillingContext
```

Never change `TaskID`, `UserId`, `SubmitTime`, or an attached upstream ID.

Implement `AttachTaskUpstreamResult` with a row lock and full JSON update:

```go
func AttachTaskUpstreamResult(
    id int64,
    publicTaskID string,
    upstreamTaskID string,
    taskData []byte,
) (*Task, error) {
    if id == 0 || publicTaskID == "" || upstreamTaskID == "" {
        return nil, ErrTaskSubmissionStateConflict
    }
    var result Task
    err := DB.Transaction(func(tx *gorm.DB) error {
        if err := lockForUpdate(tx).
            Where("id = ? AND task_id = ?", id, publicTaskID).
            First(&result).Error; err != nil {
            return err
        }
        current := result.PrivateData.UpstreamTaskID
        if current != "" {
            if current != upstreamTaskID {
                return ErrTaskUpstreamIDConflict
            }
            // The same association is already durable. Do not rewrite Data
            // or mutate a state that may have advanced since the first call.
            return nil
        }
        if result.Status != TaskStatusSubmitting ||
            result.Progress != "0%" {
            return ErrTaskSubmissionStateConflict
        }
        result.PrivateData.UpstreamTaskID = upstreamTaskID
        result.Data = append(result.Data[:0], taskData...)
        return tx.Model(&result).
            Select("private_data", "data", "updated_at").
            Updates(&result).Error
    })
    if err != nil {
        return nil, err
    }
    return &result, nil
}
```

The tests additionally prove that an identical ID on an already-advanced Task is an
idempotent no-op that does not rewrite `Data`; an empty ID on
`SUBMITTED/QUEUED/IN_PROGRESS/FAILURE/SUCCESS` is rejected; and only
`SUBMITTING/0%` permits a first association.

Implement `CommitTaskSubmission` as a transaction that accepts either
`SUBMITTING` or the already-committed `SUBMITTED/10%` state. It requires a non-empty
stored upstream ID, and it never changes a terminal Task.

Add a primary-database reader used by read-after-error:

```go
func GetTaskByPrimaryID(id int64) (*Task, error) {
    var task Task
    if err := DB.Where("id = ?", id).First(&task).Error; err != nil {
        return nil, err
    }
    return &task, nil
}
```

Modify only `GetAllUnFinishSyncTasks` to exclude `SUBMITTING`. Keep
`HasUnfinishedSyncTasks` and `GetTimedOutUnfinishedTasks` aware of that status so the
scheduler still runs timeout recovery when it is the only row.

- [ ] **Step 4: Write and implement the core full-prepaid billing gate**

Add tests for all seven states:

```text
free: quota 0 and Billing nil => pass
free: non-zero quota => fail
free: Billing non-nil => fail
paid: ForcePreConsume false => fail
paid: Billing nil => fail
paid: quota differs from GetPreConsumedQuota => fail
paid: all values match => pass
```

Implement:

```go
func validateFullPrepaidTaskBilling(
    info *relaycommon.RelayInfo,
    quota int,
) *dto.TaskError {
    retryable := false
    fail := func(message string) *dto.TaskError {
        return &dto.TaskError{
            Code:       "seedance_billing_invariant_failed",
            Message:    message,
            StatusCode: http.StatusInternalServerError,
            LocalError: true,
            Retryable:  &retryable,
        }
    }
    if info == nil {
        return fail("task billing state is unavailable")
    }
    if info.PriceData.FreeModel {
        if quota != 0 || info.Billing != nil {
            return fail("free task billing invariant failed")
        }
        return nil
    }
    if !info.ForcePreConsume ||
        info.Billing == nil ||
        quota != info.Billing.GetPreConsumedQuota() {
        return fail("full prepaid task billing invariant failed")
    }
    return nil
}
```

In `RelayTaskSubmit`, discover the marker before pre-consumption:

```go
fullPrepaid := false
if policy, ok := adaptor.(channel.FullPrepaidTaskSubmitter); ok {
    fullPrepaid = policy.RequiresFullPrepaidBilling()
}
if fullPrepaid {
    info.ForcePreConsume = true
}
```

After `PreConsumeBilling`, call the validator before `BuildRequestBody`.
The failure test must assert:

```text
BuildRequestBody calls = 0
Task.Insert calls = 0
POST /generate calls = 0
request Billing.Refund calls = 1
```

- [ ] **Step 5: Persist or refresh the provisional Task immediately before POST**

Add:

```go
func newProvisionalTask(
    platform constant.TaskPlatform,
    info *relaycommon.RelayInfo,
    quota int,
) *model.Task {
    task := model.InitTask(platform, info)
    task.Status = model.TaskStatusSubmitting
    task.Progress = "0%"
    task.Quota = quota
    task.Action = info.Action
    task.PrivateData.Key = info.ApiKey
    task.PrivateData.BillingSource = info.BillingSource
    task.PrivateData.SubscriptionId = info.SubscriptionId
    task.PrivateData.TokenId = info.TokenId
    task.PrivateData.NodeName = common.NodeName
    task.PrivateData.BillingContext = &model.TaskBillingContext{
        ModelPrice: info.PriceData.ModelPrice,
        GroupRatio: info.PriceData.GroupRatioInfo.GroupRatio,
        ModelRatio: info.PriceData.ModelRatio,
        OtherRatios: info.PriceData.OtherRatios(),
        OriginModelName: info.OriginModelName,
        PerCallBilling: info.PriceData.UsePrice,
    }
    return task
}
```

Extend `TaskRelayInfo`:

```go
PersistentTaskID  int64
RefundOwnedByTask bool
```

After `BuildRequestBody` succeeds but before `DoRequest`:

```go
if durable, ok := adaptor.(channel.DurableTaskSubmitter);
    ok && durable.RequiresDurableTaskBeforeSubmit() {
    candidate := newProvisionalTask(platform, info, info.PriceData.Quota)
    persisted, err := model.PrepareTaskSubmissionAttempt(
        candidate,
        info.PersistentTaskID,
    )
    if err != nil {
        return nil, service.TaskErrorWrapperLocal(
            err,
            "persist_task_before_submit_failed",
            http.StatusInternalServerError,
        )
    }
    info.PersistentTaskID = persisted.ID
    info.RefundOwnedByTask = true
}
```

Use a Mock handler that queries the test database from inside `/generate` and proves the
row already exists as `SUBMITTING/0%` with quota, Key, and billing snapshot.

- [ ] **Step 6: Attach every reliable upstream ID before handling success or error**

Change `TaskSubmitResult`:

```go
type TaskSubmitResult struct {
    UpstreamTaskID string
    TaskData       []byte
    Platform       constant.TaskPlatform
    Quota          int
    HTTPResponse   *channel.TaskSubmitHTTPResponse
}
```

Add an idempotent helper:

```go
func attachUpstreamResultWithRetry(
    ctx context.Context,
    info *relaycommon.RelayInfo,
    upstreamID string,
    taskData []byte,
) error
```

It calls `model.AttachTaskUpstreamResult` up to three times with delays of 25 ms and
100 ms. After every returned error, re-read the Task from the primary database; an
already-stored identical upstream ID counts as success. It stops on context cancellation
or `ErrTaskUpstreamIDConflict`.

Use:

```go
func nonRetryablePersistenceError(err error) *dto.TaskError {
    retryable := false
    return &dto.TaskError{
        Code:       "persist_task_submit_result_failed",
        Message:    "failed to persist upstream task result",
        StatusCode: http.StatusInternalServerError,
        LocalError: true,
        Error:      err,
        Retryable:  &retryable,
    }
}
```

Replace the generic submit response branch in `RelayTaskSubmit` with this dispatch rule:

```text
DoRequest returned an error
→ optional TaskSubmitFailureClassifier runs before generic wrapping

non-2xx response
→ optional TaskSubmitFailureClassifier runs before core reads Body
→ classifier consumes and closes Body

old adaptor or nil classification
→ retain the existing generic error code/status behavior
→ core closes any non-nil Body

HTTP 200
→ adaptor.DoResponse consumes and closes Body
```

Use one common classified-result path so a reliable ID is attached before its error is
returned:

```go
resp, requestErr := adaptor.DoRequest(c, info, requestBody)
if requestErr == nil && resp == nil {
    requestErr = errors.New("upstream returned a nil response")
}

var upstreamID string
var taskData []byte
var taskErr *dto.TaskError
needsClassification := requestErr != nil ||
    (resp != nil && resp.StatusCode != http.StatusOK)

if needsClassification {
    if classifier, ok := adaptor.(channel.TaskSubmitFailureClassifier); ok {
        classified := classifier.ClassifyTaskSubmitFailure(resp, requestErr)
        if classified != nil && classified.TaskError != nil {
            upstreamID = classified.UpstreamTaskID
            taskData = classified.TaskData
            taskErr = classified.TaskError
        }
    }
    if taskErr == nil {
        if requestErr != nil {
            return nil, service.TaskErrorWrapper(
                requestErr,
                "do_request_failed",
                http.StatusInternalServerError,
            )
        }
        defer resp.Body.Close()
        responseBody, _ := io.ReadAll(resp.Body)
        return nil, service.TaskErrorWrapper(
            fmt.Errorf("%s", string(responseBody)),
            "fail_to_fetch_task",
            resp.StatusCode,
        )
    }
} else {
    upstreamID, taskData, taskErr = adaptor.DoResponse(c, resp, info)
}

if upstreamID != "" && info.RefundOwnedByTask {
    if err := attachUpstreamResultWithRetry(
        c.Request.Context(),
        info,
        upstreamID,
        taskData,
    ); err != nil {
        if recordErr := recordSeedDanceSubmitReconciliation(
            info,
            upstreamID,
            "persist_task_submit_result_failed",
        ); recordErr != nil {
            common.SysError(
                "create Seed Dance submit reconciliation record",
            )
        }
        return nil, nonRetryablePersistenceError(err)
    }
}
if taskErr != nil {
    return nil, taskErr
}
```

This preserves old adaptors while ensuring the Seed Dance classifier owns every
ambiguous network/non-2xx shape and that no reliable upstream ID is discarded.

Add the symmetric commit helper:

```go
func commitTaskSubmissionWithRetry(
    ctx context.Context,
    info *relaycommon.RelayInfo,
) (*model.Task, error) {
    delays := []time.Duration{
        0,
        25 * time.Millisecond,
        100 * time.Millisecond,
    }
    var lastErr error
    for _, delay := range delays {
        if delay > 0 {
            timer := time.NewTimer(delay)
            select {
            case <-ctx.Done():
                timer.Stop()
                return nil, ctx.Err()
            case <-timer.C:
            }
        }

        committed, err := model.CommitTaskSubmission(
            info.PersistentTaskID,
            info.PublicTaskID,
        )
        if err == nil {
            return committed, nil
        }
        lastErr = err

        persisted, readErr := model.GetTaskByPrimaryID(
            info.PersistentTaskID,
        )
        if readErr == nil {
            if persisted.TaskID != info.PublicTaskID {
                return nil, model.ErrTaskSubmissionStateConflict
            }
            if persisted.Status == model.TaskStatusSubmitted &&
                persisted.Progress == "10%" &&
                persisted.PrivateData.UpstreamTaskID != "" {
                return persisted, nil
            }
            if persisted.Status == model.TaskStatusFailure ||
                persisted.Status == model.TaskStatusSuccess {
                return nil, errors.Join(
                    lastErr,
                    model.ErrTaskSubmissionStateConflict,
                )
            }
        }
        if errors.Is(err, model.ErrTaskSubmissionStateConflict) {
            return nil, err
        }
    }
    return nil, lastErr
}
```

After the defensive full-prepaid check and successful
`BuildTaskSubmitResponse`, call `commitTaskSubmissionWithRetry`. On persistent failure,
record reconciliation with the already-known upstream ID and return a non-retryable
persistence error:

```go
committed, err := commitTaskSubmissionWithRetry(
    c.Request.Context(),
    info,
)
if err != nil {
    if recordErr := recordSeedDanceSubmitReconciliation(
        info,
        upstreamID,
        "commit_task_submission_failed",
    ); recordErr != nil {
        common.SysError(
            "create Seed Dance submit reconciliation record",
        )
    }
    return nil, nonRetryablePersistenceError(err)
}
_ = committed
```

Finally modify `shouldRetryTaskRelay` after the retry-budget, affinity, and
specific-channel guards, before any HTTP status switch:

```go
if taskErr.Retryable != nil {
    return *taskErr.Retryable
}
```

`Retryable=nil` retains every old adaptor's current HTTP-based behavior. Direct tests
assert `502/false → false`, `429/false → false`, `429/true → true`, and legacy
`429/nil → true`; all global retry guards still take precedence.

- [ ] **Step 7: Make persisted Tasks the exclusive refund owner**

Change the request defer:

```go
defer func() {
    if taskErr == nil || relayInfo.Billing == nil {
        return
    }
    if relayInfo.TaskRelayInfo != nil &&
        relayInfo.RefundOwnedByTask {
        return
    }
    relayInfo.Billing.Refund(c)
}()
```

Implement:

```go
func FailAndRefundTaskSubmission(
    ctx context.Context,
    taskID int64,
    upstreamTaskID string,
    taskData []byte,
    code string,
    message string,
) error
```

The function:

```text
loads the row directly by primary key
attaches a supplied upstream ID without overwriting a different stored ID
CASes the current non-terminal status to FAILURE/100%
sets FinishTime and a sanitized FailReason
retains Quota until RefundTaskQuota succeeds
calls RefundTaskQuota only when this caller wins the status CAS
leaves non-zero Quota for sweepUnrefundedFailedTasks after refund failure
```

After the retry loop, when `taskErr != nil && relayInfo.RefundOwnedByTask`, call this
function. Set `taskErr` before the defer runs. Tests must count request-level and task-level
refund calls separately for wallet, subscription, and Token quota and assert that exactly
one owner acts.

- [ ] **Step 8: Mark submission settlement after DB commit and zero-delta settle**

For deferred responses:

```text
RelayTaskSubmit already attached the upstream ID and committed SUBMITTED/10%
Controller repeats ValidateFullPrepaidTaskBilling
Controller calls SettleBilling(result.Quota) with delta=0
Controller calls MarkTaskBillingAttemptSubmissionSettled(requestID)
Controller calls LogTaskConsumption
Controller writes c.JSON(result.HTTPResponse.StatusCode, result.HTTPResponse.Body)
```

`SubmissionSettledAt` means only that the accepted submission was durably attached and
committed, revalidated, and zero-delta settled. It is not final async success and does
not terminate the attempt. Task 5B must not call `MarkTaskBillingAttemptSucceeded` or
exercise the final-success transition; a Task 5B boundary test may only prove
`SucceededAt` remains zero. Task 6 owns that final marker.

If validator, `SettleBilling`, or
`MarkTaskBillingAttemptSubmissionSettled` returns an error:

```text
do not write 200
do not write success consumption log
preserve every reliable upstream ID
narrow-transition the Task to FAILURE/100%
use durable task-owned refund/reconciliation only
return seedance_billing_settlement_failed
```

Before settlement, defensively call `validateFullPrepaidTaskBilling(info, result.Quota)`
again. Because the delta is zero, tests must install wallet/subscription/Token failpoints
and prove no funding or Token adjustment function runs.

Non-deferred adaptors retain their existing response and insertion order.

- [ ] **Step 9: Add a durable administrator reconciliation record**

Add:

```go
const SystemTaskTypeSeedDanceSubmitReconciliation =
    "seedance_submit_reconciliation"

type SeedDanceSubmitReconciliationPayload struct {
    PublicTaskID  string `json:"public_task_id"`
    UpstreamTaskID string `json:"upstream_task_id"`
    PersistentTaskID int64 `json:"persistent_task_id"`
    ChannelID     int    `json:"channel_id"`
    NodeName      string `json:"node_name"`
    ErrorCode     string `json:"error_code"`
    ObservedAt    int64  `json:"observed_at"`
}
```

Create `CreateSystemTaskWithActiveKey` by factoring the current JSON marshal and insert
logic from `CreateSystemTask`. `CreateSystemTask` delegates to it with
`activeKey=taskType`, preserving all existing callers:

```go
func CreateSystemTask(
    taskType string,
    payload any,
    state any,
) (*SystemTask, error) {
    return CreateSystemTaskWithActiveKey(taskType, taskType, payload, state)
}

func CreateSystemTaskWithActiveKey(
    taskType string,
    activeKey string,
    payload any,
    state any,
) (*SystemTask, error) {
    if taskType == "" || activeKey == "" || len(activeKey) > 64 {
        return nil, errors.New("invalid system task identity")
    }
    taskID, err := GenerateSystemTaskID()
    if err != nil {
        return nil, err
    }
    payloadText, err := marshalSystemTaskJSON(payload)
    if err != nil {
        return nil, err
    }
    stateText, err := marshalSystemTaskJSON(state)
    if err != nil {
        return nil, err
    }
    activeKeyCopy := activeKey
    task := &SystemTask{
        TaskID:    taskID,
        Type:      taskType,
        Status:    SystemTaskStatusPending,
        ActiveKey: &activeKeyCopy,
        Payload:   payloadText,
        State:     stateText,
    }
    if err := DB.Create(task).Error; err != nil {
        return nil, err
    }
    return task, nil
}

func GetActiveSystemTaskByActiveKey(
    taskType string,
    activeKey string,
) (*SystemTask, error) {
    var task SystemTask
    err := DB.
        Where("type = ? AND active_key = ?", taskType, activeKey).
        Where("status IN ?", activeSystemTaskStatuses()).
        Order("id DESC").
        First(&task).Error
    if errors.Is(err, gorm.ErrRecordNotFound) {
        return nil, nil
    }
    if err != nil {
        return nil, err
    }
    return &task, nil
}
```

`GetActiveSystemTaskByActiveKey` filters by both `type` and `active_key` and only returns
`pending` or `running` rows. The record helper is defined in `relay/relay_task.go`:

```go
func recordSeedDanceSubmitReconciliation(
    info *relaycommon.RelayInfo,
    upstreamTaskID string,
    errorCode string,
) error {
    if info == nil || info.PublicTaskID == "" ||
        info.PersistentTaskID == 0 || upstreamTaskID == "" {
        return errors.New("incomplete Seed Dance reconciliation identity")
    }

    sum := sha256.Sum256([]byte(info.PublicTaskID))
    activeKey := fmt.Sprintf("sd-submit:%x", sum[:16])
    payload := model.SeedDanceSubmitReconciliationPayload{
        PublicTaskID:    info.PublicTaskID,
        UpstreamTaskID:  upstreamTaskID,
        PersistentTaskID: info.PersistentTaskID,
        ChannelID:       info.ChannelId,
        NodeName:        common.NodeName,
        ErrorCode:       errorCode,
        ObservedAt:      common.GetTimestamp(),
    }
    _, err := model.CreateSystemTaskWithActiveKey(
        model.SystemTaskTypeSeedDanceSubmitReconciliation,
        activeKey,
        payload,
        nil,
    )
    if err == nil {
        return nil
    }
    existing, lookupErr := model.GetActiveSystemTaskByActiveKey(
        model.SystemTaskTypeSeedDanceSubmitReconciliation,
        activeKey,
    )
    if lookupErr == nil && existing != nil {
        return nil
    }
    if lookupErr != nil {
        return errors.Join(err, lookupErr)
    }
    return err
}
```

The payload contains no Key, prompt, image, Base64, or full response. Register a handler
that:

```text
idempotently attaches the upstream ID
marks a still-SUBMITTING Task FAILURE/100%
invokes task-level refund
never converts a client-visible failed submission to SUBMITTED
finishes the SystemTask as succeeded only after the Task is terminal
keeps a failed SystemTask record for administrator reconciliation
```

Tests cover duplicate active keys, multiple different public IDs, terminal Task
non-revival, and payload secret scanning.

- [ ] **Step 10: Prove retry semantics, write ordering, and crash recovery**

Add integration tests that record these exact events:

```text
preconsume
validate_full_prepaid
build_body
insert_provisional
post_generate
attach_upstream_id
build_public_response
mark_submitted
settle_zero_delta
mark_submission_settled
consume_log
write_http_200
```

Assertions:

```text
provisional insert failure => POST 0, request refund 1, task refund 0
safe 429 then success => POST 2, one Task row, same public ID, second channel/Key stored
429 with task ID => POST 1, upstream ID attached, no retry
HTTP 200 truncated/invalid/missing ID => POST 1, Retryable false
HTTP 200 business error with task ID => ID attached before Task failure
attach error but DB commit succeeded => read-after-error continues
attach persistent failure => POST 1, no 200, reconciliation record, task-owned refund
commit returned error after DB commit => read-after-error continues to success
commit transient failure => bounded retry succeeds without another POST
commit persistent failure => upstream ID remains, reconciliation record, no 200, task-owned refund
timeout sweep wins before commit => FAILURE remains terminal and is not revived
zero-delta settle failure => upstream ID remains, no success log, task-owned refund
SubmissionSettledAt failure => upstream ID remains, no success log/200, narrow FAILURE,
durable task-owned refund/reconciliation
successful Task 5B => SubmissionSettledAt non-zero and SucceededAt remains zero
timed-out SUBMITTING => no FetchTask call, FAILURE/100%, task-owned refund
shouldRetry 502 with Retryable=false => false
shouldRetry 429 with Retryable=false => false
shouldRetry 429 with Retryable=true => true
shouldRetry legacy error with Retryable=nil => existing status-based result
```

- [ ] **Step 11: Run focused suites**

Run:

```bash
gofmt -w model/task.go model/task_submission.go model/task_submission_test.go \
  model/system_task.go model/system_task_test.go \
  relay/common/relay_info.go relay/relay_task.go relay/relay_task_submission_test.go \
  controller/relay.go controller/relay_task_submission_test.go \
  controller/system_task_handlers.go service/task_submission.go service/task_submission_test.go
go test ./model ./relay ./service ./controller \
  -run 'TestPrepareAttach|TestSubmitting|TestFullPrepaid|TestDurableTaskSubmit|TestSeedDanceSubmit|TestRefundOwner|TestSeedDanceReconciliation' \
  -count=1
```

Expected: PASS; the Mock event list matches the required sequence.

- [ ] **Step 12: Commit**

```bash
git add model/task.go model/task_submission.go model/task_submission_test.go \
  model/system_task.go model/system_task_test.go \
  relay/common/relay_info.go relay/relay_task.go relay/relay_task_submission_test.go \
  controller/relay.go controller/relay_task_submission_test.go \
  controller/system_task_handlers.go service/task_submission.go service/task_submission_test.go
git commit -m "feat: persist Seed Dance submissions before upstream calls"
```

---

### Task 6: Context-Aware Polling, Stored-Key Ownership, and Status Transitions

**Files:**
- Modify: `service/task_polling.go`
- Modify: `service/task_polling_test.go`
- Create: `service/task_key.go`
- Create: `service/task_key_test.go`
- Modify: `relay/channel/task/seedance/adaptor.go`
- Modify: `relay/channel/task/seedance/adaptor_test.go`
- Modify: `model/task.go`
- Modify: `model/task_cas_test.go`

**Interfaces:**
- Consumes: `TaskAdaptor.FetchTask`; `TaskAdaptor.FetchTaskWithContext`; `Task.PrivateData.Key`; `Task.PrivateData.UpstreamTaskID`; `model.TaskStatusSubmitting`; `model.GetTaskBillingAttemptByTaskID`; `model.MarkTaskBillingAttemptSucceeded`.
- Produces: service-local `TaskFetcherWithContext`; `service.ResolveStoredTaskKey`; Seed Dance status mapping, sanitized polling data, locked narrow final-SUCCESS transition, and final-success billing marker orchestration.

- [ ] **Step 1: Write failing context, stored-Key, and state-machine tests**

Add:

```go
type TaskFetcherWithContext interface {
    FetchTaskWithContext(
        ctx context.Context,
        baseURL string,
        key string,
        body map[string]any,
        proxy string,
    ) (*http.Response, error)
}
```

The interface lives in `service/task_polling.go`; the Seed Dance method set satisfies it
without importing `relay/channel` into `service`.

Tests cover:

```go
func TestResolveStoredTaskKeySingleKey(t *testing.T) {
    channel := &model.Channel{
        Key: "KEY_A",
        ChannelInfo: model.ChannelInfo{IsMultiKey: false},
    }
    got, err := ResolveStoredTaskKey(channel, "KEY_A")
    require.NoError(t, err)
    assert.Equal(t, "KEY_A", got)
}

func TestResolveStoredTaskKeyNeverFallsBack(t *testing.T) {
    channel := multiKeyChannel(
        []string{"KEY_A", "KEY_B"},
        map[int]int{0: common.ChannelStatusDisabled, 1: common.ChannelStatusEnabled},
    )
    _, err := ResolveStoredTaskKey(channel, "KEY_A")
    require.Error(t, err)
    assert.NotContains(t, err.Error(), "KEY_A")
    assert.NotContains(t, err.Error(), "KEY_B")
}
```

Also test:

```text
single Key changed after submit => error
empty stored Key => error
multi Key present with no status entry => enabled
multi Key present and enabled => same Key
multi Key disabled, removed, or empty => error
duplicate Key entries resolve only when at least one matching index is enabled
error and logs never contain a Key
scheduler parent cancellation reaches the status HTTP request
legacy adaptor without context method still uses FetchTask
SUBMITTING Task never enters FetchTask or nullTaskIds failure path
```

- [ ] **Step 2: Run focused tests and verify failures**

Run:

```bash
go test ./service ./relay/channel/task/seedance \
  -run 'TestResolveStoredTaskKey|TestTaskPollingContext|TestSeedDanceStatus|TestSubmittingTaskPolling' \
  -count=1
```

Expected: FAIL because stored-Key resolution and context dispatch are absent.

- [ ] **Step 3: Implement strict stored-Key resolution**

Implement:

```go
func ResolveStoredTaskKey(
    channel *model.Channel,
    storedKey string,
) (string, error) {
    if channel == nil || storedKey == "" {
        return "", errors.New("stored task credential is unavailable")
    }
    if !channel.ChannelInfo.IsMultiKey {
        if channel.Key != storedKey {
            return "", errors.New("stored task credential is no longer configured")
        }
        return storedKey, nil
    }
    keys := channel.GetKeys()
    for index, key := range keys {
        if key != storedKey {
            continue
        }
        status := common.ChannelStatusEnabled
        if configured, ok := channel.ChannelInfo.MultiKeyStatusList[index]; ok {
            status = configured
        }
        if status == common.ChannelStatusEnabled {
            return storedKey, nil
        }
    }
    return "", errors.New("stored task credential is disabled or removed")
}
```

Do not log the returned Key or include it in wrapped error text.

- [ ] **Step 4: Dispatch polling with the scheduler context and stored Key**

In `updateVideoSingleTask`:

```go
key, err := ResolveStoredTaskKey(ch, task.PrivateData.Key)
if err != nil {
    return fmt.Errorf("resolve stored task credential: %w", err)
}
request := map[string]any{"task_id": task.GetUpstreamTaskID()}
var resp *http.Response
if withContext, ok := adaptor.(TaskFetcherWithContext); ok {
    resp, err = withContext.FetchTaskWithContext(
        ctx, ch.GetBaseURL(), key, request, ch.GetSetting().Proxy,
    )
} else {
    resp, err = adaptor.FetchTask(
        ch.GetBaseURL(), key, request, ch.GetSetting().Proxy,
    )
}
```

Before any platform grouping, skip `TaskStatusSubmitting` defensively even though the
model query excludes it. Never call `GetUpstreamTaskID` on that status.

- [ ] **Step 5: Implement sanitized status parsing and terminal behavior**

Seed Dance `FetchTaskWithContext` must inspect both HTTP status and the business envelope.
It returns a new response Body containing only:

```go
type cleanedStatusData struct {
    RequestID  string `json:"requestId,omitempty"`
    Success    *bool  `json:"success,omitempty"`
    ErrCode    string `json:"errCode,omitempty"`
    ErrMessage string `json:"errMessage,omitempty"`
    Status     string `json:"status,omitempty"`
    Message    string `json:"message,omitempty"`
}
```

Never include `task_id`, `optimized_prompt`, or a Base64 field. `ParseTaskResult` uses
the mapping from Task 4 and returns an error for any unknown state. It must set:

```text
failed => TaskStatusFailure, Progress 100%, sanitized failure reason
completed => TaskStatusSuccess, Progress 100%, ResultURL /v1/videos/{publicID}/content
accepted/queued/running/processing => no terminal billing
```

Durable failure uses the locked narrow FAILURE primitive and can refund a task-owned
attempt whenever `SucceededAt==0` and the locked Task is `FAILURE/100%`, including when
`SubmissionSettledAt!=0`. `SubmissionSettledAt` may therefore coexist with
`RefundStartedAt/RefundCompletedAt`; `SucceededAt` must remain mutually exclusive with
all refund markers.

Durable success must not use the historical stale full-row CAS path. Task 6 polling
owns this exact order:

```text
lock Task + linked TaskBillingAttempt
narrow transition expected non-terminal state -> SUCCESS/100%
complete final billing settlement successfully
call MarkTaskBillingAttemptSucceeded(attempt.RequestID)
```

Only after the narrow transition and final settlement succeed may
`MarkTaskBillingAttemptSucceeded` write `SucceededAt`. A lost transition, billing
failure, marker conflict, or active refund writes no `SucceededAt` and remains
reconcilable; Task 5B never performs or tests this sequence. Final success keeps the
pre-consumed quota. Polling never invokes `/video`.

Add tests that prove:

```text
Task 5B-created SubmissionSettledAt alone does not make the attempt terminal
settled submission + provider FAILURE/100% can complete both refund components
SUCCESS narrow transition precedes final settlement and SucceededAt
final-settlement failure leaves SucceededAt=0
SucceededAt cannot coexist with RefundStartedAt or RefundCompletedAt
active retention is SucceededAt==0 && RefundCompletedAt==0
```

- [ ] **Step 6: Run focused and existing polling regression tests**

Run:

```bash
gofmt -w service/task_polling.go service/task_polling_test.go \
  service/task_key.go service/task_key_test.go \
  relay/channel/task/seedance/adaptor.go relay/channel/task/seedance/adaptor_test.go \
  model/task.go model/task_cas_test.go
go test ./service ./model ./relay/channel/task/seedance \
  -run 'TestResolveStoredTaskKey|TestTaskPolling|TestSeedDanceStatus|TestSubmitting' \
  -count=1
```

Expected: PASS, including all pre-existing polling CAS and refund tests.

- [ ] **Step 7: Commit**

```bash
git add service/task_polling.go service/task_polling_test.go \
  service/task_key.go service/task_key_test.go \
  relay/channel/task/seedance/adaptor.go relay/channel/task/seedance/adaptor_test.go \
  model/task.go model/task_cas_test.go
git commit -m "feat: poll Seed Dance tasks with stored credentials"
```

---

### Task 7: Streaming Video JSON Extraction, Strict Base64 Decode, and MP4 Validation

**Files:**
- Create: `relay/channel/task/seedance/content_json.go`
- Create: `relay/channel/task/seedance/content.go`
- Create: `relay/channel/task/seedance/content_test.go`

**Interfaces:**
- Consumes: `channel.VideoContent`; `channel.VideoContentError`; `newStageClient`; `contentTimeout`; Seed Dance provider business envelope.
- Produces: `extractVideoBase64JSON(src io.Reader, redacted io.Writer, encoded io.Writer) error`; `decodeVideoBase64(src io.ReadSeeker, dst io.Writer) (int64, error)`; `validateMP4(src io.ReadSeeker) error`; `TaskAdaptor.FetchVideoContent`.

- [ ] **Step 1: Write failing streaming parser and lifecycle tests**

Use a test-local temporary directory through an injectable package variable:

```go
func TestExtractVideoBase64JSON(t *testing.T) {
    cases := []struct {
        name    string
        input   string
        encoded string
        wantErr bool
    }{
        {"plain", `{"requestId":"R","video_base64":"QUJDRA=="}`, "QUJDRA==", false},
        {"escaped slash", `{"video_base64":"QUJD\\/RA=="}`, "QUJD/RA==", false},
        {"unicode escape rejected in base64", `{"video_base64":"QUJD\\u003d"}`, "", true},
        {"duplicate", `{"video_base64":"QQ==","video_base64":"Qg=="}`, "", true},
        {"non string", `{"video_base64":null}`, "", true},
        {"truncated", `{"video_base64":"QQ==`, "", true},
        {"trailing junk", `{"video_base64":"QQ=="}x`, "", true},
        {"missing", `{"success":true}`, "", true},
    }
    _ = cases
}
```

Add tests for:

```text
pure Base64 MP4
data:video/mp4;base64, prefix
data:video/webm;base64, rejected
invalid alphabet in the middle and at the tail
whitespace inside Base64 rejected
non-canonical padding rejected
decoded file shorter than an MP4 box rejected
file without ftyp at offset 4 rejected
valid ftyp accepted
HTTP 200 success business shape
HTTP 200 success:false business error
401/403 => structured 502 authentication error
429 => structured 429 rate-limit error
download timeout => structured 504 timeout error
parent cancellation stops the upstream read
large declared Content-Length with a small valid Body is not pre-rejected
success, JSON error, Base64 error, MP4 error, timeout, and cancellation leave no temp files
ContentLength equals the real decoded file length
```

Use a tracking reader that fails after N bytes and a tracking temp directory; do not add
a large video or Base64 fixture to Git.

- [ ] **Step 2: Run tests and verify missing symbols**

Run:

```bash
go test ./relay/channel/task/seedance \
  -run 'TestExtractVideoBase64JSON|TestDecodeVideoBase64|TestValidateMP4|TestFetchVideoContent' \
  -count=1
```

Expected: FAIL because content extraction and fetching are undefined.

- [ ] **Step 3: Implement the streaming JSON string extractor**

The parser must consume exactly one complete JSON root object and copy every token except
the `video_base64` value to `redacted`. For that value, write the decoded JSON string
bytes to `encoded` and write the literal JSON string `"[redacted]"` to `redacted`.

Use explicit parser state:

```go
type jsonScanState uint8

const (
    scanValue jsonScanState = iota
    scanObjectKey
    scanObjectColon
    scanObjectComma
    scanArrayValue
    scanArrayComma
)

type jsonFrame struct {
    object bool
    state  jsonScanState
}
```

String decoding rules:

```go
switch escaped {
case '"', '\\', '/':
    out.Write([]byte{escaped})
case 'b':
    out.Write([]byte{'\b'})
case 'f':
    out.Write([]byte{'\f'})
case 'n':
    out.Write([]byte{'\n'})
case 'r':
    out.Write([]byte{'\r'})
case 't':
    out.Write([]byte{'\t'})
case 'u':
    // Decode exactly four hex digits and combine a valid surrogate pair.
    // Reject an isolated high or low surrogate.
default:
    return errInvalidJSONStringEscape
}
```

The root parser must reject duplicate root `video_base64` keys, a non-string value,
unbalanced arrays/objects, incomplete literals/numbers, and any non-whitespace after the
root. It may preserve nested unknown fields in the redacted output, but it only recognizes
`video_base64` at the root level. Return `errVideoBase64Missing` when the complete root has
no such field.

Do not call `io.ReadAll` or decode the Base64 value into a `string`/`[]byte`.

- [ ] **Step 4: Implement strict streaming Base64 and MP4 checks**

Before decoding, inspect only the fixed prefix:

```go
const videoDataPrefix = "data:video/mp4;base64,"
```

If the source starts with `data:` but not the exact prefix, return
`errUnsupportedVideoDataURI`. Otherwise seek back to the first Base64 byte.

Wrap the encoded reader with a validator that accepts only:

```text
A-Z a-z 0-9 + /
= only in the final legal padding positions
no spaces, tabs, CR, or LF
length divisible by 4
canonical final quantum
```

Then stream through:

```go
decoder := base64.NewDecoder(base64.StdEncoding.Strict(), validatedReader)
written, err := io.Copy(dst, decoder)
```

After `io.Copy`, force one more read to surface a deferred decoder error. Seek the MP4
file to zero and validate:

```go
header := make([]byte, 12)
if _, err := io.ReadFull(src, header); err != nil {
    return errInvalidMP4
}
boxSize := binary.BigEndian.Uint32(header[:4])
if string(header[4:8]) != "ftyp" || boxSize < 8 {
    return errInvalidMP4
}
```

Seek back to zero before returning the file.

- [ ] **Step 5: Implement `0600` temporary-file ownership**

Create files only with:

```go
os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
```

The content flow is:

```text
upstream Body -> raw response temp
raw seek(0) -> redacted JSON temp + Base64 temp
redacted seek(0) -> common.DecodeJson business-envelope validation
Base64 seek(0) -> strict decoder -> MP4 temp
MP4 seek(0) -> ftyp validation
MP4 seek(0) -> returned Body
```

Define:

```go
type removingReadCloser struct {
    file    *os.File
    paths   []string
    once    sync.Once
}

func (r *removingReadCloser) Read(p []byte) (int, error) {
    return r.file.Read(p)
}

func (r *removingReadCloser) Close() error {
    var closeErr error
    r.once.Do(func() {
        closeErr = r.file.Close()
        for _, path := range r.paths {
            _ = os.Remove(path)
        }
    })
    return closeErr
}
```

Every error path closes and removes all four files. After successful extraction and
validation, raw/redacted/Base64 files may be closed and removed immediately; the MP4 file
is removed by `VideoContent.Body.Close`.

- [ ] **Step 6: Implement `FetchVideoContent` with structured errors**

Implement the declared interface:

```go
func (a *TaskAdaptor) FetchVideoContent(
    parent context.Context,
    baseURL string,
    key string,
    upstreamTaskID string,
    proxy string,
) (*channel.VideoContent, error)
```

Request:

```go
ctx, cancel := context.WithTimeout(parent, contentTimeout)
req, err := http.NewRequestWithContext(
    ctx,
    http.MethodGet,
    strings.TrimRight(baseURL, "/")+"/video/"+url.PathEscape(upstreamTaskID),
    nil,
)
req.Header.Set("Authorization", "Bearer "+key)
```

On success, bind `cancel` to the upstream Body while copying it to the raw temp. Close
the upstream Body before parsing. Map errors with:

```go
func contentError(
    status int,
    errorType string,
    code string,
    message string,
    cause error,
) error {
    return &channel.VideoContentError{
        StatusCode: status,
        Type:       errorType,
        Code:       code,
        Message:    message,
        Cause:      cause,
    }
}
```

Use:

```text
upstream 401/403 -> 502, upstream_error, upstream_authentication_error
upstream 429 -> 429, upstream_rate_limit_error, upstream_rate_limit_error
deadline -> 504, upstream_timeout_error, upstream_timeout_error
network -> 502, upstream_error, upstream_connection_error
business/JSON -> 502, invalid_upstream_response, invalid_upstream_response
Base64/MP4 -> 502, invalid_upstream_response, invalid_upstream_response
```

Client-facing `Message` is fixed and sanitized. Put provider detail only in `Cause`.
Return:

```go
&channel.VideoContent{
    ContentType:   "video/mp4",
    ContentLength: decodedLength,
    Body:          removingBody,
}
```

Do not reject based on the upstream `Content-Length` and do not introduce a Seed Dance
JSON, Base64, or MP4 size constant.

- [ ] **Step 7: Run focused tests and a memory-shape check**

Run:

```bash
gofmt -w relay/channel/task/seedance/content_json.go \
  relay/channel/task/seedance/content.go \
  relay/channel/task/seedance/content_test.go
go test ./relay/channel/task/seedance \
  -run 'TestExtractVideoBase64JSON|TestDecodeVideoBase64|TestValidateMP4|TestFetchVideoContent' \
  -count=1
! rg -n 'io\\.ReadAll\\(resp\\.Body\\)|DecodeString\\(.*video' \
  relay/channel/task/seedance/content*.go
```

Expected: PASS; the content tests report an empty temporary directory on every subtest.

- [ ] **Step 8: Commit**

```bash
git add relay/channel/task/seedance/content_json.go \
  relay/channel/task/seedance/content.go \
  relay/channel/task/seedance/content_test.go
git commit -m "feat: stream Seed Dance video content"
```

---

### Task 8: OpenAI Video Content Controller, Nested Errors, and Route Compatibility

**Files:**
- Modify: `controller/video_proxy.go`
- Create: `controller/video_proxy_test.go`
- Modify: `controller/relay.go`
- Create: `controller/relay_task_test.go`
- Verify: `router/video-router.go`
- Create: `router/video_router_test.go`

**Interfaces:**
- Consumes: `channel.VideoContentFetcher`; `channel.VideoContentError`; `service.ResolveStoredTaskKey`; `Task.PrivateData.UpstreamTaskID`; `Task.Platform`.
- Produces: Seed Dance `/v1/videos/{task_id}/content` proxy branch; nested OpenAI video error writer; OpenAI-only 404 semantics.

- [ ] **Step 1: Write failing controller tests with a header-tracking writer**

Use a writer that records whether headers were committed:

```go
type trackingWriter struct {
    httptest.ResponseRecorder
    wroteHeader bool
}

func (w *trackingWriter) WriteHeader(code int) {
    w.wroteHeader = true
    w.ResponseRecorder.WriteHeader(code)
}

func (w *trackingWriter) Write(p []byte) (int, error) {
    if !w.wroteHeader {
        w.WriteHeader(http.StatusOK)
    }
    return w.ResponseRecorder.Write(p)
}
```

Tests:

```text
missing task on /v1/videos/{id} => 404 nested task_not_found
other user's task => 404, not 403
missing task on /v1/videos/{id}/content => 404 nested task_not_found
invalid client Token => 401
SUBMITTED/IN_PROGRESS content => 400 task_not_completed
FAILURE content => 400 task_failed
SUCCESS Seed Dance => uses stored Key and upstream ID
structured fetch 429/502/504 => exact status/type/code
plain fetch error => fixed sanitized 502
fetch failure never commits a 200 header
success headers and actual Content-Length are exact
copy closes VideoContent.Body
Gemini, Vertex, Sora, URL, and data-URL legacy branches still pass existing tests
old /v1/video/generations missing task remains its existing flat 400
```

- [ ] **Step 2: Run controller/router tests and verify failures**

Run:

```bash
go test ./controller ./router \
  -run 'TestVideoProxy|TestOpenAIVideoError|TestOpenAIVideoNotFound|TestLegacyVideo' \
  -count=1
```

Expected: FAIL because the generic content fetcher branch and nested code-aware writer
are absent.

- [ ] **Step 3: Add an OpenAI video error writer without changing legacy routes**

Implement:

```go
func writeOpenAIVideoError(
    c *gin.Context,
    status int,
    errorType string,
    code string,
    message string,
) {
    c.AbortWithStatusJSON(status, gin.H{
        "error": gin.H{
            "message": message,
            "type":    errorType,
            "code":    code,
        },
    })
}
```

Select this writer only when the request path is one of:

```text
/v1/videos
/v1/videos/{task_id}
/v1/videos/{task_id}/content
```

Keep the existing `respondTaskError`/flat writer for
`/v1/video/generations/...`. For OpenAI video status and content lookup, query by
`user_id + public task_id`; map both missing and other-user rows to 404.

- [ ] **Step 4: Dispatch Seed Dance content through the adaptor interface**

After ownership and terminal-state checks:

```go
adaptor := relay.GetTaskAdaptor(task.Platform)
fetcher, ok := adaptor.(channel.VideoContentFetcher)
if !ok {
    // Continue through the existing Gemini/Vertex/Sora/URL/data-URL logic.
}
channelModel, err := model.GetChannelById(task.ChannelId, true)
if err != nil {
    writeOpenAIVideoError(
        c, http.StatusBadGateway,
        "upstream_error", "channel_unavailable",
        "video channel is unavailable",
    )
    return
}
key, err := service.ResolveStoredTaskKey(channelModel, task.PrivateData.Key)
if err != nil {
    writeOpenAIVideoError(
        c, http.StatusBadGateway,
        "upstream_error", "stored_credential_unavailable",
        "video channel credential is unavailable",
    )
    return
}
content, err := fetcher.FetchVideoContent(
    c.Request.Context(),
    channelModel.GetBaseURL(),
    key,
    task.PrivateData.UpstreamTaskID,
    channelModel.GetSetting().Proxy,
)
```

Do not log `key`. On error:

```go
var structured *channel.VideoContentError
if errors.As(err, &structured) {
    if structured.Cause != nil {
        logger.LogError(c, structured.Cause.Error())
    }
    writeOpenAIVideoError(
        c,
        structured.StatusCode,
        structured.Type,
        structured.Code,
        structured.Message,
    )
    return
}
logger.LogError(c, err.Error())
writeOpenAIVideoError(
    c,
    http.StatusBadGateway,
    "upstream_error",
    "upstream_connection_error",
    "failed to fetch video content",
)
```

Only after `FetchVideoContent` succeeds:

```go
defer content.Body.Close()
c.Header("Content-Type", "video/mp4")
c.Header(
    "Content-Disposition",
    fmt.Sprintf(`inline; filename="%s.mp4"`, task.TaskID),
)
c.Header("Cache-Control", "private, max-age=3600")
c.Header("X-Content-Type-Options", "nosniff")
c.Header("Content-Length", strconv.FormatInt(content.ContentLength, 10))
c.Status(http.StatusOK)
_, copyErr := io.Copy(c.Writer, content.Body)
```

- [ ] **Step 5: Preserve existing routes and run the controller regression suite**

Confirm `router/video-router.go` still registers exactly:

```go
videoV1Router.POST("/videos", controller.RelayTask)
videoV1Router.GET("/videos/:task_id", controller.RelayTaskFetch)
videoProxyRouter.GET("/videos/:task_id/content", controller.VideoProxy)
```

Do not change old route registrations. Run:

```bash
gofmt -w controller/video_proxy.go controller/video_proxy_test.go \
  controller/relay.go controller/relay_task_test.go \
  router/video-router.go router/video_router_test.go
go test ./controller ./router -count=1
```

Expected: PASS. The successful response has:

```text
Content-Type: video/mp4
Content-Disposition: inline; filename="task_public.mp4"
Cache-Control: private, max-age=3600
X-Content-Type-Options: nosniff
Content-Length: exact decoded length
```

- [ ] **Step 6: Commit**

```bash
git add controller/video_proxy.go controller/video_proxy_test.go \
  controller/relay.go controller/relay_task_test.go \
  router/video-router.go router/video_router_test.go
git commit -m "feat: proxy Seed Dance video content"
```

---

### Task 9: Seed Dance Channel Test Probe

**Files:**
- Modify: `controller/channel-test.go`
- Create: `controller/channel_seedance_test.go`

**Interfaces:**
- Consumes: Type 59 constant; channel Base URL, selected enabled Key, proxy; Seed Dance
  status business-error parser.
- Produces: `testSeedDanceChannel(ctx context.Context, channel *model.Channel) error`; an
  early Type 59 branch in `testChannel`.

- [ ] **Step 1: Write failing zero-cost probe tests**

Create an `httptest.Server` and assert:

```go
func TestSeedDanceChannelTestUsesMissingTaskProbe(t *testing.T) {
    var gotPath string
    server := httptest.NewServer(http.HandlerFunc(func(
        w http.ResponseWriter,
        r *http.Request,
    ) {
        gotPath = r.URL.Path
        assert.Equal(t, "Bearer TEST_KEY", r.Header.Get("Authorization"))
        w.Header().Set("Content-Type", "application/json")
        _, _ = io.WriteString(w,
            `{"success":false,"errCode":"400","errMessage":"Task not found"}`,
        )
    }))
    defer server.Close()
    // Seed a disabled-free Type 59 channel and invoke the controller test endpoint.
    // Assert success, no /generate request, and the task row count is unchanged.
    assert.Regexp(t, `^/status/new-api-channel-test-[A-Za-z0-9]+$`, gotPath)
}
```

Add cases:

```text
explicit Task not found with valid auth => pass
401/403 => fail
DNS/network error => fail
deadline => fail
configured HTTP/SOCKS proxy is used
multi-Key picks one enabled Key
unpriced seedance-uncensored still reaches the probe
sync relay.GetAdaptor is never called
tasks count and user quota are unchanged
```

- [ ] **Step 2: Run the focused controller test**

Run:

```bash
go test ./controller -run TestSeedDanceChannelTest -count=1
```

Expected: FAIL because Type 59 still enters the generic chat-model channel test path.

- [ ] **Step 3: Implement and place the early branch**

At the start of `testChannel`, immediately after its existing `ctx == nil` normalization
and before the unsupported-type list, Gin test context creation, `GenRelayInfo`, model
price resolution, endpoint selection, or `relay.GetAdaptor`, use the function's `ctx`
argument and preserve its real `testResult` return type:

```go
if channel.Type == constant.ChannelTypeSeedDance {
    err := testSeedDanceChannel(ctx, channel)
    if err != nil {
        return testResult{
            localErr: err,
            newAPIError: types.NewError(
                err,
                types.ErrorCodeBadResponseStatusCode,
            ),
        }
    }
    return testResult{}
}
```

Do not calculate elapsed time inside this branch. The existing manual and scheduled
callers already time the complete `testChannel` call and update response-time metrics.

The helper:

```go
probeID, err := common.GenerateRandomCharsKey(24)
if err != nil {
    return err
}
key, _, apiErr := channel.GetNextEnabledKey()
if apiErr != nil {
    return apiErr
}
probeURL := strings.TrimRight(channel.GetBaseURL(), "/") +
    "/status/new-api-channel-test-" + probeID
```

Use a 30-second context, the channel proxy, and `Authorization: Bearer {key}`. Treat
only a fully decoded business response that clearly means “task not found” as success.
Authentication, transport, malformed response, and unrelated business errors fail. Do
not log the Key or full Body.

- [ ] **Step 4: Run tests and commit**

Run:

```bash
gofmt -w controller/channel-test.go controller/channel_seedance_test.go
go test ./controller -run 'TestSeedDanceChannelTest|TestChannelTest' -count=1
```

Expected: PASS.

Commit:

```bash
git add controller/channel-test.go controller/channel_seedance_test.go
git commit -m "feat: add Seed Dance channel health probe"
```

---

### Task 10: Frontend Channel Defaults, Validation, Icon, and Seven-Locale i18n

**Files:**
- Modify: `web/src/features/channels/constants.ts`
- Modify: `web/src/features/channels/lib/channel-type-config.ts`
- Modify: `web/src/features/channels/lib/channel-utils.ts`
- Modify: `web/src/features/channels/lib/channel-form.ts`
- Modify: `web/src/features/channels/components/drawers/channel-mutate-drawer.tsx`
- Create: `web/src/features/channels/lib/__tests__/seed-dance-config.test.ts`
- Modify: `web/src/i18n/static-keys.ts`
- Modify: `web/src/i18n/locales/en.json`
- Modify: `web/src/i18n/locales/zh.json`
- Modify: `web/src/i18n/locales/zh-TW.json`
- Modify: `web/src/i18n/locales/fr.json`
- Modify: `web/src/i18n/locales/ru.json`
- Modify: `web/src/i18n/locales/ja.json`
- Modify: `web/src/i18n/locales/vi.json`

**Interfaces:**
- Consumes: `CHANNEL_TYPES`; `CHANNEL_TYPE_OPTIONS`; `getChannelTypeConfig`; `channelFormSchema`; existing drawer type-change effect.
- Produces: Type 59 UI metadata; `getChannelTypeCreateDefaults(type: number): {baseUrl: string; models: string}`; Base URL validation; seven translated labels.

- [ ] **Step 1: Write the failing Node test for every user-visible contract**

Create:

```ts
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import { CHANNEL_TYPES, CHANNEL_TYPE_OPTIONS } from '../../constants'
import {
  getChannelTypeConfig,
  getChannelTypeCreateDefaults,
} from '../channel-type-config'
import { channelFormSchema } from '../channel-form'
import { getChannelTypeIcon } from '../channel-utils'

const seedDanceBaseUrl =
  'http://alb-o13xqj8f2cpjsa67ym.ap-northeast-1.alb.aliyuncsslbintl.com/v1/public_api/m-predict/polar4ai-i2v'

describe('Seed Dance channel configuration', () => {
  test('publishes one ordered Type 59 option', () => {
    assert.equal(CHANNEL_TYPES[59], 'Uncensored Seed Dance')
    const ids = CHANNEL_TYPE_OPTIONS.map((item) => item.value)
    assert.equal(ids.filter((id) => id === 59).length, 1)
    assert.ok(ids.indexOf(54) < ids.indexOf(59))
    assert.ok(ids.indexOf(59) < ids.indexOf(55))
  })

  test('provides the create defaults and icon', () => {
    assert.deepEqual(getChannelTypeCreateDefaults(59), {
      baseUrl: seedDanceBaseUrl,
      models: 'seedance-uncensored',
    })
    assert.equal(getChannelTypeConfig(59).defaultBaseUrl, seedDanceBaseUrl)
    assert.deepEqual(getChannelTypeConfig(59).supportedModels, [
      'seedance-uncensored',
    ])
    assert.equal(getChannelTypeIcon(59), 'Doubao')
  })

  test('requires a non-blank Base URL', () => {
    const base = {
      name: 'Seed Dance',
      type: 59,
      key: 'TEST_KEY',
      models: 'seedance-uncensored',
      group: ['default'],
      status: 2,
    }
    assert.equal(
      channelFormSchema.safeParse({ ...base, base_url: '   ' }).success,
      false
    )
    assert.equal(
      channelFormSchema.safeParse({
        ...base,
        base_url: seedDanceBaseUrl,
      }).success,
      true
    )
  })
})
```

- [ ] **Step 2: Run the test and verify Type 59 is absent**

Run:

```bash
cd web
bun test src/features/channels/lib/__tests__/seed-dance-config.test.ts
```

Expected: FAIL because `CHANNEL_TYPES[59]` and
`getChannelTypeCreateDefaults` do not exist.

- [ ] **Step 3: Add Type 59 metadata and a pure create-default function**

Add:

```ts
export const SEED_DANCE_DEFAULT_BASE_URL =
  'http://alb-o13xqj8f2cpjsa67ym.ap-northeast-1.alb.aliyuncsslbintl.com/v1/public_api/m-predict/polar4ai-i2v'
```

Add `59: 'Uncensored Seed Dance'` to `CHANNEL_TYPES`, and change the video tail of
`CHANNEL_TYPE_DISPLAY_ORDER` to:

```ts
50, 51, 52, 53, 54, 59, 55, 56
```

Add:

```ts
59: {
  id: 59,
  name: CHANNEL_TYPES[59],
  icon: 'doubao',
  defaultBaseUrl: SEED_DANCE_DEFAULT_BASE_URL,
  supportedModels: ['seedance-uncensored'],
  hints: {
    baseUrl: 'Seed Dance upstream base URL',
    key: 'Bearer API key',
    models: 'seedance-uncensored',
  },
},
```

Export:

```ts
export function getChannelTypeCreateDefaults(type: number): {
  baseUrl: string
  models: string
} {
  const config = getChannelTypeConfig(type)
  return {
    baseUrl: config.defaultBaseUrl || '',
    models: (config.supportedModels || []).join(','),
  }
}
```

Map `59: 'Doubao'` in `getChannelTypeIcon`.

- [ ] **Step 4: Make the drawer consume defaults once per type change**

Import `getChannelTypeCreateDefaults`. In the existing non-editing type-change effect:

```ts
if (currentType === 59) {
  const defaults = getChannelTypeCreateDefaults(59)
  form.setValue('base_url', defaults.baseUrl, {
    shouldDirty: true,
    shouldValidate: true,
  })
  form.setValue('models', defaults.models, {
    shouldDirty: true,
    shouldValidate: true,
  })
}
```

The effect dependency remains `[currentType, isEditing, form]`; it runs on an explicit
type change, not on every field render. When switching from another provider to Type 59,
it intentionally replaces that provider's Base URL and model list. Editing an existing
channel never overwrites saved values.

Change both required sets:

```ts
const providerRequiresBaseUrl = [3, 8, 36, 45, 59].includes(currentType)
```

```ts
if ([3, 8, 36, 45, 59].includes(data.type) && !data.base_url?.trim()) {
  addRequiredIssue(
    ctx,
    'base_url',
    'Base URL is required for this channel type'
  )
}
```

Keep Type 59 in the generic Base URL field by leaving it out of the exclusion array
`[3, 8, 22, 36, 45]`.

- [ ] **Step 5: Add the static key and exact seven translations**

Add `Uncensored Seed Dance` to `STATIC_I18N_KEYS` and to each locale:

```text
en     Uncensored Seed Dance
zh     无审核 Seed Dance
zh-TW  無審核 Seed Dance
fr     Seed Dance sans modération
ru     Seed Dance без модерации
ja     無審査 Seed Dance
vi     Seed Dance không kiểm duyệt
```

JSON entries:

```json
"Uncensored Seed Dance": "无审核 Seed Dance"
```

Use the corresponding value in each file; do not change language character sets.

- [ ] **Step 6: Verify deterministic i18n sync and the frontend build**

Run:

```bash
cd web
bun install --frozen-lockfile
bun test src/features/channels/lib/__tests__/seed-dance-config.test.ts
i18n_before="$(mktemp)"
git diff -- src/i18n/locales src/i18n/static-keys.ts > "$i18n_before"
bun run i18n:sync
git diff -- src/i18n/locales src/i18n/static-keys.ts > "${i18n_before}.after"
cmp "$i18n_before" "${i18n_before}.after"
rm -f "$i18n_before" "${i18n_before}.after"
bun run typecheck
bun run lint
bun run format:check
bun run build
cd ..
```

Expected: every command succeeds and a second i18n sync makes no change.

- [ ] **Step 7: Commit**

```bash
git add web/src/features/channels/constants.ts \
  web/src/features/channels/lib/channel-type-config.ts \
  web/src/features/channels/lib/channel-utils.ts \
  web/src/features/channels/lib/channel-form.ts \
  web/src/features/channels/components/drawers/channel-mutate-drawer.tsx \
  web/src/features/channels/lib/__tests__/seed-dance-config.test.ts \
  web/src/i18n/static-keys.ts \
  web/src/i18n/locales/en.json web/src/i18n/locales/zh.json \
  web/src/i18n/locales/zh-TW.json web/src/i18n/locales/fr.json \
  web/src/i18n/locales/ru.json web/src/i18n/locales/ja.json \
  web/src/i18n/locales/vi.json
git commit -m "feat: configure Seed Dance channel in the web UI"
```

---

### Task 11: Markdown Test Guide, Sanitized Fixtures, and Apifox/OpenAPI Contracts

**Files:**
- Create: `docs/api/seed-dance-testing.md`
- Create: `docs/api/seed-dance-openapi.yaml`
- Create: `docs/api/openapi_contract_test.go`
- Create: `docs/api/fixtures/seed-dance/generate-response.json`
- Create: `docs/api/fixtures/seed-dance/status-accepted.json`
- Create: `docs/api/fixtures/seed-dance/status-processing.json`
- Create: `docs/api/fixtures/seed-dance/status-completed.json`
- Create: `docs/api/fixtures/seed-dance/status-business-error.json`
- Create: `docs/api/fixtures/seed-dance/ffprobe-output.json`
- Modify: `docs/openapi/relay.json`

**Interfaces:**
- Consumes: the three public OpenAI video routes and nested error schema; normalized request fields; public task/status DTO.
- Produces: importable OpenAPI 3.0.3; synchronized canonical OpenAPI 3.0.1; Markdown/cURL/Python test guide; deterministic contract tests.

- [ ] **Step 1: Write the failing OpenAPI contract test before creating the documents**

Use `gopkg.in/yaml.v3` and `common.Unmarshal`; do not add a new parser dependency.
Create:

```go
func TestSeedDanceStandaloneOpenAPIContract(t *testing.T) {
    doc := loadYAMLDocument(t, "seed-dance-openapi.yaml")
    require.Equal(t, "3.0.3", stringAt(t, doc, "openapi"))
    assertVideoOperations(t, doc)
    assertLocalRefsResolve(t, doc)
    assertExamplesMatchBasicSchema(t, doc)
}

func TestCanonicalRelayVideoOpenAPIContract(t *testing.T) {
    doc := loadJSONDocument(t, "../openapi/relay.json")
    require.Equal(t, "3.0.1", stringAt(t, doc, "openapi"))
    assertVideoOperations(t, doc)
    assertLocalRefsResolve(t, doc)
    assertExamplesMatchBasicSchema(t, doc)
}

func TestSeedDanceVideoContractsStayInSync(t *testing.T) {
    standalone := videoContractProjection(
        loadYAMLDocument(t, "seed-dance-openapi.yaml"),
    )
    canonical := videoContractProjection(
        loadJSONDocument(t, "../openapi/relay.json"),
    )
    assert.Equal(t, standalone, canonical)
}
```

`assertVideoOperations` must deterministically assert:

```text
POST /v1/videos exists
GET /v1/videos/{task_id} exists
GET /v1/videos/{task_id}/content exists
POST has application/json and multipart/form-data
all three operations require BearerAuth
content 200 has video/mp4 with type string and format binary
400, 401, 404, 429, 502, 504 reference the nested error schema where applicable
error.message, error.type, and error.code exist and are required
```

- [ ] **Step 2: Run the contract test and verify missing artifacts**

Run:

```bash
go test ./docs/api/... -count=1
```

Expected: FAIL because `seed-dance-openapi.yaml` is not present.

- [ ] **Step 3: Create sanitized behavior fixtures**

Use only synthetic values:

```json
{
  "requestId": "REQUEST_ID",
  "task_id": "UPSTREAM_TASK_ID",
  "status": "accepted"
}
```

Status fixtures contain `accepted`, `processing`, and `completed`; the business error is:

```json
{
  "success": false,
  "errCode": "400",
  "errMessage": "{\"detail\":\"Task not found\"}",
  "data": null
}
```

`ffprobe-output.json` contains a synthetic format name and H.264/AAC stream descriptions
without a local absolute path. None of the fixtures contains `Authorization`, a Key,
real task ID, `image_base64`, or `video_base64`.

- [ ] **Step 4: Create the complete standalone OpenAPI 3.0.3 document**

The document header and security scheme are:

```yaml
openapi: 3.0.3
info:
  title: New API Seed Dance Video API
  version: 1.0.0
servers:
  - url: "{{base_url}}"
components:
  securitySchemes:
    BearerAuth:
      type: http
      scheme: bearer
      bearerFormat: API key
```

Define `VideoCreateJSON` with:

```text
model: required string, example seedance-uncensored
prompt: required string
duration: integer, minimum 1, maximum 15, default 15
seconds: oneOf integer or decimal-digit string
size: enum 854x480,480x854,1280x720,720x1280,1920x1080,1080x1920
image: string
images: array of string, maxItems 1
input_reference: string
metadata.duration: integer or decimal-digit string
metadata.resolution: enum 480P,720P,1080P
metadata.image_base64: string
metadata.prompt_optimization: boolean
metadata.multi_shot: boolean
metadata.strict_duration: boolean
metadata.negative_prompt: string
```

Define multipart with:

```text
model, prompt, duration, seconds, size: string fields
metadata: JSON-encoded string field
input_reference: type string, format binary
```

Do not add `maxLength` to images or videos. Define `OpenAIVideo` with public ID,
task ID, object, model, status, progress, timestamps, seconds, size, and optional error.
Define:

```yaml
OpenAIError:
  type: object
  required: [error]
  properties:
    error:
      type: object
      required: [message, type, code]
      properties:
        message: {type: string}
        type: {type: string}
        code: {type: string}
```

All three operations use `security: [{BearerAuth: []}]`. The content operation declares:

```yaml
content:
  video/mp4:
    schema:
      type: string
      format: binary
```

- [ ] **Step 5: Synchronize the canonical OpenAPI without changing its version**

Keep:

```json
"openapi": "3.0.1"
```

Update `/v1/videos` to include both request media types, keep the generic model field
open to other video adaptors, and add the same normalized fields and examples. Update the
status and content routes to reference shared `OpenAIVideo` and nested `ErrorResponse`.
Keep the content schema:

```json
{
  "type": "string",
  "format": "binary"
}
```

Do not remove existing Sora/OpenAI video behavior from the canonical document.

- [ ] **Step 6: Write the Markdown and executable request examples**

`seed-dance-testing.md` must include:

```text
create a disabled Type 59 channel
configure seedance-uncensored
ModelPrice and ModelRatio formulas
GroupRatio behavior
T2V JSON
I2V pure Base64/data URI/remote URL
I2V multipart file
status polling
MP4 download
Bash polling loop
Python polling loop
queued/in_progress/completed/failed
nested error examples
Apifox import and environment variables
troubleshooting
5 MB is a supplier recommendation, not a New API limit
no documented supplier JSON/Base64/MP4 maximum and no Seed Dance-specific pre-limit
actual MP4 duration does not recalculate the charge
```

Use only:

```text
{{base_url}}
{{api_key}}
{{task_id}}
```

The Bash example stores the public ID:

```bash
task_id="$(
  curl -fsS \
    -H "Authorization: Bearer {{api_key}}" \
    -H "Content-Type: application/json" \
    -d '{"model":"seedance-uncensored","prompt":"TEST_PROMPT","duration":1,"size":"1280x720"}' \
    "{{base_url}}/v1/videos" |
  jq -er '.id'
)"
```

The loop polls every 10 seconds and exits on `completed` or `failed`; the download uses
`/v1/videos/${task_id}/content`.

- [ ] **Step 7: Run schema, semantic, fixture, and secret gates**

Run:

```bash
go test ./docs/api/... -count=1
jq empty docs/openapi/relay.json
bun x @redocly/cli@1.34.5 lint docs/api/seed-dance-openapi.yaml
bun x @redocly/cli@1.34.5 lint docs/openapi/relay.json
for fixture in docs/api/fixtures/seed-dance/*.json; do
  jq -e . "$fixture" >/dev/null
done
! rg -n \
  'Authorization|Bearer [A-Za-z0-9]|sk-[A-Za-z0-9]|video_base64"[[:space:]]*:[[:space:]]*"[A-Za-z0-9+/]|image_base64"[[:space:]]*:[[:space:]]*"[A-Za-z0-9+/]' \
  docs/api
```

Expected: every command succeeds. Redocly lints both documents.

- [ ] **Step 8: Commit**

```bash
git add docs/api docs/openapi/relay.json
git commit -m "docs: add Seed Dance video API test contracts"
```

---

### Task 12: Full Repository Quality Gates and Real Docker Smoke Test

**Files:**
- Verify: all files changed by Tasks 1–11
- Verify: `Dockerfile`
- Verify: `docker-compose.yml`

**Interfaces:**
- Consumes: the complete implementation and documentation commits.
- Produces: a clean full-suite result, a runnable local image, a healthy real container, and a secret-free diff.

- [ ] **Step 1: Format every changed, staged, and untracked Go file**

Run:

```bash
set -euo pipefail
go_files_file="$(mktemp)"
trap 'rm -f "$go_files_file"' EXIT
{
  git diff HEAD --name-only --diff-filter=ACMR -- '*.go'
  git ls-files --others --exclude-standard -- '*.go'
} | sort -u > "$go_files_file"
while IFS= read -r file; do
  test -z "$file" || gofmt -w "$file"
done < "$go_files_file"
test -z "$(
  while IFS= read -r file; do
    test -z "$file" || gofmt -l "$file"
  done < "$go_files_file"
)"
```

- [ ] **Step 2: Run focused race-sensitive and contract suites**

Run:

```bash
go test ./relay/channel/task/seedance ./relay ./model ./service ./controller ./router ./docs/api/... \
  -count=1
go test -race ./relay/channel/task/seedance ./model ./service ./controller -count=1
```

Expected: PASS. If the full controller race suite exceeds local resources, run its
Seed Dance test names with `-race` and still run the non-race full controller suite.

- [ ] **Step 3: Run the complete Go gates**

Run:

```bash
go vet ./...
go test ./... -count=1
go build ./...
```

Expected: PASS with no panic or data race.

- [ ] **Step 4: Run the complete frontend gates**

Run:

```bash
cd web
bun install --frozen-lockfile
bun test src/features/channels/lib/__tests__/seed-dance-config.test.ts
bun run i18n:sync
bun run typecheck
bun run lint
bun run format:check
bun run build
cd ..
```

Expected: PASS and `git diff --check HEAD` remains clean.

- [ ] **Step 5: Re-run both OpenAPI gates and secret scans**

Run:

```bash
go test ./docs/api/... -count=1
bun x @redocly/cli@1.34.5 lint docs/api/seed-dance-openapi.yaml
bun x @redocly/cli@1.34.5 lint docs/openapi/relay.json
! rg -n \
  'Authorization:[[:space:]]*Bearer[[:space:]]+[A-Za-z0-9_-]{12,}|sk-[A-Za-z0-9_-]{20,}|video_base64"[[:space:]]*:[[:space:]]*"[A-Za-z0-9+/]{32,}|image_base64"[[:space:]]*:[[:space:]]*"[A-Za-z0-9+/]{32,}' \
  --glob '!docs/superpowers/specs/2026-07-24-seed-dance-channel-design.md' \
  --glob '!docs/superpowers/plans/2026-07-25-seed-dance-channel.md' \
  .
git diff --check HEAD
```

- [ ] **Step 6: Build and run the actual Docker image**

Run:

```bash
docker build -t new-api-seedance:test .
smoke_dir="$(mktemp -d)"
cid="$(
  docker run -d \
    --name new-api-seedance-smoke \
    -p 127.0.0.1:13000:3000 \
    -v "${smoke_dir}:/data" \
    new-api-seedance:test
)"
cleanup() {
  docker rm -f "$cid" >/dev/null 2>&1 || true
  rm -rf "$smoke_dir"
}
trap cleanup EXIT
healthy=0
for i in $(seq 1 90); do
  if curl -fsS http://127.0.0.1:13000/api/status |
    grep -q '"success":[[:space:]]*true'; then
    healthy=1
    break
  fi
  sleep 1
done
test "$healthy" -eq 1
test "$(docker inspect "$cid" --format '{{.State.Running}}')" = "true"
if docker logs "$cid" 2>&1 | grep -E 'panic:|fatal error:'; then
  exit 1
fi
```

Expected: the real container starts, `/api/status` succeeds, and logs contain no panic.

- [ ] **Step 7: Inspect the final diff and record the feature SHA**

Run:

```bash
git status --short
git diff --stat HEAD
git diff --check HEAD
git log --oneline --decorate -15
FEATURE_SHA="$(git rev-parse HEAD)"
printf '%s\n' "$FEATURE_SHA"
```

Expected: only intentional files remain. If formatting or gate fixes changed tracked
files after their owning task's commit, stage those exact files with their owning task and
create one explicit fix commit before re-running Steps 1–7.

---

### Task 13: GitHub Fork Delivery, Exact-SHA Server Deployment, Acceptance, and Rollback

**Files:**
- Runtime-only on server: `/opt/new-api-deploy/compose.override.yml`
- Runtime-only on server: `/opt/new-api-deploy/runtime.env`
- Runtime-only on server: `/opt/new-api-deploy/backups/<timestamp>/`
- No runtime secret or server override file is committed to Git.

**Interfaces:**
- Consumes: verified feature commit SHA; authenticated `gh` CLI user `wlhtea`; server source tree `/opt/new-api`; Docker/Compose; administrator API.
- Produces: `wlhtea/new-api:codex/seed-dance-channel`; exact-SHA feature and baseline images; disabled Type 59 channel; tested rollback artifacts.

- [ ] **Step 1: Create or reuse the fork and verify the pushed SHA**

Run locally:

```bash
cd /Users/lanhaowu/Documents/ali-video/new-api
FEATURE_SHA="$(git rev-parse HEAD)"
test -z "$(git status --porcelain)"
if ! gh repo view wlhtea/new-api >/dev/null 2>&1; then
  gh repo fork QuantumNous/new-api --remote --remote-name fork
fi
if ! git remote get-url fork >/dev/null 2>&1; then
  git remote add fork https://github.com/wlhtea/new-api.git
fi
git push -u fork codex/seed-dance-channel
test "$(
  gh api repos/wlhtea/new-api/git/ref/heads/codex/seed-dance-channel \
    --jq .object.sha
)" = "$FEATURE_SHA"
printf '%s\n' "$FEATURE_SHA" > /tmp/new-api-seedance-feature-sha
```

Do not force-push. If an upstream PR is later requested, use
`.github/PULL_REQUEST_TEMPLATE.md` and state that the change is AI-assisted.

- [ ] **Step 2: Connect interactively and build a real baseline rollback image**

Use interactive SSH so the password is never placed in a command, environment file, or
shell history:

```bash
ssh root@TARGET_HOST
```

On the server, use Bash:

```bash
bash
set -euo pipefail
BASE_SHA=84a79b6807ac1a679ca86f34c8c6f39175c294d8
read -r -p 'Verified feature SHA: ' FEATURE_SHA
cd /opt/new-api
test -z "$(git status --porcelain)"
git remote get-url fork >/dev/null 2>&1 ||
  git remote add fork https://github.com/wlhtea/new-api.git
git fetch --prune origin
git fetch --prune fork codex/seed-dance-channel
git cat-file -e "${BASE_SHA}^{commit}"
git cat-file -e "${FEATURE_SHA}^{commit}"
test "$(git rev-parse fork/codex/seed-dance-channel)" = "$FEATURE_SHA"
git switch --detach "$BASE_SHA"
docker build -t "new-api-baseline:${BASE_SHA}" .
docker image inspect "new-api-baseline:${BASE_SHA}" >/dev/null
```

- [ ] **Step 3: Create private deployment files with strict permissions**

Run:

```bash
install -d -m 700 \
  /opt/new-api-deploy \
  /opt/new-api-deploy/backups \
  /opt/new-api-deploy/data \
  /opt/new-api-deploy/logs
install -m 600 /dev/null /opt/new-api-deploy/runtime.env
install -m 600 /dev/null /opt/new-api-deploy/compose.override.yml
```

Generate fresh values without printing them and write the exact feature SHA:

```bash
umask 077
POSTGRES_PASSWORD="$(openssl rand -hex 32)"
REDIS_PASSWORD="$(openssl rand -hex 32)"
SESSION_SECRET="$(openssl rand -hex 32)"
CRYPTO_SECRET="$(openssl rand -hex 32)"
cat > /opt/new-api-deploy/runtime.env <<EOF
NEW_API_IMAGE=new-api-seedance:${FEATURE_SHA}
POSTGRES_USER=root
POSTGRES_DB=new-api
POSTGRES_PASSWORD=${POSTGRES_PASSWORD}
REDIS_PASSWORD=${REDIS_PASSWORD}
SESSION_SECRET=${SESSION_SECRET}
CRYPTO_SECRET=${CRYPTO_SECRET}
EOF
chmod 600 /opt/new-api-deploy/runtime.env
unset POSTGRES_PASSWORD REDIS_PASSWORD SESSION_SECRET CRYPTO_SECRET
```

Do not put the supplier Key in this file; store it only through the authenticated New API
channel configuration.

Write:

```yaml
services:
  new-api:
    build:
      context: /opt/new-api
      dockerfile: Dockerfile
    image: ${NEW_API_IMAGE:?NEW_API_IMAGE is required}
    environment:
      SQL_DSN: postgresql://${POSTGRES_USER}:${POSTGRES_PASSWORD}@postgres:5432/${POSTGRES_DB}
      REDIS_CONN_STRING: redis://:${REDIS_PASSWORD}@redis:6379
      SESSION_SECRET: ${SESSION_SECRET}
      CRYPTO_SECRET: ${CRYPTO_SECRET}
      TZ: Asia/Shanghai
    volumes:
      - /opt/new-api-deploy/data:/data
      - /opt/new-api-deploy/logs:/data/logs
  redis:
    image: redis:7-alpine
    command: ["redis-server", "--requirepass", "${REDIS_PASSWORD}"]
  postgres:
    image: postgres:15-alpine
    environment:
      POSTGRES_USER: ${POSTGRES_USER}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
      POSTGRES_DB: ${POSTGRES_DB}
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER} -d ${POSTGRES_DB}"]
      interval: 5s
      timeout: 3s
      retries: 20
```

Define a Bash array, not a string:

```bash
compose=(
  docker compose
  --env-file /opt/new-api-deploy/runtime.env
  -f /opt/new-api/docker-compose.yml
  -f /opt/new-api-deploy/compose.override.yml
)
"${compose[@]}" config --quiet
```

- [ ] **Step 4: Back up current state and build the exact feature image**

If New API already has data, save the current image, source SHA, override, env, database,
and full price maps before changing them:

```bash
backup_dir="/opt/new-api-deploy/backups/$(date +%Y%m%d-%H%M%S)"
install -d -m 700 "$backup_dir"
git -C /opt/new-api rev-parse HEAD > "$backup_dir/source-sha.txt"
cp -a /opt/new-api-deploy/compose.override.yml "$backup_dir/"
cp -a /opt/new-api-deploy/runtime.env "$backup_dir/"
chmod 600 "$backup_dir/runtime.env"
```

After PostgreSQL is available:

```bash
"${compose[@]}" exec -T postgres \
  pg_dump -U root -d new-api -Fc \
  > "$backup_dir/new-api.dump"
chmod 600 "$backup_dir/new-api.dump"
```

Use the authenticated administrator API to save the complete current `ModelPrice` and
`ModelRatio` maps; use a `0600` header file or `read -s`, never print the Token.

Build:

```bash
git -C /opt/new-api switch --detach "$FEATURE_SHA"
test "$(git -C /opt/new-api rev-parse HEAD)" = "$FEATURE_SHA"
docker build -t "new-api-seedance:${FEATURE_SHA}" /opt/new-api
docker image inspect "new-api-seedance:${FEATURE_SHA}" >/dev/null
"${compose[@]}" config --quiet
```

- [ ] **Step 5: Start dependencies and the feature image, then verify health**

Run:

```bash
"${compose[@]}" up -d postgres redis
for i in $(seq 1 60); do
  if "${compose[@]}" exec -T postgres \
    pg_isready -U root -d new-api; then
    break
  fi
  test "$i" -lt 60
  sleep 2
done
"${compose[@]}" up -d new-api
for i in $(seq 1 120); do
  if curl -fsS http://127.0.0.1:3000/api/status |
    grep -q '"success":[[:space:]]*true'; then
    break
  fi
  test "$i" -lt 120
  sleep 2
done
container_id="$("${compose[@]}" ps -q new-api)"
test "$(docker inspect "$container_id" --format '{{.Config.Image}}')" \
  = "new-api-seedance:${FEATURE_SHA}"
if docker logs "$container_id" 2>&1 | grep -E 'panic:|fatal error:'; then
  exit 1
fi
```

- [ ] **Step 6: Merge only the Seed Dance price key and create a disabled channel**

Read the complete live `ModelPrice` map, merge:

```json
{"seedance-uncensored": 0.15}
```

and PUT the complete merged map. Do not replace unrelated keys and do not add the model
to `TASK_PRICE_PATCH`. Save both the deployment-before value and the value immediately
before rollback.

Create Type 59 with:

```text
status = 2
models = seedance-uncensored
base_url = configured transport-gated supplier URL
group = intended test group
```

Enter the supplier Key through the authenticated channel API/UI. Verify with a query that
does not select `channels.key`:

```sql
SELECT id, type, status, name, base_url, models, "group"
FROM channels
WHERE type = 59;
```

Verify its ability row and `ModelPrice["seedance-uncensored"] == 0.15`.

- [ ] **Step 7: Enforce the transport gate before any real supplier request**

Confirm at least one:

```text
supplier HTTPS works
both sides use a mutually trusted private network
a VPN/equivalent encrypted tunnel carries the traffic
a TLS termination proxy inside a trusted boundary carries the traffic
```

If none is true, keep channel status 2 and complete only Mock, Docker, health, config, and
rollback drills. Do not send a real Key, image, or video through ordinary public HTTP.

- [ ] **Step 8: Run one bounded real acceptance task only after the gate passes**

Use a New API test Token via `read -s`, never in shell history. Submit:

```json
{
  "model": "seedance-uncensored",
  "prompt": "TEST_PROMPT",
  "duration": 1,
  "size": "1280x720",
  "metadata": {
    "strict_duration": true
  }
}
```

Capture only the public ID, poll every 10–15 seconds until `completed` or `failed`, and
download `/v1/videos/{public_id}/content`. Verify:

```text
public responses never contain upstream task ID
queued -> in_progress -> completed/failed is valid
MP4 has H.264 video and AAC audio when completed
Content-Length equals saved file bytes
charged quota = 0.15 * 1 * 1 * 1 * 500000 = 75000
BillingContext.GroupRatio = 1 for the intended group
database and logs contain no image_base64 or video_base64
failed and timeout paths refund once
```

Do not run I2V, 1080P, or 15-second real jobs for acceptance.

- [ ] **Step 9: Rehearse the business-safe rollback order**

While the feature image is still running:

```text
1. set every Type 59 channel status to 2
2. wait until every Type 59 Task is SUCCESS or FAILURE
3. export sanitized channel/ability/task audit fields
4. read the rollback-time complete ModelPrice and ModelRatio maps
5. restore only the seedance-uncensored key's pre-deployment existence/value
6. delete Type 59 channels through the current API so abilities are removed
7. verify no Type 59 channel and no orphan ability remains
8. switch source and image to the baseline
```

The price merge is:

```text
current complete map
+ replace/delete only seedance-uncensored according to the saved pre-deploy state
= rollback map
```

It must not restore an old complete map over unrelated price changes made after
deployment.

Switch:

```bash
"${compose[@]}" stop new-api
git -C /opt/new-api switch --detach "$BASE_SHA"
sed -i \
  "s|^NEW_API_IMAGE=.*|NEW_API_IMAGE=new-api-baseline:${BASE_SHA}|" \
  /opt/new-api-deploy/runtime.env
"${compose[@]}" config --quiet
"${compose[@]}" up -d new-api
for i in $(seq 1 120); do
  if curl -fsS http://127.0.0.1:3000/api/status |
    grep -q '"success":[[:space:]]*true'; then
    break
  fi
  test "$i" -lt 120
  sleep 2
done
container_id="$("${compose[@]}" ps -q new-api)"
test "$(docker inspect "$container_id" --format '{{.Config.Image}}')" \
  = "new-api-baseline:${BASE_SHA}"
```

Confirm the old image sees no Type 59 data, `/api/status` succeeds, and startup logs have
no array-bound panic. Keep the full `pg_dump` only for disaster recovery; do not restore
it during a normal feature rollback.

---
