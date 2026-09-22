// 判定引擎：自动建议、证据谱系、导流归因、授权闸门与汇总视图。
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type Engine struct {
	log *EventLog
	mu  sync.Mutex // 串行化“先投影再追加”的复合写操作，防止并发重复提议/决策
}

func NewEngine(log *EventLog) *Engine { return &Engine{log: log} }

// appendExternal 追加一条已通过校验的外部事件。
func (eng *Engine) appendExternal(e Event) (bool, error) {
	eng.mu.Lock()
	defer eng.mu.Unlock()
	return eng.log.Append(e)
}

// ---- 建议生成 ----

// AutoPropose 为点位生成自动建议（若当前条件需要）。
// 红/拥挤 → 导流；管制及以上或满员且无导流候选 → 暂停入场。
// 返回 nil 表示当前无需建议（或存在尚未过期/未驳回的同类建议）。
func (eng *Engine) AutoPropose(sessionID, venueID string, now time.Time) (*DirectiveProposal, error) {
	eng.mu.Lock()
	defer eng.mu.Unlock()
	p := eng.Project(sessionID, now)
	v := p.venues[venueID]
	if v == nil || v.snapshot == nil {
		return nil, fmt.Errorf("点位 %s 尚未注册容量快照", venueID)
	}
	sev, reason := p.condition(v, Event{})
	if sev == PriNormal {
		return nil, nil
	}
	kind := KindGuide
	if sev.AtLeastControl() {
		kind = KindSuspend
	}
	if p.hasOpenProposal(venueID, kind, now) {
		return nil, nil
	}

	targets := p.diversionTargets(v, now)
	if kind == KindGuide && len(targets) == 0 {
		kind = KindSuspend
		if p.hasOpenProposal(venueID, kind, now) {
			return nil, nil
		}
	}

	prop := &DirectiveProposal{
		DirectiveID: "dir_" + shortHash(fmt.Sprintf("%s|%s|%s|%s", sessionID, venueID, kind, now.Format(time.RFC3339Nano))),
		Kind:        kind,
		Priority:    string(sev),
		VenueID:     venueID,
		VenueName:   v.snapshot.Name,
		Targets:     targets,
		Reason:      reason,
		Evidence:    p.evidenceFor(v),
		ProposedAt:  now,
		TTLSeconds:  int(DefaultTTL / time.Second),
	}
	raw, _ := json.Marshal(prop)
	ev := Event{
		EventID: newEventID(), Type: EvProposed, SessionID: sessionID,
		VenueID: venueID, OccurredAt: now, ReceivedAt: now, Payload: raw,
	}
	if _, err := eng.log.Append(ev); err != nil {
		return nil, err
	}
	return prop, nil
}

// hasOpenProposal 存在“待决策”或“已接受且未过期”的同类建议时，不再重复提议。
func (p *Projection) hasOpenProposal(venueID, kind string, now time.Time) bool {
	for _, id := range p.directiveOrder {
		rec := p.directives[id]
		if rec.proposal == nil {
			continue
		}
		var dp DirectiveProposal
		if json.Unmarshal(rec.proposal.Payload, &dp) != nil {
			continue
		}
		if dp.VenueID != venueID || dp.Kind != kind {
			continue
		}
		expiry := dp.ProposedAt.Add(time.Duration(dp.TTLSeconds) * time.Second)
		if now.After(expiry) {
			continue // 已过期，可重新提议
		}
		if rec.decision == nil {
			return true
		}
		var d DecisionPayload
		if json.Unmarshal(rec.decision.Payload, &d) == nil && d.Decision == DecisionAccept {
			return true
		}
	}
	return false
}

