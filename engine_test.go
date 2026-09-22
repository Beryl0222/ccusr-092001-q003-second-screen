package main

import (
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	log, err := NewEventLog("")
	if err != nil {
		t.Fatal(err)
	}
	return NewEngine(log)
}

func mustAppend(t *testing.T, eng *Engine, e Event) bool {
	t.Helper()
	ok, err := eng.appendExternal(e)
	if err != nil {
		t.Fatalf("追加事件失败: %v", err)
	}
	return ok
}

func at(min int) time.Time {
	return time.Date(2026, 9, 22, 23, 50, 0, 0, time.UTC).Add(time.Duration(min) * time.Minute)
}

func snapEvent(sid, vid string, level VenueLevel, cap, min int) Event {
	raw, _ := json.Marshal(VenueSnapshot{Name: vid, Level: level, Capacity: cap})
	return Event{EventID: "snap-" + vid, Type: EvVenueSnapshot, SessionID: sid, VenueID: vid,
		OccurredAt: at(min), ReceivedAt: at(min), Payload: raw}
}

func deltaEvent(sid, vid, dev string, seq int64, enter, leave, min int) Event {
	raw, _ := json.Marshal(CountDelta{Enter: enter, Leave: leave})
	return Event{EventID: "d-" + vid + "-" + dev + "-" + strconv.FormatInt(seq, 10), Type: EvCountDelta,
		SessionID: sid, VenueID: vid, DeviceID: dev, Seq: seq,
		OccurredAt: at(min), ReceivedAt: at(min + 3), Payload: raw}
}

func anchorEvent(sid, vid, dev string, seq int64, occ, min int, reason string) Event {
	raw, _ := json.Marshal(CountAnchor{Occupancy: occ, Reason: reason})
	return Event{EventID: "a-" + vid + "-" + dev + "-" + strconv.FormatInt(seq, 10), Type: EvCountAnchor,
		SessionID: sid, VenueID: vid, DeviceID: dev, Seq: seq,
		OccurredAt: at(min), ReceivedAt: at(min), Payload: raw}
}

func incidentEvent(sid, vid, id string, pri Priority, active bool, min int) Event {
	raw, _ := json.Marshal(Incident{Priority: pri, Active: active})
	return Event{EventID: id, Type: EvIncident, SessionID: sid, VenueID: vid,
		OccurredAt: at(min), ReceivedAt: at(min), Payload: raw}
}

