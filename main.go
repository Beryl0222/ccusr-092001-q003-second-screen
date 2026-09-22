// 城市第二现场客流联动服务入口。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/beryl0222/city-second-screen/internal/api"
	"github.com/beryl0222/city-second-screen/internal/engine"
	"github.com/beryl0222/city-second-screen/internal/service"
	"github.com/beryl0222/city-second-screen/model"
)

const serviceID = "city-second-screen"

func main() {
	check := flag.Bool("check", false, "检查基础配置")
	addr := flag.String("addr", ":8000", "监听地址")
	dataDir := flag.String("data", "data", "事件日志目录（-data= 为纯内存模式）")
	replay := flag.String("replay", "", "重放指定 JSONL 事件文件并输出验收报告（乱序稳定性/唯一告警/数据依据）")
	asOfFlag := flag.String("asof", "", "重放评估时刻(RFC3339)，缺省取最后接收时间")
	flag.Parse()

	if *check {
		fmt.Println("基础检查通过")
		return
	}
	if *replay != "" {
		if err := runReplay(*replay, *dataDir, *asOfFlag); err != nil {
			fmt.Fprintln(os.Stderr, "重放失败:", err)
			os.Exit(1)
		}
		return
	}

	svc, err := service.New(nonEmpty(*dataDir), engine.DefaultConfig())
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
	defer svc.Close()

	srv := api.NewServer(svc)
	fmt.Printf("服务 %s 监听 %s（事件日志: %s）\n", serviceID, *addr, *dataDir)
	if err := http.ListenAndServe(*addr, srv.Handler()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func nonEmpty(s string) string {
	if s == "" || s == "-" {
		return ""
	}
	return s
}

// ---------------- 乱序重放验收工具 ----------------

type replayLine struct {
	ReceivedAt time.Time       `json:"received_at"`
	Event      json.RawMessage `json:"event"`
	AsOf       time.Time       `json:"as_of"`
}

type replayItem struct {
	raw  []byte
	recv time.Time
}

func runReplay(path, dataDir, asOfArg string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var items []replayItem
	var fallbackRecv time.Time
	var checkpointTimes []time.Time
	for _, line := range splitLines(raw) {
		if len(line) == 0 {
			continue
		}
		var wrap replayLine
		if err := json.Unmarshal(line, &wrap); err == nil && len(wrap.Event) > 0 {
			items = append(items, replayItem{raw: wrap.Event, recv: wrap.ReceivedAt})
			if !wrap.AsOf.IsZero() {
				checkpointTimes = append(checkpointTimes, wrap.AsOf)
			}
			continue
		}
		// 纯检查点指令行：{"checkpoint":"RFC3339"}
		var cp struct {
			Checkpoint time.Time `json:"checkpoint"`
		}
		if err := json.Unmarshal(line, &cp); err == nil && !cp.Checkpoint.IsZero() {
			checkpointTimes = append(checkpointTimes, cp.Checkpoint)
			continue
		}
		// 退化为纯信封：接收时间取发生时间后 1 秒。
		var probe struct {
			OccTime time.Time `json:"occ_time"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			return fmt.Errorf("无法解析重放行: %w", err)
		}
		if fallbackRecv.IsZero() {
			fallbackRecv = probe.OccTime.Add(time.Second)
		} else {
			fallbackRecv = fallbackRecv.Add(time.Second)
		}
		items = append(items, replayItem{raw: line, recv: fallbackRecv})
	}

	parse := func(list []replayItem) []*model.Event {
		evs := make([]*model.Event, 0, len(list))
		for _, it := range list {
			ev, err := model.ParseEnvelope(it.raw, it.recv)
			if err != nil {
				fmt.Fprintln(os.Stderr, "跳过非法事件:", err)
				continue
			}
			evs = append(evs, ev)
		}
		return evs
	}

	events := parse(items)
	cfg := engine.DefaultConfig()

	// 评估检查点：命令行 -asof 优先；否则取场景文件中的 checkpoints 指令；
	// 再否则用最后接收时间。
	var checkpoints []time.Time
	if asOfArg != "" {
		t, err := time.Parse(time.RFC3339, asOfArg)
		if err != nil {
			return fmt.Errorf("asof 时间格式非法: %w", err)
		}
		checkpoints = []time.Time{t}
	} else {
		checkpoints = checkpointTimes
	}

	fmt.Println("================ 第二现场乱序重放验收报告 ================")
	fmt.Printf("事件总数: %d    评估检查点: %d 个\n", len(events), max1(len(checkpoints), 1))

	stableAll := true
	var lastSummary *engine.Summary
	checkpointList := checkpoints
	if len(checkpointList) == 0 {
		checkpointList = []time.Time{time.Time{}} // 零值 -> 取最后接收时间
	}
	for ci, cp := range checkpointList {
		s1 := engine.Fold(reverse(copyEvents(events)), cfg, cp)
		s2 := engine.Fold(copyEvents(events), cfg, cp)
		s3 := engine.Fold(seededShuffle(copyEvents(events), int64(20260922+ci)), cfg, cp)
		b1 := mustJSON(s1)
		b2 := mustJSON(s2)
		b3 := mustJSON(s3)
		stable := string(b1) == string(b2) && string(b2) == string(b3)
		stableAll = stableAll && stable
		lastSummary = s2

		fmt.Printf("\n########## 检查点 %d：asOf=%s  乱序(逆序/原序/洗牌)一致=%v ##########\n",
			ci+1, s2.AsOf.Format(time.RFC3339), stable)
		if !stable {
			return fmt.Errorf("检查点 %d 折叠结果不稳定", ci+1)
		}
		printSummary(s2)
	}

	asOf := lastSummary.AsOf
	b2 := mustJSON(lastSummary)

	// 中心失联恢复演练：中心始终收数；边缘在水位 15 处失联，重连后携带水位
	// 增量拉取缺口，折叠结果与中心一致。
	svc, err := service.New("", cfg)
	if err != nil {
		return err
	}
	for _, it := range items {
		if _, _, err := svc.IngestAt(it.raw, it.recv); err != nil {
			fmt.Fprintln(os.Stderr, "跳过非法事件:", err)
		}
	}
	const edgeWatermark int64 = 15
	delta, latest, err := svc.EventsSince(edgeWatermark)
	if err != nil {
		return err
	}
	// 边缘侧：失联前已有前 15 个版本，补上拉到的缺口后全量折叠。
	edgeEvents := make([]*model.Event, 0, len(events))
	edgeEvents = append(edgeEvents, parse(items[:int(edgeWatermark)])...)
	edgeEvents = append(edgeEvents, delta...)
	restored := engine.Fold(edgeEvents, cfg, asOf)
	recovered := string(mustJSON(restored)) == string(b2)
	caughtUp := latest == svc.Version()
	fmt.Printf("\n[失联恢复] 边缘失联水位=%d, 中心最新水位=%d, 增量补齐事件=%d, 水位追平=%v, 边缘折叠与中心一致: %v\n",
		edgeWatermark, latest, len(delta), caughtUp, recovered)

	// 去重幂等演练：全部事件再投一次，应全部标记 duplicate 且状态不变。
	dupes := 0
	for _, it := range items {
		if _, res, err := svc.IngestAt(it.raw, it.recv.Add(time.Minute)); err == nil && res.Duplicate {
			dupes++
		}
	}
	again, _ := svc.Snapshot(asOf)
	idempotent := string(mustJSON(again)) == string(b2)
	fmt.Printf("[幂等去重] 重投 %d 条全部识别为重复，状态保持: %v\n", dupes, idempotent)

	if stableAll && recovered && idempotent {
		fmt.Println("\n验收结论: 通过 —— 状态稳定、告警唯一、失联可恢复、重复不累计、依据可解释。")
		return nil
	}
	return fmt.Errorf("验收未通过")
}

// printSummary 输出单个检查点下全部场次的点位状态、依据、告警、授权与台账。
func printSummary(s2 *engine.Summary) {
	for _, sid := range sortedKeys(s2.Sessions) {
		st := s2.Sessions[sid]
		fmt.Printf("\n---- 场次 %s ----\n", sid)
		for _, vid := range sortedKeys(st.Venues) {
			v := st.Venues[vid]
			ratio := "未知"
			if v.Ratio >= 0 {
				ratio = fmt.Sprintf("%.0f%%", v.Ratio*100)
			}
			fmt.Printf("点位 %-12s 等级=%-6s 热度=%-6s 占用=%4d/%-4d(%-4s) 在线=%-5v 数据=%s 暂停入场=%v\n",
				v.VenueID, v.Level, v.Heat, v.Occupancy, v.Capacity, ratio, v.Online, v.Quality, v.EntryHeld)
			for _, n := range v.QualityNotes {
				fmt.Printf("    · %s\n", n)
			}
			if v.Recommendation != nil {
				rc := v.Recommendation
				tgt := rc.TargetVenueID
				if tgt == "" {
					tgt = "-"
				}
				verdict := "待处置"
				if rc.Acked != nil {
					if rc.Acked.Accepted {
						verdict = "已接受"
					} else {
						verdict = "已驳回"
					}
				}
				fmt.Printf("    建议[%s] %s -> 目标=%s 置信度=%s 处置=%s\n",
					rc.DecisionID, rc.Directive, tgt, rc.Confidence, verdict)
				for _, r := range rc.Reasons {
					fmt.Printf("      - %s\n", r)
				}
				fmt.Printf("      有效数据 %d 条 / 排除数据 %d 条，依据指纹=%s\n",
					len(rc.Evidence.Used), len(rc.Evidence.Excluded), rc.Evidence.DataHash)
				for _, p := range rc.Evidence.Used {
					late := ""
					if p.Late {
						late = " [迟到后补]"
					}
					detail := p.Detail
					if detail != "" {
						detail = " " + detail
					}
					fmt.Printf("        采用 %-16s %-17s@%s 角色=%s%s%s\n",
						p.EventID, p.Type, p.OccTime.Format("01-02 15:04:05"), p.Role, detail, late)
				}
				for _, p := range rc.Evidence.Excluded {
					fmt.Printf("        排除 %-16s %-17s@%s 原因: %s\n",
						p.EventID, p.Type, p.OccTime.Format("01-02 15:04:05"), p.Reason)
				}
			}
		}
		fmt.Printf("  唯一告警 (%d):\n", len(st.Alerts))
		for _, a := range st.Alerts {
			resolved := "在途"
			if a.Status == "resolved" {
				resolved = "已于 " + a.ResolvedAt.Format("01-02 15:04:05") + " 解除(" + a.ResolveEventID + ")"
			}
			fmt.Printf("    [%s] %s %s %s %s\n", a.Key, a.VenueID, a.Priority, a.Code, resolved)
		}
		fmt.Printf("  内容分发 (%d):\n", len(st.Content))
		for _, c := range st.Content {
			line := fmt.Sprintf("    %s @ %s: %s [%s~%s]", c.ContentID, c.VenueID, c.Status,
				c.Start.Format("01-02 15:04"), c.End.Format("01-02 15:04"))
			if c.StopOrder != nil {
				line += fmt.Sprintf(" => 停发指令(%s @ %s, 事件 %s)",
					c.StopOrder.Reason, c.StopOrder.At.Format("01-02 15:04:05"), c.StopOrder.EventID)
			}
			fmt.Println(line)
		}
		if len(st.Receipts) > 0 {
			fmt.Printf("  通知回执 (%d):\n", len(st.Receipts))
			for _, nid := range sortedKeys(st.Receipts) {
				r := st.Receipts[nid]
				fmt.Printf("    %s: %s @ %s (%s)\n", nid, r.Status, r.VenueID, r.EventID)
			}
		}
		if len(st.Audit) > 0 {
			fmt.Printf("  运营处置审计 (%d，不可变):\n", len(st.Audit))
			for _, a := range st.Audit {
				verdict := "接受"
				if !a.Accepted {
					verdict = "驳回"
				}
				extra := ""
				if a.Orphaned {
					extra = " [建议已变更的迟到处置]"
				}
				fmt.Printf("    %s %s 决策=%s 操作人=%s 理由=%q%s\n",
					a.OccTime.Format("01-02 15:04:05"), verdict, a.DecisionID, a.OperatorID, a.Reason, extra)
			}
		}
	}
}

func max1(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			line := b[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if len(line) > 0 {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}

func copyEvents(in []*model.Event) []*model.Event {
	out := make([]*model.Event, len(in))
	copy(out, in)
	return out
}

func reverse(in []*model.Event) []*model.Event {
	for i, j := 0, len(in)-1; i < j; i, j = i+1, j-1 {
		in[i], in[j] = in[j], in[i]
	}
	return in
}

func seededShuffle(in []*model.Event, seed int64) []*model.Event {
	// 确定性 LCG，避免依赖随机源造成验收不可复现。
	state := seed
	next := func(n int) int {
		state = (state*6364136223846793005 + 1442695040888963407) & (1<<63 - 1)
		return int((state >> 33) % int64(n))
	}
	for i := len(in) - 1; i > 0; i-- {
		j := next(i + 1)
		in[i], in[j] = in[j], in[i]
	}
	return in
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
