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

1. **Task 5A：Durable billing attempt、model primitives 与 component ledger**
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

- 以 `RequestID` 唯一标识的 durable billing-attempt ledger 必须先于全额预扣
  创建；`/generate` 前必须完成同步主库预扣、ledger owner transfer 和
  `SUBMITTING/0%` provisional Task；
- request-owned 与 task-owned 退款共用同一 billing-attempt ledger 和同一组
  component markers；Task insert、ledger link 和 owner transfer 同事务；
- 主库暂时不可读时 fail closed，不执行 ledger 外的 `BillingSession.Refund`
  或任何裸余额增量，由 stale-attempt/failed-Task sweep 恢复；
- 正常 Attach 和 Commit 严格，reconciliation-only attach 只能在完整的已持久
  身份匹配后补写，且绝不复活状态；
- wallet/subscription funding 与 Token 是两个独立 component；每个同步
  preconsume/refund 主数据库 mutation 和对应 marker 在同一事务，且
  `BatchUpdateEnabled=true` 也不得经过 batch queue；
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

### 3.2 新增 `TaskBillingAttempt`

新增 `model/task_billing_attempt.go`。ledger 在 Task 之前存在，以
`RequestID` 唯一定位一次计费尝试，而不是以尚不存在的 Task 为主键：

```go
type TaskBillingAttempt struct {
    ID        int64  `json:"-"`
    RequestID string `json:"-" gorm:"type:varchar(64);uniqueIndex"`
    TaskID    *int64 `json:"-" gorm:"uniqueIndex"`
    Owner     string `json:"-" gorm:"type:varchar(16);index"` // request/task

    PublicTaskID   string `json:"-" gorm:"type:varchar(64);index"`
    SubmitTime     int64  `json:"-"`
    IsFree         bool   `json:"-"`
    UserID         int    `json:"-" gorm:"index"`
    FundingSource  string `json:"-" gorm:"type:varchar(32)"`
    SubscriptionID int    `json:"-" gorm:"index"`
    FundingAmount  int    `json:"-"`
    TokenID        int    `json:"-" gorm:"index"`
    TokenAmount    int    `json:"-"`

    FundingConsumedAt    int64 `json:"-" gorm:"index"`
    TokenConsumedAt      int64 `json:"-" gorm:"index"`
    PreconsumeCompletedAt int64 `json:"-" gorm:"index"`
    FundingRefundedAt    int64 `json:"-" gorm:"index"`
    TokenRefundedAt      int64 `json:"-" gorm:"index"`
    RefundStartedAt      int64 `json:"-" gorm:"index"`
    RefundCompletedAt    int64 `json:"-" gorm:"index"`
    OwnerTransferredAt   int64 `json:"-"`
    SucceededAt          int64 `json:"-" gorm:"index"`
    CreatedAt            int64 `json:"-"`
    UpdatedAt            int64 `json:"-" gorm:"index"`
}
```

约束：

- `RequestID` 非空且全局唯一；同一 request 的 retry 只能复用该行；
- 初始 `Owner="request"`、`TaskID=nil`；只有 Task link 事务可原子写入唯一
  `TaskID`、`Owner="task"` 和 `OwnerTransferredAt`；
- immutable identity 包含 public Task ID、首次 `SubmitTime`、free/paid
  分类、user/source/subscription/Token identity 和两个 amount；
- paid attempt 的 `UserID` 非零、`FundingSource` 为 wallet/subscription；
  wallet 要求 `SubscriptionID=0`，subscription 要求
  `SubscriptionID>0` 和非空 `RequestID`；
- `FundingSource==""` 只对已由 full-prepaid validator 确认的 free attempt
  合法；此时 `IsFree=true`、两个 amount 和 `SubscriptionID` 都为零；
- free attempt 仍保留真实 `TokenID`，并在 Task link 时要求它与
  `Task.PrivateData.TokenId` 相等；不得把 free 当成“无 Token identity”；
