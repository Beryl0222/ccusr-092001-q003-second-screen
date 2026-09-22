// 汇总视图：把投影渲染为运营接口使用的只读结构。
package main

import (
	"encoding/json"
	"time"
)

func (p *Projection) Summary() SummaryView {
	view := SummaryView{
		SessionID:    p.sessionID,
		AsOf:         p.asOf.Format(time.RFC3339),
		Venues:       []VenueView{},
		Alarms:       []AlarmView{},
		Directives:   []DirectiveView{},
		Attributions: []Attribution{},
		Gaps:         []DeviceGap{},
		Excluded:     append([]ExcludedItem{}, p.exclusions...),
	}

	for _, id := range p.order {
		v := p.venues[id]
		var name, level string
		var cap int
		registered := v.snapshot != nil
		if registered {
			name, level, cap = v.snapshot.Name, string(v.snapshot.Level), v.snapshot.Capacity
		}
		r := v.ratio()
		heat := HeatGreen
		if registered {
			heat = heatOf(r)
		}
		incident := ""
		if in := v.activeIncident(); in != nil {
			incident = string(in.in.Priority)
		}
		vv := VenueView{
			VenueID: id, Name: name, Level: level, Capacity: cap,
			Occupancy: v.occupancy, Ratio: round2(r), Heat: string(heat),
			Held: p.held(v, p.asOf), Incident: incident, Registered: registered,
			Stale: p.stale(v),
		}
		if !v.lastCount.IsZero() {
			vv.LastCountAt = v.lastCount.Format(time.RFC3339)
			vv.DataFreshnessS = int64(p.asOf.Sub(v.lastCount).Seconds())
		}
		// 管制/疏散期间热度强制为红，避免运营误读。
		if vv.Held && incident != "" {
			heat = HeatRed
			vv.Heat = string(heat)
		}
		view.Venues = append(view.Venues, vv)
		view.Gaps = append(view.Gaps, p.gapsFor(v)...)

		for i := range v.alarms {
			a := &v.alarms[i]
			av := AlarmView{
				AlarmID: a.id, VenueID: id, VenueName: name, Priority: string(a.priority),
				StartedAt: a.startedAt.Format(time.RFC3339), TriggerEventID: a.triggerEventID,
				Active: a.active, Reason: a.reason,
			}
			if !a.endedAt.IsZero() {
				av.EndedAt = a.endedAt.Format(time.RFC3339)
			}
			view.Alarms = append(view.Alarms, av)
		}
	}

	// 归因标注到对应告警。
	attrByAlarm := map[string]int{}
	for i, at := range p.attributions {
		attrByAlarm[at.TargetVenue+"|"+at.AlarmID] = i
	}
	for i := range view.Alarms {
		if idx, ok := attrByAlarm[view.Alarms[i].VenueID+"|"+view.Alarms[i].AlarmID]; ok {
			at := p.attributions[idx]
			view.Alarms[i].AttributedTo = at.DirectiveID
			view.Alarms[i].AttributionNote = at.Note
			view.Alarms[i].PreEnterRate = round2(at.PreRate)
			view.Alarms[i].PostEnterRate = round2(at.PostRate)
		}
	}
	view.Attributions = append([]Attribution{}, p.attributions...)

	for _, id := range p.directiveOrder {
		rec := p.directives[id]
		if rec.proposal == nil {
			continue
		}
		var dp DirectiveProposal
		_ = json.Unmarshal(rec.proposal.Payload, &dp)
		dv := DirectiveView{
			DirectiveID: dp.DirectiveID, Kind: dp.Kind, Priority: dp.Priority,
			VenueID: dp.VenueID, VenueName: dp.VenueName, Targets: dp.Targets,
			Reason: dp.Reason, ProposedAt: dp.ProposedAt.Format(time.RFC3339),
			TTLSeconds: dp.TTLSeconds, Evidence: dp.Evidence,
		}
		expiry := dp.ProposedAt.Add(time.Duration(dp.TTLSeconds) * time.Second)
		switch {
		case rec.decision != nil:
			var d DecisionPayload
			_ = json.Unmarshal(rec.decision.Payload, &d)
			dv.Operator = d.Operator
			dv.DecisionRsn = d.Reason
			dv.DecidedAt = rec.decision.OccurredAt.Format(time.RFC3339)
			switch {
			case d.Decision == DecisionReject:
				dv.Status = "已驳回"
			case hasReceipt(rec.receipts, RcptExecuted):
				dv.Status = "已接受·已执行"
			case p.asOf.After(expiry):
				// 接受后超过有效期仍无“已执行”回执，按领域回执口径标记过期。
				dv.Status = "已过期（已接受未执行）"
			default:
				dv.Status = "已接受·待执行"
			}
		case p.asOf.After(expiry):
			dv.Status = "已过期"
		default:
			dv.Status = "待决策"
		}
		if len(rec.receipts) > 0 {
			dv.Receipts = map[string]string{}
			for k, rc := range rec.receipts {
				dv.Receipts[k] = string(rc)
			}
		}
		view.Directives = append(view.Directives, dv)
	}
	return view
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}

func hasReceipt(rs map[string]Receipt, want Receipt) bool {
	for _, r := range rs {
		if r == want {
			return true
		}
	}
	return false
}
