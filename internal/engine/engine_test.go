package engine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/beryl0222/city-second-screen/model"
)

func base(t time.Time) time.Time { return t }

func mustEvent(t *testing.T, raw string, recv time.Time) *model.Event {
	t.Helper()
	ev, err := model.ParseEnvelope([]byte(raw), recv)
	if err != nil {
		t.Fatalf("事件解析失败: %v\n%s", err, raw)
	}
	return ev
}

// 构造辅助。
func regEvent(id, venue, sid string, occ time.Time, level string, cap int, alts []string) *model.Event {
	b, _ := json.Marshal(model.RegisterPayload{Level: level, Capacity: cap, AlternateVenues: alts})
	return &model.Event{EventID: id, Type: model.TypeVenueRegister, VenueID: venue,
		SessionID: sid, OccTime: occ, ReceivedAt: occ, Register: mustPayload[model.RegisterPayload](b)}
}

func countEvent(id, venue, dev, sid string, seq int64, occ, recv time.Time, enter, exit int) *model.Event {
	return &model.Event{EventID: id, Type: model.TypeCount, VenueID: venue, DeviceID: dev,
		SessionID: sid, Seq: seq, OccTime: occ, ReceivedAt: recv,
		Count: &model.CountPayload{Enter: enter, Exit: exit, Confidence: 100}}
}

func snapEvent(id, venue, sid string, occ time.Time, occN, cap int, auth bool) *model.Event {
	return &model.Event{EventID: id, Type: model.TypeSnapshot, VenueID: venue, SessionID: sid,
		OccTime: occ, ReceivedAt: occ, Snapshot: &model.SnapshotPayload{Occupancy: occN, Capacity: cap, Authoritative: auth}}
}

func incEvent(id, venue, sid string, occ time.Time, prio, code string, resolved bool, resolves string) *model.Event {
	return &model.Event{EventID: id, Type: model.TypeIncident, VenueID: venue, SessionID: sid,
		OccTime: occ, ReceivedAt: occ,
		Incident: &model.IncidentPayload{Priority: prio, Code: code, Resolved: resolved, ResolvesCode: resolves,
			Message: code + " 事件"}}
}

func hbEvent(id, venue, sid string, occ time.Time, online bool, gap int64) *model.Event {
	return &model.Event{EventID: id, Type: model.TypeHeartbeat, VenueID: venue, SessionID: sid,
		OccTime: occ, ReceivedAt: occ, Heartbeat: &model.HeartbeatPayload{Online: online, GapSeconds: gap}}
}

func mustPayload[T any](b []byte) *T {
	var p T
	_ = json.Unmarshal(b, &p)
	return &p
}

