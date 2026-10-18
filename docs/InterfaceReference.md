## 8 接口定义

### 8.1 EventUnit：统一事件通信单元

EventUnit 是系统内部统一的事件通信抽象。各组件通过事件传递状态变化、执行结果和控制信号，减少组件之间的直接依赖。

每个 EventUnit 可以维护自己的订阅者列表、事件缓冲区和内部处理逻辑。组件之间的订阅关系在初始化阶段由 Runtime 根据统一的订阅配置建立，避免各组件自行维护通信关系。

#### 8.1.1 接口说明

* **Event**：事件的统一抽象，通过 `EventType()` 标识事件类型，便于接收方识别和分发事件。
* **EventSender**：对外暴露的事件发送接口。订阅者只能通过 `Send()` 向目标事件单元发送事件，不直接访问其内部处理逻辑。
* **EventAcceptor**：事件单元内部的事件接纳能力。负责将事件接收到内部处理流程或缓冲区中，避免发送方直接参与业务处理。该能力按需实现，不要求所有组件都具备。
* **EventEmitter**：事件单元内部的事件发送能力。负责将事件发送给已注册的订阅者，不要求所有组件都具备。
* **EventUnit**：事件通信的基础载体。`Contacts` 保存当前事件单元的订阅者接口，内部可根据组件职责实现事件接纳、发送、缓冲和处理逻辑。

#### 8.1.2 通信约定

1. **统一发送入口**：组件之间通过 `EventSender.Send()` 发送事件，不直接调用其他组件的业务方法。
2. **单一订阅配置**：订阅关系由 Runtime 统一初始化。配置只维护一个方向，Runtime 根据配置构建实际的订阅者列表。
3. **发送与处理解耦**：`Send()` 负责将事件交给目标单元的接收机制，不应默认同步执行完整的业务处理流程。
4. **按需接纳与发送**：`EventAcceptor` 和 `EventEmitter` 是可选能力，根据组件是否需要接纳事件、主动广播事件决定是否实现。
5. **可靠性由事件类型决定**：研究结果、状态提交等关键事件不能被静默丢弃；可合并的状态通知可以合并，进度类事件则可以按需限流。

```go
// Event 表示系统内部传递的事件。
// EventType 用于标识事件类型，供接收方识别和分发。
type Event interface {
    EventType() string
}

// EventSender 是对外暴露的事件发送接口。
// 发送方通过 Send 将事件交给目标单元的接收机制，
// 不直接调用目标单元的内部处理逻辑。
type EventSender interface {
    Send(ctx context.Context, e Event) error
}

// EventAcceptor 表示事件单元内部的事件接纳能力。
// 可用于将事件放入缓冲区或内部处理流程，
// 属于可选能力，不要求所有事件单元都实现。
type EventAcceptor interface {
    Accept(ctx context.Context, e Event) error
}

// EventEmitter 表示事件单元内部的事件发送能力。
// 负责将事件发送给当前单元的订阅者。
type EventEmitter interface {
    Emit(ctx context.Context, e Event) error
}

// EventUnit 是系统内部统一的事件通信单元。
// Contacts 保存当前事件单元的订阅者；
// 内部可根据组件职责实现事件接纳、发送、缓冲及处理逻辑。
type EventUnit struct {
    Contacts []EventSender

    // 内部处理器、缓冲区、队列等。
}
```

### 8.2 Task 与 RunTask

Task 和 RunTask 分别负责用户侧的任务管理与研究执行管理。两者通过 `TaskID` 建立关联，将用户请求、任务元信息与具体的执行过程分离。

#### 8.2.1 Task

Task 表示用户创建的研究任务，是用户侧的任务入口，负责记录任务的基本信息和元数据。

**字段说明：**

* `ID`：Task 的唯一标识。
* `UserID`：任务所属用户的标识。
* `Title`：任务标题，用于展示和检索。
* `Description`：任务描述，用于记录研究目标或任务背景。
* `CreatedAt`：任务创建时间。
* `UpdatedAt`：任务信息最后更新时间。

Task 不直接承载研究执行状态，也不直接管理 Runtime、Assignment 或 SubAgent。用户后续发送消息时，通过 Task 定位关联的 RunTask，再由对应的 Runtime 处理。

#### 8.2.2 RunTask

RunTask 表示与 Task 关联的一次具体研究执行实例，负责记录执行标识、执行状态、轮次和资源预算等信息，并作为 Runtime 生命周期管理的关联对象。

**字段说明：**

* `ID`：RunTask 的唯一标识。
* `TaskID`：关联的 Task 标识，用于从用户侧任务定位研究执行实例。
* `Status`：RunTask 的生命周期状态，用于约束执行启动、停止和后续恢复等操作
* `Turn`：当前研究交互轮次，用于记录研究过程中的轮次推进。
* `Budget`：执行预算，用于约束研究过程中的资源消耗。
* `CreatedAt`：RunTask 创建时间。
* `UpdatedAt`：RunTask 信息最后更新时间。

