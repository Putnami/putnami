package abort

import "testing"

// A held child is killed by KillInteractive until its holder releases it.
func TestHoldInteractiveUntilReleased(t *testing.T) {
	if InteractiveHeld() {
		t.Fatal("InteractiveHeld = true before any hold")
	}
	kills := 0
	release := HoldInteractive(func() { kills++ })
	if !InteractiveHeld() {
		t.Fatal("InteractiveHeld = false while a child is held")
	}
	KillInteractive()
	if kills != 1 {
		t.Fatalf("KillInteractive killed %d times, want 1", kills)
	}
	release()
	release()
	if InteractiveHeld() {
		t.Fatal("InteractiveHeld = true after the release")
	}
	KillInteractive()
	if kills != 1 {
		t.Fatalf("KillInteractive killed a released child: %d kills", kills)
	}
}

// Two holders are independent: releasing one keeps the other held.
func TestHoldInteractiveKeepsEachHolder(t *testing.T) {
	first, second := 0, 0
	releaseFirst := HoldInteractive(func() { first++ })
	releaseSecond := HoldInteractive(func() { second++ })
	releaseFirst()
	if !InteractiveHeld() {
		t.Fatal("releasing one holder released the other")
	}
	KillInteractive()
	releaseSecond()
	if first != 0 || second != 1 {
		t.Fatalf("kills = %d, %d; want 0, 1", first, second)
	}
}