// 验收1：乱序/迟到/重复/修正折叠后状态稳定，且与顺序提交完全一致。
func TestOutOfOrderReplayStable(t *testing.T) {
	sid := "S1"
	build := func(eng *Engine) {
		evs := []Event{
			snapEvent(sid, "A", LevelCore, 100, -10),
			snapEvent(sid, "B", LevelCoord, 500, -10),
			deltaEvent(sid, "A", "g", 1, 50, 0, -5),
			deltaEvent(sid, "A", "g", 2, 60, 0, -3),
			deltaEvent(sid, "A", "g", 3, 0, 10, -1), // 跨午夜前
			deltaEvent(sid, "A", "g", 4, 10, 0, 1),  // 跨午夜后
			anchorEvent(sid, "A", "g", 5, 95, 2, "人工核验"),
			deltaEvent(sid, "A", "g", 6, 5, 0, 3), // 95+5=100 红区
			deltaEvent(sid, "B", "c", 1, 30, 0, -4),
			incidentEvent(sid, "A", "inc1", PriControl, true, 4),
			incidentEvent(sid, "A", "rel1", PriNormal, false, 6),
		}
		for _, e := range evs {
			mustAppend(t, eng, e)
		}
	}

	eng1 := newTestEngine(t)
	build(eng1)

	eng2 := newTestEngine(t)
	// 逆序追加，模拟全部乱序到达。
	evs := []Event{
		incidentEvent(sid, "A", "rel1", PriNormal, false, 6),
		incidentEvent(sid, "A", "inc1", PriControl, true, 4),
		deltaEvent(sid, "B", "c", 1, 30, 0, -4),
		deltaEvent(sid, "A", "g", 6, 5, 0, 3),
		anchorEvent(sid, "A", "g", 5, 95, 2, "人工核验"),
		deltaEvent(sid, "A", "g", 4, 10, 0, 1),
		deltaEvent(sid, "A", "g", 3, 0, 10, -1),
		deltaEvent(sid, "A", "g", 2, 60, 0, -3),
		deltaEvent(sid, "A", "g", 1, 50, 0, -5),
		snapEvent(sid, "B", LevelCoord, 500, -10),
		snapEvent(sid, "A", LevelCore, 100, -10),
	}
	for _, e := range evs {
		mustAppend(t, eng2, e)
	}

	s1 := eng1.Project(sid, at(8)).Summary()
	s2 := eng2.Project(sid, at(8)).Summary()
	b1, _ := json.Marshal(s1)
	b2, _ := json.Marshal(s2)
	if !reflect.DeepEqual(s1, s2) {
		t.Fatalf("乱序重放状态不一致：\n顺序=%s\n乱序=%s", b1, b2)
	}

	// 同一时刻在两个引擎上生成建议，ID 与证据也必须一致。
	p1, err := eng1.AutoPropose(sid, "A", at(3))
	if err != nil || p1 == nil {
		t.Fatalf("应生成建议: %v %v", p1, err)
	}
	p2, err := eng2.AutoPropose(sid, "A", at(3))
	if err != nil || p2 == nil {
		t.Fatalf("乱序引擎应生成相同建议: %v", err)
	}
	if p1.DirectiveID != p2.DirectiveID {
		t.Fatalf("建议ID不稳定: %s vs %s", p1.DirectiveID, p2.DirectiveID)
	}
	if !reflect.DeepEqual(p1.Evidence, p2.Evidence) {
		t.Fatalf("证据谱系不一致")
	}

	// 占用：锚点 95 + 序号6 的 +5 = 100（序号1-4 已被锚点取代）。
	var occA int
	for _, v := range s2.Venues {
		if v.VenueID == "A" {
			occA = v.Occupancy
		}
	}
	if occA != 100 {
		t.Fatalf("锚点修正后占用应为100，实际 %d", occA)
	}
}

// 验收2：同一设备重复上报（同 eventId 或同序号）只计一次。
func TestDuplicateReports(t *testing.T) {
	sid := "S2"
	eng := newTestEngine(t)
	mustAppend(t, eng, snapEvent(sid, "A", LevelCore, 1000, 0))

	e1 := deltaEvent(sid, "A", "g", 1, 100, 0, 1)
	if !mustAppend(t, eng, e1) {
		t.Fatal("首次上报应接受")
	}
	if mustAppend(t, eng, e1) {
		t.Fatal("同 eventId 重复上报必须判重")
	}
	// 同设备同序号、不同 eventId（设备重传）仍须判重。
	e1b := e1
	e1b.EventID = "d-retransmit"
	if mustAppend(t, eng, e1b) {
		t.Fatal("同设备同序号重传必须判重")
	}
	// 同序号但不同类型（锚点）语义不同，应接受。
	an := anchorEvent(sid, "A", "g", 1, 100, 1, "重传竞态核验")
	if !mustAppend(t, eng, an) {
		t.Fatal("同序号锚点不应被增量判重键误杀")
	}

	s := eng.Project(sid, at(5)).Summary()
	for _, v := range s.Venues {
		if v.VenueID == "A" && v.Occupancy != 100 {
			t.Fatalf("重复计数后占用应为锚点值100，实际 %d", v.Occupancy)
		}
	}
}

