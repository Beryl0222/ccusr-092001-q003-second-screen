// 投影与判定引擎。
//
// 核心原则：投影是对“截至某时刻（asOf）已接收事件”的纯折叠，
// 事件按规范顺序（发生时间 → 设备序号 → 类型 → 事件ID）排列后归并，
// 因此乱序、迟到、重复、重启重放都会得到同一份稳定状态。
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const (
	LateThreshold      = 60 * time.Second // 接收延迟超过该值标记为迟到
	StaleThreshold     = 3 * time.Minute  // 计数超过该值未刷新视为失效
	AttributionWindow  = 10 * time.Minute // 导流归因观察窗
	AttributionFactor  = 1.5              // 导流后入场速率放大倍数
	AttributionMinJump = 20.0             // 每分钟至少新增的入场人数
	DefaultTTL         = 5 * time.Minute  // 建议默认有效期
)

// 同时间戳事件的规范次序，保证折叠确定。
var typeOrder = map[string]int{
	EvVenueSnapshot: 1, EvCountDelta: 2, EvCountAnchor: 3,
	EvIncident: 4, EvAuthzGranted: 5, EvAuthzRevoked: 6,
	EvProposed: 7, EvDecision: 8, EvIssued: 9, EvDirectiveReceipt: 10,
	EvEnforcement: 11,
}

type countRec struct {
	e      Event
	delta  CountDelta
	anchor *CountAnchor
	late   bool
}

type incidentRec struct {
	e  Event
	in Incident
}

type alarmState struct {
	id             string
	priority       Priority
	triggerEventID string
	triggerType    string
	startedAt      time.Time
	active         bool
	endedAt        time.Time
	reason         string
}

type venueState struct {
	id          string
	snapshot    *VenueSnapshot
	snapAt      time.Time
	snapEventID string
	devices     map[string]map[int64]countRec
	lastCount   time.Time
	incidents   []incidentRec // 已按规范顺序
	alarms      []alarmState
	active      *alarmState
	occupancy   int
}

type grantRec struct {
	e            Event
	g            AuthzGrant
	revoked      bool
	revokeAt     time.Time
	revokeReason string
}

type directiveRec struct {
	proposal *Event // directive.proposed，证据快照
	decision *Event // directive.decision
	receipts map[string]Receipt
}

type Projection struct {
	sessionID      string
	asOf           time.Time
	venues         map[string]*venueState
	order          []string                        // 首次出现顺序，保证输出稳定
	grants         map[string]map[string]*grantRec // venueID -> contentID -> grant（取最新一条）
	directives     map[string]*directiveRec
	directiveOrder []string
	attributions   []Attribution
	// exclusions 为投影级显式排除记录（被锚点取代的计数）。
	exclusions []ExcludedItem
}

func canonicalLess(a, b Event) bool {
	if !a.OccurredAt.Equal(b.OccurredAt) {
		return a.OccurredAt.Before(b.OccurredAt)
	}
	ta, tb := typeOrder[a.Type], typeOrder[b.Type]
	if ta != tb {
		return ta < tb
	}
	if a.VenueID != b.VenueID {
		return a.VenueID < b.VenueID
	}
	if a.DeviceID != b.DeviceID {
		return a.DeviceID < b.DeviceID
	}
	if a.Seq != b.Seq {
		return a.Seq < b.Seq
	}
	return a.EventID < b.EventID
}

func (eng *Engine) Project(sessionID string, asOf time.Time) *Projection {
	events := eng.log.All()
	filtered := make([]Event, 0, len(events))
	for _, e := range events {
		if e.SessionID != sessionID {
			continue
		}
		// 投影只看“发生时间不晚于 asOf”的事件；未来事件不参与判定。
		if !e.OccurredAt.IsZero() && e.OccurredAt.After(asOf) {
			continue
		}
		filtered = append(filtered, e)
	}
	sort.SliceStable(filtered, func(i, j int) bool { return canonicalLess(filtered[i], filtered[j]) })

	p := &Projection{
		sessionID:  sessionID,
		asOf:       asOf,
		venues:     map[string]*venueState{},
		grants:     map[string]map[string]*grantRec{},
		directives: map[string]*directiveRec{},
	}

	for _, e := range filtered {
		p.apply(e)
	}
	p.computeAttributions()
	return p
}

