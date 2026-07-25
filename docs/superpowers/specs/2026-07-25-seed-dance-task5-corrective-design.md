# Seed Dance Task 5 修订设计：Durable Submission 与崩溃可恢复退款

**日期：** 2026-07-25

**状态：** Approved corrective amendment

**适用基线：** `b14fd32` 及其后继实现

**修订对象：** `docs/superpowers/plans/2026-07-25-seed-dance-channel.md` 的 Task 5

## 1. 权威性与实施顺序

本文是 Seed Dance Task 5 的约束性修订。原 Task 5 中与本文冲突的函数可见性、
签名、退款算法、事务边界、文件范围和测试要求，以本文为准。原计划未冲突的
提交前持久化、失败分类、重试和旧 adaptor 兼容要求继续有效。

Task 5 拆为两个可独立审查的提交，严格按以下顺序实施：

1. **Task 5A：Durable model primitives 与 component refund ledger**
2. **Task 5B：Relay/controller orchestration 与 administrator reconciliation**

Task 5B 依赖 Task 5A，不得用旧的 `ClaimQuotaForRefund`/
`RestoreQuotaAfterFailedRefund` 路径临时接线，也不得在 5A 完成前启用 Seed
Dance durable submit。

## 2. 修订原因

原 Task 5 存在四个会阻止实现或破坏故障恢复的不一致：

1. billing validator 在 `relay` 包定义为未导出函数，但 controller 需要再次
   调用；
2. attach/commit 持久化失败时 `RelayTaskSubmit` 返回 `nil` result，已经从
   adaptor/classifier 取得的可靠 upstream ID 无法继续传给 controller；
3. 现有 `RefundTaskQuota` 先执行非幂等余额增量、最后才清 `Task.Quota`，而
   sweep 又会先 claim/清 marker 再执行退款；两种顺序分别留下双退或漏退窗口；
4. provisional `TaskBillingContext.PerCallBilling` 只考虑
   `PriceData.UsePrice`，漏掉 `constant.TaskPricePatches`。

本文保持以下总不变量：

- `/generate` 前完成全额预扣和 `SUBMITTING/0%` provisional Task；
- Task 持久化前由 request `BillingSession` 持有退款责任，Task 与 refund
  ledger 同事务提交后由 Task 唯一持有；
- 正常 Attach 和 Commit 严格，reconciliation-only attach 只能在完整的已持久
  身份匹配后补写，且绝不复活状态；
- wallet/subscription funding 与 Token 是两个独立 component；每个
  component 的主数据库余额 mutation 和完成 marker 在同一事务；
- `Task.Quota` 只在两个 component 都完成时清零；
- 成功消费日志和 HTTP 200 只能发生在 Task commit 与 zero-delta settle 之后；
- 不改变未实现 durable marker 的旧 adaptor 的响应、插入和错误处理语义。

## 3. 数据模型

### 3.1 `TaskPrivateData` 的边界

`TaskPrivateData` 继续保存：

- 实际渠道 Key；
- upstream task ID；
- billing source、subscription ID、Token ID 和 node name；
- `TaskBillingContext`。

退款 component 状态不得放入 `TaskPrivateData`。该字段由 GORM 作为完整 JSON
列序列化；并发 writer 若使用旧结构体副本更新，会覆盖其他 writer 已写入的
JSON 字段。跨 SQLite、MySQL 和 PostgreSQL 的 JSON path CAS 也不是当前模型
层的公共能力。

Attach 更新 `private_data` 时必须在事务内锁行、读取最新 JSON、修改并完整
回写。退款 marker 使用独立的标量表，避免与 Attach、轮询结果或 stored Key
互相覆盖。

### 3.2 新增 `TaskRefundState`

新增 `model/task_refund.go`：

