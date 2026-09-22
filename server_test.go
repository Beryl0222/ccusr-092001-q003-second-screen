package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*httptest.Server, *Engine) {
	t.Helper()
	log, err := NewEventLog("")
	if err != nil {
		t.Fatal(err)
	}
	eng := NewEngine(log)
	ts := httptest.NewServer(NewServer(eng).Routes())
	t.Cleanup(ts.Close)
	return ts, eng
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHTTPEndToEnd(t *testing.T) {
	ts, _ := newTestServer(t)
	sid := "e2e"
	t0 := time.Date(2026, 9, 22, 20, 0, 0, 0, time.UTC)
	at := func(m int) string { return t0.Add(time.Duration(m) * time.Minute).Format(time.RFC3339) }
	q := func(m int) string { return "?at=" + at(m) }

	post := func(path string, body any) (int, map[string]any) {
		var rdr *bytes.Reader
		if body != nil {
			rdr = bytes.NewReader(mustJSON(t, body))
		} else {
			rdr = bytes.NewReader(nil)
		}
		resp, err := http.Post(ts.URL+path, "application/json", rdr)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// 批量摄入：快照 + 红区计数，夹带一个重复 eventId 与一个 PII 拒绝项。
	snap := Event{EventID: "s1", Type: EvVenueSnapshot, VenueID: "A",
		OccurredAt: t0, Payload: mustJSON(t, VenueSnapshot{Name: "甲广场", Level: LevelCore, Capacity: 100})}
	snapB := Event{EventID: "s2", Type: EvVenueSnapshot, VenueID: "B",
		OccurredAt: t0, Payload: mustJSON(t, VenueSnapshot{Name: "乙商圈", Level: LevelCoord, Capacity: 500})}
	occ := func(id string, seq int64, occVal int, m int) Event {
		return Event{EventID: id, Type: EvCountDelta, VenueID: "A", DeviceID: "g1", Seq: seq,
			OccurredAt: t0.Add(time.Duration(m) * time.Minute),
			Payload:    mustJSON(t, CountDelta{Enter: occVal})}
	}
	d1 := occ("c1", 1, 95, 1)
	dup := d1
	pii := Event{EventID: "p1", Type: EvCountDelta, VenueID: "A", DeviceID: "g1", Seq: 2,
		OccurredAt: t0.Add(time.Minute), Payload: []byte(`{"enter":1,"身份证":"x"}`)}

	code, res := post("/v1/sessions/"+sid+"/events"+q(1), map[string]any{
		"events": []Event{snap, snapB, d1, dup, pii},
	})
	if code != http.StatusOK {
		t.Fatalf("部分成功应返回200，实际 %d: %v", code, res)
	}
	if res["accepted"].(float64) != 3 || res["duplicate"].(float64) != 1 || res["rejected"].(float64) != 1 {
		t.Fatalf("摄入计数错误: %v", res)
	}

	// 汇总：A 红。
	resp, err := http.Get(ts.URL + "/v1/sessions/" + sid + "/summary" + q(2))
	if err != nil {
		t.Fatal(err)
	}
	var summary SummaryView
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var a VenueView
	for _, v := range summary.Venues {
		if v.VenueID == "A" {
			a = v
		}
	}
	if a.Heat != "红" || a.Occupancy != 95 {
		t.Fatalf("A 应为红区95人: %+v", a)
	}
	if len(summary.Alarms) != 1 || !summary.Alarms[0].Active {
		t.Fatalf("应恰好1个活动告警: %+v", summary.Alarms)
	}

	// 自动建议。
	code, res = post("/v1/sessions/"+sid+"/venues/A/propose"+q(2), nil)
	if code != 200 {
		t.Fatalf("建议失败: %d %v", code, res)
	}
	propMap := res["proposal"].(map[string]any)
	did := propMap["directiveId"].(string)
	if propMap["kind"] != KindGuide {
		t.Fatalf("应为导流: %v", propMap["kind"])
	}
	if evs := propMap["evidence"].([]any); len(evs) == 0 {
		t.Fatal("建议必须携带证据")
	}

	// 缺少 operator 的决策应被拒绝。
	if code, _ = post("/v1/sessions/"+sid+"/directives/"+did+"/decision"+q(2),
		map[string]any{"decision": "接受"}); code != http.StatusBadRequest {
		t.Fatalf("无 operator 应 400，实际 %d", code)
	}
	// 正常接受。
	if code, res = post("/v1/sessions/"+sid+"/directives/"+did+"/decision"+q(2),
		map[string]any{"decision": "接受", "operator": "值班-周", "reason": "执行"}); code != 200 {
		t.Fatalf("接受失败: %d %v", code, res)
	}
	// 再次决策 → 冲突。
	if code, _ = post("/v1/sessions/"+sid+"/directives/"+did+"/decision"+q(2),
		map[string]any{"decision": "驳回", "operator": "值班-周"}); code != http.StatusConflict {
		t.Fatalf("重复决策应 409，实际 %d", code)
	}

	// 授权：先登记一个即将过期的窗口，过期后校验必须拒绝并产生 enforcement 记录。
	grant := Event{EventID: "g1", Type: EvAuthzGranted, VenueID: "A",
		OccurredAt: t0, Payload: mustJSON(t, AuthzGrant{ContentID: "feed", Start: t0, End: t0.Add(10 * time.Minute)})}
	post("/v1/sessions/"+sid+"/events"+q(0), map[string]any{"events": []Event{grant}})
	if code, res = post("/v1/sessions/"+sid+"/venues/A/authorize?contentId=feed&"+strings.TrimPrefix(q(5), "?"), nil); code != 200 || res["allowed"] != true {
		t.Fatalf("窗口内应允许: %d %v", code, res)
	}
	if code, res = post("/v1/sessions/"+sid+"/venues/A/authorize?contentId=feed&"+strings.TrimPrefix(q(15), "?"), nil); code != 200 || res["allowed"] != false {
		t.Fatalf("过期应拒绝: %d %v", code, res)
	}

	// 审计：应含 建议、接受、闸门拒绝。
	resp, err = http.Get(ts.URL + "/v1/sessions/" + sid + "/audit")
	if err != nil {
		t.Fatal(err)
	}
	var audit map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&audit)
	resp.Body.Close()
	kinds := map[string]int{}
	for _, e := range audit["entries"].([]any) {
		kinds[e.(map[string]any)["kind"].(string)]++
	}
	if kinds["自动建议"] != 1 || kinds["运营接受（不可变）"] != 1 || kinds["分发闸门拒绝"] != 1 {
		t.Fatalf("审计构成错误: %v", kinds)
	}

	// 水位缺口：制造缺口后查询。
	gap := Event{EventID: "c3", Type: EvCountDelta, VenueID: "A", DeviceID: "g1", Seq: 3,
		OccurredAt: t0.Add(3 * time.Minute), Payload: mustJSON(t, CountDelta{Enter: 1})}
	post("/v1/sessions/"+sid+"/events"+q(3), map[string]any{"events": []Event{gap}})
	resp, _ = http.Get(ts.URL + "/v1/sessions/" + sid + "/gaps" + q(3))
	var gaps struct {
		Gaps []DeviceGap `json:"gaps"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&gaps)
	resp.Body.Close()
	if len(gaps.Gaps) != 1 || len(gaps.Gaps[0].Missing) != 1 || gaps.Gaps[0].Missing[0] != 2 {
		t.Fatalf("缺口检测错误: %+v", gaps.Gaps)
	}
}

func TestHealthHTTP(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health 状态码 %d", resp.StatusCode)
	}
}
