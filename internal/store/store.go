// Package store 是只追加的事件日志：负责幂等去重、全局单调版本号、
// WAL 持久化与按版本回放（支撑中心失联恢复与乱序重放）。
//
// 设计要点：
//   - 同一条 event_id 永远只生效一次，重复上报返回首个版本号（幂等）；
//   - 版本号按“服务端接收顺序”单调递增，作为失联恢复的水位线；注意折叠
//     结果并不依赖版本号，而是按事件内容 (occ_time,event_id) 定序，因此
//     任意乱序/迟到重放都得到确定且一致的状态；
//   - 日志每行一条 JSON 记录（原始报文 + 接收时间 + 版本），重启后可重建。
package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/beryl0222/city-second-screen/model"
)

// Record 是 WAL 中的一条记录：原始报文加上服务端元数据。
type Record struct {
	Version    int64     `json:"version"`
	ReceivedAt time.Time `json:"received_at"`
	Raw        []byte    `json:"raw"`
}

// EventStore 线程安全的追加日志。
type EventStore struct {
	mu      sync.Mutex
	path    string
	f       *os.File
	w       *bufio.Writer
	records []Record
	index   map[string]int64 // event_id -> version
	nextVer int64
	dirs    map[string]int64 // 每场次最大版本（快速恢复游标）
}

// Open 打开（或创建）位于 dir 的事件日志，内存模式传空字符串。
func Open(dir string) (*EventStore, error) {
	s := &EventStore{
		index:   map[string]int64{},
		nextVer: 1,
		dirs:    map[string]int64{},
	}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s.path = filepath.Join(dir, "events.log")
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.f = f
	if err := s.recover(); err != nil {
		return nil, err
	}
	s.w = bufio.NewWriter(f)
	return s, nil
}

// AppendResult 描述一次追加的结果。
type AppendResult struct {
	Version   int64
	Duplicate bool // true 表示 event_id 已存在，未重复写入
}

// Append 校验、去重并持久化一条原始事件。
// 同一 event_id 的并发/重试上报只生效一次，这是“同一设备重复上报”的第一道防线。
func (s *EventStore) Append(raw []byte, now time.Time) (*model.Event, *AppendResult, error) {
	ev, err := model.ParseEnvelope(raw, now)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if ver, ok := s.index[ev.EventID]; ok {
		// 回填已分配的版本与接收时间，供调用方构造一致响应。
		ev.Version = ver
		if r := s.recordByVersionLocked(ver); r != nil {
			ev.ReceivedAt = r.ReceivedAt
		}
		return ev, &AppendResult{Version: ver, Duplicate: true}, nil
	}

	ver := s.nextVer
	ev.Version = ver
	rec := Record{Version: ver, ReceivedAt: now, Raw: append([]byte(nil), raw...)}

	if s.f != nil {
		line, err := json.Marshal(rec)
		if err != nil {
			return nil, nil, err
		}
		if _, err := s.w.Write(append(line, '\n')); err != nil {
			return nil, nil, err
		}
		if err := s.w.Flush(); err != nil {
			return nil, nil, err
		}
		if err := s.f.Sync(); err != nil {
			return nil, nil, err
		}
	}

	s.records = append(s.records, rec)
	s.index[ev.EventID] = ver
	s.nextVer = ver + 1
	s.dirs[ev.SessionID] = ver
	return ev, &AppendResult{Version: ver, Duplicate: false}, nil
}

// All 返回按版本顺序排列的全部已解析事件（用于全量折叠）。
func (s *EventStore) All() ([]*model.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.materializeLocked(0)
}

// Since 返回版本号严格大于 sinceVersion 的事件（中心失联后增量恢复用）。
func (s *EventStore) Since(sinceVersion int64) ([]*model.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.materializeLocked(sinceVersion)
}

// SessionVersion 返回某场次当前最新版本号，无场次时返回 0。
func (s *EventStore) SessionVersion(session string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dirs[session]
}

// Version 返回全局最新版本号。
func (s *EventStore) Version() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextVer - 1
}

func (s *EventStore) materializeLocked(afterVersion int64) ([]*model.Event, error) {
	out := make([]*model.Event, 0, len(s.records))
	for _, r := range s.records {
		if r.Version <= afterVersion {
			continue
		}
		ev, err := model.ParseEnvelope(r.Raw, r.ReceivedAt)
		if err != nil {
			return nil, fmt.Errorf("版本 %d 的持久化事件无法解析: %w", r.Version, err)
		}
		ev.Version = r.Version
		out = append(out, ev)
	}
	return out, nil
}

func (s *EventStore) recordByVersionLocked(ver int64) *Record {
	idx := ver - 1
	if ver >= 1 && int(idx) < len(s.records) && s.records[idx].Version == ver {
		return &s.records[idx]
	}
	for i := range s.records {
		if s.records[i].Version == ver {
			return &s.records[i]
		}
	}
	return nil
}

// recover 从 WAL 重建内存索引。最后一行若损坏（写入中断）则截断。
func (s *EventStore) recover() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	truncateAt := int64(len(data))
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	valid := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			// 定位损坏行的字节偏移并截断。
			cut := int64(0)
			if valid > 0 {
				// 找到第 valid 个换行符之后的位置。
				cut = indexNthNewline(data, valid)
			}
			truncateAt = cut
			break
		}
		ev, err := model.ParseEnvelope(rec.Raw, rec.ReceivedAt)
		if err != nil {
			return fmt.Errorf("WAL 恢复遇到非法事件(version=%d): %w", rec.Version, err)
		}
		if rec.Version <= 0 {
			return errors.New("WAL 记录版本号非法")
		}
		s.records = append(s.records, rec)
		s.index[ev.EventID] = rec.Version
		if rec.Version >= s.nextVer {
			s.nextVer = rec.Version + 1
		}
		s.dirs[ev.SessionID] = rec.Version
		valid++
	}
	if truncateAt != int64(len(data)) {
		if err := s.f.Truncate(truncateAt); err != nil {
			return err
		}
	}
	return nil
}

func indexNthNewline(data []byte, n int) int64 {
	count := 0
	for i, b := range data {
		if b == '\n' {
			count++
			if count == n {
				return int64(i + 1)
			}
		}
	}
	return int64(len(data))
}

// Close 刷盘并关闭日志文件。
func (s *EventStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w != nil {
		if err := s.w.Flush(); err != nil {
			return err
		}
	}
	if s.f != nil {
		return s.f.Close()
	}
	return nil
}