```go
type TaskRefundState struct {
    TaskID int64 `json:"-" gorm:"primaryKey;autoIncrement:false"`

    RequestID      string `json:"-" gorm:"type:varchar(64);index"`
    FundingSource  string `json:"-" gorm:"type:varchar(32)"`
    UserID         int    `json:"-" gorm:"index"`
    SubscriptionID int    `json:"-" gorm:"index"`
    FundingAmount  int    `json:"-"`
    TokenID        int    `json:"-" gorm:"index"`
    TokenAmount    int    `json:"-"`

    FundingAppliedAt int64 `json:"-" gorm:"index"`
    TokenAppliedAt   int64 `json:"-" gorm:"index"`
    CompletedAt      int64 `json:"-" gorm:"index"`
    CreatedAt        int64 `json:"-"`
    UpdatedAt        int64 `json:"-"`
}
```

约束：

- `TaskID` 对应 `tasks.id`，每个 durable Task 恰好一行；
- `RequestID`、source、user/subscription/Token identity 和两个 amount 在创建
  后不可修改；
- `FundingAmount == Task.Quota == Billing.GetPreConsumedQuota()`；
- 非 playground Token 请求的 `TokenAmount` 等于实际 Token 预扣额；
- playground 或未扣 Token 时 `TokenAmount=0`；
- Token unlimited 不能仅因 unlimited 标志而把 `TokenAmount` 置零，因为现有
  预扣逻辑仍会扣 Token quota；
- free Task 的 `Quota=0`、`Billing=nil`，仍在同一事务创建零额 ledger，并把
  两个 component marker 与 `CompletedAt` 初始化为已完成；
- `CompletedAt != 0` 当且仅当 `FundingAppliedAt != 0 &&
  TokenAppliedAt != 0`；除 free Task 的创建事务外，任何无需余额 mutation
  的 component 也必须由对应 Apply 事务先持久化 marker，之后才可完成；
- 表内不得保存 Key、TokenKey、prompt、图片、Base64、请求体或供应商响应。

把 `TaskRefundState` 加到普通和 fast 两条 AutoMigrate 路径以及 model 测试
数据库。不要依赖隐式外键 cascade；Task 与 ledger 的创建和读取由明确事务
控制。`model/task_cas_test.go` 与 `service/task_billing_test.go` 的测试
setup/cleanup 必须显式 migrate 并 truncate `TaskRefundState` 与
`SubscriptionPreConsumeRecord`，且先清 ledger/record 再清其关联主体。

### 3.3 Task 与 ledger 同事务

新增：

```go
type TaskRefundSnapshot struct {
    RequestID      string
    FundingSource  string
    SubscriptionID int
    FundingAmount  int
    TokenID        int
    TokenAmount    int
}

func PrepareTaskSubmissionAttempt(
    candidate *Task,
    persistentID int64,
    refund TaskRefundSnapshot,
) (*Task, error)
```

首次 prepare：

```text
BEGIN
INSERT tasks (..., status=SUBMITTING, progress=0%)
INSERT task_refund_states (task_id=tasks.id, immutable snapshot)
COMMIT
```

任一 INSERT 失败则整体回滚。只有事务成功，或者 commit 返回错误但主库
read-after-error 同时确认 Task 和 ledger 完整匹配，调用方才可设置：

```text
PersistentTaskID  = Task.ID
RefundOwnedByTask = true
```

在此之前 request `BillingSession` 仍是退款 owner。Task 已提交但 ledger
缺失不算 ownership transfer 成功。

`TaskRefundState.UserID` 由锁定的 `candidate.UserId` 填充，不接受独立的调用方
覆盖值；retry 时必须与 Task.UserId 相等。

首次 prepare 在任何 INSERT 前也必须 fail closed 校验 Task 与 refund snapshot：

```text
FundingAmount == Task.Quota
FundingSource == Task.PrivateData.BillingSource
SubscriptionID == Task.PrivateData.SubscriptionId
TokenID == Task.PrivateData.TokenId
UserID 只能由 Task.UserId 派生，且 paid Task 的 UserId 必须非零
FundingAmount、TokenAmount 不得为负数
```

