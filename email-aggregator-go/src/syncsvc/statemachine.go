// Package syncsvc 实现同步编排器与 7 态同步状态机（深化文档 §13）。
// 状态机为纯函数式转换，非法迁移显式报错，可测试、可审计。
package syncsvc

import "errors"

// SyncState 同步状态
type SyncState string

const (
	StateUnconnected SyncState = "UNCONNECTED"
	StateAuthorizing SyncState = "AUTHORIZING"
	StateInitialFull SyncState = "INITIAL_FULL"
	StateIncremental SyncState = "INCREMENTAL"
	StateThrottled   SyncState = "THROTTLED"
	StateError       SyncState = "ERROR"
	StatePaused      SyncState = "PAUSED"
)

// SyncEvent 驱动状态迁移的事件
type SyncEvent string

const (
	EvAuthorize SyncEvent = "AUTHORIZE"
	EvFullDone  SyncEvent = "FULL_DONE"
	EvTick      SyncEvent = "TICK"
	EvThrottle  SyncEvent = "THROTTLE"
	EvResume    SyncEvent = "RESUME"
	EvFail      SyncEvent = "FAIL"
	EvPause     SyncEvent = "PAUSE"
	EvReset     SyncEvent = "RESET"
)

// transitions 显式状态迁移表（白名单；不在表内即非法）
var transitions = map[SyncState]map[SyncEvent]SyncState{
	StateUnconnected: {EvAuthorize: StateAuthorizing},
	StateAuthorizing: {EvFullDone: StateInitialFull, EvFail: StateError},
	StateInitialFull: {EvFullDone: StateIncremental, EvThrottle: StateThrottled, EvFail: StateError, EvPause: StatePaused},
	StateIncremental: {EvThrottle: StateThrottled, EvFail: StateError, EvPause: StatePaused, EvReset: StateInitialFull},
	StateThrottled:   {EvResume: StateIncremental, EvFail: StateError, EvPause: StatePaused},
	StateError:       {EvReset: StateInitialFull, EvResume: StateAuthorizing, EvPause: StatePaused},
	StatePaused:      {EvResume: StateIncremental, EvReset: StateInitialFull},
}

// CanTransition 查询 from 经 ev 能否到达 next
func CanTransition(from SyncState, ev SyncEvent) (SyncState, bool) {
	next, ok := transitions[from][ev]
	return next, ok
}

// Advance 推进状态机，非法迁移返回错误（不静默）
func Advance(from SyncState, ev SyncEvent) (SyncState, error) {
	if next, ok := CanTransition(from, ev); ok {
		return next, nil
	}
	return from, errors.New("illegal transition: " + string(from) + " --" + string(ev) + "-->")
}
