// 追加式事件存储：JSONL 日志 + 双重幂等键（eventId 与 设备序号）。
// 进程重启后从日志重放即可恢复全部状态，这是中心失联恢复的基础。
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type EventLog struct {
	mu       sync.RWMutex
	path     string
	events   []Event
	seen     map[string]bool // 幂等键
	BadLines int             // 重放时跳过的损坏行数
}

func NewEventLog(path string) (*EventLog, error) {
	l := &EventLog{path: path, seen: map[string]bool{}}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := l.replay(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// dedupeKeys 返回一条事件的全部幂等键：
//   - id  键：同场次同 eventId 视为同一次上报（跨场次不互相判重）；
//   - seq 键：同一设备同一序号同一类型视为同一次上报（设备重传即使换了 eventId 也判重）。
//
// 任一键命中即判为重复。
func dedupeKeys(e Event) []string {
	var keys []string
	if e.EventID != "" {
		keys = append(keys, "id:"+e.SessionID+"|"+e.EventID)
	}
	if (e.Type == EvCountDelta || e.Type == EvCountAnchor) && e.DeviceID != "" && e.Seq > 0 {
		keys = append(keys, fmt.Sprintf("seq:%s|%s|%s|%s|%d", e.Type, e.SessionID, e.VenueID, e.DeviceID, e.Seq))
	}
	return keys
}

func (l *EventLog) replay() error {
	f, err := os.OpenFile(l.path, os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			l.BadLines++
			fmt.Fprintf(os.Stderr, "事件日志损坏行已跳过: %v\n", err)
			continue
		}
		for _, k := range dedupeKeys(e) {
			if l.seen[k] {
				goto nextLine
			}
		}
		for _, k := range dedupeKeys(e) {
			l.seen[k] = true
		}
		l.events = append(l.events, e)
	nextLine:
	}
	return sc.Err()
}

// Append 追加事件；重复上报返回 accepted=false（幂等成功，不报错）。
func (l *EventLog) Append(e Event) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := dedupeKeys(e)
	for _, k := range keys {
		if l.seen[k] {
			return false, nil
		}
	}
	if l.path != "" {
		b, err := json.Marshal(e)
		if err != nil {
			return false, err
		}
		f, err := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return false, err
		}
		_, werr := f.Write(append(b, '\n'))
		cerr := f.Close()
		if werr != nil {
			return false, werr
		}
		if cerr != nil {
			return false, cerr
		}
	}
	for _, k := range keys {
		l.seen[k] = true
	}
	l.events = append(l.events, e)
	return true, nil
}

func (l *EventLog) All() []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out
}

func (l *EventLog) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.events)
}

func newEventID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "ev_" + hex.EncodeToString(b[:])
}
