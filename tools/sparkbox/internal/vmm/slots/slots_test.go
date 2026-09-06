package slots

import "testing"

func TestClaimTakesTheLowestFreeSlot(t *testing.T) {
	p := New("172.30.0.0/30-ish", 4)
	for want, name := range []string{"a", "b", "c", "d"} {
		got, err := p.Claim(name)
		if err != nil {
			t.Fatalf("Claim(%q): %v", name, err)
		}
		if got != want {
			t.Fatalf("Claim(%q) = %d, want %d", name, got, want)
		}
	}
	if _, err := p.Claim("e"); err == nil {
		t.Fatal("a fifth claim was served from a pool of four")
	}
}

// Lowest-free rather than next-unused, which both drivers already did: a node
// that churns sandboxes would otherwise walk off the end of its subnet with
// almost every slot in it idle.
func TestAReleasedSlotIsReusedBeforeAFreshOne(t *testing.T) {
	p := New("subnet", 8)
	for _, name := range []string{"a", "b", "c"} {
		if _, err := p.Claim(name); err != nil {
			t.Fatal(err)
		}
	}
	p.Release("b")
	got, err := p.Claim("d")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("Claim after releasing slot 1 = %d, want 1", got)
	}
}

func TestReservedSlotsAreNeverHandedOut(t *testing.T) {
	p := New("subnet", 4, 0, 2)
	first, err := p.Claim("a")
	if err != nil || first != 1 {
		t.Fatalf("Claim = %d, %v; want slot 1 with 0 and 2 reserved", first, err)
	}
	second, err := p.Claim("b")
	if err != nil || second != 3 {
		t.Fatalf("Claim = %d, %v; want slot 3", second, err)
	}
	if _, err := p.Claim("c"); err == nil {
		t.Fatal("a reserved slot was handed to a guest")
	}
}

// The whole reason this type exists: two drivers sharing one pool cannot both
// be given slot 0, which is what their private copies each did.
func TestTwoHoldersOfOnePoolNeverShareASlot(t *testing.T) {
	p := New("subnet", 16)
	seen := map[int]string{}
	for _, name := range []string{"fc-1", "qemu-1", "fc-2", "qemu-2", "fc-3"} {
		idx, err := p.Claim(name)
		if err != nil {
			t.Fatal(err)
		}
		if other, taken := seen[idx]; taken {
			t.Fatalf("slot %d handed to both %q and %q", idx, other, name)
		}
		seen[idx] = name
	}
}

func TestClaimIsIdempotentForOneName(t *testing.T) {
	p := New("subnet", 4)
	first, err := p.Claim("a")
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.Claim("a")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("a second Claim for one name moved it from %d to %d", first, again)
	}
	if len(p.Held()) != 1 {
		t.Fatalf("held = %v, want one entry", p.Held())
	}
}

func TestHoldRefusesASlotSomebodyElseHas(t *testing.T) {
	p := New("subnet", 4)
	if err := p.Hold("a", 2); err != nil {
		t.Fatal(err)
	}
	if err := p.Hold("b", 2); err == nil {
		t.Fatal("two names were allowed to hold one slot; they would derive the same tap and uid")
	}
	if err := p.Hold("a", 2); err != nil {
		t.Fatalf("re-holding the same slot for the same name: %v", err)
	}
	if err := p.Hold("c", 9); err == nil {
		t.Fatal("a slot outside the namespace was accepted")
	}
}

// Release runs from several paths for one VM — Destroy, DropSnapshots, a
// rollback — so a second call must not free a slot a later create already has.
func TestReleaseIsIdempotent(t *testing.T) {
	p := New("subnet", 4)
	if _, err := p.Claim("a"); err != nil {
		t.Fatal(err)
	}
	p.Release("a")
	next, err := p.Claim("b")
	if err != nil {
		t.Fatal(err)
	}
	p.Release("a")
	if held := p.Held()["b"]; held != next {
		t.Fatalf("a repeated release took slot %d away from b (now %d)", next, held)
	}
}

// A rename is the same guest with the same tap and the same addresses, so the
// slot must move rather than be released and re-taken.
func TestRenameKeepsTheSlot(t *testing.T) {
	p := New("subnet", 4)
	idx, err := p.Claim("old")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Rename("old", "new"); err != nil {
		t.Fatal(err)
	}
	held := p.Held()
	if held["new"] != idx {
		t.Fatalf("after rename new holds %d, want %d", held["new"], idx)
	}
	if _, still := held["old"]; still {
		t.Fatal("the old name still holds a slot")
	}
	if err := p.Rename("nobody", "somebody"); err != nil {
		t.Fatalf("renaming a name that holds nothing: %v", err)
	}
}