// 验收3：迟到数据按发生时间归并；水位缺口可发现、补传后消失。
func TestLateDataAndGaps(t *testing.T) {
	sid := "S3"
	eng := newTestEngine(t)
	mustAppend(t, eng, snapEvent(sid, "A", LevelCore, 1000, 0))
	mustAppend(t, eng, deltaEvent(sid, "A", "g", 1, 100, 0, 1))
	mustAppend(t, eng, deltaEvent(sid, "A", "g", 3, 100, 0, 3)) // 序号2缺失

	p := eng.Project(sid, at(5))
	gaps := p.gapsFor(p.venues["A"])
	if len(gaps) != 1 || !reflect.DeepEqual(gaps[0].Missing, []int64{2}) {
		t.Fatalf("应发现缺口序号2，实际 %+v", gaps)
	}
	// 迟到补传序号2（发生时间在3之前），占用保持顺序无关正确。
	mustAppend(t, eng, deltaEvent(sid, "A", "g", 2, 50, 0, 2))
	p = eng.Project(sid, at(5))
	if gaps := p.gapsFor(p.venues["A"]); len(gaps) != 0 {
		t.Fatalf("补传后缺口应消失，实际 %+v", gaps)
	}
	if got := p.venues["A"].occupancy; got != 250 {
		t.Fatalf("迟到补传归并后占用应为250，实际 %d", got)
	}
}

// 验收4：告警在一个活跃期内唯一（升级不产生新告警），解除后重发才是新告警。
func TestUniqueAlarmLifecycle(t *testing.T) {
	sid := "S4"
	eng := newTestEngine(t)
	mustAppend(t, eng, snapEvent(sid, "A", LevelCore, 100, 0))

	// 拥挤告警（占用 95）。
	mustAppend(t, eng, anchorEvent(sid, "A", "g", 1, 95, 1, ""))
	// 升级为管制，必须沿用同一告警。
	mustAppend(t, eng, incidentEvent(sid, "A", "inc1", PriControl, true, 2))
	// 解除管制：占用仍95，告警降级回拥挤而不是新建。
	mustAppend(t, eng, incidentEvent(sid, "A", "rel1", PriNormal, false, 3))
	// 占用回落，告警关闭。
	mustAppend(t, eng, deltaEvent(sid, "A", "g", 2, 0, 10, 4))
	// 再次拥挤 → 新的活跃期、新告警。
	mustAppend(t, eng, anchorEvent(sid, "A", "g", 3, 97, 5, "二次高峰"))

	s := eng.Project(sid, at(6)).Summary()
	var active, closed int
	var activeID, firstID string
	for _, a := range s.Alarms {
		if a.VenueID != "A" {
			continue
		}
		if a.Active {
			active++
			activeID = a.AlarmID
		} else {
			closed++
			firstID = a.AlarmID
		}
	}
	if active != 1 || closed != 1 {
		t.Fatalf("期望 1 个活动 + 1 个已关闭告警，实际活动=%d 关闭=%d（%+v）", active, closed, s.Alarms)
	}
	if activeID == firstID {
		t.Fatal("第二次拥挤应产生新告警ID")
	}
}

// 验收5：跨午夜场次以 sessionId 归并，不同场次互不串扰。
func TestCrossMidnightSessions(t *testing.T) {
	eng := newTestEngine(t)
	mustAppend(t, eng, snapEvent("night-1", "A", LevelCore, 1000, -20))
	mustAppend(t, eng, deltaEvent("night-1", "A", "g", 1, 300, 0, -15)) // 23:35
	mustAppend(t, eng, deltaEvent("night-1", "A", "g", 2, 200, 0, 15))  // 次日 00:05
	mustAppend(t, eng, snapEvent("night-2", "A", LevelCore, 1000, 100))
	mustAppend(t, eng, deltaEvent("night-2", "A", "g", 1, 10, 0, 105))

	p1 := eng.Project("night-1", at(20))
	if got := p1.venues["A"].occupancy; got != 500 {
		t.Fatalf("跨午夜同一场次占用应连续累加为500，实际 %d", got)
	}
	p2 := eng.Project("night-2", at(110))
	if got := p2.venues["A"].occupancy; got != 10 {
		t.Fatalf("不同场次必须隔离，实际 %d", got)
	}
}

