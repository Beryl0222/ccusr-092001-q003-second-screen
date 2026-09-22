package engine

import (
	"sort"
	"strconv"
	"time"

	"github.com/beryl0222/city-second-screen/model"
)

// counterBatch 表示一批参与占用求和的计数（原始上报或修正后的最终值）。
type counterBatch struct {
	eventID       string
	device        string
	seq           int64
	occ           time.Time // 计入占用时的落位时刻（修正批次对齐到被修正批次）
	reportOcc     time.Time // 本批数据实际到达的发生时刻（用于知识新鲜度）
	recv          time.Time // 服务端接收时刻（用于判断迟到数据是否在最近窗口内到达）
	enter         int
	exit          int
	conf          int
	late          bool
	correctedFrom string // 非空表示这批数据是对另一批的修正
}

type snapRec struct {
	ev *model.Event
	p  *model.SnapshotPayload
}

type incRec struct {
	ev *model.Event
	p  *model.IncidentPayload
}
type authRec struct {
	ev *model.Event
	p  *model.AuthorizePayload
}
type rcpRec struct {
	ev *model.Event
	p  *model.ReceiptPayload
}
type ackRec struct {
	ev *model.Event
	p  *model.DecisionAckPayload
}
type hbRec struct {
	ev *model.Event
	p  *model.HeartbeatPayload
}

