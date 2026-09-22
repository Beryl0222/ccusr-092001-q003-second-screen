// Package service 编排事件存储与确定性折叠，提供线上查询与恢复能力。
package service

import (
	"sync"
	"time"

	"github.com/beryl0222/city-second-screen/internal/engine"
	"github.com/beryl0222/city-second-screen/internal/store"
	"github.com/beryl0222/city-second-screen/model"
)

// Service 是线程安全的应用服务门面。
type Service struct {
	mu    sync.RWMutex
	store *store.EventStore
	cfg   engine.Config
	now   func() time.Time
}

// New 创建服务；dataDir 为空时使用纯内存日志（测试/演示）。
func New(dataDir string, cfg engine.Config) (*Service, error) {
	st, err := store.Open(dataDir)
	if err != nil {
		return nil, err
	}
	return &Service{store: st, cfg: cfg, now: time.Now}, nil
}

// WithClock 替换系统时钟（测试/重放用），返回服务自身以便链式调用。
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// Ingest 接收一条原始事件，返回解析后的事件与追加结果（重复上报幂等）。
func (s *Service) Ingest(raw []byte) (*model.Event, *store.AppendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.Append(raw, s.now())
}

// IngestAt 以指定接收时间入库（乱序重放/恢复演练专用）。
func (s *Service) IngestAt(raw []byte, receivedAt time.Time) (*model.Event, *store.AppendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.Append(raw, receivedAt)
}

// Snapshot 返回当前时刻（或指定 asOf）的全量折叠状态。
func (s *Service) Snapshot(asOf time.Time) (*engine.Summary, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	events, err := s.store.All()
	if err != nil {
		return nil, err
	}
	return engine.Fold(events, s.cfg, asOf), nil
}

// Replay 对给定事件集做纯折叠（不落库），供乱序重放验收使用。
func (s *Service) Replay(events []*model.Event, asOf time.Time) *engine.Summary {
	return engine.Fold(events, s.cfg, asOf)
}

// EventsSince 返回版本号严格大于 since 的已解析事件（中心失联增量恢复）。
func (s *Service) EventsSince(since int64) ([]*model.Event, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	evs, err := s.store.Since(since)
	if err != nil {
		return nil, 0, err
	}
	return evs, s.store.Version(), nil
}

// Version 返回全局最新版本号（恢复协商的水位线）。
func (s *Service) Version() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.store.Version()
}

// Config 暴露当前口径配置。
func (s *Service) Config() engine.Config { return s.cfg }

// Close 关闭底层日志。
func (s *Service) Close() error { return s.store.Close() }