- paid-zero attempt 保持 `IsFree=false` 和原 wallet/subscription source；
  其零额 component 必须通过同步 Apply 事务写 marker，不能伪装成 free；
- 非 playground Token 请求的 `TokenAmount` 等于实际 Token 预扣额；
  playground/未扣 Token 可为零；unlimited 不能仅因 unlimited 标志置零；
- `PreconsumeCompletedAt != 0` 当且仅当两个 `*ConsumedAt` 均非零；
  `RefundCompletedAt != 0` 当且仅当两个 `*RefundedAt` 均非零；
- `SucceededAt` 与 `RefundStartedAt/RefundCompletedAt` 互斥；
- 表内不得保存 Key、TokenKey、prompt、图片、Base64、请求体或供应商响应。

把 `TaskBillingAttempt` 加到普通、fast 和 model/service 测试 AutoMigrate。
测试 cleanup 先清 attempt/record 再清其关联主体。

### 3.3 Attempt 必须先于同步全额预扣

新增：

```go
type TaskBillingAttemptSnapshot struct {
    RequestID      string
    PublicTaskID   string
    SubmitTime     int64
    IsFree         bool
    UserID         int
    FundingSource  string
    SubscriptionID int
    FundingAmount  int
    TokenID        int
    TokenAmount    int
}

func BeginTaskBillingAttempt(
    snapshot TaskBillingAttemptSnapshot,
) (*TaskBillingAttempt, error)

func ApplyTaskFundingPreconsume(
    requestID string,
) (TaskBillingApplyResult, error)

func ApplyTaskTokenPreconsume(
    requestID string,
) (TaskBillingApplyResult, error)

func VerifyTaskBillingAttemptPreconsumed(
    requestID string,
) (*TaskBillingAttempt, error)
```

顺序固定为：

```text
validate snapshot shape
INSERT/GET UNIQUE TaskBillingAttempt owner=request
ApplyTaskFundingPreconsume
ApplyTaskTokenPreconsume
primary DB VerifyTaskBillingAttemptPreconsumed
```

free attempt 没有余额动作，`BeginTaskBillingAttempt` 在创建事务内同时初始化
两个 `*ConsumedAt` 与 `PreconsumeCompletedAt`；后续 Apply 观察 marker 幂等
返回。paid-zero 不走该捷径，仍由对应 Apply 事务写 no-op marker。

wallet 与 Token preconsume 都锁 attempt 和相应主库余额行，执行同步主库扣减，
并在同一事务写对应 `*ConsumedAt`；`BatchUpdateEnabled=true` 时也必须绕过
batch queue。subscription preconsume 在同一 transaction 内创建/锁定
`SubscriptionPreConsumeRecord`、更新 `UserSubscription` 并写
`FundingConsumedAt`。零额 component 不改余额，但必须持久化 marker。

任一 component API 重试时先在锁内检查 marker；已提交 mutation 不重放。
commit 返回错误时只以 primary DB marker read-back 判定；数据库不可读时
fail closed，不调用 cache 增量或旧 `BillingSession.Refund`。commit 后只
invalidate cache，或从主库读取权威绝对余额写 cache。

### 3.4 Task link 与 owner transfer 同事务

新增：

```go
func PrepareTaskSubmissionAttempt(
    candidate *Task,
    persistentID int64,
    requestID string,
) (*Task, *TaskBillingAttempt, error)
```

首次 link：

```text
BEGIN
LOCK TaskBillingAttempt BY RequestID
require owner=request, TaskID=nil, both ConsumedAt + PreconsumeCompletedAt non-zero
validate candidate financial identity and SubmitTime against immutable attempt
INSERT tasks (..., status=SUBMITTING, progress=0%)
UPDATE attempt
  SET TaskID=tasks.id, Owner='task', OwnerTransferredAt=now
COMMIT
```