`FundingSource` 只允许当前受支持的 wallet/subscription 值。wallet 必须满足
`SubscriptionID == 0`；subscription 必须满足非空 `RequestID`、
`SubscriptionID > 0`，并在同一事务锁定 `SubscriptionPreConsumeRecord`，
确认其 user、subscription、amount 和 `consumed` 状态全部匹配。任一不匹配
都不得创建 Task 或 ledger。free Task 使用明确的零额 snapshot，不得伪造
subscription identity。

## 4. Task 5A：Model lifecycle 与退款原子性

### 4.1 `SUBMITTING` 与 polling

新增：

```go
const TaskStatusSubmitting TaskStatus = "SUBMITTING"
```

要求：

- `GetAllUnFinishSyncTasks` 排除 `SUBMITTING`，不得用公开 TaskID 轮询供应商；
- `HasTaskPollingWork` 和 timeout 查询仍包含 `SUBMITTING`；
- timeout sweep 可把它 CAS 为 `FAILURE/100%`；
- timeout winner 和提交 controller 都调用同一个幂等 Task refund orchestrator。

### 4.2 Prepare retry 的财务快照不可漂移

`persistentID != 0` 时锁定现有 Task 和 ledger，并验证：

```text
Task.ID、TaskID、UserId、Platform、Action、SubmitTime 不变
Status == SUBMITTING
Progress == 0%
UpstreamTaskID 为空
Group、Quota、BillingSource、SubscriptionId、TokenId 不变
BillingContext 深度相等
TaskRefundState immutable snapshot 逐项相等
```

只允许刷新当前尝试的路由字段：

```text
ChannelId
PrivateData.Key
Properties.UpstreamModelName
必要的非财务 route metadata
```

不得在 retry 中更新 `Quota`、billing source、subscription/Token identity、
request ID、`BillingContext` 或 refund ledger。若新尝试重新计算的 quota 与
首次预扣不相等，应在 prepare 前由 full-prepaid validator 拒绝。

### 4.3 正常 Attach

```go
func AttachTaskUpstreamResult(
    id int64,
    publicTaskID string,
    upstreamTaskID string,
    taskData []byte,
) (*Task, error)
```

事务内按 `id + task_id` 锁行：

- 参数为空：state conflict；
- stored ID 为空：仅 `SUBMITTING/0%` 可首次写入；
- stored ID 相同：幂等成功，不重写 `Data`，不改变可能已推进的状态；
- stored ID 不同：`ErrTaskUpstreamIDConflict`，绝不覆盖；
- 任一其他状态且 stored ID 为空：state conflict。

`taskData` 必须是 adaptor 已清理的数据。函数不接受完整 upstream response。

### 4.4 Commit

```go
func CommitTaskSubmission(
    id int64,
    publicTaskID string,
) (*Task, error)
```

状态机：

```text
SUBMITTING/0% + stored upstream ID 非空
  -> SUBMITTED/10%

SUBMITTED/10% + stored upstream ID 非空
  -> 幂等成功

FAILURE/SUCCESS/QUEUED/IN_PROGRESS
  -> state conflict

任意状态 + stored upstream ID 为空
  -> state conflict
```

commit error 后只从主库按 `id + task_id` 重读；观察到
`SUBMITTED/10% + non-empty upstream ID` 才可把 ambiguous commit 当作成功。

### 4.5 Reconciliation-only Attach

不得放宽正常 Attach。新增：

```go
func AttachTaskUpstreamResultForReconciliation(
    id int64,
    publicTaskID string,
    channelID int,
    upstreamTaskID string,
) (*Task, error)
```

handler 先从主库加载 Task，并在调用该函数前验证：

- primary ID、PublicTaskID、ChannelID 与 payload 一致；
- Task 内部 `Platform` 是 Seed Dance；
- Task 的 channel 从主库加载后确认为 Seed Dance channel type。

函数事务内再次验证 `id + task_id + channel_id`，并使用以下状态机：

```text
stored ID 相同
  -> 幂等成功，不改 Data/状态

stored ID 不同
  -> upstream ID conflict

stored ID 为空，SUBMITTING/0%
  -> 只补 upstream ID，状态仍 SUBMITTING

stored ID 为空，FAILURE/100%
  -> 只补 upstream ID
  -> 保持 FAILURE、Progress、FinishTime、FailReason、Quota

stored ID 为空，其他状态
  -> state conflict
```