// 验收6：重启后从 JSONL 重放恢复，状态字节级一致。
func TestRecoveryFromLog(t *testing.T) {
	sid := "S6"
	path := t.TempDir() + "/events.jsonl"
	log1, err := NewEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	eng1 := NewEngine(log1)
	mustAppend(t, eng1, snapEvent(sid, "A", LevelCore, 100, 0))
	mustAppend(t, eng1, anchorEvent(sid, "A", "g", 1, 95, 1, ""))
	mustAppend(t, eng1, incidentEvent(sid, "A", "i", PriControl, true, 2))
	want, _ := json.Marshal(eng1.Project(sid, at(3)).Summary())

	// 模拟中心进程重启。
	log2, err := NewEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	eng2 := NewEngine(log2)
	got, _ := json.Marshal(eng2.Project(sid, at(3)).Summary())
	if string(want) != string(got) {
		t.Fatalf("重启重放状态不一致\nwant=%s\ngot =%s", want, got)
	}
}

// 验收7：证据谱系可解释——被锚点取代的计数显式标 used=false，锚点与后续计数 used=true。
func TestEvidenceExplainsDecision(t *testing.T) {
	sid := "S7"
	eng := newTestEngine(t)
	mustAppend(t, eng, snapEvent(sid, "A", LevelCore, 100, 0))
	mustAppend(t, eng, snapEvent(sid, "B", LevelCoord, 400, 0))
	mustAppend(t, eng, deltaEvent(sid, "A", "g", 1, 80, 0, 1))
	mustAppend(t, eng, deltaEvent(sid, "A", "g", 2, 80, 0, 2))
	mustAppend(t, eng, anchorEvent(sid, "A", "g", 3, 95, 3, "闸机核验"))
	mustAppend(t, eng, deltaEvent(sid, "A", "g", 4, 2, 0, 4)) // 97 红区

	prop, err := eng.AutoPropose(sid, "A", at(4))
	if err != nil || prop == nil {
		t.Fatalf("应生成导流建议: %v", err)
	}
	if prop.Kind != KindGuide || len(prop.Targets) != 1 || prop.Targets[0].VenueID != "B" {
		t.Fatalf("建议应导向B，实际 %+v", prop.Targets)
	}
	used, excluded := map[string]bool{}, map[string]bool{}
	for _, ev := range prop.Evidence {
		if ev.Used {
			used[ev.EventID] = true
		} else {
			excluded[ev.EventID] = true
		}
	}
	_ = used
	_ = excluded
	// 直接核验：锚点与序号4有效，序号1/2被排除。
	var anchorUsed, postUsed, preExcluded int
	for _, ev := range prop.Evidence {
		switch ev.Type {
		case EvCountAnchor:
			if ev.Used {
				anchorUsed++
			}
		case EvCountDelta:
			if ev.Used {
				postUsed++
			} else {
				preExcluded++
			}
		}
	}
	if anchorUsed != 1 || postUsed != 1 || preExcluded != 2 {
		t.Fatalf("证据构成错误：锚点有效%d、锚点后有效%d、被取代%d（%+v）", anchorUsed, postUsed, preExcluded, prop.Evidence)
	}
}

// 验收8：导流归因——已接受导流后目标点位速率抬升并产生新告警。
func TestDiversionAttribution(t *testing.T) {
	sid := "S8"
	eng := newTestEngine(t)
	mustAppend(t, eng, snapEvent(sid, "A", LevelCore, 100, -20))
	mustAppend(t, eng, snapEvent(sid, "B", LevelCoord, 500, -20))
	mustAppend(t, eng, anchorEvent(sid, "A", "g", 1, 98, -1, "满员"))
	// B 在导流前保持低速率。
	for m := -9; m <= -1; m++ {
		mustAppend(t, eng, deltaEvent(sid, "B", "c", int64(m+10), 10, 0, m))
	}
	prop, err := eng.AutoPropose(sid, "A", at(0))
	if err != nil || prop == nil || prop.Kind != KindGuide {
		t.Fatalf("应生成导流建议: %v %v", prop, err)
	}
	if err := eng.Decide(sid, prop.DirectiveID, DecisionAccept, "op-赵", "", at(0)); err != nil {
		t.Fatal(err)
	}
	// 导流后 B 大量入场（seq 10 起步避免与前序冲突）。
	mustAppend(t, eng, deltaEvent(sid, "B", "c", 20, 100, 0, 1))
	mustAppend(t, eng, deltaEvent(sid, "B", "c", 21, 100, 0, 2))
	mustAppend(t, eng, deltaEvent(sid, "B", "c", 22, 100, 0, 3))
	mustAppend(t, eng, deltaEvent(sid, "B", "c", 23, 100, 0, 4)) // 90+400=490/500 红区

	s := eng.Project(sid, at(5)).Summary()
	if len(s.Attributions) != 1 {
		t.Fatalf("应产生1条导流归因，实际 %d (%+v)", len(s.Attributions), s.Attributions)
	}
	a := s.Attributions[0]
	if a.DirectiveID != prop.DirectiveID || a.TargetVenue != "B" {
		t.Fatalf("归因指向错误: %+v", a)
	}
	if a.PostRate <= a.PreRate*AttributionFactor {
		t.Fatalf("速率对比不成立: pre=%v post=%v", a.PreRate, a.PostRate)
	}
}