Task insert、attempt link 和 owner transfer 缺一不可。commit/API 返回错误时，
调用方必须按 `RequestID` 从主库重读 owner：`request` 由 request recovery
退款，`task` 由 linked Task failure path 退款。若 read-back 暂时不可用，
不得猜 owner、不得立即执行 ledger 外退款、不得调用 `/generate`；保留 attempt
给 stale request-owned sweep 或 linked Task timeout sweep 收敛。

request failure refund 与 Task failure refund 都按同一 `RequestID` 锁定同一
attempt，使用同一组 consumed/refunded marker。`BillingSession` 只携带计划
数据和 zero-delta settle 状态；durable 路径禁止再调用 ledger 外的非幂等
`BillingSession.Refund`。

link 前必须 fail closed 验证：

```text
attempt.PublicTaskID == Task.TaskID
attempt.UserID == Task.UserId
attempt.FundingAmount == Task.Quota
attempt.FundingSource == Task.PrivateData.BillingSource
attempt.SubscriptionID == Task.PrivateData.SubscriptionId
attempt.TokenID == Task.PrivateData.TokenId
attempt.SubmitTime == Task.SubmitTime
amounts non-negative and free/paid shape remains valid
```

任一不匹配不创建 Task、不改变 owner。Task retry 时也以 attempt 内首次
`SubmitTime` 为真值，不得使用当前 wall clock 重建。

## 4. Task 5A：Model lifecycle 与退款原子性

### 4.1 `SUBMITTING` 与 polling

新增：

```go
const TaskStatusSubmitting TaskStatus = "SUBMITTING"
```

要求：

- `GetAllUnFinishSyncTasks` 排除 `SUBMITTING`，不得用公开 TaskID 轮询供应商；
- `HasTaskPollingWork` 和 timeout 查询仍包含 `SUBMITTING`；
- timeout sweep 只能通过下述锁行、窄列 transition 把它变为
  `FAILURE/100%`；
- timeout winner 和提交 controller 都调用同一个幂等 Task refund orchestrator。

新增：

```go
func TransitionTaskSubmissionToFailure(
    id int64,
    publicTaskID string,
    upstreamTaskID string,
    code string,
    message string,
) (*Task, error)
```

该 primitive 在 transaction 内锁定最新 Task 行，只允许
`SUBMITTING/SUBMITTED -> FAILURE/100%`，并使用窄列 update：

```text
status, progress, finish_time, fail_reason, updated_at
```

只有 supplied upstream ID 非空且 locked row 尚无 ID 时，才从锁内最新
`PrivateData` 副本补写 ID 后更新 `private_data`；相同 ID 幂等，不同 ID
冲突。不得对 `SUBMITTING` 调用旧 `UpdateWithStatus`、
`Select("*")`、`Save(staleTask)` 或任何 stale 全行回写。timeout sweep、
`FailAndRefundTaskSubmission` 和 reconciliation handler 必须共用该 primitive。

并发测试必须固定以下 interleaving：sweep discovery 先读到空 ID，正常 Attach
随后 commit，sweep 再进入 transition 锁并重读；最终必须是
`FAILURE/100%`，且最新 upstream ID、Key、billing identity 和其他
`PrivateData` 均保留。

### 4.2 Prepare retry 的财务快照与时间不可漂移

`persistentID != 0` 时按 `RequestID` 锁定现有 Task 和 attempt，并验证：

```text
Task.ID、TaskID、UserId、Platform、Action、SubmitTime 不变
Status == SUBMITTING
Progress == 0%
UpstreamTaskID 为空
Group、Quota、BillingSource、SubscriptionId、TokenId 不变
BillingContext 深度相等
TaskBillingAttempt immutable snapshot 逐项相等
Task.SubmitTime == TaskBillingAttempt.SubmitTime
```

只允许刷新当前尝试的路由字段：

```text
ChannelId
PrivateData.Key
Properties.UpstreamModelName
必要的非财务 route metadata
```