它不接收 `taskData`、不调用 Commit、绝不把任何终态恢复为活动状态。

### 4.6 Component refund 主库原子 API

新增：

```go
type TaskRefundApplyResult struct {
    Applied   bool
    Completed bool
    UserID    int
    TokenID   int
}

func ApplyTaskFundingRefund(
    taskID int64,
) (TaskRefundApplyResult, error)

func ApplyTaskTokenRefund(
    taskID int64,
) (TaskRefundApplyResult, error)

func GetTaskRefundState(
    taskID int64,
) (*TaskRefundState, error)
```

两种 Apply 都锁定 Task 和 ledger，并要求 Task 已是 `FAILURE`。每个
component 的主数据库余额 mutation 与对应 `AppliedAt` marker 必须在同一
事务；禁止在事务中调用会使用全局 `DB`、batch queue 或先更新 Redis 的旧
增量 helper。

每次 Apply 都必须在任何余额 mutation、marker 写入或“已完成”返回前重验：

```text
TaskRefundState.TaskID == Task.ID
TaskRefundState.UserID == Task.UserId
FundingSource == Task.PrivateData.BillingSource
SubscriptionID == Task.PrivateData.SubscriptionId
TokenID == Task.PrivateData.TokenId
CompletedAt == 0 时 FundingAmount == Task.Quota
CompletedAt != 0 时 Task.Quota == 0 且两个 AppliedAt 均非零
wallet/subscription 的 RequestID/source/ID 组合合法
subscription record 的 request/user/subscription/amount identity 完整匹配
```

任何 identity、amount、source 或 completion invariant 漂移都必须整体回滚，
余额 mutation 和 marker mutation 均为零；不得依赖调用方传入 identity 修复
已损坏数据。

#### Wallet funding

```text
BEGIN
LOCK Task + TaskRefundState
重验 Task/ledger 财务 identity 与 amount
FundingAppliedAt != 0 -> 幂等成功
FundingAmount == 0 -> 不做余额 mutation
否则 UPDATE users SET quota = quota + FundingAmount WHERE id = UserID
非零 mutation 要求 RowsAffected == 1
SET FundingAppliedAt
仅若 TokenAppliedAt != 0，SET CompletedAt 并清 Task.Quota
COMMIT
```

#### Subscription funding

拆出仅在传入 transaction 上运行的内部 helper：

```go
func postConsumeUserSubscriptionDeltaTx(
    tx *gorm.DB,
    subscriptionID int,
    delta int64,
) error

func refundSubscriptionPreConsumeTx(
    tx *gorm.DB,
    requestID string,
    expectedUserID int,
    expectedSubscriptionID int,
    expectedAmount int64,
) error
```

同一事务锁定 Task、ledger、`SubscriptionPreConsumeRecord` 和
`UserSubscription`，验证 request/user/subscription/amount 全部相等。

- record 为 `consumed`：subscription delta、record=`refunded` 和
  `FundingAppliedAt` 同事务；
- record 已 `refunded`：不再修改 subscription，只收敛 Task ledger marker；
- identity/status 不一致：不执行任何 mutation。

旧公开 subscription API 继续包装 tx helper，保持其他调用方兼容。

#### Token

```text
BEGIN
LOCK Task + TaskRefundState
重验 Task/ledger 财务 identity 与 amount
TokenAppliedAt != 0 -> 幂等成功
TokenAmount == 0 -> 只写 marker
Token 存在 -> 在 tx 上原子更新 remain_quota/used_quota
Token 已删除 -> component 记为无可恢复对象，不重建 Token
SET TokenAppliedAt
仅若 FundingAppliedAt != 0，SET CompletedAt 并清 Task.Quota
COMMIT
```