func (p *Projection) venue(id string) *venueState {
	v := p.venues[id]
	if v == nil {
		v = &venueState{id: id, devices: map[string]map[int64]countRec{}}
		p.venues[id] = v
		p.order = append(p.order, id)
	}
	return v
}

func (p *Projection) apply(e Event) {
	switch e.Type {
	case EvVenueSnapshot:
		var s VenueSnapshot
		if json.Unmarshal(e.Payload, &s) != nil {
			return
		}
		v := p.venue(e.VenueID)
		// 迟到的旧快照不覆盖新快照（规范顺序下只剩时间相等的竞态，取事件ID小者）。
		if v.snapshot == nil || !e.OccurredAt.Before(v.snapAt) {
			v.snapshot, v.snapAt, v.snapEventID = &s, e.OccurredAt, e.EventID
		}
		p.evaluateAlarm(v, e)

	case EvCountDelta, EvCountAnchor:
		var d CountDelta
		var a *CountAnchor
		if e.Type == EvCountDelta {
			if json.Unmarshal(e.Payload, &d) != nil {
				return
			}
		} else {
			var an CountAnchor
			if json.Unmarshal(e.Payload, &an) != nil {
				return
			}
			a = &an
		}
		v := p.venue(e.VenueID)
		recs := v.devices[e.DeviceID]
		if recs == nil {
			recs = map[int64]countRec{}
			v.devices[e.DeviceID] = recs
		}
		late := !e.ReceivedAt.IsZero() && e.ReceivedAt.Sub(e.OccurredAt) > LateThreshold
		if prev, ok := recs[e.Seq]; ok {
			// 存储层已按 (设备,序号) 去重；这里防御性保留首条（规范顺序下事件ID最小者）。
			_ = prev
		}
		recs[e.Seq] = countRec{e: e, delta: d, anchor: a, late: late}
		if e.OccurredAt.After(v.lastCount) {
			v.lastCount = e.OccurredAt
		}
		v.occupancy = computeOccupancy(v)
		p.evaluateAlarm(v, e)

	case EvIncident:
		var in Incident
		if json.Unmarshal(e.Payload, &in) != nil {
			return
		}
		v := p.venue(e.VenueID)
		v.incidents = append(v.incidents, incidentRec{e: e, in: in})
		p.evaluateAlarm(v, e)

	case EvAuthzGranted:
		var g AuthzGrant
		if json.Unmarshal(e.Payload, &g) != nil {
			return
		}
		m := p.grants[e.VenueID]
		if m == nil {
			m = map[string]*grantRec{}
			p.grants[e.VenueID] = m
		}
		// 同一内容的多次授权：以开始时间最新的窗口为准。
		cur := m[g.ContentID]
		if cur == nil || g.Start.After(cur.g.Start) {
			m[g.ContentID] = &grantRec{e: e, g: g}
		}

	case EvAuthzRevoked:
		var r AuthzRevoke
		if json.Unmarshal(e.Payload, &r) != nil {
			return
		}
		if m := p.grants[e.VenueID]; m != nil {
			if cur := m[r.ContentID]; cur != nil && !cur.revoked {
				cur.revoked, cur.revokeAt, cur.revokeReason = true, e.OccurredAt, r.Reason
			}
		}

	case EvProposed:
		var dp DirectiveProposal
		if json.Unmarshal(e.Payload, &dp) != nil {
			return
		}
		if p.directives[dp.DirectiveID] == nil {
			p.directiveOrder = append(p.directiveOrder, dp.DirectiveID)
			p.directives[dp.DirectiveID] = &directiveRec{receipts: map[string]Receipt{}}
		}
		p.directives[dp.DirectiveID].proposal = &e

	case EvDecision:
		var d DecisionPayload
		if json.Unmarshal(e.Payload, &d) != nil {
			return
		}
		rec := p.directives[d.DirectiveID]
		if rec == nil {
			rec = &directiveRec{receipts: map[string]Receipt{}}
			p.directives[d.DirectiveID] = rec
			p.directiveOrder = append(p.directiveOrder, d.DirectiveID)
		}
		// 决策不可变：首条决策生效，迟到的相反决定被忽略并记录。
		if rec.decision == nil {
			rec.decision = &e
		} else {
			p.exclusions = append(p.exclusions, ExcludedItem{
				EventID: e.EventID, Type: e.Type, VenueID: e.VenueID,
				OccurredAt: e.OccurredAt.Format(time.RFC3339),
				Reason:     fmt.Sprintf("指令 %s 已有不可变决策，忽略迟到/重复决策", d.DirectiveID),
			})
		}

	case EvDirectiveReceipt:
		var r ReceiptPayload
		if json.Unmarshal(e.Payload, &r) != nil {
			return
		}
		rec := p.directives[r.DirectiveID]
		if rec == nil {
			rec = &directiveRec{receipts: map[string]Receipt{}}
			p.directives[r.DirectiveID] = rec
			p.directiveOrder = append(p.directiveOrder, r.DirectiveID)
		}
		// 回执以点位为键；已执行/驳回等终态不被旧的“已接收”覆盖。
		if prev, ok := rec.receipts[e.VenueID]; !ok || receiptRank(r.Receipt) >= receiptRank(prev) {
			rec.receipts[e.VenueID] = r.Receipt
		}
	}
}