不得在 retry 中更新 `Quota`、billing source、subscription/Token identity、
request ID、首次 `SubmitTime`、`BillingContext` 或 attempt ledger。5B 将
首次时间写入 `TaskRelayInfo.DurableSubmitTime`；若内存值丢失，必须从 linked
Task/attempt 回填。即使两次 retry 间 wall clock 推进两秒以上，也复用首次
时间。若新尝试重新计算的 quota 与首次预扣不相等，应在 prepare 前由
full-prepaid validator 拒绝。

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
type TaskBillingApplyResult struct {
    Applied   bool
    Completed bool
    Owner     string
    TaskID    int64
    UserID    int
    TokenID   int
}

func ApplyTaskFundingRefund(
    requestID string,
) (TaskBillingApplyResult, error)

func ApplyTaskTokenRefund(
    requestID string,
) (TaskBillingApplyResult, error)

func GetTaskBillingAttemptByRequestID(
    requestID string,
) (*TaskBillingAttempt, error)

func GetTaskBillingAttemptByTaskID(
    taskID int64,
) (*TaskBillingAttempt, error)
```

request failure 与 Task failure 都用 `RequestID` 调用这两个 Apply。每次 Apply
锁定 attempt；若 owner 为 task，再锁定 linked Task 并要求它是 `FAILURE`。
主数据库余额 mutation 与对应 `*RefundedAt` marker 必须在同一事务，禁止调用
全局 `DB`、batch queue、先改 Redis 或 ledger 外的旧增量 helper。
任一首次 refund Apply 都在锁内先写/确认 `RefundStartedAt`，并要求
`SucceededAt==0`；从此所有 preconsume API 均拒绝该 attempt。

任何首次 mutation 前重验：

```text
RequestID、owner、TaskID link 合法
task-owned 时 attempt.UserID/source/subscription/token == locked Task identity
task-owned 且 RefundCompletedAt==0 时 FundingAmount == Task.Quota
request-owned 时 TaskID 必须 nil
free/paid/source/amount shape 未漂移
subscription positive amount 时 record identity 完整匹配
```

若对应 `*RefundedAt` 已非零，立即以 marker 幂等成功；这一分支不得依赖可能已
按保留策略清理的 `SubscriptionPreConsumeRecord`。首次 subscription refund
且 amount 非零时才锁 record，并要求 request/user/subscription/amount 和
`consumed/refunded` 状态合法。任何 identity 漂移整体回滚，余额和 marker
mutation 均为零。

#### Funding refund

```text
BEGIN
LOCK TaskBillingAttempt BY RequestID
if owner=task: LOCK linked Task and require FAILURE
FundingRefundedAt != 0 -> idempotent success without loading old record
FundingConsumedAt == 0 or FundingAmount == 0 -> no balance mutation
wallet -> UPDATE users SET quota = quota + FundingAmount
subscription -> lock matching record + subscription, restore amount and mark refunded
SET FundingRefundedAt
if TokenRefundedAt != 0:
  SET RefundCompletedAt
  if owner=task: clear locked Task.Quota
COMMIT
```

#### Token refund

```text
BEGIN
LOCK TaskBillingAttempt BY RequestID
if owner=task: LOCK linked Task and require FAILURE
TokenRefundedAt != 0 -> idempotent success
TokenConsumedAt == 0 or TokenAmount == 0 -> no balance mutation
Token exists -> atomically restore remain_quota/used_quota
Token deleted -> do not recreate; mark component not applicable
SET TokenRefundedAt
if FundingRefundedAt != 0:
  SET RefundCompletedAt
  if owner=task: clear locked Task.Quota