// 场景：快照锚点 + 迟到计数 + 修正 + 同设备重复 + 失联恢复。
func TestCountingGovernance(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 19, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	sid := "match-night"
	events := []*model.Event{
		regEvent("reg", "plaza-a", sid, t0.Add(-2*time.Hour), model.LevelCore, 1000, []string{"plaza-b"}),
		regEvent("reg2", "plaza-b", sid, t0.Add(-2*time.Hour), model.LevelCoop, 800, nil),
		snapEvent("snap1", "plaza-a", sid, t0, 500, 0, true),
		// 正常增量。
		countEvent("c1", "plaza-a", "dev1", sid, 1, t0.Add(10*time.Second), t0.Add(12*time.Second), 100, 0),
		// 迟到数据：occ 在 c1 后，但 5 分钟后才到。
		countEvent("c2-late", "plaza-a", "dev1", sid, 2, t0.Add(20*time.Second), t0.Add(5*time.Minute), 60, 0),
		// 同设备重复上报：换了 event_id 但 seq 与 c1 相同（内容一致，重传补偿）。
		countEvent("c1-dup-newid", "plaza-a", "dev1", sid, 1, t0.Add(10*time.Second), t0.Add(30*time.Second), 100, 0),
		// 同一批次第三次重传。
		countEvent("c1-rerun", "plaza-a", "dev1", sid, 1, t0.Add(10*time.Second), t0.Add(40*time.Second), 100, 0),
		// 修正 c2-late：实际只进了 20 人（原批次作废）。
		{EventID: "c2-fix", Type: model.TypeCount, VenueID: "plaza-a", DeviceID: "dev1", SessionID: sid,
			Seq: 3, OccTime: t0.Add(6 * time.Minute), ReceivedAt: t0.Add(6 * time.Minute),
			Count: &model.CountPayload{Enter: 20, Exit: 0, Confidence: 100, CorrectsEventID: "c2-late"}},
		// 失联与恢复。
		hbEvent("hb-off", "plaza-a", sid, t0.Add(2*time.Minute), false, 0),
		hbEvent("hb-on", "plaza-a", sid, t0.Add(4*time.Minute), true, 120),
	}
	asOf := t0.Add(6 * time.Minute)
	sum := Fold(events, cfg, asOf)
	v := sum.Sessions[sid].Venues["plaza-a"]

	// 锚点 500 + c1(+100) + 修正批次(+20)；c2-late 被取代，重复与旧序号被排除。
	if v.Occupancy != 620 {
		t.Fatalf("占用应为 620，实际 %d（原始=%d）", v.Occupancy, v.RawOccupancy)
	}
	if v.CountsAccepted != 2 {
		t.Fatalf("有效计数应为 2 批，实际 %d", v.CountsAccepted)
	}
	if v.CountsExcluded < 3 {
		t.Fatalf("应至少排除 3 批（重复2 + 锚点前/修正取代），实际 %d", v.CountsExcluded)
	}
	if v.Quality != model.QualityRecovered {
		t.Fatalf("数据质量应为 recovered，实际 %s", v.Quality)
	}
	if !v.Online {
		t.Fatal("恢复心跳后应在线")
	}
}

// 乱序稳定性：同一事件集任意接收/输入顺序，折叠结果必须一致。
func TestOutOfOrderStability(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 19, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	sid := "s"
	var events []*model.Event
	for i := 0; i < 30; i++ {
		occ := t0.Add(time.Duration(i) * 15 * time.Second)
		events = append(events, countEvent(
			"e"+itoa(i), "v", "d", sid, int64(i+1), occ, occ.Add(time.Second),
			30, 5))
	}
	events = append(events,
		regEvent("r", "v", sid, t0.Add(-time.Hour), model.LevelCore, 1000, nil),
		snapEvent("sp", "v", sid, t0, 100, 0, true),
		incEvent("i1", "v", sid, t0.Add(5*time.Minute), model.PriorityControl, "CTRL-1", false, ""),
		incEvent("i1x", "v", sid, t0.Add(8*time.Minute), model.PriorityControl, "CTRL-1", true, ""),
	)
	asOf := t0.Add(10 * time.Minute)
	ref := json2(Fold(events, cfg, asOf))

	// 逆序。
	rev := make([]*model.Event, len(events))
	for i := range events {
		rev[len(events)-1-i] = events[i]
	}
	if got := json2(Fold(rev, cfg, asOf)); got != ref {
		t.Fatal("逆序折叠结果与原序不一致")
	}
	// 多次循环移位。
	for shift := 1; shift < len(events); shift += 7 {
		sh := make([]*model.Event, len(events))
		for i := range events {
			sh[(i+shift)%len(events)] = events[i]
		}
		if got := json2(Fold(sh, cfg, asOf)); got != ref {
			t.Fatalf("移位 %d 后折叠结果不一致", shift)
		}
	}
}

// 唯一告警：在途重复不新增；解除后再发生产生新序号；解除不凭空造告警。
func TestUniqueAlerts(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 20, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	sid := "s"
	events := []*model.Event{
		incEvent("a1", "v", sid, t0, model.PriorityCrowd, "CROWD-9", false, ""),
		incEvent("a1-dup", "v", sid, t0.Add(30*time.Second), model.PriorityCrowd, "CROWD-9", false, ""),
		incEvent("a2", "v", sid, t0.Add(1*time.Minute), model.PriorityControl, "CTRL-2", false, ""),
		// 解除不存在的事件，不应造告警。
		incEvent("ghost", "v", sid, t0.Add(2*time.Minute), model.PriorityNormal, "NOPE", true, ""),
		incEvent("a2-res", "v", sid, t0.Add(3*time.Minute), model.PriorityControl, "CTRL-2", true, ""),
		// 同一 code 再次发生 -> 新一次。
		incEvent("a2-again", "v", sid, t0.Add(4*time.Minute), model.PriorityControl, "CTRL-2", false, ""),
	}
	sum := Fold(events, cfg, t0.Add(5*time.Minute))
	alerts := sum.Sessions[sid].Alerts
	wantKeys := map[string]bool{
		sid + "|v|CROWD-9#1": true,
		sid + "|v|CTRL-2#1":  true,
		sid + "|v|CTRL-2#2":  true,
	}
	if len(alerts) != 3 {
		for _, a := range alerts {
			t.Logf("告警: %s %s", a.Key, a.Status)
		}
		t.Fatalf("应恰好 3 条唯一告警，实际 %d", len(alerts))
	}
	for _, a := range alerts {
		if !wantKeys[a.Key] {
			t.Fatalf("意外告警键 %s", a.Key)
		}
	}
}

