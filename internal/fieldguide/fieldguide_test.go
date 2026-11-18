package fieldguide

import "testing"

func TestLoadGuideAndSearch(t *testing.T) {
	g, err := LoadGuide()
	if err != nil {
		t.Fatalf("LoadGuide failed: %v", err)
	}
	if len(g.Conditions) < 20 {
		t.Errorf("expected at least 20 conditions, got %d", len(g.Conditions))
	}
	if len(g.Remedies) < 20 {
		t.Errorf("expected at least 20 remedies, got %d", len(g.Remedies))
	}

	burns := g.Search("burn", "", "")
	if len(burns) == 0 {
		t.Errorf("expected remedies matching 'burn'")
	}

	coughs := g.Search("", "cough", "Home Remedy")
	if len(coughs) == 0 {
		t.Errorf("expected home remedies for 'cough'")
	}

	packs, err := GetCreatorPacks()
	if err != nil || len(packs) == 0 {
		t.Errorf("expected creator packs to load, got len=%d err=%v", len(packs), err)
	}
}
