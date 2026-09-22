package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/beryl0222/city-second-screen/model"
)

// buildAlerts 生成唯一告警台账：同一 code 的在途重复上报不产生新告警；
// 解除后再次发生产生新的一次（序号自增）。键稳定为 session|venue|code#n。
func buildAlerts(st *SessionState, incidents []incRec, asOf time.Time) {
	// 仅处理本会话事件；按 (occ,id) 已排序。
	type openState struct {
		occ *AlertOccurrence
	}
	open := map[string]*AlertOccurrence{}
	seqByKey := map[string]int{}
	var out []*AlertOccurrence

	for _, r := range incidents {
		p := r.p
		code := p.Code
		resolving := p.Resolved || p.ResolvesCode != ""
		if resolving {
			target := code
			if p.ResolvesCode != "" {
				target = p.ResolvesCode
			}
			if a, ok := open[r.ev.VenueID+"|"+target]; ok {
				rs := r.ev.OccTime
				a.ResolvedAt = &rs
				a.ResolveEventID = r.ev.EventID
				a.Status = "resolved"
				delete(open, r.ev.VenueID+"|"+target)
			}
			// 对不存在在途告警的解除事件：忽略，不凭空造告警。
			continue
		}
		keyBase := r.ev.VenueID + "|" + code
		if a, ok := open[keyBase]; ok {
			// 在途同 code：更新信息，不新增（唯一告警）。
			if priorityRank(p.Priority) > priorityRank(a.Priority) {
				a.Priority = p.Priority
			}
			if p.Message != "" {
				a.Message = p.Message
			}
			continue
		}
		seqByKey[keyBase]++
		a := &AlertOccurrence{
			Key:           st.SessionID + "|" + keyBase + "#" + strconv.Itoa(seqByKey[keyBase]),
			VenueID:       r.ev.VenueID,
			Code:          code,
			Priority:      p.Priority,
			Message:       p.Message,
			RaisedAt:      r.ev.OccTime,
			RaisedEventID: r.ev.EventID,
			Status:        "active",
		}
		open[keyBase] = a
		out = append(out, a)
	}

	// 输出按点位、发生时刻排序，已解除的告警同样保留（完整生命周期、便于审计）。
	// 为获得稳定顺序，重扫 incidents 记录每个告警首次出现次序。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].VenueID != out[j].VenueID {
			return out[i].VenueID < out[j].VenueID
		}
		if !out[i].RaisedAt.Equal(out[j].RaisedAt) {
			return out[i].RaisedAt.Before(out[j].RaisedAt)
		}
		return out[i].Key < out[j].Key
	})
	_ = asOf
	st.Alerts = out
}

// buildContent 对每路内容授权求在 asOf 的最新状态，并在过期/撤销时生成
// 必须执行的停发指令（“过期授权的画面不得继续分发”）。
func buildContent(st *SessionState, auths []authRec, asOf time.Time) {
	// (venue,content) -> 最新一条（按 occ,id 有序，后者覆盖）。
	latest := map[string]*authRec{}
	var order []string
	for i := range auths {
		a := &auths[i]
		key := a.ev.VenueID + "|" + a.p.ContentID
		if _, ok := latest[key]; !ok {
			order = append(order, key)
		}
		latest[key] = a
	}
	sort.Strings(order)
	for _, key := range order {
		a := latest[key]
		cs := &ContentState{
			VenueID: a.ev.VenueID, ContentID: a.p.ContentID,
			Start: a.p.Start, End: a.p.End,
		}
		switch {
		case a.p.Revoked:
			cs.Status = "revoked"
			cs.StopOrder = &StopOrder{Reason: "revoked", At: a.ev.OccTime, EventID: a.ev.EventID}
		case asOf.Before(a.p.Start):
			cs.Status = "pending"
		case asOf.Before(a.p.End):
			cs.Status = "distributing"
		default:
			cs.Status = "expired"
			// 过期时刻即应停发；指令在 asOf 必然已生效。
			cs.StopOrder = &StopOrder{Reason: "expired", At: a.p.End, EventID: a.ev.EventID}
		}
		st.Content = append(st.Content, cs)
	}
}

// buildReceipts 汇总每个通知的最新回执状态（状态机单调推进，重复回执幂等）。
func buildReceipts(st *SessionState, receipts []rcpRec) {
	for _, r := range receipts {
		nid := r.p.NotificationID
		cur := st.Receipts[nid]
		if cur == nil {
			cur = &ReceiptView{NotificationID: nid, VenueID: r.ev.VenueID}
			st.Receipts[nid] = cur
		}
		// “已驳回/已过期”为终态；同状态重复上报保持幂等。
		if (cur.Status == model.ReceiptRejected || cur.Status == model.ReceiptExpired) &&
			r.p.Status != cur.Status {
			continue
		}
		cur.Status = r.p.Status
		cur.OperatorID = r.p.OperatorID
		cur.Detail = r.p.Detail
		cur.UpdatedAt = r.ev.OccTime
		cur.EventID = r.ev.EventID
	}
}

// bindAudit 生成不可变审计序列，并把处置绑定到当前仍存在的建议；
// 对已不存在建议的迟到处置，标记 orphaned=true 仍予以保留（可审计）。
func bindAudit(st *SessionState, acks []ackRec) {
	curByVenue := map[string]*Recommendation{}
	for vid, v := range st.Venues {
		if v.Recommendation != nil {
			curByVenue[vid] = v.Recommendation
		}
	}
	decisionVenue := map[string]string{}
	for vid, rec := range curByVenue {
		decisionVenue[rec.DecisionID] = vid
	}

	var audit []*AuditEntry
	for _, a := range acks {
		e := &AuditEntry{
			DecisionID: a.p.DecisionID,
			Accepted:   *a.p.Accepted,
			OperatorID: a.p.OperatorID,
			Reason:     a.p.Reason,
			OccTime:    a.ev.OccTime,
			EventID:    a.ev.EventID,
		}
		vid, ok := decisionVenue[a.p.DecisionID]
		if !ok {
			e.Orphaned = true
		} else {
			curByVenue[vid].Acked = e
		}
		audit = append(audit, e)
	}
	sort.SliceStable(audit, func(i, j int) bool {
		if !audit[i].OccTime.Equal(audit[j].OccTime) {
			return audit[i].OccTime.Before(audit[j].OccTime)
		}
		return audit[i].EventID < audit[j].EventID
	})
	st.Audit = audit
}

// evidenceHash 对“被采用的有效数据”做规范摘要：同一数据集合任意顺序得到同一值，
// 从而让决策 ID 可复现、可核对；被排除的数据不参与决策身份。
func evidenceHash(session, venue, directive string, used []BasisPoint) string {
	u := append([]BasisPoint(nil), used...)
	sort.Slice(u, func(i, j int) bool {
		if !u[i].OccTime.Equal(u[j].OccTime) {
			return u[i].OccTime.Before(u[j].OccTime)
		}
		return u[i].EventID < u[j].EventID
	})
	var b strings.Builder
	b.WriteString(session)
	b.WriteByte(0)
	b.WriteString(venue)
	b.WriteByte(0)
	b.WriteString(directive)
	for _, p := range u {
		b.WriteByte(0)
		b.WriteString(p.EventID)
		b.WriteByte(0)
		b.WriteString(p.Role)
	}
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:])[:16]
}
