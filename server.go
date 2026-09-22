// HTTP API：事件批摄入、汇总、建议、决策、授权闸门、审计、水位缺口。
package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type Server struct {
	eng *Engine
}

func NewServer(eng *Engine) *Server { return &Server{eng: eng} }

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": serviceID})
	})
	mux.HandleFunc("/v1/sessions/", s.dispatch)
	return mux
}

// 路由：
//
//	POST /v1/sessions/{sid}/events
//	GET  /v1/sessions/{sid}/summary
//	POST /v1/sessions/{sid}/venues/{vid}/propose
//	POST /v1/sessions/{sid}/directives/{did}/decision
//	POST /v1/sessions/{sid}/venues/{vid}/authorize
//	GET  /v1/sessions/{sid}/audit
//	GET  /v1/sessions/{sid}/gaps
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[0] == "" {
		writeErr(w, http.StatusNotFound, "未知路径")
		return
	}
	sid := parts[0]
	now := clockFromRequest(r)

	switch {
	case len(parts) == 2 && parts[1] == "events" && r.Method == http.MethodPost:
		s.ingest(w, r, sid, now)
	case len(parts) == 2 && parts[1] == "summary" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.eng.Project(sid, now).Summary())
	case len(parts) == 4 && parts[1] == "venues" && parts[3] == "propose" && r.Method == http.MethodPost:
		s.propose(w, sid, parts[2], now)
	case len(parts) == 3 && parts[1] == "directives" && parts[2] != "":
		// /directives/{did}/... 需要 4 段，此处 3 段不合法
		writeErr(w, http.StatusNotFound, "未知路径")
	case len(parts) == 4 && parts[1] == "directives" && parts[3] == "decision" && r.Method == http.MethodPost:
		s.decide(w, r, sid, parts[2], now)
	case len(parts) == 4 && parts[1] == "venues" && parts[3] == "authorize" && r.Method == http.MethodPost:
		s.authorize(w, r, sid, parts[2], now)
	case len(parts) == 2 && parts[1] == "audit" && r.Method == http.MethodGet:
		s.audit(w, sid)
	case len(parts) == 2 && parts[1] == "gaps" && r.Method == http.MethodGet:
		p := s.eng.Project(sid, now)
		gaps := []DeviceGap{}
		for _, id := range p.order {
			gaps = append(gaps, p.gapsFor(p.venues[id])...)
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessionId": sid, "asOf": now.Format(time.RFC3339), "gaps": gaps})
	default:
		writeErr(w, http.StatusNotFound, "未知路径或方法")
	}
}

type ingestReq struct {
	Events []Event `json:"events"`
}