// diversionTargets 选择可承接的去向：已注册、未满员、未暂停、非红区、数据未失效。
// 排序：协同点位优先于备用点位，再按空闲座位数降序，保证选择确定。
func (p *Projection) diversionTargets(src *venueState, now time.Time) []TargetChoice {
	type cand struct {
		v    *venueState
		free int
	}
	var cands []cand
	totalFree := 0
	for _, id := range p.order {
		v := p.venues[id]
		if id == src.id || v.snapshot == nil || p.stale(v) || p.held(v, now) {
			continue
		}
		if in := v.activeIncident(); in != nil && in.in.Priority.rank() >= PriCrowd.rank() {
			continue
		}
		if heatOf(v.ratio()) == HeatRed {
			continue
		}
		free := v.snapshot.Capacity - v.occupancy
		if free <= 0 {
			continue
		}
		cands = append(cands, cand{v, free})
		totalFree += free
	}
	sort.Slice(cands, func(i, j int) bool {
		wi, wj := levelWeight(cands[i].v.snapshot.Level), levelWeight(cands[j].v.snapshot.Level)
		if wi != wj {
			return wi > wj
		}
		if cands[i].free != cands[j].free {
			return cands[i].free > cands[j].free
		}
		return cands[i].v.id < cands[j].v.id
	})
	// 需要分流的规模：红区溢出量，至少按当前在场的 15% 估算。
	need := src.occupancy - int(0.85*float64(src.snapshot.Capacity))
	if need < int(0.15*float64(src.occupancy))+1 {
		need = int(0.15*float64(src.occupancy)) + 1
	}
	out := []TargetChoice{}
	remaining := need
	for _, c := range cands {
		if remaining <= 0 {
			break
		}
		seats := c.free
		if seats > remaining {
			seats = remaining
		}
		out = append(out, TargetChoice{
			VenueID: c.v.id, Name: c.v.snapshot.Name, Level: string(c.v.snapshot.Level),
			Ratio:     float64(seats) / float64(need),
			FreeSeats: c.free,
		})
		remaining -= seats
	}
	return out
}

func levelWeight(l VenueLevel) int {
	switch l {
	case LevelCoord:
		return 3
	case LevelBackup:
		return 2
	case LevelCore:
		return 1
	}
	return 0
}