每个 RunTask 对应一套独立的运行环境，包括 Runtime、SupervisorLoop、Research State、Finding Graph、Assignment 和 Checkpoint 等。不同 RunTask 之间的研究状态与执行资源相互隔离。

##### 8.2.2.1 RunStatus：研究执行状态

RunTask 使用 `RunStatus` 描述当前研究执行状态，共定义四种状态：

* **Stopped**：研究被用户主动中止。当前执行流程已停止，新消息可以进入，并基于已有研究状态开启新一轮执行。已提交的研究数据必须保留。
* **Interrupted**：研究执行过程中存在局部中断，或者当前执行正在等待中断相关审批。RunTask 尚未完成，但当前执行流程受到中断状态约束，需要根据中断原因和处理结果决定后续动作。
* **Running**：研究正在正常执行，当前不存在等待处理的局部中断或相关审批。
* **Finish**：研究已经结束，不再继续当前研究执行。

这四种状态由 Runtime 与 Service 协同维护，用于控制新一轮执行的准入、当前执行流程的推进，以及研究结束后的行为。

其中，`Stopped` 与 `Interrupted` 的区别在于：前者表示用户主动停止了当前研究执行，但允许后续消息重新进入；后者表示当前研究尚未结束，只是存在需要处理的局部中断或审批事项。

`Finish` 是研究结束状态，与 `Stopped` 不同。用户中止并不意味着研究任务永久结束，因此不能将 `Stopped` 等同于 `Finish`。

```go
type RunStatus string

const (
    RunStatusStopped     RunStatus = "stopped"
    RunStatusInterrupted RunStatus = "interrupted"
    RunStatusRunning     RunStatus = "running"
    RunStatusFinish      RunStatus = "finish"
)
```

| 当前状态 | 事件 | 后续状态 |
|---|---|---|
| Running | 用户 Stop | Stopped |
| Running | 出现需要处理的局部中断 | Interrupted |
| Interrupted | 中断处理完成，研究继续 | Running |
| Stopped | 新消息通过准入并启动新一轮执行 | Running |
| Running / Interrupted | 研究确认结束 | Finish |

#### 8.2.3 Task 与 RunTask 的关系

当前设计采用 **一个 Task 关联一个 RunTask** 的关系。

* **创建阶段**：Service 完成请求校验与执行准入后，创建 Task 和 RunTask，建立关联，再初始化并启动对应的 Runtime。
* **后续交互**：用户继续发送消息时，通过 Task 找到已有的 RunTask，并交由对应 Runtime 处理，不重复创建 RunTask。
* **停止与继续**：用户 Stop 当前研究时，停止当前执行过程并保留已提交的研究数据。后续消息可以继续关联原 RunTask，基于最新已提交的研究状态启动新一轮执行。
* **异常处理**：Runtime 启动失败或执行异常时，记录相应状态与错误信息；清理运行时实例不等于删除 Task、RunTask 或已经提交的研究数据。

通过这一划分，Task 负责维持用户侧任务的连续性，RunTask 负责承载具体研究执行的状态与资源，两者共同支持多轮交互、停止后继续以及执行异常后的恢复。


```go
type Task struct {
    ID          string
    UserID      string

    Title       string
    Description string

    CreatedAt   time.Time
    UpdatedAt   time.Time
}
```

```go
type RunTask struct {
    ID       string
    TaskID   string

    Status   RunStatus

    Turn     int

    Budget   RunBudget

    CreatedAt time.Time
    UpdatedAt time.Time
}
```

### 8.3 Supervisor

Supervisor 是研究任务的决策主体，负责根据用户输入、当前 Research State 和已有研究结果，决定下一步研究方向，并生成或调整 Assignment。

Supervisor 实现 Eino 中定义的 Agent 接口，遵循统一的 Agent 调用规范，但不直接使用 ADK 封装的 `SupervisorAgent`。ADK 提供的封装具有自身的运行模式，而本项目需要由 Runtime 统一协调事件处理、中断、状态更新、Assignment 调度和恢复流程，因此需要保留 Supervisor 的执行控制权。

Supervisor 本身负责研究决策，不直接执行 Assignment，也不直接修改共享 Research State。生成或调整的 Assignment 交由 Scheduler 管理和调度，研究结果由 ResultAggregator 聚合并提交到 Research State。

```go
type Agent interface {
    Name(ctx context.Context) string
    Description(ctx context.Context) string
    
    // Run 执行 Agent，返回事件流
    Run(ctx context.Context, input *AgentInput, options ...AgentRunOption) *AsyncIterator[*AgentEvent]
}
```

### 8.4 Supervisor Loop

SupervisorLoop 是 Supervisor 的运行控制循环，负责组织每一轮研究决策，并协调输入、上下文和执行控制。