type ingestItem struct {
	EventID    string `json:"eventId"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	OccurredAt string `json:"occurredAt,omitempty"`
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request, sid string, now time.Time) {
	var req ingestReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if len(req.Events) == 0 {
		writeErr(w, http.StatusBadRequest, "events 为空")
		return
	}
	items := make([]ingestItem, 0, len(req.Events))
	dups, rejected := 0, 0
	for _, e := range req.Events {
		e.SessionID = sid
		if e.EventID == "" {
			e.EventID = newEventID()
		}
		if e.ReceivedAt.IsZero() {
			e.ReceivedAt = now
		}
		item := ingestItem{EventID: e.EventID, OccurredAt: e.OccurredAt.Format(time.RFC3339)}
		if err := e.Validate(now); err != nil {
			item.Status, item.Error = "rejected", err.Error()
			rejected++
			items = append(items, item)
			continue
		}
		accepted, err := s.eng.appendExternal(e)
		if err != nil {
			item.Status, item.Error = "rejected", err.Error()
			rejected++
		} else if !accepted {
			item.Status = "duplicate"
			dups++
		} else {
			item.Status = "accepted"
		}
		items = append(items, item)
	}
	status := http.StatusOK
	if rejected == len(items) {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]any{
		"sessionId": sid, "receivedAt": now.Format(time.RFC3339),
		"total": len(items), "accepted": len(items) - dups - rejected,
		"duplicate": dups, "rejected": rejected, "items": items,
	})
}

func (s *Server) propose(w http.ResponseWriter, sid, vid string, now time.Time) {
	prop, err := s.eng.AutoPropose(sid, vid, now)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if prop == nil {
		writeJSON(w, http.StatusOK, map[string]any{"sessionId": sid, "venueId": vid, "proposal": nil, "message": "当前无需建议，或已有未过期同类建议"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessionId": sid, "proposal": prop})
}

type decideReq struct {
	Decision Decision `json:"decision"`
	Operator string   `json:"operator"`
	Reason   string   `json:"reason,omitempty"`
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request, sid, did string, now time.Time) {
	var req decideReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if req.Operator == "" {
		writeErr(w, http.StatusBadRequest, "operator 不能为空（决策必须可审计到人）")
		return
	}
	if err := s.eng.Decide(sid, did, req.Decision, req.Operator, req.Reason, now); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"directiveId": did, "decision": string(req.Decision), "operator": req.Operator, "status": "已记录且不可变",
	})
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request, sid, vid string, now time.Time) {
	contentID := r.URL.Query().Get("contentId")
	if contentID == "" {
		writeErr(w, http.StatusBadRequest, "contentId 必填")
		return
	}
	p := s.eng.Project(sid, now)
	d := p.Authorize(vid, contentID, now)
	if !d.Allowed {
		// 所有拒绝（含过期授权继续分发的尝试）都落审计日志。
		_ = s.eng.LogEnforcement(sid, vid, contentID, d.Reason, now)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessionId": sid, "venueId": vid, "contentId": contentID,
		"at": now.Format(time.RFC3339), "allowed": d.Allowed, "reason": d.Reason,
	})
}

type AuditEntry struct {
	Kind        string              `json:"kind"`
	At          string              `json:"at"`
	EventID     string              `json:"eventId"`
	VenueID     string              `json:"venueId,omitempty"`
	Directive   *DirectiveView      `json:"directive,omitempty"`
	Enforcement *EnforcementPayload `json:"enforcement,omitempty"`
}

func (s *Server) audit(w http.ResponseWriter, sid string) {
	// 以最新时刻投影取得指令视图（含证据快照与决策），再附全部闸门拒绝记录。
	p := s.eng.Project(sid, latestOf(s.eng.log))
	summary := p.Summary()
	dvByID := map[string]DirectiveView{}
	for _, dv := range summary.Directives {
		dvByID[dv.DirectiveID] = dv
	}
	var entries []AuditEntry
	for _, e := range s.eng.log.All() {
		if e.SessionID != sid {
			continue
		}
		switch e.Type {
		case EvProposed:
			var dp DirectiveProposal
			if json.Unmarshal(e.Payload, &dp) != nil {
				continue
			}
			dv := dvByID[dp.DirectiveID]
			entries = append(entries, AuditEntry{
				Kind: "自动建议", At: e.OccurredAt.Format(time.RFC3339),
				EventID: e.EventID, VenueID: e.VenueID, Directive: &dv,
			})
		case EvDecision:
			var d DecisionPayload
			if json.Unmarshal(e.Payload, &d) != nil {
				continue
			}
			dv := dvByID[d.DirectiveID]
			kind := "运营驳回"
			if d.Decision == DecisionAccept {
				kind = "运营接受"
			}
			entries = append(entries, AuditEntry{
				Kind: kind + "（不可变）", At: e.OccurredAt.Format(time.RFC3339),
				EventID: e.EventID, VenueID: e.VenueID, Directive: &dv,
			})
		case EvEnforcement:
			var en EnforcementPayload
			if json.Unmarshal(e.Payload, &en) != nil {
				continue
			}
			entries = append(entries, AuditEntry{
				Kind: "分发闸门拒绝", At: e.OccurredAt.Format(time.RFC3339),
				EventID: e.EventID, VenueID: e.VenueID, Enforcement: &en,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessionId": sid, "count": len(entries), "entries": entries})
}

func latestOf(log *EventLog) time.Time {
	var latest time.Time
	for _, e := range log.All() {
		if e.OccurredAt.After(latest) {
			latest = e.OccurredAt
		}
	}
	if latest.IsZero() {
		latest = time.Now()
	}
	return latest
}

// clockFromRequest 支持 ?at= / X-Now 注入回放时钟；缺省取服务器当前时间。
func clockFromRequest(r *http.Request) time.Time {
	v := r.URL.Query().Get("at")
	if v == "" {
		v = r.Header.Get("X-Now")
	}
	if v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
	}
	return time.Now()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