// foldSession 处理单场赛事内全部已按 (occ_time,event_id) 排好序的事件。
func foldSession(sid string, events []*model.Event, cfg Config, asOf time.Time) *SessionState {
	st := &SessionState{
		SessionID: sid,
		Venues:    map[string]*VenueState{},
		Receipts:  map[string]*ReceiptView{},
	}

	venueMeta := map[string]*model.RegisterPayload{}
	var clearBefore time.Time // 最近一次 clear_session 的 occ_time；不晚于它的动态数据作废

	counterSeen := map[string]*counterBatch{}  // event_id -> 批次
	correctionOf := map[string]*counterBatch{} // 被修正事件 id -> 最新修正批次
	var incidents []incRec
	var auths []authRec
	var receipts []rcpRec
	var acks []ackRec
	var heartbeats []hbRec
	idIndex := map[string]*model.Event{}

	venue := func(id string) *VenueState {
		v := st.Venues[id]
		if v == nil {
			v = &VenueState{VenueID: id, SessionID: sid, Ratio: -1, Online: true}
			st.Venues[id] = v
		}
		return v
	}

	// 第一遍：分类。clear_session 重置其后的所有动态台账；回执与审计是
	// 跨重置的合规记录，不予清空。
	for _, ev := range events {
		idIndex[ev.EventID] = ev
		if ev.OccTime.After(asOf) {
			continue
		}
		switch ev.Type {
		case model.TypeClear:
			clearBefore = ev.OccTime
			if ev.Clear == nil || !ev.Clear.KeepConfig {
				venueMeta = map[string]*model.RegisterPayload{}
			}
			counterSeen = map[string]*counterBatch{}
			correctionOf = map[string]*counterBatch{}
			incidents = nil
			auths = nil
			heartbeats = nil
		case model.TypeVenueRegister:
			if !lteq(ev.OccTime, clearBefore) {
				venueMeta[ev.VenueID] = ev.Register
				venue(ev.VenueID)
			}
		case model.TypeIncident:
			if !lteq(ev.OccTime, clearBefore) {
				incidents = append(incidents, incRec{ev, ev.Incident})
			}
		case model.TypeAuthorize:
			if !lteq(ev.OccTime, clearBefore) {
				auths = append(auths, authRec{ev, ev.Authorize})
			}
		case model.TypeReceipt:
			receipts = append(receipts, rcpRec{ev, ev.Receipt})
		case model.TypeDecisionAck:
			acks = append(acks, ackRec{ev, ev.DecisionAck})
		case model.TypeHeartbeat:
			if !lteq(ev.OccTime, clearBefore) {
				heartbeats = append(heartbeats, hbRec{ev, ev.Heartbeat})
			}
		}
	}

	// 第二遍：收集 clear 之后的计数与快照（事件已按 occ 排序，修正登记因此
	// 天然“后者覆盖前者”）。
	snapsByVenue := map[string][]snapRec{}
	excludedByVenue := map[string][]ExcludedPoint{}
	addExcluded := func(ev *model.Event, reason string) {
		excludedByVenue[ev.VenueID] = append(excludedByVenue[ev.VenueID], ExcludedPoint{
			EventID: ev.EventID, Type: ev.Type, OccTime: ev.OccTime, Reason: reason,
		})
	}

	for _, ev := range events {
		if ev.OccTime.After(asOf) {
			continue
		}
		if !clearBefore.IsZero() && lteq(ev.OccTime, clearBefore) {
			if ev.Type == model.TypeCount || ev.Type == model.TypeSnapshot {
				addExcluded(ev, "场次重置(clear_session)之前的数据")
			}
			continue
		}
		switch ev.Type {
		case model.TypeSnapshot:
			snapsByVenue[ev.VenueID] = append(snapsByVenue[ev.VenueID], snapRec{ev, ev.Snapshot})
		case model.TypeCount:
			p := ev.Count
			ent, ex := p.Enter, p.Exit
			for _, g := range p.Gates {
				ent += g.Enter
				ex += g.Exit
			}
			b := &counterBatch{
				eventID: ev.EventID, device: ev.DeviceID, seq: ev.Seq,
				occ: ev.OccTime, reportOcc: ev.OccTime, recv: ev.ReceivedAt,
				enter: ent, exit: ex, conf: p.Confidence,
				late:          ev.ReceivedAt.Sub(ev.OccTime) > cfg.LateThreshold,
				correctedFrom: p.CorrectsEventID,
			}
			if p.CorrectsEventID != "" {
				correctionOf[p.CorrectsEventID] = b
			}
			counterSeen[ev.EventID] = b
		}
	}

	// 第三遍：按确定顺序（沿用 events 的 occ,id 排序，而非遍历 map）应用
	// 修正/重复/置信度规则，产出每点位有效批次。
	batchesByVenue := map[string][]*counterBatch{}
	deviceMaxSeq := map[string]int64{}
	deviceSeqOwner := map[string]string{} // device|seq -> 首个持有者

	for _, ev := range events {
		b, ok := counterSeen[ev.EventID]
		if !ok {
			continue
		}
		// 规则 A：本批次已被更新的修正取代 -> 整体作废。
		if corr, superseded := correctionOf[ev.EventID]; superseded {
			addExcluded(ev, "已被计数修正事件 "+corr.eventID+" 取代")
			delete(counterSeen, ev.EventID)
			continue
		}
		// 规则 B：同一设备序号去重（seq>0 才启用序号语义）。
		if b.seq > 0 && b.device != "" {
			dsKey := b.device + "|" + strconv.FormatInt(b.seq, 10)
			if b.seq <= deviceMaxSeq[b.device] {
				addExcluded(ev, "同一设备重复上报(seq 已处理过)")
				delete(counterSeen, ev.EventID)
				continue
			}
			if owner, dup := deviceSeqOwner[dsKey]; dup && owner != ev.EventID {
				addExcluded(ev, "同一设备相同序号的重复上报(与 "+owner+")")
				delete(counterSeen, ev.EventID)
				continue
			}
			deviceMaxSeq[b.device] = b.seq
			deviceSeqOwner[dsKey] = ev.EventID
		}
		// 规则 C：置信度过低不作为占用依据。
		if b.conf < 30 {
			addExcluded(ev, "置信度过低(<30)，仅作参考")
			delete(counterSeen, ev.EventID)
			continue
		}
		// 修正批次按“被修正批次的发生时刻”落位，保持时间线一致。
		if b.correctedFrom != "" {
			if orig := counterSeen[b.correctedFrom]; orig != nil {
				b.occ = orig.occ
			} else if o := idIndex[b.correctedFrom]; o != nil {
				b.occ = o.OccTime
			}
		}
		batchesByVenue[ev.VenueID] = append(batchesByVenue[ev.VenueID], b)
	}

	// 汇总需要出现在状态里的全部点位（注册/计数/快照/事件/心跳）。
	allVenue := map[string]bool{}
	for id := range venueMeta {
		allVenue[id] = true
	}
	for id := range batchesByVenue {
		allVenue[id] = true
	}
	for id := range snapsByVenue {
		allVenue[id] = true
	}
	for _, r := range incidents {
		allVenue[r.ev.VenueID] = true
	}
	for _, r := range heartbeats {
		allVenue[r.ev.VenueID] = true
	}
	venueIDs := make([]string, 0, len(allVenue))
	for id := range allVenue {
		venueIDs = append(venueIDs, id)
	}
	sort.Strings(venueIDs)

	for _, vid := range venueIDs {
		v := venue(vid)
		if meta := venueMeta[vid]; meta != nil {
			v.Level = meta.Level
			if meta.Capacity > 0 {
				v.Capacity = meta.Capacity
			}
		}
		buildVenue(v, vid, snapsByVenue[vid], batchesByVenue[vid], excludedByVenue[vid],
			incidents, heartbeats, venueMeta, cfg, asOf, idIndex)
	}

	// 跨点位步骤：导流目标选择、决策 ID 派生、热度兜底。
	finalizeRecommendations(st, venueMeta, cfg)

	buildAlerts(st, incidents, asOf)
	buildContent(st, auths, asOf)
	buildReceipts(st, receipts)
	bindAudit(st, acks)

	return st
}

// lteq: a <= b（b 为零值时视为无上界，返回 false）。
func lteq(a, b time.Time) bool {
	if b.IsZero() {
		return false
	}
	return !a.After(b)
}
