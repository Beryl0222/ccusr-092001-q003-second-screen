package engine

import (
	"fmt"
	"sort"
	"time"

	"github.com/beryl0222/city-second-screen/model"
)

// buildVenue 在不依赖其他点位的前提下，重建单点位占用、质量、热度、生效事件
// 与建议（建议目标点位与决策 ID 在 finalizeRecommendations 中补全）。
func buildVenue(
	v *VenueState, vid string,
	snaps []snapRec, batches []*counterBatch, excluded []ExcludedPoint,
	incidents []incRec, heartbeats []hbRec,
	meta map[string]*model.RegisterPayload,
	cfg Config, asOf time.Time, idIndex map[string]*model.Event,
) {
	var notes []string
	used := []BasisPoint{}
	exc := append([]ExcludedPoint(nil), excluded...)

	// --- 容量：注册值为基础，快照携带的容量可更新 ---
	// snaps 已按 (occ,id) 有序。
	var anchor *snapRec
	var latestCapUpdate *snapRec
	for i := range snaps {
		s := &snaps[i]
		if s.p.Authoritative {
			anchor = s // snaps 已按 (occ,id) 有序，最后一个权威快照胜出
		}
		if s.p.Capacity > 0 {
			latestCapUpdate = s
		}
	}
	if latestCapUpdate != nil {
		v.Capacity = latestCapUpdate.p.Capacity
	}

	// --- 有效计数：严格晚于锚点的批次；锚点及其之前的计数不再计入占用 ---
	var effective []*counterBatch
	anchorOcc := time.Time{}
	if anchor != nil {
		anchorOcc = anchor.ev.OccTime
		used = append(used, BasisPoint{
			EventID: anchor.ev.EventID, Type: model.TypeSnapshot, VenueID: vid,
			OccTime: anchor.ev.OccTime, Role: "anchor",
			Occupancy: anchor.p.Occupancy,
			Detail:    "权威占用快照，作为计数锚点",
		})
		// 较早的权威快照降为参考，非权威快照标参考。
		for i := range snaps {
			s := &snaps[i]
			if s.ev.EventID == anchor.ev.EventID {
				continue
			}
			reason := "非权威快照，仅作参考"
			if s.p.Authoritative {
				reason = "已有更新的权威快照锚点，本快照不再锚定"
			}
			exc = append(exc, ExcludedPoint{
				EventID: s.ev.EventID, Type: model.TypeSnapshot,
				OccTime: s.ev.OccTime, Reason: reason,
			})
		}
	}

	raw := 0
	if anchor != nil {
		raw = anchor.p.Occupancy
	}
	running := raw
	wentNegative := false
	for _, b := range batches {
		if anchor != nil && !b.occ.After(anchorOcc) {
			exc = append(exc, ExcludedPoint{
				EventID: b.eventID, Type: model.TypeCount, OccTime: b.occ,
				Reason: "发生时间早于权威快照锚点，占用以快照为准",
			})
			continue
		}
		effective = append(effective, b)
		running += b.enter - b.exit
		if running < 0 {
			wentNegative = true
		}
		role := "delta"
		detail := ""
		if b.correctedFrom != "" {
			role = "correction"
			detail = "修正批次，替换 " + b.correctedFrom
		}
		used = append(used, BasisPoint{
			EventID: b.eventID, Type: model.TypeCount, VenueID: vid,
			OccTime: b.occ, Late: b.late, Role: role, Detail: detail,
			Enter: b.enter, Exit: b.exit,
		})
		raw += b.enter - b.exit
	}
	v.RawOccupancy = raw

	if wentNegative {
		notes = append(notes, "累计占用一度为负，计数链可能漏报离场，已截断展示")
		v.Anomalies = append(v.Anomalies, "negative_running_occupancy")
	}
	if v.Capacity > 0 && raw > v.Capacity {
		notes = append(notes, fmt.Sprintf("有效计数(%d)超出容量(%d)，建议核检", raw, v.Capacity))
		v.Anomalies = append(v.Anomalies, "over_capacity")
	}
	v.Occupancy = raw
	if v.Occupancy < 0 {
		v.Occupancy = 0
	}
	if v.Capacity > 0 && v.Occupancy > v.Capacity {
		v.Occupancy = v.Capacity
	}
	if v.Capacity > 0 {
		v.Ratio = float64(raw) / float64(v.Capacity)
		if v.Ratio < 0 {
			v.Ratio = 0
		}
		if v.Ratio > 1 {
			v.Ratio = 1
		}
	}

	// --- 最近报数时间（计数/快照/心跳三类信源取最新）---
	var lastReport time.Time
	// 知识新鲜度按“数据实际到达的发生时刻”计算（修正批次虽对齐到旧时刻，
	// 但它代表修正到达时才获得的新知识）。
	for _, b := range effective {
		if b.reportOcc.After(lastReport) {
			lastReport = b.reportOcc
		}
	}
	for i := range snaps {
		if snaps[i].ev.OccTime.After(lastReport) {
			lastReport = snaps[i].ev.OccTime
		}
	}
	recovered := false
	var lastHB *hbRec
	for i := range heartbeats {
		h := &heartbeats[i]
		if h.ev.VenueID != vid {
			continue
		}
		lastHB = h
		if h.p.Online && h.p.GapSeconds > 0 && asOf.Sub(h.ev.OccTime) <= cfg.RecoverWindow {
			recovered = true
			notes = append(notes, fmt.Sprintf("设备/链路曾失联 %ds，已于 %s 恢复，缺口时段数据可能不完整",
				h.p.GapSeconds, h.ev.OccTime.Format("15:04:05")))
		}
		if h.ev.OccTime.After(lastReport) {
			lastReport = h.ev.OccTime
		}
	}
	if lastHB != nil {
		v.Online = lastHB.p.Online
	}
	if !lastReport.IsZero() {
		lr := lastReport
		v.LastReportAt = &lr
	}

	// --- 数据质量 ---
	// 迟到标记只描述“最近窗口内新合并进来的迟到后补数据”；陈旧的迟到历史
	// 不再让当前质量长期停留在 late_arrivals。
	recentLate := false
	for _, b := range effective {
		if b.late && asOf.Sub(b.recv) <= cfg.FreshThreshold {
			recentLate = true
		}
	}
	age := time.Duration(0)
	if !lastReport.IsZero() {
		age = asOf.Sub(lastReport)
	}
	switch {
	case lastReport.IsZero() || age > cfg.StaleThreshold:
		v.Quality = model.QualityStale
		notes = append(notes, "超过容忍窗口无任何有效报数，占用不可采信")
	case age > cfg.GapThreshold:
		v.Quality = model.QualityGap
		notes = append(notes, fmt.Sprintf("最近报数距今 %s，存在数据缺口", age.Round(time.Second)))
	case recovered:
		v.Quality = model.QualityRecovered
	case recentLate:
		v.Quality = model.QualityLateArrivals
		notes = append(notes, "本次评估合并了迟到后补的计数")
	default:
		v.Quality = model.QualityFresh
	}

	// --- 生效事件（仅本点位）---
	active := activeIncidents(incidents, vid, asOf)
	var top *incRec
	for i := range active {
		if top == nil || priorityRank(active[i].p.Priority) > priorityRank(top.p.Priority) {
			top = &active[i]
		}
	}
	if top != nil {
		v.ActiveIncident = &IncidentView{
			Code: top.p.Code, Priority: top.p.Priority,
			Message: top.p.Message, Since: top.ev.OccTime,
		}
		used = append(used, BasisPoint{
			EventID: top.ev.EventID, Type: model.TypeIncident, VenueID: vid,
			OccTime: top.ev.OccTime, Role: "incident",
			Detail: fmt.Sprintf("生效事件：%s/%s", top.p.Priority, top.p.Code),
		})
		if top.p.Priority == model.PriorityControl || top.p.Priority == model.PriorityEvac {
			v.EntryHeld = true
		}
	}

	// --- 建议（目标点位与 DecisionID 在 finalize 阶段补）---
	rec := buildRecommendation(v, used, exc, notes, cfg, asOf, incidents, vid)
	v.Recommendation = rec

	v.QualityNotes = notes
	v.CountsAccepted = len(effective)
	v.CountsExcluded = len(exc)
	sort.Slice(exc, func(i, j int) bool {
		if !exc[i].OccTime.Equal(exc[j].OccTime) {
			return exc[i].OccTime.Before(exc[j].OccTime)
		}
		return exc[i].EventID < exc[j].EventID
	})
	if rec != nil {
		rec.Evidence.Excluded = exc
	}
}