// evidenceFor 固化一次导流/暂停判断使用的全部有效数据，并显式列出被排除项。
func (p *Projection) evidenceFor(src *venueState) []EvidenceItem {
	var out []EvidenceItem

	if src.snapshot != nil {
		out = append(out, EvidenceItem{
			EventID: src.snapEventID, Type: EvVenueSnapshot,
			OccurredAt: src.snapAt.Format(time.RFC3339), Used: true,
			Detail: fmt.Sprintf("有效容量快照：%s，等级 %s，容量 %d", src.snapshot.Name, src.snapshot.Level, src.snapshot.Capacity),
		})
	}

	// 每台设备：最高序号锚点为基准，其后的计数有效，其前的计数显式排除。
	devIDs := make([]string, 0, len(src.devices))
	for devID := range src.devices {
		devIDs = append(devIDs, devID)
	}
	sort.Strings(devIDs)
	for _, devID := range devIDs {
		recs := src.devices[devID]
		var anchorSeq int64
		hasAnchor := false
		for seq, r := range recs {
			if r.anchor != nil && (!hasAnchor || seq > anchorSeq) {
				hasAnchor, anchorSeq = true, seq
			}
		}
		seqs := make([]int64, 0, len(recs))
		for seq := range recs {
			seqs = append(seqs, seq)
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		for _, seq := range seqs {
			r := recs[seq]
			lateTag := ""
			if r.late {
				lateTag = "（迟到上报，仍按发生时间纳入）"
			}
			switch {
			case r.anchor != nil:
				out = append(out, EvidenceItem{
					EventID: r.e.EventID, Type: EvCountAnchor,
					OccurredAt: r.e.OccurredAt.Format(time.RFC3339), Used: true,
					Detail: fmt.Sprintf("设备 %s 序号 %d 计数锚点：在场真值 %d（%s）%s",
						devID, seq, r.anchor.Occupancy, orDefault(r.anchor.Reason, "现场核验"), lateTag),
				})
			case hasAnchor && seq < anchorSeq:
				out = append(out, EvidenceItem{
					EventID: r.e.EventID, Type: EvCountDelta,
					OccurredAt: r.e.OccurredAt.Format(time.RFC3339), Used: false,
					Detail: fmt.Sprintf("设备 %s 序号 %d 的计数被序号 %d 的锚点取代，不参与计算", devID, seq, anchorSeq),
				})
			default:
				out = append(out, EvidenceItem{
					EventID: r.e.EventID, Type: EvCountDelta,
					OccurredAt: r.e.OccurredAt.Format(time.RFC3339), Used: true,
					Detail: fmt.Sprintf("设备 %s 序号 %d 有效计数：进 %d / 出 %d%s",
						devID, seq, r.delta.Enter, r.delta.Leave, lateTag),
				})
			}
		}
	}

	if in := src.activeIncident(); in != nil {
		out = append(out, EvidenceItem{
			EventID: in.e.EventID, Type: EvIncident,
			OccurredAt: in.e.OccurredAt.Format(time.RFC3339), Used: true,
			Detail: fmt.Sprintf("生效中的管制事件：%s（%s）", in.in.Priority, orDefault(in.in.Note, "无备注")),
		})
	}

	if gaps := p.gapsFor(src); len(gaps) > 0 {
		parts := []string{}
		for _, g := range gaps {
			parts = append(parts, fmt.Sprintf("设备 %s 缺序号 %v（水位 %d）", g.DeviceID, g.Missing, g.MaxSeq))
		}
		out = append(out, EvidenceItem{
			Type: "device.gap", Used: true, OccurredAt: p.asOf.Format(time.RFC3339),
			Detail: "计数缺口提示，占用为下限估计：" + strings.Join(parts, "；"),
		})
	}

	if p.stale(src) {
		out = append(out, EvidenceItem{
			Type: "data.stale", Used: true, OccurredAt: p.asOf.Format(time.RFC3339),
			Detail: fmt.Sprintf("最近计数距今 %d 秒，超过 %v，判定按失效数据降权", int64(p.asOf.Sub(src.lastCount).Seconds()), StaleThreshold),
		})
	}
	return out
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ---- 计数速率与导流归因 ----

func enterRate(v *venueState, from, to time.Time) (float64, int) {
	count := 0
	for _, recs := range v.devices {
		for _, r := range recs {
			if r.anchor != nil {
				continue
			}
			t := r.e.OccurredAt
			if t.After(from) && !t.After(to) {
				count += r.delta.Enter
			}
		}
	}
	mins := to.Sub(from).Minutes()
	if mins <= 0 {
		return 0, count
	}
	return float64(count) / mins, count
}

// computeAttributions 回答“哪一次导流通知造成了新的拥堵”：
// 对每条已接受导流，比较指令前后目标点位的入场速率，
// 速率显著抬升且目标在观察窗内产生新告警时，建立归因。
func (p *Projection) computeAttributions() {
	p.attributions = nil
	for _, id := range p.directiveOrder {
		rec := p.directives[id]
		if rec.proposal == nil || rec.decision == nil {
			continue
		}
		var dp DirectiveProposal
		if json.Unmarshal(rec.proposal.Payload, &dp) != nil || dp.Kind != KindGuide {
			continue
		}
		var d DecisionPayload
		if json.Unmarshal(rec.decision.Payload, &d) != nil || d.Decision != DecisionAccept {
			continue
		}
		t0 := rec.decision.OccurredAt
		preFrom, preTo := t0.Add(-AttributionWindow), t0
		postFrom, postTo := t0, t0.Add(AttributionWindow)
		for _, tgt := range dp.Targets {
			tv := p.venues[tgt.VenueID]
			if tv == nil {
				continue
			}
			preRate, _ := enterRate(tv, preFrom, preTo)
			postRate, _ := enterRate(tv, postFrom, postTo)
			if postRate < AttributionFactor*preRate || postRate-preRate < AttributionMinJump {
				continue
			}
			// 找到观察窗内由“导流后入场”触发的新告警（非管制引发）。
			for i := range tv.alarms {
				a := &tv.alarms[i]
				if a.startedAt.Before(postFrom) || a.startedAt.After(postTo) {
					continue
				}
				if a.triggerType == EvIncident {
					continue // 管制本身导致的告警不归因于导流
				}
				alarmID := a.id
				note := fmt.Sprintf("导流指令 %s 生效后，%s 入场速率由 %.1f 人/分升至 %.1f 人/分，并于 %s 触发%s告警",
					dp.DirectiveID, tgt.Name, preRate, postRate, a.startedAt.Format(time.RFC3339), a.priority)
				p.attributions = append(p.attributions, Attribution{
					DirectiveID: dp.DirectiveID, SourceVenue: dp.VenueID, TargetVenue: tgt.VenueID,
					AlarmID: alarmID, PreRate: preRate, PostRate: postRate, Note: note,
				})
			}
		}
	}
}

// ---- 水位缺口 ----

const maxGapList = 100

func (p *Projection) gapsFor(v *venueState) []DeviceGap {
	var out []DeviceGap
	devs := make([]string, 0, len(v.devices))
	for d := range v.devices {
		devs = append(devs, d)
	}
	sort.Strings(devs)
	for _, d := range devs {
		recs := v.devices[d]
		var minSeq, maxSeq int64
		first := true
		for seq := range recs {
			if first || seq < minSeq {
				minSeq = seq
			}
			if seq > maxSeq {
				maxSeq = seq
			}
			first = false
		}
		want := maxSeq - minSeq + 1
		if int64(len(recs)) == want || want <= 1 {
			continue
		}
		var missing []int64
		for seq := minSeq; seq < maxSeq && len(missing) < maxGapList; seq++ {
			if _, ok := recs[seq]; !ok {
				missing = append(missing, seq)
			}
		}
		out = append(out, DeviceGap{VenueID: v.id, DeviceID: d, MaxSeq: maxSeq, NextExpected: maxSeq + 1, Missing: missing})
	}
	return out
}

// ---- 授权分发闸门 ----

type AuthzDecision struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}

// Authorize 内容分发闸门：默认拒绝；必须存在覆盖该时刻且未撤销的授权窗口。
// 点位处于管制/紧急疏散或暂停入场时，即使授权有效也停止分发。
func (p *Projection) Authorize(venueID, contentID string, at time.Time) AuthzDecision {
	v := p.venues[venueID]
	if v == nil || v.snapshot == nil {
		return AuthzDecision{false, "点位未注册，默认拒绝分发"}
	}
	if p.held(v, at) {
		return AuthzDecision{false, "点位处于管制/疏散或暂停入场状态，停止一切内容分发"}
	}
	m := p.grants[venueID]
	if m == nil {
		return AuthzDecision{false, "无任何授权记录，默认拒绝分发"}
	}
	g := m[contentID]
	if g == nil {
		return AuthzDecision{false, fmt.Sprintf("内容 %s 未授权，拒绝分发", contentID)}
	}
	if g.revoked {
		return AuthzDecision{false, fmt.Sprintf("授权已于 %s 被撤销（%s）", g.revokeAt.Format(time.RFC3339), orDefault(g.revokeReason, "未说明"))}
	}
	if at.Before(g.g.Start) {
		return AuthzDecision{false, fmt.Sprintf("授权尚未生效（开始于 %s）", g.g.Start.Format(time.RFC3339))}
	}
	if at.After(g.g.End) || at.Equal(g.g.End) {
		return AuthzDecision{false, fmt.Sprintf("授权已于 %s 过期，画面不得继续分发", g.g.End.Format(time.RFC3339))}
	}
	return AuthzDecision{true, fmt.Sprintf("授权有效（%s 至 %s）", g.g.Start.Format(time.RFC3339), g.g.End.Format(time.RFC3339))}
}

// LogEnforcement 记录一次闸门拒绝（包括过期授权尝试），用于审计。
func (eng *Engine) LogEnforcement(sessionID, venueID, contentID, reason string, at time.Time) error {
	eng.mu.Lock()
	defer eng.mu.Unlock()
	raw, _ := json.Marshal(EnforcementPayload{VenueID: venueID, ContentID: contentID, Reason: reason})
	_, err := eng.log.Append(Event{
		EventID: newEventID(), Type: EvEnforcement, SessionID: sessionID, VenueID: venueID,
		OccurredAt: at, ReceivedAt: at, Payload: raw,
	})
	return err
}

// ---- 运营决策（不可变审计） ----

// Decide 记录运营对建议的接受/驳回。接受时追加 directive.issued 生效事件。
func (eng *Engine) Decide(sessionID, directiveID string, decision Decision, operator, reason string, now time.Time) error {
	if !validDecisions[decision] {
		return fmt.Errorf("非法决策: %q", decision)
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	p := eng.Project(sessionID, now)
	rec := p.directives[directiveID]
	if rec == nil || rec.proposal == nil {
		return fmt.Errorf("指令 %s 不存在", directiveID)
	}
	var dp DirectiveProposal
	_ = json.Unmarshal(rec.proposal.Payload, &dp)
	if now.After(dp.ProposedAt.Add(time.Duration(dp.TTLSeconds) * time.Second)) {
		return fmt.Errorf("指令 %s 已超过 %d 秒决策有效期", directiveID, dp.TTLSeconds)
	}
	if rec.decision != nil {
		var d DecisionPayload
		_ = json.Unmarshal(rec.decision.Payload, &d)
		return fmt.Errorf("指令 %s 已有不可变决策：%s（操作人 %s）", directiveID, d.Decision, d.Operator)
	}
	payload, _ := json.Marshal(DecisionPayload{
		DirectiveID: directiveID, Decision: decision, Operator: operator, Reason: reason,
	})
	if _, err := eng.log.Append(Event{
		EventID: newEventID(), Type: EvDecision, SessionID: sessionID, VenueID: dp.VenueID,
		OccurredAt: now, ReceivedAt: now, Operator: operator, Payload: payload,
	}); err != nil {
		return err
	}
	if decision == DecisionAccept {
		issued, _ := json.Marshal(dp)
		_, err := eng.log.Append(Event{
			EventID: newEventID(), Type: EvIssued, SessionID: sessionID, VenueID: dp.VenueID,
			OccurredAt: now, ReceivedAt: now, Operator: operator, Payload: issued,
		})
		return err
	}
	return nil
}