paid Task 的 `TokenAmount == 0` 不是隐式“已完成”；上述 no-op Apply 仍必须
写入 `TokenAppliedAt`。同理，任何合法的零额 funding component 都必须由
funding Apply 写入 `FundingAppliedAt`。wallet Apply 不得仅凭“Token 无需
余额动作”提前写 `CompletedAt` 或清 `Task.Quota`。

TokenKey 只可在 model 内部用于 commit 后 cache invalidation，不能持久化到
Task、ledger、SystemTask、日志或错误。

### 4.7 Exactly-once 的精确定义

本文的 exactly-once 声明限定为：

> 对每个 Task refund component，主数据库中的 wallet/subscription/Token
> 余额 mutation 与该 component 的 durable marker 在同一事务提交；任意
> 进程崩溃、事务回滚、commit 结果不确定或 sweep 重试后，已提交 component
> 不重放，未提交 component 可继续执行。

Redis/cache、消费/退款日志和统计是派生状态，不包含在主库事务承诺中：

- commit 后只做 cache invalidation，或从主库读取权威绝对余额后写入 cache；
  绝不再次写主库，也不执行可重复的 cache 增量；
- cache/log 失败不得触发主库额度 mutation 重放；
- cache 最终通过 invalidation、回源或 TTL 收敛；
- 若以后要求日志 exactly-once，应另设唯一 operation key，不得复用余额
  marker 暗示跨库原子性。

### 4.8 Sweep

`sweepUnrefundedFailedTasks` 不再：

```text
ClaimQuotaForRefund
先清 quota
RestoreQuotaAfterFailedRefund
```

它直接对每个 `FAILURE + quota != 0` Task 调用幂等 component orchestrator：

```text
ApplyTaskFundingRefund
ApplyTaskTokenRefund
```

若 funding 已完成而 Token 未完成，下一次 sweep 只执行 Token。第二个
component 完成时，在同一事务写 `CompletedAt` 并清 `Task.Quota`。

为保持旧 adaptor/历史 Task 兼容：

- 所有新 durable Task 都必须有 `TaskRefundState` 并走新路径；
- 没有 ledger 的历史/旧 adaptor Task 保持已有 legacy refund 兼容路径；
- 不得从缺少 request ID 的历史 subscription Task 猜造新 ledger；
- exactly-once 测试和声明只覆盖有 ledger 的 Task。

## 5. Task 5B：Relay、Controller 与 SystemTask 编排

### 5.1 导出 full-prepaid validator

```go
func ValidateFullPrepaidTaskBilling(
    info *relaycommon.RelayInfo,
    quota int,
) *dto.TaskError
```

规则：

```text
info == nil -> fail

free:
  quota == 0
  Billing == nil

paid:
  ForcePreConsume == true
  Billing != nil
  quota == Billing.GetPreConsumedQuota()
```

错误固定为：

```text
Code=seedance_billing_invariant_failed
StatusCode=500
LocalError=true
Retryable=false
```

`RelayTaskSubmit` 在 `BuildRequestBody` 前调用；controller 在 deferred
settle 前调用。controller 不复制 validator。

### 5.2 Provisional billing snapshot

`newProvisionalTask` 必须写：

```go
PerCallBilling:
    common.StringsContains(
        constant.TaskPricePatches,
        info.OriginModelName,
    ) || info.PriceData.UsePrice
```

Prepare 在 `BuildRequestBody` 成功后、`DoRequest` 前执行。这样 body 构造
失败仍由 request owner 退款，Task/ledger 插入失败时供应商调用次数为零。

### 5.3 Ownership transfer

扩展：

```go
type TaskRelayInfo struct {
    // existing fields...
    PersistentTaskID  int64
    RefundOwnedByTask bool
}
```

request defer：

```go
if taskErr != nil && relayInfo.Billing != nil &&
    !relayInfo.RefundOwnedByTask {
    relayInfo.Billing.Refund(c)
}
```

Prepare transaction/read-back 确认前保持 `false`；确认 Task+ledger 后设置
`true`。设置后所有错误都通过 Task component ledger 退款，请求 defer 不再
调用 `Billing.Refund`。