COMMIT
```

未完成 preconsume 的 component 也必须由 refund Apply 写 no-op marker，避免
stale request-owned attempt 永久悬挂。paid-zero 保留 paid source，并通过
上述 Apply 写 marker；free 也通过同一 marker 路径收敛。TokenKey 只可在
model 内部用于 commit 后 cache invalidation，不能持久化到 Task、attempt、
SystemTask、日志或错误。

### 4.7 Subscription record retention 与 ledger lifecycle

`CleanupSubscriptionPreConsumeRecords` 不得仅按 `updated_at < now-7d` 删除。
对每个候选 record，必须排除仍由相同 `RequestID` 的 active
`TaskBillingAttempt` 引用的行：

```text
active := SucceededAt == 0 && RefundCompletedAt == 0
active attempt exists -> retain record regardless of age
terminal attempt + record older than retention -> eligible for cleanup
```

测试必须把 record 和 active request-owned/task-owned attempt 的时间推进超过
七天，再证明 funding refund 仍能完成。refund component marker 已完成后，
重复 Apply 先读 marker 幂等返回，不依赖 record 是否已经清理。

本 Task 不物理删除 billing-attempt ledger。成功 zero-delta settle 原子写
`SucceededAt`；完整退款写 `RefundCompletedAt`。未来归档只能选择这两类
terminal attempt，必须保留唯一 `RequestID`、owner/link、immutable identity
和全部 component markers，且 lookup/recovery 仍可判定已完成。

### 4.8 Exactly-once 的精确定义

本文的 exactly-once 声明限定为：

> 对每个 billing-attempt preconsume/refund component，主数据库中的
> wallet/subscription/Token 余额 mutation 与对应 durable marker 在同一事务
> 提交；任意进程崩溃、事务回滚、commit 结果不确定或 sweep 重试后，已提交
> component 不重放，未提交 component 可继续执行。

Redis/cache、消费/退款日志和统计是派生状态，不包含在主库事务承诺中：

- commit 后只做 cache invalidation，或从主库读取权威绝对余额后写入 cache；
  绝不再次写主库，也不执行可重复的 cache 增量；
- cache/log 失败不得触发主库额度 mutation 重放；
- cache 最终通过 invalidation、回源或 TTL 收敛；
- 若以后要求日志 exactly-once，应另设唯一 operation key，不得复用余额
  marker 暗示跨库原子性。

### 4.9 Sweep

`sweepUnrefundedFailedTasks` 不再：

```text
ClaimQuotaForRefund
先清 quota
RestoreQuotaAfterFailedRefund
```

新 sweep 以 incomplete attempt marker 为选择条件，绝不只看
`Task.Quota != 0`：

```text
request-owned + stale + SucceededAt=0 + RefundCompletedAt=0
  -> lock attempt, set RefundStartedAt, refund consumed components
task-owned + linked Task FAILURE + RefundCompletedAt=0
  -> refund incomplete components
task-owned + linked Task SUBMITTING timeout
  -> TransitionTaskSubmissionToFailure, then refund incomplete components
```

stale request-owned sweep 覆盖进程在 preconsume 任意 component 后、Task
insert/link 前崩溃；它与 preconsume API 都锁 attempt，`RefundStartedAt`
一旦写入，任何新的 preconsume 都 fail closed。paid-zero 即使
`Task.Quota==0` 也能按缺失 refund marker 被选择。第二个 refund component
完成时同事务写 `RefundCompletedAt`；task-owned attempt 同时清 Task quota。

为保持旧 adaptor/历史 Task 兼容：

- 所有新 durable Task 都必须有 linked `TaskBillingAttempt` 并走新路径；
- lookup 只有明确返回 `gorm.ErrRecordNotFound` 才允许进入历史/旧 adaptor
  legacy refund；timeout、连接错误或其他 DB error 一律 fail closed；
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

controller `/generate` handler 在 Begin attempt 前调用该导出函数完成
free/paid shape 分类，但不做余额 mutation；`RelayTaskSubmit` 在 primary DB
preconsume verification 后、`BuildRequestBody` 前再次调用；controller 在
deferred settle 前第三次调用。controller 不复制 validator。

### 5.2 Provisional billing snapshot

`newProvisionalTask` 必须写：

```go
PerCallBilling:
    common.StringsContains(
        constant.TaskPricePatches,
        info.OriginModelName,
    ) || info.PriceData.UsePrice