// 验收9：管制引发的拥堵不得归因于导流。
func TestAttributionExcludesIncident(t *testing.T) {
	sid := "S9"
	eng := newTestEngine(t)
	mustAppend(t, eng, snapEvent(sid, "A", LevelCore, 100, -20))
	mustAppend(t, eng, snapEvent(sid, "B", LevelCoord, 500, -20))
	mustAppend(t, eng, anchorEvent(sid, "A", "g", 1, 98, -1, ""))
	prop, _ := eng.AutoPropose(sid, "A", at(0))
	if err := eng.Decide(sid, prop.DirectiveID, DecisionAccept, "op", "", at(0)); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, eng, deltaEvent(sid, "B", "c", 20, 400, 0, 2))
	// B 的告警由管制事件触发，即使占用高也不应归因。
	mustAppend(t, eng, incidentEvent(sid, "B", "ctrl", PriControl, true, 3))

	s := eng.Project(sid, at(5)).Summary()
	for _, at := range s.Attributions {
		for _, al := range s.Alarms {
			if al.AlarmID == at.AlarmID && al.TriggerEventID == "ctrl" {
				t.Fatalf("管制告警被错误归因: %+v", at)
			}
		}
	}
}

// 验收10：授权闸门——默认拒绝、窗口内允许、过期拒绝、撤销拒绝、管制中止分发。
func TestAuthorizationGate(t *testing.T) {
	sid := "S10"
	eng := newTestEngine(t)
	mustAppend(t, eng, snapEvent(sid, "A", LevelCore, 100, -20))
	grant, _ := json.Marshal(AuthzGrant{ContentID: "feed", Start: at(-5), End: at(5)})
	mustAppend(t, eng, Event{EventID: "g1", Type: EvAuthzGranted, SessionID: sid, VenueID: "A",
		OccurredAt: at(-5), ReceivedAt: at(-5), Payload: grant})

	p := eng.Project(sid, at(0))
	if d := p.Authorize("A", "feed", at(0)); !d.Allowed {
		t.Fatalf("窗口内应允许: %s", d.Reason)
	}
	if d := p.Authorize("A", "feed", at(6)); d.Allowed {
		t.Fatal("过期后必须拒绝，画面不得继续分发")
	}
	if d := p.Authorize("A", "other", at(0)); d.Allowed {
		t.Fatal("未授权内容默认拒绝")
	}
	if d := p.Authorize("unknown", "feed", at(0)); d.Allowed {
		t.Fatal("未注册点位默认拒绝")
	}

	// 撤销。
	rev, _ := json.Marshal(AuthzRevoke{ContentID: "feed", Reason: "版权终止"})
	mustAppend(t, eng, Event{EventID: "r1", Type: EvAuthzRevoked, SessionID: sid, VenueID: "A",
		OccurredAt: at(-2), ReceivedAt: at(-2), Payload: rev})
	if d := eng.Project(sid, at(0)).Authorize("A", "feed", at(0)); d.Allowed {
		t.Fatal("撤销后必须拒绝")
	}

	// 另一场有效授权遇到管制：停止分发。
	grant2, _ := json.Marshal(AuthzGrant{ContentID: "feed2", Start: at(-5), End: at(15)})
	mustAppend(t, eng, Event{EventID: "g2", Type: EvAuthzGranted, SessionID: sid, VenueID: "A",
		OccurredAt: at(-5), ReceivedAt: at(-5), Payload: grant2})
	mustAppend(t, eng, incidentEvent(sid, "A", "c", PriControl, true, 8))
	if d := eng.Project(sid, at(9)).Authorize("A", "feed2", at(9)); d.Allowed {
		t.Fatal("管制期间即使授权有效也必须停止分发")
	}
}