* `ResearchRunID`：关联当前研究执行实例，用于定位对应的 RunTask 及其运行状态。
* `TurnLoopCfg`：当前研究决策循环的配置，用于控制每轮决策的输入组织与执行行为。

SupervisorLoop 根据用户消息、Research State 更新和内部控制事件组织决策输入，并驱动 Supervisor 运行。遇到用户 Stop、普通消息抢占、状态更新或 MemoryManager 上下文压缩等情况时，由 Runtime 与 SupervisorLoop 协同处理当前决策轮次的中断和后续恢复。

SupervisorLoop 负责控制研究决策的推进，不替代 Scheduler 管理 Assignment 生命周期，也不直接承担 SubAgent 的执行管理。

```go
type SupervisorLoop struct{
    ResearchRunID RuntaskID

    TurnLoopcfg TurnLoopConfig
}
```

### 8.5 Scheduler

Scheduler 是研究任务的调度组件，负责管理 Assignment 的提交与取消，并协调 Assignment 的执行生命周期。

Supervisor 生成或调整 Assignment 后，通过 `Scheduler.Submit()` 提交任务。Scheduler 负责完成必要的注册和调度准备，并根据运行资源情况安排执行。Assignment 的具体执行由 Work Pool 承担，Scheduler 不直接执行 SubAgent，也不负责研究决策。

当需要取消某个 Assignment 时，调用 `Scheduler.Cancel()`。Scheduler 负责协调对应执行任务的取消与清理，并处理其调度状态。

Scheduler 与 ResultAggregator 协同管理 Assignment 的结果交付过程：Assignment 注册时，需要为其预留结果接收槽位；只有预留成功后，Assignment 才能进入可调度状态。Assignment 被取消时，Scheduler 通知 ResultAggregator 取消对应槽位。Assignment 完成后，只有在结果成功提交到 Research State 后，Scheduler 才能回收对应的 Assignment 记录，避免结果尚未提交就丢失任务关联信息。

#### 8.5.1 接口说明

* `assignmentID`：Assignment 的唯一标识，用于定位任务、执行取消操作及关联结果交付过程。
* `SchedulerRegister`：嵌入 `EventUnit`，使调度组件具备统一事件通信能力。
* `Scheduler`：定义调度组件对外提供的核心操作。
* `Submit()`：提交 Assignment，启动后续注册与调度流程。
* `Cancel()`：根据 Assignment ID 请求取消任务，并协调后续清理。

Scheduler 只维护调度所需的任务状态与生命周期信息，不负责维护 Assignment 之间的依赖图，也不负责聚合 Result 或直接修改 Research State。


```go
type assignmentID string

type SchedulerRegister interface {
    EventSender
    EventAcceptor
    EventEmitter
}

type Scheduler interface {
    Submit(ctx context.Context, assignment *Assignment) error
    Cancel(ctx context.Context, assignmentID string) error
}
```

### 8.6  Work

Work 是 Assignment 的实际执行单元，负责承载 SubAgent 的运行过程。每个 Work 关联一个 Assignment，通过 ChatModelAgent 执行研究任务，并持有执行所需的 Context 和运行配置。

Work 与 WorkInfo 分离：Work 负责实际执行，WorkInfo 负责记录执行实例的身份、状态和生命周期信息。两者的生命周期不同，WorkInfo 在 Work 创建之前建立，并在 Work 销毁后继续保留一段时间，用于执行状态查询、生命周期协调和异常排查。

#### 8.6.1 WorkInfo

WorkInfo 是 Work 的执行记录，在实际 Work 创建之前建立。它记录执行实例的身份、所属 Assignment、当前状态、执行约束、起止时间和尝试次数。

字段说明：

* `WorkerID`：执行实例的唯一标识，用于区分不同的 Work 执行实例。
* `AssignmentID`：所属 Assignment 的标识，用于关联调度任务和执行结果。
* `Status`：Work 的执行状态，用于记录当前执行阶段及最终结果。
* `Constraint`：当前执行实例适用的约束，包括工具使用限制和执行时间限制。
* `StartAt`：Work 实际开始执行的时间。
* `EndAt`：Work 执行结束的时间；尚未结束时为空值。
* `Attempt`：当前 Assignment 的执行尝试次数，用于记录重试过程。

WorkInfo 不依赖 Work 对象的存活而存在。Work 结束并完成资源清理后，WorkInfo 仍然保留，直到对应记录不再需要被后续流程使用。具体保留时长与持久化策略由后续实现确定。

#### 8.6.2 Work

Work 是短生命周期的执行对象，负责实际运行研究任务。

字段说明：

* `Context`：当前 Work 的执行上下文，用于传递取消信号、控制执行生命周期。
* `WorkAgent`：实际执行研究任务的 ChatModelAgent。
* `Assignment`：当前 Work 执行的任务内容与相关信息。
* `WorkInfo`：当前执行实例对应的生命周期记录，用于查询和更新执行状态。