```

durable 路径的顺序是：先 Begin attempt，再执行同步主库 preconsume，随后
`BuildRequestBody`，最后在 `DoRequest` 前执行 Task link/owner transfer。
body 构造或 link 失败都按同一 attempt markers 退款，供应商调用次数为零。

### 5.3 Ownership transfer

扩展：

```go
type TaskRelayInfo struct {
    // existing fields...
    PersistentTaskID       int64
    BillingAttemptRequestID string
    DurableSubmitTime      int64
}
```

删除以内存 bool 决定退款 owner 的模式。request/controller failure recovery
始终以 `BillingAttemptRequestID` 查询 durable owner：

```text
owner=request, TaskID=nil
  -> 用 attempt consumed/refunded markers 执行 request-owned refund
owner=task, TaskID!=nil
  -> Transition linked Task to FAILURE, 再用同一 markers refund
DB unavailable / owner unreadable
  -> fail closed，不执行裸退款；交给 stale/timeout sweep
```

`BillingSession.Refund` 不得参与 durable 路径。即使 link transaction 实际
commit、API 返回错误且 read-back 失败，后续也不根据内存
`PersistentTaskID`/bool 猜测；ledger owner 是唯一真值。

付费任务从计划中的 Billing session 构造 attempt snapshot，然后由 5A 的同步
主库 primitive 扣减；经 validator 确认的免费任务使用 `IsFree=true`、
`FundingSource=""`、零 amount/SubscriptionID 的 snapshot，并保留真实
TokenID。paid-zero 则必须保留 paid source 和 `IsFree=false`。

每次 preconsume 后，5B 在 `/generate` handler 内直接调用 primary-DB
`VerifyTaskBillingAttemptPreconsumed`，证明 wallet/subscription 与 Token
marker 已随主库扣减提交，再允许 link/POST。该验证不得读 Redis/batch
shadow：它从主库加载 attempt 以及对应 user/subscription/Token identity；
marker 是同事务扣减的 durable 证明。测试从已知初始余额直接查询主库并断言
wallet/Token 精确 delta。测试必须打开
`BatchUpdateEnabled=true`、模拟 batch queue 丢失，仍观察到主库 wallet/Token
已扣且 POST 只在验证后发生。

Safe 429 retry 复用同一 RequestID、attempt、Task 和首次
`DurableSubmitTime`。内存时间为空时从 attempt/Task 回填；禁止用 retry 时的
当前时间覆盖。

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
    requestID string,
    upstreamTaskID string,
    taskData []byte,
    code string,
    message string,
) error
```

内部：

1. 以 request ID 从主库加载 attempt/owner，并核对 linked Task；
2. supplied upstream ID 为空时不伪造；
3. 非空时先使用正常严格 attach；若状态竞争，则交给
   `TransitionTaskSubmissionToFailure` 在同一锁内按严格 ID 规则补写；
4. 只通过 `TransitionTaskSubmissionToFailure` 允许
   `SUBMITTING/SUBMITTED -> FAILURE/100%`；
5. 已是 `FAILURE` 为幂等；不覆盖 `QUEUED/IN_PROGRESS/SUCCESS`；
6. transition 只写窄列和锁内最新 `PrivateData`，不得 stale 全行回写；
7. primary read 确认为 FAILURE 后按 attempt RequestID 调用 component refund。

退款调用不限定为 status CAS winner。ledger marker 才是每个 component 的
唯一执行权；CAS winner 崩溃后，CAS loser 或 sweep 可以安全补偿。

