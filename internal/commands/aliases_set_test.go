package commands

// mbx aliases set — added alongside the W10a review fix so cards create's
// alias-save-failure recovery message ("run 'mbx aliases set ...' manually")
// names a real command rather than a hypothetical one. Local file write
// only, no network — round-trips through aliases.Load/Find like `aliases
// sync` does.

import (
	"testing"

	"github.com/nicolasacchi/mbx/internal/aliases"
)

func resetAliasesSetFlags(t *testing.T) {
	t.Helper()
	origCardID, origDBID, origName := aliasesSetCardID, aliasesSetDatabaseID, aliasesSetName
	t.Cleanup(func() {
		aliasesSetCardID, aliasesSetDatabaseID, aliasesSetName = origCardID, origDBID, origName
	})
}

func TestAliasesSet_RoundTrips(t *testing.T) {
	resetAliasesSetFlags(t)
	isolateConfigEnv(t)

	aliasesSetCardID = 792
	aliasesSetDatabaseID = 2
	aliasesSetName = "mbx-smoke-w4-card3"

	if err := aliasesSetCmd.RunE(aliasesSetCmd, []string{"mbx-smoke-w4-card3"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	store, err := aliases.Load()
	if err != nil {
		t.Fatalf("aliases.Load: %v", err)
	}
	a, ok := store.Find("mbx-smoke-w4-card3")
	if !ok {
		t.Fatal("expected the alias to be registered")
	}
	if a.CardID != 792 {
		t.Errorf("CardID = %d, want 792", a.CardID)
	}
	if a.DatabaseID != 2 {
		t.Errorf("DatabaseID = %d, want 2", a.DatabaseID)
	}
	if a.Name != "mbx-smoke-w4-card3" {
		t.Errorf("Name = %q, want mbx-smoke-w4-card3", a.Name)
	}
}

func TestAliasesSet_RequiresCardID(t *testing.T) {
	resetAliasesSetFlags(t)
	isolateConfigEnv(t)

	aliasesSetCardID = 0
	if err := aliasesSetCmd.RunE(aliasesSetCmd, []string{"x"}); err == nil {
		t.Fatal("expected an error when --card-id is missing/zero")
	}
}