// activeIncidents 返回某点位在 asOf 仍未解除的事件（resolve 事件 occ≤asOf 即生效）。
func activeIncidents(all []incRec, vid string, asOf time.Time) []incRec {
	open := map[string]incRec{}
	var order []string
	for _, r := range all {
		if r.ev.VenueID != vid {
			continue
		}
		code := r.p.Code
		if r.p.ResolvesCode != "" {
			code = r.p.ResolvesCode
		}
		if r.p.Resolved || r.p.ResolvesCode != "" {
			delete(open, code)
			continue
		}
		if _, exists := open[code]; !exists {
			order = append(order, code)
		}
		open[code] = r // 重复上报同一在途事件：更新内容，不产生第二条告警
	}
	out := make([]incRec, 0, len(order))
	for _, c := range order {
		if r, ok := open[c]; ok {
			out = append(out, r)
		}
	}
	return out
}

func priorityRank(p string) int {
	switch p {
	case model.PriorityNormal:
		return 0
	case model.PriorityCrowd:
		return 1
	case model.PriorityControl:
		return 2
	case model.PriorityEvac:
		return 3
	}
	return -1
}

// buildRecommendation 依据已冻结的有效数据产生建议；数据失出可采信范围时，
// 仅保留由权威事件驱动的暂停指令，抑制基于计数的导流/暂停。
func buildRecommendation(
	v *VenueState, used []BasisPoint, exc []ExcludedPoint, notes []string,
	cfg Config, asOf time.Time, incidents []incRec, vid string,
) *Recommendation {
	dataTrusted := v.Quality != model.QualityStale
	var directive string
	var reasons []string
	confidence := "high"

	switch {
	case v.ActiveIncident != nil &&
		(v.ActiveIncident.Priority == model.PriorityControl || v.ActiveIncident.Priority == model.PriorityEvac):
		directive = model.DirectiveHold
		reasons = []string{"生效事件 " + v.ActiveIncident.Priority + "（" + v.ActiveIncident.Code + "）要求暂停入场"}
		confidence = "high"
	case dataTrusted && v.Capacity > 0 && v.Ratio >= cfg.HoldRatio:
		directive = model.DirectiveHold
		reasons = []string{fmt.Sprintf("占用率 %.0f%% 达到暂停阈值 %.0f%%", v.Ratio*100, cfg.HoldRatio*100)}
		confidence = countConfidence(v.Quality)
	case dataTrusted && v.Capacity > 0 && v.Ratio >= cfg.CrowdRatio:
		directive = model.DirectiveDivert
		reasons = []string{fmt.Sprintf("占用率 %.0f%% 达到导流阈值 %.0f%%", v.Ratio*100, cfg.CrowdRatio*100)}
		confidence = countConfidence(v.Quality)
	default:
		// 管制/疏散刚解除且占用已回落：建议恢复入场（可从事件历史推导，无需跨折叠记忆）。
		if recentlyResolved(incidents, vid, asOf, cfg.RecoverWindow) &&
			(v.Capacity == 0 || v.Ratio < cfg.CrowdRatio) && v.Online {
			directive = model.DirectiveRelease
			reasons = []string{"管制事件已解除且占用率低于导流阈值，建议恢复入场"}
			confidence = "high"
		}
	}

	if directive == "" {
		return nil
	}
	if v.Quality == model.QualityGap || v.Quality == model.QualityRecovered {
		reasons = append(reasons, "数据存在缺口/刚恢复，建议人工复核后执行")
	}

	sort.Slice(used, func(i, j int) bool {
		if !used[i].OccTime.Equal(used[j].OccTime) {
			return used[i].OccTime.Before(used[j].OccTime)
		}
		return used[i].EventID < used[j].EventID
	})
	ev := Evidence{Used: used, Excluded: exc}
	ev.DataHash = evidenceHash(v.SessionID, vid, directive, used)

	heat := heatFor(v)
	v.Heat = heat
	return &Recommendation{
		Directive: directive, Reasons: reasons, Confidence: confidence, Evidence: ev,
	}
}

