package obs

import (
	"sync"
	"testing"
	"time"
)

func TestAuditLog_RecentEmpty(t *testing.T) {
	al := NewAuditLog(10)
	if got := al.Recent(5); len(got) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(got))
	}
}

func TestAuditLog_RecentOrder(t *testing.T) {
	al := NewAuditLog(10)
	for i := 0; i < 5; i++ {
		al.Append(AuditEntry{Path: string(rune('a' + i))})
	}
	got := al.Recent(5)
	if len(got) != 5 {
		t.Fatalf("want 5, got %d", len(got))
	}
	// newest first: last appended was 'e'
	if got[0].Path != "e" {
		t.Errorf("newest first: want 'e', got %q", got[0].Path)
	}
	if got[4].Path != "a" {
		t.Errorf("oldest last: want 'a', got %q", got[4].Path)
	}
}

func TestAuditLog_WrapAround(t *testing.T) {
	al := NewAuditLog(3)
	for i := 0; i < 7; i++ {
		al.Append(AuditEntry{Path: string(rune('a' + i))})
	}
	got := al.Recent(3)
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	// Last 3 written were 'e','f','g' — newest first
	if got[0].Path != "g" || got[1].Path != "f" || got[2].Path != "e" {
		t.Errorf("wrap-around order wrong: %v", func() []string {
			s := make([]string, len(got)); for i, e := range got { s[i] = e.Path }; return s
		}())
	}
}

func TestAuditLog_AskMoreThanStored(t *testing.T) {
	al := NewAuditLog(10)
	al.Append(AuditEntry{Path: "x"})
	got := al.Recent(50)
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
}

func TestAuditLog_Concurrent(t *testing.T) {
	al := NewAuditLog(100)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				al.Append(AuditEntry{Timestamp: time.Now(), Path: "/x"})
				_ = al.Recent(10)
			}
		}()
	}
	wg.Wait()
}