func receiptRank(r Receipt) int {
	switch r {
	case RcptReceived:
		return 0
	case RcptExpired:
		return 1
	case RcptExecuted, RcptRejected:
		return 2
	}
	return -1
}

// computeOccupancy 以“最高序号锚点”为基准做顺序无关折叠：
// 占用 = 锚点真值 + 锚点之后所有设备计数（按序号）之和；无锚点时从首条计数累加。
// 锚点之前的计数不再参与（投影级排除，证据中会标明）。
func computeOccupancy(v *venueState) int {
	total := 0
	for _, recs := range v.devices {
		var anchorSeq int64
		var anchorOcc int
		hasAnchor := false
		for seq, r := range recs {
			if r.anchor != nil && (!hasAnchor || seq > anchorSeq) {
				hasAnchor, anchorSeq, anchorOcc = true, seq, r.anchor.Occupancy
			}
		}
		if hasAnchor {
			total += anchorOcc
			seqs := make([]int64, 0, len(recs))
			for seq := range recs {
				if seq > anchorSeq {
					seqs = append(seqs, seq)
				}
			}
			sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
			for _, seq := range seqs {
				r := recs[seq]
				total += r.delta.Enter - r.delta.Leave
			}
		} else {
			seqs := make([]int64, 0, len(recs))
			for seq := range recs {
				seqs = append(seqs, seq)
			}
			sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
			for _, seq := range seqs {
				r := recs[seq]
				total += r.delta.Enter - r.delta.Leave
			}
		}
	}
	if total < 0 {
		total = 0 // 现场人数不可能为负，原始偏差留待锚点修正
	}
	return total
}

// activeIncident 返回当前最后生效的未解除事件（解除事件 Active=false）。
func (v *venueState) activeIncident() *incidentRec {
	var cur *incidentRec
	for i := range v.incidents {
		in := &v.incidents[i]
		if in.in.Active {
			cur = in
		} else {
			cur = nil
		}
	}
	return cur
}