付费任务从已创建的 Billing session 构造 ledger snapshot；经 validator
确认的免费任务使用 source/amount 为零的 snapshot，并由 Prepare 在同一事务
创建已完成的零额 ledger。

### 5.4 Reliable partial result

```go
type TaskSubmitResult struct {
    UpstreamTaskID string
    TaskData       []byte
    Platform       constant.TaskPlatform
    Quota          int
    HTTPResponse   *channel.TaskSubmitHTTPResponse
}
```

adaptor/classifier 一旦返回非空 upstream ID，立即创建 partial result。此后
Attach、response build、Commit 或 reconciliation record 失败时，函数必须：

```go
return partial, taskErr
```

不得返回 `nil, taskErr`。controller 对每次 attempt 使用局部变量，并保留
已有 reliable partial：

```text
空 ID 可被非空 ID 补齐
相同 ID 可幂等合并
不同非空 ID 返回 conflict，绝不覆盖
```

partial result 不写入 `dto.TaskError.Data`，不出现在客户端错误和日志中。

### 5.5 Attach、public response、Commit

可靠 ID 的正常顺序：

```text
DoRequest/classifier/DoResponse
构造 partial result
AttachTaskUpstreamResult（有限 retry + primary read-after-error）
处理 TaskError；若无错误则执行 billing adjustment
防御性 ValidateFullPrepaidTaskBilling
BuildTaskSubmitResponse（只构造，不写 HTTP）
CommitTaskSubmission（有限 retry + primary read-after-error）
返回 result
```

一旦可靠 ID 已知，后续任何错误都不得再次调用 `/generate`。

Attach 或 Commit 的 bounded retry 持久失败时，先用该 reliable ID 创建
administrator reconciliation record，再返回同一个 partial result 和
non-retryable persistence error。reconciliation 创建本身失败也不得清空
partial；controller 仍把同一 ID 交给 `FailAndRefundTaskSubmission` 再尝试
持久化，并输出不含 ID/Key/媒体的内部故障事件。

### 5.6 FailAndRefund

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

内部：

1. 主库加载 Task；
2. supplied upstream ID 为空时不伪造；
3. 非空时使用正常严格 attach；若 Task 已由其他路径进入 FAILURE，允许通过
   reconciliation-only attach 的严格条件补 ID；
4. 仅允许 `SUBMITTING` 或 `SUBMITTED` 转 `FAILURE/100%`；
5. 已是 `FAILURE` 为幂等；不覆盖 `QUEUED/IN_PROGRESS/SUCCESS`；
6. 写脱敏、截断的 FailReason 和 FinishTime；
7. primary read 确认为 FAILURE 后调用 Task component refund。

退款调用不限定为 status CAS winner。ledger marker 才是每个 component 的
唯一执行权；CAS winner 崩溃后，CAS loser 或 sweep 可以安全补偿。

controller 退出 retry loop 后，只要 `taskErr != nil &&
RefundOwnedByTask`，就必须调用该函数，并把保留的 partial
`UpstreamTaskID/TaskData` 一并传入；不得让 request defer 接管。

### 5.7 Zero-delta settle 与成功输出

deferred response 的 controller 顺序固定为：

```text
Task 已 attach
Task 已 Commit 为 SUBMITTED/10%
ValidateFullPrepaidTaskBilling
SettleBilling(result.Quota)
LogTaskConsumption
c.JSON(HTTPResponse.StatusCode, HTTPResponse.Body)
```

validator 保证 paid Task 的 actual quota 等于 pre-consumed quota。真实
`BillingSession.Settle` 的 delta=0 分支只标记 settled，不调用 wallet、
subscription 或 Token adjustment。

若 validator 或 settle 返回错误：

- 不写 HTTP 200；
- 不写成功消费日志；
- 保留 upstream ID；
- `SUBMITTED/10% -> FAILURE/100%`；
- 仅执行 Task component refund；
- 返回非 retryable 本地 billing settlement error。

### 5.8 Administrator reconciliation

允许的 payload 字段白名单固定为：

