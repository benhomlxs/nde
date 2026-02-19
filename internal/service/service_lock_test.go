package service

import (
	"testing"
	"time"
)

func TestLockUserCleansUpAfterUnlock(t *testing.T) {
	s := &Service{
		userLocks: make(map[uint32]*userLockState),
	}

	unlock := s.lockUser(1001)
	if got := len(s.userLocks); got != 1 {
		t.Fatalf("expected 1 lock entry, got %d", got)
	}

	unlock()
	if got := len(s.userLocks); got != 0 {
		t.Fatalf("expected lock map to be cleaned, got %d entries", got)
	}
}

func TestLockUserCleanupWithWaitingGoroutine(t *testing.T) {
	s := &Service{
		userLocks: make(map[uint32]*userLockState),
	}

	uid := uint32(2002)
	unlockFirst := s.lockUser(uid)

	done := make(chan struct{})
	go func() {
		unlockSecond := s.lockUser(uid)
		unlockSecond()
		close(done)
	}()

	// Wait until second goroutine has registered interest in the same user lock.
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.lockMu.Lock()
		lk := s.userLocks[uid]
		refs := 0
		if lk != nil {
			refs = lk.refs
		}
		s.lockMu.Unlock()
		if refs >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for lock refs to reach 2, got %d", refs)
		}
		time.Sleep(2 * time.Millisecond)
	}

	unlockFirst()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for second goroutine to finish")
	}

	if got := len(s.userLocks); got != 0 {
		t.Fatalf("expected lock map to be cleaned after both unlocks, got %d entries", got)
	}
}