func countConfidence(q string) string {
	switch q {
	case model.QualityFresh:
		return "high"
	case model.QualityLateArrivals, model.QualityRecovered:
		return "medium"
	case model.QualityGap:
		return "low"
	}
	return "low"
}

// recentlyResolved 判断点位在窗口内是否有管制/疏散事件被解除，且当前无在途同类事件。
func recentlyResolved(all []incRec, vid string, asOf time.Time, win time.Duration) bool {
	found := false
	for _, r := range all {
		if r.ev.VenueID != vid {
			continue
		}
		if (r.p.Resolved || r.p.ResolvesCode != "") && asOf.Sub(r.ev.OccTime) <= win {
			found = true
		}
	}
	return found
}

// heatFor 计算热度；无建议路径下也会被调用以兜底填充。
func heatFor(v *VenueState) string {
	if v.Quality == model.QualityStale {
		return HeatUnknown
	}
	if v.ActiveIncident != nil {
		switch v.ActiveIncident.Priority {
		case model.PriorityControl, model.PriorityEvac:
			return HeatControl
		case model.PriorityCrowd:
			return HeatCrowded
		}
	}
	if v.Capacity == 0 {
		return HeatUnknown
	}
	switch {
	case v.Ratio >= 0.90:
		return HeatFull
	case v.Ratio >= 0.75:
		return HeatCrowded
	case v.Ratio >= 0.40:
		return HeatBusy
	default:
		return HeatCalm
	}
}