controller 退出 retry loop 后，只要 `taskErr != nil` 且存在 durable attempt，
就必须按 RequestID 解析 owner：task-owned 调用该函数并传入保留的 partial
`UpstreamTaskID/TaskData`；request-owned 调用同一 ledger 的 request recovery。
owner 查询失败时 fail closed，禁止 request defer 执行
`BillingSession.Refund`。

### 5.7 Zero-delta settle 与成功输出

deferred response 的 controller 顺序固定为：

```text
Task 已 attach
Task 已 Commit 为 SUBMITTED/10%
ValidateFullPrepaidTaskBilling
SettleBilling(result.Quota)
MarkTaskBillingAttemptSucceeded(requestID)
LogTaskConsumption
c.JSON(HTTPResponse.StatusCode, HTTPResponse.Body)
```

validator 保证 paid Task 的 actual quota 等于 pre-consumed quota。真实
`BillingSession.Settle` 的 delta=0 分支只标记 settled，不调用 wallet、
subscription 或 Token adjustment。`MarkTaskBillingAttemptSucceeded` 从主库
锁定 task-owned attempt 与 linked `SUBMITTED` Task，要求两个 consumed marker
完整、refund 尚未开始，再写 `SucceededAt`；不得依赖 cache。

若 validator 或 settle 返回错误：