// 跨午夜：不同 session_id 状态严格隔离。
func TestCrossMidnightSessions(t *testing.T) {
	night := time.Date(2026, 9, 22, 23, 55, 0, 0, time.UTC)
	day := time.Date(2026, 9, 23, 0, 5, 0, 0, time.UTC)
	cfg := DefaultConfig()
	events := []*model.Event{
		regEvent("r1", "v", "late", night.Add(-2*time.Hour), model.LevelCore, 100, nil),
		snapEvent("sp1", "v", "late", night, 80, 0, true),
		regEvent("r2", "v", "late2", day.Add(-2*time.Hour), model.LevelCore, 200, nil),
		snapEvent("sp2", "v", "late2", day, 30, 0, true),
	}
	sum := Fold(events, cfg, day.Add(time.Hour))
	if got := sum.Sessions["late"].Venues["v"].Occupancy; got != 80 {
		t.Fatalf("晚场占用应为 80，实际 %d", got)
	}
	if got := sum.Sessions["late2"].Venues["v"].Occupancy; got != 30 {
		t.Fatalf("午夜后场次占用应为 30，实际 %d", got)
	}
}

// 内容授权：窗口内分发，过期/撤销生成停发指令。
func TestContentAuthorization(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 19, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	sid := "s"
	auth := func(id, content string, start, end time.Time, revoked bool) *model.Event {
		return &model.Event{EventID: id, Type: model.TypeAuthorize, VenueID: "v", SessionID: sid,
			OccTime: start.Add(-time.Minute), ReceivedAt: start.Add(-time.Minute),
			Authorize: &model.AuthorizePayload{ContentID: content, Start: start, End: end, Revoked: revoked}}
	}
	events := []*model.Event{
		auth("az1", "feed-1", t0, t0.Add(2*time.Hour), false),
		auth("az2", "feed-2", t0, t0.Add(30*time.Minute), false),
		auth("az3", "feed-3", t0, t0.Add(3*time.Hour), true), // 提前撤销
	}
	sum := Fold(events, cfg, t0.Add(time.Hour))
	st := sum.Sessions[sid]
	byContent := map[string]*ContentState{}
	for _, c := range st.Content {
		byContent[c.ContentID] = c
	}
	if byContent["feed-1"].Status != "distributing" || byContent["feed-1"].StopOrder != nil {
		t.Fatal("feed-1 应在分发中且无停发指令")
	}
	if byContent["feed-2"].Status != "expired" || byContent["feed-2"].StopOrder == nil ||
		byContent["feed-2"].StopOrder.Reason != "expired" {
		t.Fatal("feed-2 应已过期并产生 expired 停发指令")
	}
	if byContent["feed-3"].Status != "revoked" || byContent["feed-3"].StopOrder == nil ||
		byContent["feed-3"].StopOrder.Reason != "revoked" {
		t.Fatal("feed-3 应已撤销并产生 revoked 停发指令")
	}
}

