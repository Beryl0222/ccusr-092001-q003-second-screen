# 城市第二现场客流联动

为大型赛事期间的商圈、文化空间与公共广场“第二现场”提供统一的点位状态口径：
接收容量快照、入离场聚合计数、内容授权时段、突发管制事件与通知回执，
在**不接触任何单个观众身份**的前提下，发布分区热度、导流建议、暂停入场指令
与内容停发指令。

`domain.json` 描述点位等级（核心/协同/备用）、事件优先级（普通/拥挤/管制/
紧急疏散）与通知回执（已接收/已执行/已驳回/已过期），代码枚举与其严格对齐。

## 架构

```
采集端(闸机/相机/运营台)
   │  JSON 事件信封（仅聚合值，PII 在入口被拒绝）
   ▼
internal/model      事件模型 + 校验（枚举、时间、PII 递归扫描）
   ▼
internal/store      只追加事件日志：event_id 幂等去重、全局版本号、WAL 落盘、按版本恢复
   ▼
internal/engine     纯函数 Fold(events, asOf)：确定性折叠，状态只取决于事件内容
   ▼
internal/service    编排（入库 / 全量快照 / 增量恢复 / 离线重放）
   ▼
internal/api        HTTP 接口
```

**为什么乱序重放能得到稳定状态**：折叠与“服务端接收顺序”解耦，事件按
`(occ_time, event_id)` 全序排列后归约。同一事件集合无论按原序、逆序还是
洗牌输入，输出逐字节一致；服务端版本号只作为失联恢复的水位线。

## 关键机制

| 需求 | 处理方式 |
| --- | --- |
| 迟到数据 | `occ_time ≤ as_of` 即纳入；迟到批次在依据中标记 `late=true`，且只有近窗内到达才影响当前质量标记 |
| 计数修正 | `corrects_event_id` 指向的旧批次整体作废，修正批次按旧批次发生时刻落位，占用重建不跳变 |
| 同一设备重复上报 | 存储层按 `event_id` 幂等；引擎再按 `(device_id, seq)` 拦截换 ID 重发与序号回退 |
| 跨午夜场次 | 一切状态以 `session_id` 隔离；`clear_session` 清零动态数据（可保留配置），审计/回执不清 |
| 中心失联恢复 | 边缘携带本地版本号调用 `/v1/replay/stream?since=N` 拉缺口；事件日志 WAL 重启重建索引 |
| 占用权威锚点 | 取最新权威快照为锚，仅对其后的有效增量求和；超容量截断展示并记录异常 |
| 数据质量 | `fresh / late_arrivals / stale_gap / recovered / stale`；陈旧时抑制基于计数的建议，管制指令不被抑制 |
| 唯一告警 | 键为 `session|venue|code#序号`；在途重复只更新不新增，解除后再发生产生新序号 |
| 运营处置审计 | 接受/驳回本身作为 `decision_ack` 事件入同一条只追加日志，不可篡改；迟到处置标记 `orphaned` |
| 授权过期停发 | 授权窗口外（过期或提前撤销）生成确定性 `stop_order`，汇总接口直接下发 |
| 隐私 | 姓名/手机号/证件号/人脸/硬件标识等字段与可疑值在入口递归扫描拒绝；只接受聚合计数 |

每条建议都冻结其**数据依据**：`evidence.used`（采用了哪些快照/增量/事件，
含迟到标记）与 `evidence.excluded`（排除了什么、为什么），并有确定性
`data_hash`；`decision_id` 由场次、点位、动作、目标与依据指纹派生，可复现核对。

## HTTP 接口

| 方法/路径 | 说明 |
| --- | --- |
| `POST /v1/events` | 上报单条事件；重复 `event_id` 返回 `duplicate:true` 且不重复入库 |
| `GET  /v1/summary?session=&as_of=` | 分区热度、占用率、建议、告警、停发指令总览 |
| `GET  /v1/venues/{id}` | 单点位完整状态（含质量说明、异常） |
| `GET  /v1/decisions/{id}` | 解释一次判断用了哪些有效数据、排除了什么 |
| `POST /v1/decisions/{id}/ack` | 运营接受/驳回（写入不可变审计） |
| `GET  /v1/alerts` / `/v1/content` / `/v1/receipts` / `/v1/audit` | 各台账 |
| `GET  /v1/replay/stream?since=N` | 失联增量恢复：版本 > N 的事件与最新水位 |
| `GET  /health` | 巡检 |

事件信封示例：

```json
{
  "event_id": "cnt-1002", "type": "count_report",
  "venue_id": "plaza-core", "device_id": "gate-core-1",
  "session_id": "final-2026", "seq": 17,
  "occ_time": "2026-09-22T18:35:00Z",
  "content": {"enter": 120, "exit": 8, "confidence": 95}
}
```

支持的 `type`：`venue_register`、`count_report`（可带 `gates` 分项与
`corrects_event_id` 修正）、`capacity_snapshot`、`incident`、
`content_authorization`、`receipt`、`decision_ack`、`heartbeat`、
`clear_session`。

## 运行与验收

```bash
go run . -check                 # 配置自检
go test ./...                   # 单元/集成测试
go run . -addr :8000 -data data # 启动服务（-data= 为纯内存模式）

# 乱序事件重放验收（内置 5 个评估检查点）
go run . -replay testdata/scenario_out_of_order.jsonl
```

`testdata/scenario_out_of_order.jsonl` 是故意打乱顺序的 31 个事件，覆盖：
迟到修正、同设备重发、管制发生/重复/解除/再发、设备失联与恢复、授权过期与
撤销、回执全生命周期、跨午夜第二场次及其清理。重放工具会自动校验：

1. **稳定性**：逆序 / 原序 / 固定种子洗牌三种输入在每个检查点结果逐字节一致；
2. **唯一告警**：同一事件码在途不重复，解除后再发生递增序号；
3. **失联恢复**：边缘从水位 15 增量补齐 16 条后，折叠结果与中心一致；
4. **幂等**：全部事件重投一次均识别为重复，状态不变；
5. **可解释**：打印每条建议采用/排除的具体事件及原因、依据指纹。

重放文件每行是 `{"received_at":..., "event":{...}}`，也可用
`{"checkpoint":"RFC3339"}` 声明评估检查点；命令行 `-asof` 可指定单一时刻。

## 口径配置（`internal/engine.DefaultConfig`）

- 导流阈值 75%，暂停入场阈值 90%；
- 计数新鲜窗口 60s，数据缺口阈值 180s，不可采信阈值 300s；
- 迟到判定：接收晚于发生 60s；
- 失联恢复标记保留 5 分钟。

阈值可按赛事预案调整；数据存在缺口或刚恢复时，建议置信度自动下调并附加
“人工复核”说明。
