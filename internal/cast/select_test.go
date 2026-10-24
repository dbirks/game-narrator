package cast

import "testing"

func TestSelectFix(t *testing.T) {
	rows := []Row{{Character: "Alda"}, {Character: "Bren"}, {Character: "Cato"}}
	if got, err := SelectFix(rows, ""); err != nil || len(got) != 3 {
		t.Fatalf("empty only = %v, %v; want all 3 rows", got, err)
	}
	got, err := SelectFix(rows, " Bren , Cato")
	if err != nil || len(got) != 2 || got[0].Character != "Bren" || got[1].Character != "Cato" {
		t.Fatalf("only Bren,Cato = %v, %v", got, err)
	}
	if _, err := SelectFix(rows, "Bren,Zed"); err == nil {
		t.Fatal("a name that is not a mismatch row must be an error")
	}
}
