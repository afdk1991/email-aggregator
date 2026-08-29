package syncsvc

import "testing"

// 合法迁移：UNCONNECTED --AUTHORIZE--> AUTHORIZING --FULL_DONE--> INITIAL_FULL --FULL_DONE--> INCREMENTAL
func TestStateMachine_LegalPath(t *testing.T) {
	states := []struct {
		from SyncState
		ev   SyncEvent
		want SyncState
	}{
		{StateUnconnected, EvAuthorize, StateAuthorizing},
		{StateAuthorizing, EvFullDone, StateInitialFull},
		{StateInitialFull, EvFullDone, StateIncremental},
		{StateIncremental, EvThrottle, StateThrottled},
		{StateThrottled, EvResume, StateIncremental},
		{StateIncremental, EvPause, StatePaused},
		{StatePaused, EvResume, StateIncremental},
		{StateIncremental, EvReset, StateInitialFull},
		{StateInitialFull, EvFail, StateError},
		{StateError, EvReset, StateInitialFull},
	}
	for _, tc := range states {
		got, err := Advance(tc.from, tc.ev)
		if err != nil {
			t.Fatalf("%s --%s--> expected %s, got error %v", tc.from, tc.ev, tc.want, err)
		}
		if got != tc.want {
			t.Fatalf("%s --%s--> want %s, got %s", tc.from, tc.ev, tc.want, got)
		}
	}
}

// 非法迁移必须报错（不静默）
func TestStateMachine_IllegalRejected(t *testing.T) {
	illegal := []struct {
		from SyncState
		ev   SyncEvent
	}{
		{StateUnconnected, EvFullDone},
		{StateUnconnected, EvThrottle},
		{StateInitialFull, EvAuthorize},
		{StateThrottled, EvFullDone},
		{StateError, EvThrottle},
	}
	for _, tc := range illegal {
		got, err := Advance(tc.from, tc.ev)
		if err == nil {
			t.Fatalf("%s --%s--> expected error, got %s (illegal migration accepted)", tc.from, tc.ev, got)
		}
	}
}

// CanTransition 查询一致性
func TestCanTransition(t *testing.T) {
	if next, ok := CanTransition(StateIncremental, EvThrottle); !ok || next != StateThrottled {
		t.Fatalf("CanTransition incremental->throttled mismatch: %v %s", ok, next)
	}
	if _, ok := CanTransition(StateUnconnected, EvFullDone); ok {
		t.Fatalf("CanTransition unconnected->full_done should be false")
	}
}