// finalizeRecommendations 必须在所有点位占用重建完成后执行：
// 补全导流目标（注册协同点位中占用最低、未管控者）、决策 ID，并给未生成
// 建议的点位兜底填充热度。
func finalizeRecommendations(st *SessionState, meta map[string]*model.RegisterPayload, cfg Config) {
	vids := make([]string, 0, len(st.Venues))
	for id := range st.Venues {
		vids = append(vids, id)
	}
	sort.Strings(vids)
	for _, id := range vids {
		v := st.Venues[id]
		if v.Recommendation != nil && v.Recommendation.Directive == model.DirectiveDivert {
			v.Recommendation.TargetVenueID = pickDiversionTarget(id, st, meta)
			if v.Recommendation.TargetVenueID != "" {
				v.Recommendation.Reasons = append(v.Recommendation.Reasons,
					"目标点位当前承载最低且未管控："+v.Recommendation.TargetVenueID)
			}
			v.Recommendation.DecisionID = hash8(
				st.SessionID, id, v.Recommendation.Directive,
				v.Recommendation.TargetVenueID, v.Recommendation.Evidence.DataHash)
		} else if v.Recommendation != nil {
			v.Recommendation.DecisionID = hash8(
				st.SessionID, id, v.Recommendation.Directive, v.Recommendation.Evidence.DataHash)
		}
		if v.Heat == "" {
			v.Heat = heatFor(v)
		}
	}
}

// pickDiversionTarget 在注册的协同/备用点位中选占用率最低、在线且未暂停入场者；
// 注册候选不足时退化为全场最空闲点位。
func pickDiversionTarget(from string, st *SessionState, meta map[string]*model.RegisterPayload) string {
	candidates := map[string]bool{}
	if m := meta[from]; m != nil {
		for _, a := range m.AlternateVenues {
			candidates[a] = true
		}
	}
	if len(candidates) == 0 {
		for id, m := range meta {
			if id != from && (m.Level == model.LevelCoop || m.Level == model.LevelBackup) {
				candidates[id] = true
			}
		}
	}
	best := ""
	bestRatio := 2.0
	ids := make([]string, 0, len(candidates))
	for c := range candidates {
		ids = append(ids, c)
	}
	sort.Strings(ids)
	for _, c := range ids {
		t := st.Venues[c]
		if t == nil || !t.Online || t.EntryHeld {
			continue
		}
		if t.ActiveIncident != nil {
			continue
		}
		// 数据不可采信的点位不能作为导流去向。
		if t.Quality == model.QualityStale || t.Quality == model.QualityGap {
			continue
		}
		r := t.Ratio
		if r < 0 {
			r = 1.5 // 容量未知者排在所有已知占用率（≤1）之后
		}
		if r < bestRatio {
			bestRatio = r
			best = c
		}
	}
	return best
}
