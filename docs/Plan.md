## 17 MVP 路线

# Go DeepResearch Agent：MVP 开发路线

## 阶段 0：工程骨架与接口打通

首先完成

目标：项目能启动、能接收请求、能调用模型。

* 初始化 Go 工程、配置管理、日志与错误处理。

* 使用 Hertz 实现 HTTP API。

* 接入 Eino 的模型调用能力。

* 实现最小 SSE 输出，把模型结果返回前端。

* 先建立 Session、Task 的基本数据结构与存储接口。

  验收：发送一条请求，服务调用模型并通过 SSE 返回结果。

## 阶段 1：单 Agent 研究闭环

目标：先证明研究流程真的成立。

* 实现 SupervisorLoop，负责多轮研究决策。

* 实现最小 Assignment 和单个 Work。

* 接入一个可用的搜索工具或 MCP 工具。

* Work 执行研究后返回 Result。

* ResultAggregator 将结果提交到 ResearchState。

* Supervisor 根据新结果决定继续研究或结束。

  验收：输入研究问题，系统能搜索资料、分析结果、继续研究并给出最终回答。

## 阶段 2：持久化与生命周期

目标：运行环境销毁后，研究数据仍然存在。

* 实现 Session、Task、RunTask、ResearchState 的持久化。

* Task 创建时初始化 ResearchState。

* 实现 RunTask 状态与 Runtime 初始化、销毁流程。

* 实现 Result Commit、StateVersion 和失败处理。

* 支持服务重启后读取已有研究状态。

  验收：研究执行后重启服务，仍能查询已提交结果；Runtime 重建不会重复创建 Task 或覆盖已有研究数据。

## 阶段 3：动态调度与并发执行

目标：从单个 Work 发展为多个动态研究单元。

* 实现 Scheduler、WorkPool、WorkInfo。

* 支持多个 Assignment 排队与有限并发执行。

* WorkInfo 提供并发安全的执行状态记录。

* ResultAggregator 接收多个 Work 的最终 Result。

* Commit 成功后才允许 Scheduler 回收对应 Assignment。

  验收：Supervisor 创建多个研究方向，多个 Work 并发运行，结果能够正确聚合，不发生重复提交或提前回收。

## 阶段 4：事件、中断与恢复

目标：系统能被安全停止、处理失败并恢复。

* 建立统一事件接口和内存事件队列。

* 关键状态具备持久化或可恢复依据。

* 实现错误分类、超时处理与审批流程。

* 实现 Work 协作式取消和 RunTask 全局 Stop。

* 处理 Commit 失败、Runtime 重建和重复事件。

  验收：Stop 后不再启动新 Work；已提交数据保留；恢复时不会出现旧 Work 与新 Work 同时修改研究状态。

## 阶段 5：研究质量与可观测性

目标：让项目具备可展示、可评估的工程价值。

* 构建 FindingGraph 派生视图。

* 完善 MemoryManager 与上下文压缩。

* 增加运行事件、耗时、错误率、模型调用次数等指标。

* 建立研究结果质量评测集。

* 展示研究进度、子任务状态、来源与最终结论。

  验收：能够展示完整研究轨迹，并通过固定测试集对研究质量和运行成本进行对比。

## 第一版到底做什么、不做什么？

这张表是为了防止 MVP 再次扩张。

| 模块                   | 第一版策略                      |
| -------------------- | -------------------------- |
| Hertz + Eino         | 必须实现                       |
| SupervisorLoop       | 必须实现最小研究循环                 |
| Assignment / Work    | 必须实现，但先串行执行                |
| Scheduler / WorkPool | 先用简单版本，阶段 3 再扩展并发          |
| ResearchState        | 必须实现，先保障结果正确提交             |
| ResultAggregator     | 必须实现最小 Commit 闭环           |
| 事件系统                 | 先用直接调用或最小事件接口，阶段 4 再完善统一分发 |
| WorkInfo             | 先记录基本执行状态，阶段 3 引入完整并发协调    |
| FindingGraph         | 延后                         |
| MemoryManager 压缩     | 延后                         |
| Checkpoint / 复杂恢复    | 先保证持久化数据可恢复，阶段 4 再完善执行级恢复  |
| Auth、限流、熔断           | 暂缓                         |
| 沙箱、进程隔离              | 暂缓                         |
| 分布式 Scheduler        | 不纳入 MVP                    |

## MVP 的开发顺序与依赖关系

![开发阶段规划]

虽然图上是线性阶段，但实际开发时，阶段 1 就应该保留必要的 `AssignmentID`、`RunTaskID` 和 `StateVersion` 等关联信息，避免后续重写。只是暂时不需要实现完整的并发协议、事件恢复和复杂调度机制。

---
