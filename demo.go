// 内置演示：把一段乱序事件喂给引擎，验证稳定状态、唯一告警与证据可解释。
package main

import (
	"encoding/json"
	"fmt"
	"time"
)

func RunDemo() error {
	log, err := NewEventLog("")
	if err != nil {
		return err
	}
	eng := NewEngine(log)

	base := time.Date(2026, 9, 22, 23, 50, 0, 0, time.UTC) // 跨午夜场次起点
	sid := "final-2026-09-22"
	t := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }

	snap := func(vid, name string, level VenueLevel, cap int, min int) Event {
		raw, _ := json.Marshal(VenueSnapshot{Name: name, Level: level, Capacity: cap, Entrances: 2})
		return Event{EventID: "ev_snap_" + vid, Type: EvVenueSnapshot, SessionID: sid, VenueID: vid,
			OccurredAt: t(min), ReceivedAt: t(min), Payload: raw}
	}

	var batch []Event
	add := func(e Event) { batch = append(batch, e) }

	add(snap("core-plaza", "中心广场", LevelCore, 1000, -10))
	add(snap("coord-mall", "城西商圈", LevelCoord, 800, -10))
	add(snap("backup-culture", "河滨文化空间", LevelBackup, 500, -10))

	// core-plaza 设备 g1：序号 1..6（跨午夜 23:5x → 00:0x）
	add(demoDelta(sid, "core-plaza", "g1", 1, 600, 0, -5, false))
	add(demoDelta(sid, "core-plaza", "g1", 2, 200, 10, -2, false))
	add(demoDelta(sid, "core-plaza", "g1", 3, 120, 5, 0, false)) // 跨午夜
	add(demoDelta(sid, "core-plaza", "g1", 4, 130, 5, 2, true))  // 迟到数据
	// g1 序号 5 暂时缺失（设备失联），先补序号 6
	add(demoDelta(sid, "core-plaza", "g1", 6, 40, 200, 6, false))
	// 序号 5 迟到补传
	add(demoDelta(sid, "core-plaza", "g1", 5, 30, 40, 4, true))

	// 计数修正：现场核验后打锚点，此前累积被取代
	anchorRaw, _ := json.Marshal(CountAnchor{Occupancy: 930, Reason: "闸机核验"})
	add(Event{EventID: "ev_anchor1", Type: EvCountAnchor, SessionID: sid, VenueID: "core-plaza",
		DeviceID: "g1", Seq: 7, OccurredAt: t(7), ReceivedAt: t(7), Payload: anchorRaw})
	add(demoDelta(sid, "core-plaza", "g1", 8, 20, 0, 8, false)) // 930+20=950 红区

	// 同设备重复上报：完全相同的 eventId 再来一次
	dup := demoDelta(sid, "core-plaza", "g1", 8, 20, 0, 8, false)
	add(dup)

	// coord-mall 已有部分客流（导流承接能力受限）
	add(demoDelta(sid, "coord-mall", "c1", 1, 300, 0, -3, false))
	add(demoDelta(sid, "coord-mall", "c1", 2, 100, 0, 5, false)) // 400/800 可承接

	// 个人标识字段必须被拒绝
	badRaw, _ := json.Marshal(map[string]any{"enter": 10, "手机号": "13800000000"})
	bad := Event{EventID: "ev_bad_pii", Type: EvCountDelta, SessionID: sid, VenueID: "coord-mall",
		DeviceID: "c1", Seq: 3, OccurredAt: t(6), ReceivedAt: t(6), Payload: badRaw}

	// 内容授权：23:55–00:05，之后过期
	grantRaw, _ := json.Marshal(AuthzGrant{ContentID: "feed-A", Start: t(-5), End: t(5)})
	add(Event{EventID: "ev_grant", Type: EvAuthzGranted, SessionID: sid, VenueID: "core-plaza",
		OccurredAt: t(-5), ReceivedAt: t(-5), Payload: grantRaw})

	// —— 打乱提交顺序：把较晚的事件排到前面 ——
	shuffled := make([]Event, len(batch))
	copy(shuffled, batch)
	for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	for _, e := range shuffled {
		if err := e.Validate(t(20)); err != nil {
			return fmt.Errorf("演示数据校验失败: %w", err)
		}
		if _, err := eng.appendExternal(e); err != nil {
			return err
		}
	}
	if err := bad.Validate(t(20)); err == nil {
		return fmt.Errorf("个人标识字段未被拦截")
	} else {
		fmt.Printf("✔ 隐私拦截：%v\n", err)
	}
	if _, err := eng.appendExternal(dup); err != nil {
		return err
	} // 第二次追加同 eventId，应判重复

	// 在 t=8 生成自动建议（应为导流到 coord-mall / backup-culture）
	prop, err := eng.AutoPropose(sid, "core-plaza", t(8))
	if err != nil {
		return err
	}
	if prop == nil {
		return fmt.Errorf("应在红区生成导流建议")
	}
	fmt.Printf("✔ 自动建议：%s %s，去向 %d 个，证据 %d 条\n", prop.Kind, prop.DirectiveID, len(prop.Targets), len(prop.Evidence))

	// 运营接受（审计）
	if err := eng.Decide(sid, prop.DirectiveID, DecisionAccept, "调度员-林", "按建议分流", t(9)); err != nil {
		return err
	}
	// 重复/相反决策必须被拒绝
	if err := eng.Decide(sid, prop.DirectiveID, DecisionReject, "调度员-林", "", t(9)); err == nil {
		return fmt.Errorf("不可变决策被覆盖")
	} else {
		fmt.Printf("✔ 决策不可变：%v\n", err)
	}

	// 导流后 coord-mall 入场速率显著抬升并进入红区 → 归因（重复提交验证幂等）
	post := []Event{
		demoDelta(sid, "coord-mall", "c1", 3, 200, 0, 10, false),
		demoDelta(sid, "coord-mall", "c1", 4, 220, 0, 11, false), // 累计 820/800 触发新拥堵
	}
	for _, e := range post {
		if _, err := eng.appendExternal(e); err != nil {
			return err
		}
	}
	for _, e := range post {
		if accepted, err := eng.appendExternal(e); err != nil || accepted {
			return fmt.Errorf("重复上报应判为幂等重复")
		}
	}

	// 管制事件：core-plaza 暂停入场 + 分发中止，随后解除
	// 分流见效：管制期间离场 100 人，解除后占用 850/1000 已回绿区
	add2 := demoDelta(sid, "core-plaza", "g1", 9, 0, 100, 14, false)
	if _, err := eng.appendExternal(add2); err != nil {
		return err
	}
	incRaw, _ := json.Marshal(Incident{Priority: PriControl, Active: true, Note: "临时安保管制", Source: "安保指挥部"})
	inc := Event{EventID: "ev_inc1", Type: EvIncident, SessionID: sid, VenueID: "core-plaza",
		OccurredAt: t(12), ReceivedAt: t(12), Payload: incRaw}
	if _, err := eng.appendExternal(inc); err != nil {
		return err
	}
	relRaw, _ := json.Marshal(Incident{Priority: PriNormal, Active: false, Note: "管制解除"})
	rel := Event{EventID: "ev_rel1", Type: EvIncident, SessionID: sid, VenueID: "core-plaza",
		OccurredAt: t(16), ReceivedAt: t(16), Payload: relRaw}
	if _, err := eng.appendExternal(rel); err != nil {
		return err
	}

	// 授权闸门：窗口内允许；过期后（t=6）拒绝并记录
	p := eng.Project(sid, t(0))
	if d := p.Authorize("core-plaza", "feed-A", t(0)); !d.Allowed {
		return fmt.Errorf("授权窗口内应允许分发: %s", d.Reason)
	}
	p2 := eng.Project(sid, t(6))
	d2 := p2.Authorize("core-plaza", "feed-A", t(6))
	if d2.Allowed {
		return fmt.Errorf("授权过期后仍允许分发")
	}
	fmt.Printf("✔ 授权闸门：%s\n", d2.Reason)
	if err := eng.LogEnforcement(sid, "core-plaza", "feed-A", d2.Reason, t(6)); err != nil {
		return err
	}

	// —— 汇总输出 ——
	final := eng.Project(sid, t(18))
	summary := final.Summary()
	out, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(out))

	// 控制台核对要点
	if n := countActiveAlarms(summary); n != 1 {
		return fmt.Errorf("期望恰好 1 个活动告警（coord-mall 拥堵），实际 %d", n)
	}
	fmt.Printf("✔ 唯一活动告警数：%d\n", countActiveAlarms(summary))
	if len(summary.Attributions) != 1 {
		return fmt.Errorf("期望 1 条导流归因，实际 %d", len(summary.Attributions))
	}
	fmt.Printf("✔ 导流归因：%s\n", summary.Attributions[0].Note)
	if v := findVenue(summary, "core-plaza"); v.Occupancy != 850 {
		return fmt.Errorf("锚点修正+分流后占用应为 850，实际 %d", v.Occupancy)
	}
	fmt.Println("✔ 锚点修正后 core-plaza 占用为 930+20-100=850（乱序/迟到/重复/修正均已归并）")
	fmt.Println("演示通过")
	return nil
}

var demoBase = time.Date(2026, 9, 22, 23, 50, 0, 0, time.UTC)

func demoDelta(sid, vid, dev string, seqVal int64, enter, leave, min int, late bool) Event {
	raw, _ := json.Marshal(CountDelta{Enter: enter, Leave: leave})
	occ := demoBase.Add(time.Duration(min) * time.Minute)
	rcv := occ
	if late {
		rcv = occ.Add(5 * time.Minute)
	}
	return Event{
		EventID: fmt.Sprintf("ev_%s_%s_%d", vid, dev, seqVal), Type: EvCountDelta,
		SessionID: sid, VenueID: vid, DeviceID: dev, Seq: seqVal,
		OccurredAt: occ, ReceivedAt: rcv, Payload: raw,
	}
}

func countActiveAlarms(s SummaryView) int {
	n := 0
	for _, a := range s.Alarms {
		if a.Active {
			n++
		}
	}
	return n
}

func findVenue(s SummaryView, id string) VenueView {
	for _, v := range s.Venues {
		if v.VenueID == id {
			return v
		}
	}
	return VenueView{}
}