Work 创建后，依据 Assignment 和执行约束运行 SubAgent。执行过程中更新对应的 WorkInfo；执行结束后，将最终 Result 交由结果处理流程，再释放 Work 持有的运行资源。

Work 的销毁不代表 Assignment 的结果已经成功提交。Scheduler 应根据结果交付与提交状态决定何时回收 Assignment，避免执行对象被销毁后，尚未提交的研究结果失去关联。

#### 8.6.3 WorkConstraint

WorkConstraint 描述 Work 执行期间需要遵守的限制。

* `ToolAllowed`：允许使用的工具列表，当前暂不实现。
* `TimeLimit`：单次 Work 的执行时间限制，用于识别执行超时。

执行超时不直接等同于执行失败，也不自动触发重试。Scheduler 根据超时状态发起后续处理；需要用户审批时，由 Runtime 协调等待、继续执行或取消等流程。

#### 8.6.4 生命周期关系

1. Assignment 注册时，创建对应的 WorkInfo，记录执行身份和约束。
2. Scheduler 完成调度准入后，创建 Work，并关联对应的 WorkInfo。
3. Work 执行期间，根据实际进展更新 WorkInfo。
4. Work 结束后，完成最终 Result 的交付，并清理执行资源。
5. Work 销毁后，WorkInfo 继续保留，供结果提交确认、执行状态查询及异常排查使用。
6. 确认后续流程不再需要相关记录后，才考虑清理 WorkInfo。

通过这种划分，Work 的运行资源可以及时释放，而执行记录仍然能够支持调度协调、结果交付和异常处理。

```go
type WorkInfo struct {
    mu           sync.RWMutex
    WorkerID     string
    AssignmentID string

    Status       WorkStatus
    Constraint   *WorkConstraint

    StartAt      time.time
    EndAt        time.time

    Attempt      int
}

type Work struct{
    Context     content.context
    WorkAgent   ChatModelagent
    Assignment  Assignment

    WorkInfo    *WorkInfo
}

type WorkConsrtraint struct{
    ToolAllowed  []ToolInfo//暂时不用实现
    TimeLimit    time.Duration
}
```

### 8.7 WorkPool

WorkPool 是研究任务的执行资源管理组件，负责管理 Work 的创建、运行和清理，并限制同时运行的 Work 数量。

WorkPool 根据 Scheduler 提交的 Assignment 创建 WorkInfo，并在调度条件满足后创建对应的 Work。执行期间，WorkPool 跟踪 Work 的运行状态，并根据并发上限控制执行资源的分配。

字段说明：

* `AssignmentStatus`：记录 Assignment 的调度与执行状态，用于协调任务注册、执行和结束等流程。
* `WorkInfos`：保存 WorkInfo 列表，记录各执行实例的身份、状态、约束和生命周期信息。
* `Works`：保存当前存活的 Work 对象，用于管理正在执行或尚未完成清理的实例。
* `MaximumParallelLimit`：最大并发执行数量，用于限制同时运行的 Work 数量，避免并发任务过多导致资源竞争。

WorkPool 负责 Work 的运行生命周期，不负责生成 Assignment，也不负责聚合或提交 Research State。Work 完成后，其最终 Result 交由 ResultAggregator 处理；Work 清理完成后，WorkInfo 可以继续保留，以支持结果提交确认、状态查询和异常排查。

```go
type WorkPool type{
    AssignmentStatus struct
    WorkInfos []*WorkInfo

    Works []*Work
    MaximumParallelLimit int
}
```

### 8.8 ResultAggregator

ResultAggregator 是研究结果的聚合与提交组件，负责接收 Work 返回的最终 Result，将已到达且尚未提交的结果组织成批次，并提交到 Research State。

ResultAggregator 与 WorkPool、Scheduler 协同工作。Assignment 注册时，需要预留对应的结果接收槽位；Work 完成后，ResultAggregator 根据 AssignmentID 关联结果。提交成功后，ResultAggregator 通知 Scheduler，Scheduler 才能回收对应的 Assignment 记录。

字段与类型说明：

* `PendingSubmissionID`：以 AssignmentID 为基础的类型别名，用于标识等待结果提交的任务关联关系。
* `ResultAggregator`：定义结果聚合组件对外提供的操作接口。

结果提交遵循以下原则：

1. Work 的最终 Result 与 ToolResult 原始数据采用独立的处理链路。
2. ResultAggregator 只聚合已经到达且尚未提交的最终 Result，不因 Assignment 之间存在依赖而阻止提交。
3. 一次批量提交对应一次 Research State Commit；提交成功后更新状态版本并通知 Scheduler。
4. 提交失败时保留必要的结果与关联记录，不能提前回收 Assignment。
5. FindingGraph 根据已提交的结果和 Commit 历史构建，不作为独立的权威数据来源。

ResultAggregator 只负责结果聚合与提交，不直接执行 Work，也不负责调度新的 Assignment。