// 导流建议阈值、目标选择与运营处置审计。
func TestDiversionAndAudit(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 19, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	sid := "s"
	events := []*model.Event{
		regEvent("r1", "full", sid, t0.Add(-time.Hour), model.LevelCore, 100, []string{"spare"}),
		regEvent("r2", "spare", sid, t0.Add(-time.Hour), model.LevelCoop, 100, nil),
		snapEvent("sp1", "full", sid, t0, 92, 0, true), // 92% 暂停入场
		snapEvent("sp2", "spare", sid, t0, 10, 0, true),
		hbEvent("h1", "full", sid, t0.Add(10*time.Second), true, 0),
		hbEvent("h2", "spare", sid, t0.Add(10*time.Second), true, 0),
	}
	asOf := t0.Add(20 * time.Second)
	sum := Fold(events, cfg, asOf)
	full := sum.Sessions[sid].Venues["full"]
	if full.Recommendation == nil || full.Recommendation.Directive != model.DirectiveHold {
		t.Fatalf("92%% 占用应建议暂停入场，实际 %+v", full.Recommendation)
	}
	if full.Recommendation.TargetVenueID != "" {
		t.Fatal("暂停入场不应有导流目标")
	}
	if len(full.Recommendation.Evidence.Used) == 0 {
		t.Fatal("建议必须冻结有效数据依据")
	}

	// 将占用降到 80% -> 导流，目标应是 spare。
	events = append(events, snapEvent("sp3", "full", sid, t0.Add(30*time.Second), 80, 0, true))
	asOf2 := t0.Add(40 * time.Second)
	sum2 := Fold(events, cfg, asOf2)
	full2 := sum2.Sessions[sid].Venues["full"]
	if full2.Recommendation == nil || full2.Recommendation.Directive != model.DirectiveDivert {
		t.Fatalf("80%% 应导流，实际 %+v", full2.Recommendation)
	}
	if full2.Recommendation.TargetVenueID != "spare" {
		t.Fatalf("导流目标应为 spare，实际 %q", full2.Recommendation.TargetVenueID)
	}

	// 运营驳回 -> 审计绑定。
	decisionID := full2.Recommendation.DecisionID
	accepted := false
	events = append(events, &model.Event{
		EventID: "ack1", Type: model.TypeDecisionAck, VenueID: "full", SessionID: sid,
		OccTime: t0.Add(50 * time.Second), ReceivedAt: t0.Add(50 * time.Second),
		DecisionAck: &model.DecisionAckPayload{DecisionID: decisionID, Accepted: &accepted,
			OperatorID: "op-7", Reason: "spare 正在施工"},
	})
	sum3 := Fold(events, cfg, t0.Add(60*time.Second))
	st3 := sum3.Sessions[sid]
	if len(st3.Audit) != 1 || st3.Audit[0].Accepted || st3.Audit[0].OperatorID != "op-7" {
		t.Fatalf("审计记录异常: %+v", st3.Audit)
	}
	if st3.Venues["full"].Recommendation.Acked == nil ||
		st3.Venues["full"].Recommendation.Acked.Accepted {
		t.Fatal("建议应绑定驳回审计")
	}
}

// 陈旧数据：超窗口无报数时抑制基于计数的建议，占用标记不可采信。
func TestStaleSuppressesCountAdvice(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 19, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	sid := "s"
	events := []*model.Event{
		regEvent("r1", "v", sid, t0.Add(-2*time.Hour), model.LevelCore, 100, nil),
		snapEvent("sp", "v", sid, t0, 95, 0, true),
	}
	sum := Fold(events, cfg, t0.Add(10*time.Minute))
	v := sum.Sessions[sid].Venues["v"]
	if v.Quality != model.QualityStale {
		t.Fatalf("应标记 stale，实际 %s", v.Quality)
	}
	if v.Heat != HeatUnknown {
		t.Fatalf("陈旧时热度应为未知，实际 %s", v.Heat)
	}
	if v.Recommendation != nil {
		t.Fatalf("陈旧数据不得产生基于计数的建议，实际 %+v", v.Recommendation)
	}
	// 但管制事件仍应强制暂停。
	events = append(events, incEvent("i1", "v", sid, t0.Add(9*time.Minute),
		model.PriorityControl, "CTRL-X", false, ""))
	sum2 := Fold(events, cfg, t0.Add(10*time.Minute))
	v2 := sum2.Sessions[sid].Venues["v"]
	if !v2.EntryHeld || v2.Recommendation == nil ||
		v2.Recommendation.Directive != model.DirectiveHold {
		t.Fatal("管制事件即便数据陈旧也必须暂停入场")
	}
}

func json2(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
