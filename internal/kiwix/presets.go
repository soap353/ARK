package kiwix

// PresetTier represents a curated tier/option for a knowledge archive.
type PresetTier struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	EstimatedSize int64    `json:"estimated_size"` // bytes
	KiwixQuery    string   `json:"kiwix_query"`
	Flavour       string   `json:"flavour"`
	Category      string   `json:"category"`
	Language      string   `json:"language"`
	Tags          []string `json:"tags"`
}

// PresetBundle represents a collection of preset items grouped into a bundle (e.g. Starter Pack).
type PresetBundle struct {
	ID                 string       `json:"id"`
	Name               string       `json:"name"`
	Description        string       `json:"description"`
	EstimatedTotalSize int64        `json:"estimated_total_size"`
	Items              []PresetTier `json:"items"`
}

// Preset catalog definitions
var (
	TierWikipediaTop100 = PresetTier{
		ID:            "wikipedia-en-top100",
		Name:          "Wikipedia (EN) — Top 100",
		Description:   "Essential 100 articles with full media for ultra-compact storage.",
		EstimatedSize: 300 * 1024 * 1024, // ~300 MB
		KiwixQuery:    "wikipedia_en_top",
		Flavour:       "maxi",
		Category:      "knowledge",
		Language:      "eng",
		Tags:          []string{"wikipedia", "essential", "offline"},
	}

	TierWikipediaMini = PresetTier{
		ID:            "wikipedia-en-mini",
		Name:          "Wikipedia (EN) — Mini",
		Description:   "Curated lead summaries and essential articles (~12 GB).",
		EstimatedSize: 12 * 1024 * 1024 * 1024,
		KiwixQuery:    "wikipedia_en_all_mini",
		Flavour:       "mini",
		Category:      "knowledge",
		Language:      "eng",
		Tags:          []string{"wikipedia", "mini", "starter"},
	}

	TierWikipediaNoPics = PresetTier{
		ID:            "wikipedia-en-nopic",
		Name:          "Wikipedia (EN) — No Pictures",
		Description:   "All English Wikipedia articles without images (~55 GB).",
		EstimatedSize: 55 * 1024 * 1024 * 1024,
		KiwixQuery:    "wikipedia_en_all_nopic",
		Flavour:       "nopic",
		Category:      "knowledge",
		Language:      "eng",
		Tags:          []string{"wikipedia", "complete", "text-only"},
	}

	TierWikipediaFull = PresetTier{
		ID:            "wikipedia-en-full",
		Name:          "Wikipedia (EN) — Full (Maxi)",
		Description:   "Complete English Wikipedia with all images and media (~110 GB).",
		EstimatedSize: 110 * 1024 * 1024 * 1024,
		KiwixQuery:    "wikipedia_en_all_maxi",
		Flavour:       "maxi",
		Category:      "knowledge",
		Language:      "eng",
		Tags:          []string{"wikipedia", "full", "media"},
	}

	TierWikiMed = PresetTier{
		ID:            "wikimed-en",
		Name:          "WikiMed (Medical Encyclopedia)",
		Description:   "Complete offline medical encyclopedia from Wiki Project Med (~5 GB).",
		EstimatedSize: 5 * 1024 * 1024 * 1024,
		KiwixQuery:    "wikimed_en_all_maxi",
		Flavour:       "maxi",
		Category:      "medical",
		Language:      "eng",
		Tags:          []string{"medical", "health", "survival", "first-aid"},
	}

	TierIFixit = PresetTier{
		ID:            "ifixit-en",
		Name:          "iFixit Repair Manuals",
		Description:   "Thousands of step-by-step repair guides for electronics, appliances, and vehicles.",
		EstimatedSize: 3 * 1024 * 1024 * 1024, // ~3 GB
		KiwixQuery:    "ifixit_en_all",
		Flavour:       "maxi",
		Category:      "repair",
		Language:      "eng",
		Tags:          []string{"repair", "survival", "tools", "diy"},
	}

	TierWikiHow = PresetTier{
		ID:            "wikihow-en",
		Name:          "WikiHow",
		Description:   "Illustrated how-to survival and life guides (~7 GB).",
		EstimatedSize: 7 * 1024 * 1024 * 1024,
		KiwixQuery:    "wikihow_en_all",
		Flavour:       "maxi",
		Category:      "survival",
		Language:      "eng",
		Tags:          []string{"survival", "howto", "guides"},
	}

	TierGutenberg = PresetTier{
		ID:            "gutenberg-en",
		Name:          "Project Gutenberg (EN)",
		Description:   "Over 70,000 free classic public domain books (~70 GB).",
		EstimatedSize: 70 * 1024 * 1024 * 1024,
		KiwixQuery:    "gutenberg_en_all",
		Flavour:       "maxi",
		Category:      "books",
		Language:      "eng",
		Tags:          []string{"books", "literature", "gutenberg"},
	}

	TierStackExchange = PresetTier{
		ID:            "stackexchange-diy-superuser",
		Name:          "Stack Exchange (DIY, SuperUser & Outdoor)",
		Description:   "Offline questions and answers for computer repair, gardening, DIY, and outdoors.",
		EstimatedSize: 4 * 1024 * 1024 * 1024,
		KiwixQuery:    "stackexchange_en_diy",
		Flavour:       "maxi",
		Category:      "tools",
		Language:      "eng",
		Tags:          []string{"tech", "diy", "answers"},
	}
)

// GetAllPresetTiers returns all available individual preset tiers.
func GetAllPresetTiers() []PresetTier {
	return []PresetTier{
		TierWikipediaTop100,
		TierWikipediaMini,
		TierWikipediaNoPics,
		TierWikipediaFull,
		TierWikiMed,
		TierIFixit,
		TierWikiHow,
		TierGutenberg,
		TierStackExchange,
	}
}

// GetStarterPack returns the Prepper Starter Pack preset bundle (~27 GB).
func GetStarterPack() PresetBundle {
	items := []PresetTier{
		TierWikiMed,       // ~5 GB
		TierWikipediaMini, // ~12 GB
		TierIFixit,        // ~3 GB
		TierWikiHow,       // ~7 GB
	}

	var totalSize int64
	for _, it := range items {
		totalSize += it.EstimatedSize
	}

	return PresetBundle{
		ID:                 "prepper-starter-pack",
		Name:               "Prepper Starter Pack",
		Description:        "The essential offline survival library: WikiMed + Wikipedia Mini + iFixit + WikiHow (~27 GB).",
		EstimatedTotalSize: totalSize,
		Items:              items,
	}
}

// FindPresetByID finds a preset tier by its ID.
func FindPresetByID(id string) *PresetTier {
	for _, t := range GetAllPresetTiers() {
		if t.ID == id {
			return &t
		}
	}
	return nil
}