```go
type PendingSubmissionID assigenmentID

type ResultAggregator interface {
    EventSender
    EventAcceptor
    EventEmitter
    GetResult(ctx context.Context, assignmentID string) error
}
```

### 8.9 Research State

Research State 是每个 RunTask 的研究状态数据结构，负责保存研究输入、研究产物、检索上下文、研究结论以及状态版本等信息。

研究执行过程中，ResultAggregator 将 Work 返回的最终 Result 聚合后提交到 Research State。每次成功 Commit 后，状态版本递增。发生上下文压缩、研究结果更新或用户中止等情况时，当前状态可以根据需要固定下来，形成历史状态，供后续研究上下文引用。

Research State 是研究数据的核心载体。历史状态通过 `RelatedFindings` 建立引用关系，FindingGraph 则负责组织多个 ResearchState，形成可追溯的研究状态图。

#### 8.9.1 ResearchState

ResearchState 描述某一版本的研究状态，包含以下字段：

* `UserInput`：当前研究任务的用户输入，记录研究目标及相关要求。
* `Results`：已提交到研究状态中的研究产物，是后续结论归纳和研究推进的依据。
* `RetrieverContext`：研究过程中使用的检索上下文，为后续研究决策提供相关资料。
* `RelatedFindings`：当前研究状态引用的历史状态集合。当历史状态与当前研究相关时，后续上下文可以引用这些状态，避免重复研究并保留已有研究背景。
* `Finding`：当前研究状态对应的结构化研究结论。
* `ContextLengthLimit`：上下文容量限制，用于判断当前上下文是否需要压缩。
* `TokenUsage`：当前状态固定时记录的 Token 使用量，用于反映该状态对应的上下文规模。
* `Version`：当前 Research State 的版本号。每次成功 Commit 后递增，用于标识状态变化。

ResearchState 的历史状态一旦固定，就应保持不可变。后续研究产生的新状态通过引用历史状态保留研究脉络，避免修改历史状态时影响已有引用。

#### 8.9.2 StateVersion

StateVersion 是 Research State 的版本标识，使用 `uint64` 表示。

每个 RunTask 独立维护状态版本。初始化时版本为零；每次成功 Commit 后递增。同一次批量 Commit 即使包含多个 Result，也只增加一次版本。

Result 到达、Assignment 完成、事件通知以及 Stop 或 Interrupt 本身都不直接增加版本。Commit 失败时，版本保持不变。

#### 8.9.3 FindingGraph

FindingGraph 负责组织同一研究任务中的多个 ResearchState，记录不同状态之间的研究关联。

* `ResearchStates`：图中包含的研究状态节点。
* 状态引用关系：通过 ResearchState 中的历史状态引用表达后续研究对已有状态的依赖。

FindingGraph 是基于已提交的研究数据和历史状态关系构建的派生结构，不替代 Research State 作为权威数据来源。图结构发生丢失或损坏时，应能够根据已持久化的研究数据和历史状态关系重新构建。

#### 8.9.4 ResearchStateStore

ResearchStateStore 负责管理 FindingGraph 的存储与访问，为研究状态的读取、持久化和恢复提供统一入口。

* `FindingGraphs`：当前 Store 管理的 FindingGraph 集合。

每个 RunTask 的研究状态相互隔离。ResearchStateStore 应能够根据对应的研究执行标识定位目标图，并确保已提交的研究状态在 Stop、Interrupt 或 Runtime 异常后仍然可恢复。

#### 8.9.5 FindingStore

FindingStore 是研究状态相关的事件通信接口，嵌入统一事件体系中的三种能力：

* `EventSender`：向其他订阅组件发送事件。
* `EventAcceptor`：接纳进入当前组件的事件。
* `EventEmitter`：将事件发送给已注册的订阅者。

FindingStore 通过统一事件通道参与研究状态相关的协作。事件通信本身不代表研究数据已经成功提交；研究状态的更新与版本推进仍以 Commit 成功为准。

```go

type StateVersion uint64

type ResearchStateStore struct{
    FindingGraphs []*FindingGraph
}

type FindingGraph struct{
    ResearchStates []*ResearchState 
}

type ResearchState struct{
    UserInput           *UserInput
    Results             []*Result
    RetrieverContext    []*RetrieverContext
    RelatedFindings     []*HistoricalState
    Finding             *Finding

    ContextLenthLimit   int64
    TokenUsage          int64
    Version             StateVersion
}

type FindingStore interface {
    EventSender
    EventAcceptor
    EventEmitter
}
```

### 8.10 CheckpointStore

```go
type CheckpointStore interface {
    Save(ctx context.Context, cp *Checkpoint) error
    Load(ctx context.Context, runID string) (*Checkpoint, error)
}
```

### 8.11 Session、Task、RunTask 与 Runtime 生命周期

本项目通过 Session、Task、RunTask 和 Runtime 四个对象管理用户会话、研究任务与具体执行过程。四者职责相互独立，生命周期不完全一致。