```go
type SeedDanceSubmitReconciliationPayload struct {
    PublicTaskID     string `json:"public_task_id"`
    UpstreamTaskID   string `json:"upstream_task_id"`
    PersistentTaskID int64  `json:"persistent_task_id"`
    ChannelID        int    `json:"channel_id"`
    NodeName         string `json:"node_name"`
    ErrorCode        string `json:"error_code"`
    ObservedAt       int64  `json:"observed_at"`
}
```

不得增加 `UserID`、`Platform`，也不得包含 Key、TokenKey、prompt、图片、
Base64、TaskData 或完整响应。handler 从主库加载 Task 后验证内部 UserID/
Platform/Channel type，不把这些值复制到 payload。

active key 公式保持：

```go
sum := sha256.Sum256([]byte(info.PublicTaskID))
activeKey := fmt.Sprintf("sd-submit:%x", sum[:16])
```

不要加入 upstream ID 或其他字段。重复 active key 由
`GetActiveSystemTaskByActiveKey(type, activeKey)` 收敛；若发现不同 upstream
ID，由严格 Attach 报 conflict 并保留现有 active reconciliation 供管理员
处理。

handler 是 non-scheduled `SystemTaskHandler`：

```text
decode 白名单 payload
主库加载 Task
核对 primary ID、public ID、channel ID
验证 Task.UserId 非零且 TaskRefundState.UserID 与 Task.UserId 一致
验证 Task.Platform 与 channel type 为 Seed Dance
reconciliation-only attach
若仍 SUBMITTING，转 FAILURE/100%
若已 FAILURE，保持状态
执行/补齐 component refund
从 primary DB 重载 Task + TaskRefundState
只有 FAILURE/100%、stored ID == payload ID、Task.Quota == 0、
两个 AppliedAt 与 CompletedAt 均非零时才标记 SystemTask succeeded
冲突、不完整身份或退款未完整提交均标记 SystemTask failed，保留管理员记录
永不 Commit，永不复活 Task
```

## 6. 旧 adaptor 兼容与数据安全

所有新行为由 `FullPrepaidTaskSubmitter`、`DurableTaskSubmitter` 和
`DeferredTaskSubmitResponder` 可选接口守卫。未实现这些接口的 adaptor：

- 继续使用既有通用 HTTP error code/status；
- 继续由既有 `DoResponse` 决定响应；
- 不提前插入 provisional Task；
- 不要求 `TaskRefundState`；
- 保持现有 Task 插入和 legacy refund 行为；
- `Retryable=nil` 继续使用原状态码重试规则。

Seed Dance 路径不得：

- 把 stored Key、TokenKey 或 upstream ID 写到客户端错误；
- 记录 prompt、图片、Base64、完整供应商响应或完整 request body；
- 新增 Seed Dance 专用请求/响应大小阈值；
- 在 reconciliation payload 中加入白名单外字段。

## 7. 文件范围

### 7.1 Task 5A

**Create**

- `model/task_refund.go`
- `model/task_refund_test.go`
- `model/task_submission.go`
- `model/task_submission_test.go`

**Modify**

- `model/task.go`
- `model/task_cas_test.go`
- `model/main.go`
- `model/subscription.go`
- `service/task_billing.go`
- `service/task_billing_test.go`
- `service/task_polling.go`
- `service/task_polling_test.go`

5A 不修改 `relay`、`controller` 或 Seed Dance adaptor。

### 7.2 Task 5B

**Create**

- `relay/relay_task_submission_test.go`
- `controller/relay_task_submission_test.go`
- `service/task_submission.go`
- `service/task_submission_test.go`

**Modify**

- `model/system_task.go`
- `model/system_task_test.go`
- `relay/common/relay_info.go`
- `relay/relay_task.go`
- `controller/relay.go`
- `controller/system_task_handlers.go`

5B 不重写 5A 的 ledger 或 component mutation。

## 8. TDD 与测试矩阵

### 8.1 Task 5A RED/GREEN

先写失败测试，证明：