- 不写 HTTP 200；
- 不写成功消费日志；
- 保留 upstream ID；
- 通过窄列 transition 执行 `SUBMITTED/10% -> FAILURE/100%`；
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
按 TaskID 加载 TaskBillingAttempt，验证 owner=task、RequestID/link/UserID
验证 Task.Platform 与 channel type 为 Seed Dance
reconciliation-only attach
若仍 SUBMITTING，通过 TransitionTaskSubmissionToFailure 转 FAILURE/100%
若已 FAILURE，保持状态
按 attempt RequestID 执行/补齐 component refund
从 primary DB 重载 Task + TaskBillingAttempt
只有 FAILURE/100%、stored ID == payload ID、Task.Quota == 0、
两个 RefundedAt 与 RefundCompletedAt 均非零时才标记 SystemTask succeeded
冲突、不完整身份或退款未完整提交均标记 SystemTask failed，保留管理员记录
永不 Commit，永不复活 Task
```

## 6. 旧 adaptor 兼容与数据安全

所有新行为由 `FullPrepaidTaskSubmitter`、`DurableTaskSubmitter` 和
`DeferredTaskSubmitResponder` 可选接口守卫。未实现这些接口的 adaptor：

- 继续使用既有通用 HTTP error code/status；
- 继续由既有 `DoResponse` 决定响应；
- 不提前插入 provisional Task；
- 不要求 `TaskBillingAttempt`；
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

- `model/task_billing_attempt.go`
- `model/task_billing_attempt_test.go`
- `model/task_submission.go`
- `model/task_submission_test.go`

**Modify**

- `model/task.go`
- `model/task_cas_test.go`
- `model/main.go`
- `model/subscription.go`
- `model/user.go`
- `model/token.go`
- `service/billing_session.go`
- `service/funding_source.go`
- `service/quota.go`
- `service/task_billing.go`
- `service/task_billing_test.go`
- `service/task_polling.go`
- `service/task_polling_test.go`

5A 的新 atomic model 文件持有 wallet/Token 主库 transaction primitive；
service 层只编排，不能经 batch queue。5A 不修改 `relay`、`controller` 或
Seed Dance adaptor。

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

- attempt 在任何余额预扣前按 RequestID 唯一创建，重复 begin 不漂移；
- wallet/Token/subscription 的主库 preconsume 与各自 `ConsumedAt` 同事务；
- `BatchUpdateEnabled=true` 且 batch queue 丢失时，wallet/Token 主库扣减和
  markers 仍准确；
- preconsume 任一 commit ambiguous 时，primary marker read-back 不重放；
- Task insert、attempt link 与 request→task owner transfer 同事务；
- link commit 实际成功但 API/read-back 失败时不执行 request 裸退款；
- DB 临时不可读时 owner resolution fail closed，后续 sweep 能按 durable owner
  恢复；
- stale request-owned attempt sweep 覆盖 funding-only、token-only 和两个
  preconsume 后但尚未 link 的进程崩溃；
- 首次 Prepare 的 Task/private financial identity 与 attempt 任一不匹配时
  fail closed，且不留下 Task/owner 漂移；
- Prepare retry 只刷新 route 字段，财务快照与首次 SubmitTime 均不漂移；
- normal Attach/Commit 状态机与 read-after-error；
- `TransitionTaskSubmissionToFailure` 只写窄列，不调用 stale 全行
  `UpdateWithStatus`；
- sweep 先读空 ID、Attach commit、transition 后最终 FAILURE/100% 且 ID、
  Key 和最新 PrivateData 保留；
- reconciliation-only attach 可给完全匹配的 `FAILURE/100%` 补 ID，但状态、
  reason、finish time 和 quota 不变；
- request-owned 与 task-owned refund 复用同一 attempt/component markers；
- wallet/subscription/Token 主库 refund 与 `RefundedAt` 同事务；
- `RefundCompletedAt != 0` 当且仅当两个 RefundedAt 均非零；
- paid-zero/free 的 no-op component 均以幂等事务持久化 marker；
- 每次 Apply 重验 Task/attempt/private-data/subscription identity；任一漂移
  不产生余额或 marker mutation；
- funding 已完成而 Token 失败时，重试不再 funding；
- commit 已成功但 API 返回错误时，read-back 不重放 mutation；
- 两个并发 sweep 对每个 component 只产生一次主库 mutation；
- incomplete-ledger sweep 不依赖 `Task.Quota != 0`，可恢复 paid-zero；
- attempt lookup 仅 `gorm.ErrRecordNotFound` 进入 legacy；其他 error 不产生
  legacy mutation；
- active subscription record 超过七天仍保留并可退款；terminal record cleanup
  后重复 Apply 只靠 marker 幂等成功；
- 第二 component 完成与 `Task.Quota=0` 同事务；
- cache/log failpoint 不触发主库 mutation 重放；
- legacy Task 无 ledger 时保持旧行为。

### 8.2 Task 5B RED/GREEN

先写失败测试，证明：

- exported validator 的七种 free/paid 状态；
- `TASK_PRICE_PATCH` 且 `UsePrice=false` 时 `PerCallBilling=true`；
- attempt 在 preconsume 前、provisional Task/link 在 `/generate` 前可从 DB
  观察；
- `/generate` handler 从 primary DB 验证两项 consumed markers；batch queue
  丢失也不影响主库扣减；
- provisional link transaction 失败时 POST=0，并按 durable owner/markers
  refund；DB 不可读时先不退款、由 sweep 后续收敛；
- Safe 429 retry 复用同一 attempt/Task/首次 SubmitTime，测试时钟推进至少
  两秒仍成功；
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
validate_full_prepaid_shape
begin_billing_attempt_owner_request
sync_funding_preconsume_and_marker
sync_token_preconsume_and_marker
primary_db_verify_preconsume
validate_full_prepaid_before_build
build_body
insert_provisional_link_attempt_transfer_owner
post_generate
attach_upstream_id
build_public_response
mark_submitted
validate_full_prepaid_again
settle_zero_delta
mark_billing_attempt_succeeded
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
- request/task owner、sync preconsume/refund marker、stale-attempt sweep 和窄列
  failure transition 的 failpoint/concurrency 矩阵通过；
- `BatchUpdateEnabled=true` 的主库事实测试与七天 subscription retention
  测试通过；
- secret scan 未发现 Key、TokenKey、prompt、图片、Base64 或完整响应；
- 没有 Seed Dance 专用大小阈值；
- 旧 adaptor 兼容测试通过。