核心设计原则是：**Session 决定 Task 的存续，Task 表示持续存在的研究任务，RunTask 记录该任务的研究执行状态，Runtime 承载实际运行所需的资源与组件。**

本文档中的结构和生命周期规则是后续 AI 编码的设计参考。具体实现可以根据 Go 语言特性与 Eino 框架能力调整，但应保持核心语义一致。

#### 8.11.1 Session

Session 是用户侧的会话容器，用于区分不同的研究任务及其多轮交互。

* 创建 Session 时，不立即创建 Task。
* Session 收到第一条用户消息时，同时创建对应的 Task。
* 当前阶段，一个 Session 只关联一个 Task。
* Session 存在期间，其关联的 Task 始终保留，不因研究停止、结束或 Runtime 销毁而删除。
* Session 删除时，进入关联 Task 及其相关数据的清理流程。具体删除范围与持久化策略由后续实现确定。

限制一个 Session 只关联一个 Task，是为了保证多轮对话始终围绕同一个研究任务展开，避免不同研究主题共享上下文后产生混淆。用户希望开展新的独立研究时，应创建新的 Session。

#### 8.11.2 Task

Task 是用户侧的研究任务对象，负责保存任务身份与基本信息，不直接承担研究执行。

* Task 在 Session 的第一条用户消息到达时创建，并与 Session 建立一对一关联。
* 同一 Session 后续收到的消息默认属于同一个 Task 的多轮交互。
* Task 的存续不受某次研究执行的影响。
* Stop、Interrupt、RunTask 结束以及 Runtime 销毁均不直接删除 Task。
* Task 仅在关联 Session 删除时进入删除流程。

Task 负责维持研究任务的连续性，使用户可以在同一个研究任务中多次交互，而不需要为每一轮消息创建新的 Task。

#### 8.11.3 RunTask

RunTask 是 Task 对应的研究执行记录，负责记录执行状态、交互轮次、资源预算等信息，并关联该任务的 Research State。

当前阶段，一个 Task 对应一个 RunTask。RunTask 在首次研究执行启动前创建，后续交互复用该 RunTask，不因每轮消息到达而重新创建。

RunTask 的核心职责包括：

* 记录研究执行状态及其变化。
* 关联当前研究状态与必要的恢复信息。
* 协调多轮研究执行之间的状态衔接。
* 在 Stop 后保留已提交的研究数据，支持后续继续研究。
* 在 Finish 后保留研究执行记录，但不自动启动新的研究轮次。

RunTask 的生命周期与 Runtime 不相同。Runtime 可以销毁并重新初始化，而 RunTask 及其已提交的研究数据继续保留。

#### 8.11.4 Runtime

Runtime 是 RunTask 的实际运行环境，负责组织和协调 SupervisorLoop、Scheduler、WorkPool、ResultAggregator、事件通信组件等运行资源。

Runtime 的主要职责包括：

* 初始化并启动研究执行所需的组件。
* 协调 SupervisorLoop 的决策、中断和恢复。
* 管理 Assignment 与 Work 的执行生命周期。
* 协调结果提交、状态更新及事件通知。
* 在 Stop、Finish 或执行异常时，完成必要的清理与资源释放。

Runtime 是可初始化、可销毁的运行时对象，不是研究数据的权威来源。Runtime 销毁后，已提交的 Research State、RunTask 记录及其他必要的持久化数据仍应保留。

当用户后续发送消息时，Service 根据 Session 找到 Task，再定位 RunTask。若需要继续研究且原 Runtime 已销毁，则根据 RunTask 的最新状态重新初始化 Runtime；若原执行仍处于有效状态，则按对应状态规则处理，不允许新一轮执行与尚未清理完成的旧执行发生冲突。

#### 8.11.5 RunStatus 与生命周期规则

RunTask 使用四种状态描述整体研究执行状态：

* **Running**：研究正常执行，当前不存在等待处理的局部中断或相关审批。
* **Interrupted**：研究执行存在需要处理的局部中断，或正在等待相关审批。
* **Stopped**：用户主动中止了当前研究执行。当前执行流程停止，但 Task、RunTask 和已提交的研究数据保留，后续消息可以继续进入。
* **Finish**：研究已经结束，不再自动续跑当前研究。

状态转换遵循以下规则：

| 当前状态                  | 事件              | 后续状态        |
| --------------------- | --------------- | ----------- |
| Running               | 用户 Stop         | Stopped     |
| Running               | 出现需要处理的局部中断     | Interrupted |
| Interrupted           | 中断处理完成，研究继续     | Running     |
| Stopped               | 新消息通过准入并启动新一轮执行 | Running     |
| Running / Interrupted | 研究确认结束          | Finish      |

`Stopped` 与 `Finish` 的语义不同。Stop 只停止当前执行过程，不永久关闭研究任务；Finish 则表示当前研究已经结束。