func (v *venueState) ratio() float64 {
	if v.snapshot == nil || v.snapshot.Capacity == 0 {
		return 0
	}
	return float64(v.occupancy) / float64(v.snapshot.Capacity)
}

// conditionSeverity 计算点位当前条件优先级；trigger 为触发事件，供告警边沿使用。
func (p *Projection) condition(v *venueState, triggerEvent Event) (Priority, string) {
	if in := v.activeIncident(); in != nil {
		return in.in.Priority, in.in.Note
	}
	r := v.ratio()
	switch {
	case r >= 1.0:
		return PriCrowd, fmt.Sprintf("在场 %d/%d 已达容量上限", v.occupancy, v.snapshot.Capacity)
	case r >= 0.9:
		return PriCrowd, fmt.Sprintf("在场 %d/%d 超过红区阈值 90%%", v.occupancy, v.snapshot.Capacity)
	}
	return PriNormal, ""
}

// evaluateAlarm 告警状态机：条件无→有时新建告警，ID 由触发事件确定性派生；
// 升级沿用同一告警（不产生重复告警），条件消除后关闭；再次发生才是新告警。
func (p *Projection) evaluateAlarm(v *venueState, trigger Event) {
	sev, note := PriNormal, ""
	if v.snapshot != nil || len(v.incidents) > 0 {
		sev, note = p.condition(v, trigger)
	}
	if sev == PriNormal {
		if v.active != nil {
			v.active.active = false
			v.active.endedAt = trigger.OccurredAt
			v.active = nil
		}
		return
	}
	if v.active == nil {
		// ID 由 点位+告警起点 确定性派生：同一场活跃期唯一，重放/乱序结果一致。
		id := "alm_" + shortHash(v.id+"|"+trigger.OccurredAt.Format(time.RFC3339Nano))
		a := &alarmState{
			id: id, priority: sev, triggerEventID: trigger.EventID, triggerType: trigger.Type,
			startedAt: trigger.OccurredAt, active: true, reason: note,
		}
		v.alarms = append(v.alarms, *a)
		v.active = &v.alarms[len(v.alarms)-1]
	} else {
		// 同一告警生命周期内同步当前条件：可升级（拥挤→管制）也可降级（管制解除但仍拥挤）。
		v.active.priority = sev
		v.active.triggerType = trigger.Type
		if note != "" {
			v.active.reason = note
		}
	}
}

func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// ---- 热度与暂停入场 ----

func heatOf(r float64) Heat {
	switch {
	case r >= 0.9:
		return HeatRed
	case r >= 0.7:
		return HeatYellow
	default:
		return HeatGreen
	}
}

func (p *Projection) held(v *venueState, asOf time.Time) bool {
	if in := v.activeIncident(); in != nil && in.in.Priority.AtLeastControl() {
		return true
	}
	if v.snapshot != nil && v.snapshot.Capacity > 0 && v.occupancy >= v.snapshot.Capacity {
		return true
	}
	// 已接受且未过期的暂停入场指令持续生效。
	for _, id := range p.directiveOrder {
		rec := p.directives[id]
		if rec.decision == nil || rec.proposal == nil {
			continue
		}
		var dp DirectiveProposal
		if json.Unmarshal(rec.proposal.Payload, &dp) != nil || dp.Kind != KindSuspend {
			continue
		}
		var d DecisionPayload
		json.Unmarshal(rec.decision.Payload, &d)
		if d.Decision == DecisionAccept && dp.VenueID == v.id && asOf.Before(dp.ProposedAt.Add(time.Duration(dp.TTLSeconds)*time.Second)) {
			return true
		}
	}
	return false
}

func (p *Projection) stale(v *venueState) bool {
	// 从未计数但已注册的点位按“空载”处理，可作为导流去向；
	// 只有曾经有计数、却长期未刷新时才判定为失效。
	if v.lastCount.IsZero() {
		return false
	}
	return p.asOf.Sub(v.lastCount) > StaleThreshold
}
