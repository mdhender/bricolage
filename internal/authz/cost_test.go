// Copyright (c) 2026 Michael D Henderson.

package authz

import (
	"testing"

	"github.com/mdhender/bricolage/internal/domain"
	"golang.org/x/crypto/bcrypt"
)

// The bcrypt cost is cheap under "go test" and only under "go test".
//
// The risk being defended is one-directional and worth naming: a test that
// hashes expensively wastes a few minutes, and a *binary* that hashes cheaply
// writes passwords that are weak for ever, because bcrypt records the cost
// inside the hash and nothing here rehashes on login. So the assertions below
// lean on the second case, and the decision is a pure function precisely so
// that the branch no test process ever takes can still be tested.

// TestCostForBothCircumstances covers the answer this package gives in each
// case, including the one a test binary can never observe directly.
func TestCostForBothCircumstances(t *testing.T) {
	if got := costFor(true); got != TestCost {
		t.Errorf("costFor(inTest=true) = %d, want TestCost (%d)", got, TestCost)
	}
	if got := costFor(false); got != Cost {
		t.Errorf("costFor(inTest=false) = %d, want Cost (%d)", got, Cost)
	}

	// The two constants themselves, because a change that made them equal
	// would leave every assertion above passing and mean the opposite thing.
	if Cost != bcrypt.DefaultCost {
		t.Errorf("Cost = %d, want bcrypt.DefaultCost (%d); a real run must hash properly",
			Cost, bcrypt.DefaultCost)
	}
	if TestCost != bcrypt.MinCost {
		t.Errorf("TestCost = %d, want bcrypt.MinCost (%d)", TestCost, bcrypt.MinCost)
	}
	if TestCost >= Cost {
		t.Errorf("TestCost (%d) is not cheaper than Cost (%d), so this costs time and buys nothing",
			TestCost, Cost)
	}
}

// TestThisBinaryHashesCheaply confirms the wiring rather than the decision: that
// testing.Testing() is actually consulted, and that a hash written here really
// carries the cheap cost.
//
// It reads the cost back out of the hash rather than trusting the constant,
// because the hash is what a database keeps and the constant is only what the
// code believes.
func TestThisBinaryHashesCheaply(t *testing.T) {
	if !testing.Testing() {
		t.Fatal("testing.Testing() is false inside a test; the mechanism this package relies on does not work")
	}
	if got := cost(); got != TestCost {
		t.Fatalf("cost() = %d in a test binary, want TestCost (%d)", got, TestCost)
	}

	hash, err := HashPassword("a sufficiently long password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	got, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		t.Fatalf("reading the cost back out of the hash: %v", err)
	}
	if got != TestCost {
		t.Errorf("the hash records cost %d, want %d", got, TestCost)
	}
}

// TestCheapHashesStillVerify is the property that makes the whole thing safe to
// do: bcrypt reads the cost out of the hash, so a password hashed at one cost
// verifies at that cost whatever the current setting is.
//
// It is also what would let production hashes survive a cost change, and it is
// why raising Cost needs no migration.
func TestCheapHashesStillVerify(t *testing.T) {
	const password = "a sufficiently long password"
	for _, c := range []int{bcrypt.MinCost, bcrypt.DefaultCost} {
		h, err := bcrypt.GenerateFromPassword([]byte(password), c)
		if err != nil {
			t.Fatalf("hashing at cost %d: %v", c, err)
		}
		// Active, because VerifyPassword refuses an inactive user before it
		// compares anything -- which is a different rule, tested elsewhere,
		// and would mask what this test is asking.
		u := domain.User{Active: true, PasswordHash: string(h)}
		if err := VerifyPassword(u, password); err != nil {
			t.Errorf("a hash written at cost %d does not verify: %v", c, err)
		}
		if err := VerifyPassword(u, "the wrong password entirely"); err == nil {
			t.Errorf("the wrong password verified against a cost-%d hash", c)
		}
	}
}