// 验收11：运营决策不可变、驳回不生效、TTL 后不可决策。
func TestDecisionImmutabilityAndTTL(t *testing.T) {
	sid := "S11"
	eng := newTestEngine(t)
	mustAppend(t, eng, snapEvent(sid, "A", LevelCore, 100, -10))
	mustAppend(t, eng, anchorEvent(sid, "A", "g", 1, 95, 0, ""))

	prop, _ := eng.AutoPropose(sid, "A", at(0))
	if err := eng.Decide(sid, prop.DirectiveID, DecisionReject, "op-钱", "我有别的安排", at(1)); err != nil {
		t.Fatal(err)
	}
	if err := eng.Decide(sid, prop.DirectiveID, DecisionAccept, "op-钱", "", at(1)); err == nil {
		t.Fatal("驳回后不得被接受覆盖")
	}
	s := eng.Project(sid, at(2)).Summary()
	for _, d := range s.Directives {
		if d.DirectiveID == prop.DirectiveID && d.Status != "已驳回" {
			t.Fatalf("状态应为已驳回: %s", d.Status)
		}
	}

	// 过期后再决策应被拒绝（t2 时驳回可见且已过原建议的开放期，可重新提议）。
	prop2, _ := eng.AutoPropose(sid, "A", at(2))
	if prop2 == nil {
		t.Fatal("驳回后应允许重新提议")
	}
	if err := eng.Decide(sid, prop2.DirectiveID, DecisionAccept, "op", "", at(10)); err == nil {
		t.Fatal("超过 TTL 必须拒绝决策")
	}
}

// 验收12：无导流去向时建议升级为暂停入场，接受后点位 held。
func TestSuspendWhenNoTarget(t *testing.T) {
	sid := "S12"
	eng := newTestEngine(t)
	mustAppend(t, eng, snapEvent(sid, "A", LevelCore, 100, -10))
	mustAppend(t, eng, anchorEvent(sid, "A", "g", 1, 95, 0, "")) // 无其他点位
	prop, err := eng.AutoPropose(sid, "A", at(0))
	if err != nil || prop == nil || prop.Kind != KindSuspend {
		t.Fatalf("无去向时应建议暂停入场: %v %v", prop, err)
	}
	if err := eng.Decide(sid, prop.DirectiveID, DecisionAccept, "op", "", at(0)); err != nil {
		t.Fatal(err)
	}
	pj := eng.Project(sid, at(1))
	if !pj.held(pj.venues["A"], at(1)) {
		t.Fatal("接受暂停入场后点位应处于 held")
	}
}
func TestPersonalIdentifierRejected(t *testing.T) {
	sid := "S13"
	cases := []map[string]any{
		{"enter": 1, "手机号": "13800000000"},
		{"enter": 1, "nested": []any{map[string]any{"身份证": "x"}}},
		{"enter": 1, "deviceFingerprint": "abc"},
	}
	for i, payload := range cases {
		raw, _ := json.Marshal(payload)
		e := Event{EventID: "pii", Type: EvCountDelta, SessionID: sid, VenueID: "A",
			DeviceID: "g", Seq: int64(i + 1), OccurredAt: at(1), Payload: raw}
		if err := e.Validate(at(2)); err == nil {
			t.Fatalf("用例%d 应被拒绝", i)
		}
	}
}