当 RunTask 进入 Finish 状态时，表示当前研究执行已经结束，Supervisor、Scheduler、Work、ResultAggregator 等运行时实体应完成各自的收尾工作并释放相关运行资源。但这不意味着研究数据生命周期结束：ResearchState 及其已提交的研究数据仍需持续保留，以支持后续查询、历史状态引用和研究结果追溯。

当 RunTask 处于 Finish 时，不自动恢复原研究。若用户希望开展新的独立研究，应创建新的 Session 和 Task，而不是将原 Task 用于另一个研究主题。

#### 8.11.6 典型生命周期流程

1. 用户创建 Session。
2. 用户发送第一条消息，Service 创建 Task，并建立 Session 与 Task 的关联。
3. Service 创建 RunTask，初始化必要的研究状态，并准备启动 Runtime。
4. Runtime 初始化研究执行组件，进入 Running 状态。
5. 用户继续发送消息时，Service 根据 Session 定位已有 Task 和 RunTask，交由对应 Runtime 处理。
6. 用户主动 Stop 时，Runtime 停止新任务启动、协调现有 Work 清理，并保留已提交的研究数据；执行清理完成后，RunTask 进入 Stopped，Runtime 可以销毁。
7. 用户在 Stopped 状态下发送新消息时，Service 复用原 Task 和 RunTask。通过准入后，重新初始化或恢复所需的运行环境，并将 RunTask 转为 Running。
8. 如果研究出现需要处理的局部中断，RunTask 进入 Interrupted。中断处理完成后可恢复 Running；如果研究确认结束，则进入 Finish。
9. RunTask 进入 Finish 后，释放 Runtime 运行资源，但保留 Task、RunTask 记录和必要的研究数据。
10. Session 删除时，进入关联 Task 及其数据的删除流程；必须先确保相关运行资源得到安全清理。

### 8.12 Work、WorkInfo 与 Assignment 的生命周期

#### 8.12.1 Assignment 创建与登记

Supervisor 生成 Assignment，并交由 Scheduler 登记。

Scheduler 登记 Assignment 时，需要先向 ResultAggregator 预留对应的结果接收槽位。只有预留成功，Assignment 才能进入可调度状态。

Assignment 在等待调度、执行、结果提交期间持续存在，不与某个 Work 实例绑定为相同生命周期。

#### 8.12.2 WorkInfo 建立与 Work 创建

WorkPool 准备执行 Assignment 时，先建立本次执行对应的 WorkInfo，再创建 Work 实例。

WorkInfo 用于记录该次执行的身份、状态、约束、开始时间、结束时间及 Attempt 等信息。

Work 创建后，持有本次执行所需的 Context、WorkAgent、Assignment 和 WorkInfo，并负责执行 SubAgent。

一个 Assignment 可以对应多次执行尝试。每次尝试创建独立的 Work 和对应的 WorkInfo，并通过稳定的 AssignmentID 关联同一项研究工作。

#### 8.12.3 WorkInfo：共享执行状态与并发协调

WorkInfo 不仅用于记录 Work 的执行历史，还作为 Scheduler 与 ResultAggregator 之间共享的执行状态记录。

WorkInfo 的生命周期独立于 Work：它在 Work 创建前或创建时建立，在 Work 销毁后继续保留一段时间，用于执行状态查询、结果提交协调、故障排查与历史追溯。

##### 8.12.3.1 共享状态职责

Scheduler 与 ResultAggregator 可以通过 WorkInfo 获取同一项 Assignment 对应的执行进度和结果提交状态，避免双方分别维护相互矛盾的状态副本。

WorkInfo 至少需要能够关联 AssignmentID、WorkID，并记录本次执行的状态、执行时间、Attempt 等信息。为协调执行与结果提交，还应能够表达必要的结果接收、提交或取消状态。

具体字段可以根据 Go 和 Eino 的实际实现调整，不要求严格遵循某个固定结构体。

##### 8.12.3.2 并发安全

WorkInfo 是多个组件可能同时访问的共享状态，因此必须具备并发保护机制。

如果 WorkInfo 由多个 goroutine 并发读写，可以使用 sync.Mutex 或 sync.RWMutex 保护共享字段。也可以采用集中式状态管理、原子操作或其他能够提供等价一致性保证的实现。

并发保护不仅需要防止数据竞争，还需要保证相关状态转换的原子性。例如，不能出现 Scheduler 已将 Assignment 标记为可回收，而 ResultAggregator 仍在处理该 Assignment 的未提交 Result 的情况。

所有涉及状态转换的操作应通过受控方法完成，不应允许不同组件绕过状态管理直接修改共享字段。

##### 8.12.3.3 状态写入边界

WorkInfo 可以作为共享状态栏，但不同组件应具有明确的状态写入职责：

