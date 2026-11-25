package fieldguide

import (
	"embed"
	"encoding/json"
	"strings"
)

//go:embed data/*.json
var guideFS embed.FS

// Condition represents an offline medical / field condition entry for Ark.
type Condition struct {
	Slug        string   `json:"slug"`
	Label       string   `json:"label"`
	Category    string   `json:"category"`
	SearchTerms []string `json:"searchTerms"`
}

// Remedy represents an offline home or natural remedy entry for Ark.
type Remedy struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	CommonNames []string `json:"commonNames"`
	Conditions  []string `json:"conditions"`
	Uses        string   `json:"uses"`
	How         string   `json:"how"`
	Evidence    string   `json:"evidence"`
	Cautions    string   `json:"cautions"`
	SourceUrl   string   `json:"sourceUrl"`
	Kind        string   `json:"kind"` // "Home Remedy" or "Natural Remedy"
}

// CreatorPack represents an offline video/creator bundle from creator-packs.json.
type CreatorPack struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Creator     string `json:"creator"`
	Description string `json:"description"`
	Version     string `json:"version"`
	ResourceID  string `json:"resource_id"`
	VideoCount  int    `json:"video_count"`
	SizeMB      int    `json:"size_mb"`
}

// Guide holds all loaded offline field reference datasets.
type Guide struct {
	Conditions []Condition
	Remedies   []Remedy
	Categories []string
}

// LoadGuide parses the embedded Ark conditions, home remedies, and natural remedies files.
func LoadGuide() (*Guide, error) {
	g := &Guide{}

	var condWrap struct {
		Conditions []Condition `json:"conditions"`
	}
	if raw, err := guideFS.ReadFile("data/conditions.json"); err == nil {
		if err := json.Unmarshal(raw, &condWrap); err != nil {
			return nil, err
		}
		g.Conditions = condWrap.Conditions
	}

	catSeen := make(map[string]bool)
	for _, c := range g.Conditions {
		if c.Category != "" && !catSeen[c.Category] {
			catSeen[c.Category] = true
			g.Categories = append(g.Categories, c.Category)
		}
	}

	for _, file := range []struct {
		path string
		kind string
	}{
		{"data/home_remedies.json", "Home Remedy"},
		{"data/natural_remedies.json", "Natural Remedy"},
	} {
		var remWrap struct {
			Remedies []Remedy `json:"remedies"`
		}
		if raw, err := guideFS.ReadFile(file.path); err == nil {
			if err := json.Unmarshal(raw, &remWrap); err != nil {
				return nil, err
			}
			for _, r := range remWrap.Remedies {
				r.Kind = file.kind
				g.Remedies = append(g.Remedies, r)
			}
		}
	}

	return g, nil
}

// Search filters remedies matching query and/or condition slug.
func (g *Guide) Search(query, conditionSlug, kind string) []Remedy {
	q := strings.ToLower(strings.TrimSpace(query))
	cond := strings.ToLower(strings.TrimSpace(conditionSlug))
	kindFilter := strings.ToLower(strings.TrimSpace(kind))

	// Collect matching condition slugs from search query too so searching "cough" finds honey etc.
	matchedCondSlugs := make(map[string]bool)
	if q != "" {
		for _, c := range g.Conditions {
			if strings.Contains(strings.ToLower(c.Label), q) || strings.Contains(strings.ToLower(c.Category), q) {
				matchedCondSlugs[c.Slug] = true
				continue
			}
			for _, st := range c.SearchTerms {
				if strings.Contains(strings.ToLower(st), q) {
					matchedCondSlugs[c.Slug] = true
					break
				}
			}
		}
	}

	var out []Remedy
	for _, r := range g.Remedies {
		if kindFilter != "" && kindFilter != "all" && !strings.EqualFold(r.Kind, kind) {
			continue
		}
		if cond != "" {
			hasCond := false
			for _, rc := range r.Conditions {
				if strings.EqualFold(rc, cond) {
					hasCond = true
					break
				}
			}
			if !hasCond {
				continue
			}
		}
		if q != "" {
			haystack := strings.ToLower(r.Name + " " + strings.Join(r.CommonNames, " ") + " " + r.Uses + " " + r.How + " " + strings.Join(r.Conditions, " "))
			matched := strings.Contains(haystack, q)
			if !matched {
				for _, rc := range r.Conditions {
					if matchedCondSlugs[rc] {
						matched = true
						break
					}
				}
			}
			if !matched {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// GetCreatorPacks returns embedded Ark creator pack entries.
func GetCreatorPacks() ([]CreatorPack, error) {
	var wrap struct {
		Packs []CreatorPack `json:"packs"`
	}
	raw, err := guideFS.ReadFile("data/creator-packs.json")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, err
	}
	return wrap.Packs, nil
}