- Task 与 ledger 同事务；ledger insert fail 不留下 Task；
- 首次 Prepare 的 Task/private financial identity 与 snapshot 任一不匹配时
  fail closed，且不留下 Task/ledger；
- Prepare retry 只刷新 route 字段，任一财务快照漂移均拒绝；
- normal Attach/Commit 状态机与 read-after-error；
- reconciliation-only attach 可给完全匹配的 `FAILURE/100%` 补 ID，但状态、
  reason、finish time 和 quota 不变；
- wallet balance update 与 marker 同事务；
- subscription balance、pre-consume record 与 marker 同事务；
- Token balance update 与 marker 同事务；
- `CompletedAt != 0` 当且仅当两个 AppliedAt 均非零；
- `TokenAmount=0` 在 `TokenAppliedAt` 持久化前不得清 quota；任何零额
  component 也必须以幂等事务持久化 marker；
- 每次 Apply 重验 Task/ledger/private-data/subscription identity；任一漂移
  不产生余额或 marker mutation；
- funding 已完成而 Token 失败时，重试不再 funding；
- commit 已成功但 API 返回错误时，read-back 不重放 mutation；
- 两个并发 sweep 对每个 component 只产生一次主库 mutation；
- `BatchUpdateEnabled=true` 时 Task refund 仍绕过 batch queue；
- 第二 component 完成与 `Task.Quota=0` 同事务；
- cache/log failpoint 不触发主库 mutation 重放；
- legacy Task 无 ledger 时保持旧行为。

### 8.2 Task 5B RED/GREEN

先写失败测试，证明：

- exported validator 的七种 free/paid 状态；
- `TASK_PRICE_PATCH` 且 `UsePrice=false` 时 `PerCallBilling=true`；
- provisional Task+ledger 在 `/generate` 前可从 DB 观察；
- provisional transaction 失败时 POST=0、request refund=1、task refund=0；
- Safe 429 retry 复用同一 Task，且财务快照不漂移；
- classifier/DoResponse 给可靠 ID 后，错误返回仍带 partial result；
- attach/commit 持久失败：POST=1、无 200、无成功日志、ID 未丢；
- reconciliation payload 恰好等于白名单，active key 等于
  `sd-submit:` + `sha256(PublicTaskID)` 前 16 字节的 hex；
- reconciliation-only handler 从 DB 验证 platform/channel type；
- handler 可给匹配的 FAILURE 补 ID，但不复活状态；
- reconciliation refund 任一 component failpoint 或 partial completion 都
  不得标记 succeeded；只有 primary reload 证明 ID、FAILURE/100%、quota
  和三个 ledger completion marker 全部收敛才 succeeded；
- zero-delta settle 不调用 wallet/subscription/Token adjustment；
- settle fail：Task FAILURE、task-only refund、无成功日志/200；
- `Retryable=false/true/nil` 与现有全局 retry guard 的优先级；
- 旧 adaptor 的 response/error/body-close/insert 顺序保持。

### 8.3 端到端事件顺序

成功事件必须严格为：

```text
preconsume
validate_full_prepaid
build_body
insert_provisional_and_refund_ledger
post_generate
attach_upstream_id
build_public_response
mark_submitted
validate_full_prepaid_again
settle_zero_delta
consume_log
write_http_200
```

任何失败测试都必须断言不会提前出现 `consume_log` 或 `write_http_200`。

## 9. 完成门槛

Task 5 只有在以下条件同时满足时才完成：

- 5A 和 5B 各自具有独立 RED 证据、GREEN 证据和提交；
- focused model/service/relay/controller suites 通过；
- `go test ./model ./service ./relay ./controller` 通过；
- `go test ./...` 通过，或仅存在有记录、与本变更无关的基线失败；
- `git diff --check` 通过；
- reconciliation payload/active key 的 whitelist 测试通过；
- secret scan 未发现 Key、TokenKey、prompt、图片、Base64 或完整响应；
- 没有 Seed Dance 专用大小阈值；
- 旧 adaptor 兼容测试通过。