* WorkPool 负责更新 Work 创建、启动、执行结束与资源清理等执行生命周期信息。
* Scheduler 负责更新 Assignment 的排队、调度、取消和回收等调度信息。
* ResultAggregator 负责更新 Result 接收、聚合、Commit 成功或失败等结果处理信息。
* 各组件可以读取协调流程所需的共享状态，但不能仅根据单个字段推断整个流程已经完成。

上述状态可以存储在一个 WorkInfo 中，也可以拆分为职责明确的内部状态结构；关键是它们必须具有一致的关联关系，并且跨组件的状态转换不能产生矛盾。

##### 8.12.3.4 状态一致性原则

Work 执行结束不代表 Result 已提交，Result 提交成功也不代表所有清理工作已经完成。

Assignment 是否可以回收，必须由 Scheduler 根据 ResultAggregator 的提交确认及当前取消状态等必要条件综合决定，不能仅根据 Work 的执行状态判断。

WorkInfo 的并发锁只负责保护共享数据和受控状态转换，不能代替 ResultAggregator 的可靠提交机制，也不能保证已经确认提交的数据在进程崩溃后仍然存在。持久化与恢复仍由相应的基础设施负责。

#### 8.12.4 Work 执行结束

Work 执行结束后，将最终 Result 交给 ResultAggregator，并释放自身持有的运行资源。

Work 执行结束不代表 Assignment 已完成，也不代表 Result 已成功提交到 ResearchState。

WorkInfo 在 Work 销毁后继续保留一段时间，以便查询执行状态、排查错误及追溯执行历史。

#### 8.12.5 Result 提交与 Assignment 生命周期结束

ResultAggregator 接收到最终 Result 后，负责聚合结果并提交 ResearchState。

只有在 Commit 成功后，ResultAggregator 才通知 Scheduler 回收对应的 Assignment。

如果 Commit 失败，Assignment 不得仅因为 Work 已经结束而提前回收。系统应保留必要的结果与提交状态，以便进行错误处理或恢复。

如果 Assignment 被取消，Scheduler 应通知 ResultAggregator 取消对应的结果接收槽位，并协调 WorkPool 清理相关 Work。已经提交的研究数据不因取消而被删除。

#### 8.12.6 生命周期原则

* Assignment 管理一项研究工作的调度与完成状态。
* Work 承载一次实际执行，结束后释放运行资源。
* WorkInfo 记录一次执行的生命周期，其存续时间长于 Work。
* Work 结束、Result 提交成功、Assignment 回收是三个不同的事件，不能合并为同一个状态转换。
* Assignment 的最终状态与每次 Work 的执行状态分别维护，避免将单次执行失败直接等同于整个研究工作的失败。

### 8.13 ResearchState 的生命周期与初始上下文构建

#### 8.13.1 生命周期归属

ResearchState 是 Task 持有的持久研究状态对象，其生命周期与 Task 绑定，而不是与 Runtime、SupervisorLoop 或某次研究执行绑定。

* 创建 Task 时，同时创建对应的 ResearchState，并初始化必要字段。
* Task 存续期间，ResearchState 持续存在，保存用户输入、已提交研究结果、Finding 及相关历史状态。
* Runtime、SupervisorLoop、Scheduler、Work 或 ResultAggregator 销毁时，不得因此删除 ResearchState。
* RunTask 进入 Stopped、Interrupted 或 Finish 状态时，ResearchState 均继续保留。
* 只有用户显式删除 Session 或 Task，并由相应删除流程确认需要删除关联数据时，才删除 ResearchState。

删除 Session 与删除 Task 的关联删除规则应保持一致，避免出现 Task 已删除但 ResearchState 仍然孤立存留，或执行实例结束时误删研究数据的情况。

#### 8.13.2 SupervisorLoop 的初始上下文构建

SupervisorLoop 创建时，统一读取 ResearchState 中的 UserInput 字段，使用它构建初始运行上下文。

由于 Task 创建时已经创建 ResearchState，且其中包含初始 UserInput，因此不需要额外判断当前是首次运行还是中间轮次，也不需要为首次运行单独设计上下文初始化分支。

SupervisorLoop 的初始上下文构建与后续上下文刷新采用统一的上下文管理机制。具体上下文应由 MemoryManager 根据 ResearchState 的当前内容进行构建或压缩；UserInput 是初始上下文的基础，而不是唯一允许使用的上下文内容。

#### 8.13.3 数据与执行状态分离

ResearchState 负责持久研究数据及其版本管理；Runtime 和 SupervisorLoop 负责当前执行过程。

Runtime 重建时，应从保留的 ResearchState 恢复必要的研究上下文，而不是依赖旧 Runtime 实例的内存状态。

ResearchState 的历史快照与版本数据应遵循既定的不可变性约束。删除 ResearchState 时，还需要按照存储实现清理或归档其关联的历史状态、FindingGraph 派生数据及其他关联记录。

核心原则：ResearchState 的生命周期由 Task 的存续决定，研究执行组件的生命周期不得反向决定 ResearchState 的存续。

---