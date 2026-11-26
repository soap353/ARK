package kiwix

import (
	"fmt"
	"strings"
)

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

// ContentResource represents a single downloadable offline asset (ZIM or PMTiles).
type ContentResource struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	KiwixQuery  string `json:"kiwix_query,omitempty"`
	FallbackURL string `json:"fallback_url"`
	SizeMB      int    `json:"size_mb"`
	Kind        string `json:"kind"` // "zim" or "pmtiles"
}

// WikipediaOption represents one of Ark's Wikipedia package tiers.
type WikipediaOption struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	SizeLabel    string `json:"size_label"`
	KiwixQuery   string `json:"kiwix_query"`
	FallbackURL  string `json:"fallback_url"`
	PresetTarget string `json:"preset_target"`
	MatchPrefix  string `json:"match_prefix"`
}

// ArkTier represents a curated difficulty/size tier within an Additional Content collection.
type ArkTier struct {
	Slug         string            `json:"slug"`
	Name         string            `json:"name"` // Essential, Standard, Comprehensive
	Description  string            `json:"description"`
	SizeLabel    string            `json:"size_label"`
	Recommended  bool              `json:"recommended,omitempty"`
	IncludesTier string            `json:"includes_tier,omitempty"`
	Resources    []ContentResource `json:"resources"`
}

// ArkCollection represents one of the 6 Ark curated content collections.
type ArkCollection struct {
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	SizeRange   string    `json:"size_range"`
	Tiers       []ArkTier `json:"tiers"`
}

// MapRegion represents one of the 9 US regional PMTiles map bundles.
type MapRegion struct {
	Slug        string            `json:"slug"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	ItemCount   int               `json:"item_count"`
	SizeLabel   string            `json:"size_label"`
	Resources   []ContentResource `json:"resources"`
}

// GetWikipediaOptions returns all 5 downloadable Ark Wikipedia package options.
func GetWikipediaOptions() []WikipediaOption {
	return []WikipediaOption{
		{
			ID:           "top-mini",
			Name:         "Quick Reference",
			Description:  "Top 100,000 articles with minimal images. Great for quick lookups.",
			SizeLabel:    "331 MB",
			KiwixQuery:   "wikipedia_en_top_mini",
			FallbackURL:  "https://download.kiwix.org/zim/wikipedia/wikipedia_en_top_mini_2026-06.zim",
			PresetTarget: "wikipedia-top-mini",
			MatchPrefix:  "wikipedia_en_top_mini",
		},
		{
			ID:           "top-nopic",
			Name:         "Popular Articles",
			Description:  "Top articles without images. Good balance of content and size.",
			SizeLabel:    "2.2 GB",
			KiwixQuery:   "wikipedia_en_top_nopic",
			FallbackURL:  "https://download.kiwix.org/zim/wikipedia/wikipedia_en_top_nopic_2026-06.zim",
			PresetTarget: "wikipedia-top-nopic",
			MatchPrefix:  "wikipedia_en_top_nopic",
		},
		{
			ID:           "all-mini",
			Name:         "Complete Wikipedia (Compact)",
			Description:  "All 6+ million articles in condensed format.",
			SizeLabel:    "12.2 GB",
			KiwixQuery:   "wikipedia_en_all_mini",
			FallbackURL:  "https://download.kiwix.org/zim/wikipedia/wikipedia_en_all_mini_2026-06.zim",
			PresetTarget: "wikipedia_en_mini",
			MatchPrefix:  "wikipedia_en_all_mini",
		},
		{
			ID:           "all-nopic",
			Name:         "Complete Wikipedia (No Images)",
			Description:  "All articles without images. Comprehensive offline reference.",
			SizeLabel:    "51.5 GB",
			KiwixQuery:   "wikipedia_en_all_nopic",
			FallbackURL:  "https://download.kiwix.org/zim/wikipedia/wikipedia_en_all_nopic_2026-06.zim",
			PresetTarget: "wikipedia_en_nopic",
			MatchPrefix:  "wikipedia_en_all_nopic",
		},
		{
			ID:           "all-maxi",
			Name:         "Complete Wikipedia (Full)",
			Description:  "The complete experience with all images and media.",
			SizeLabel:    "121.1 GB",
			KiwixQuery:   "wikipedia_en_all_maxi",
			FallbackURL:  "https://download.kiwix.org/zim/wikipedia/wikipedia_en_all_maxi_2026-02.zim",
			PresetTarget: "wikipedia_en_full",
			MatchPrefix:  "wikipedia_en_all_maxi",
		},
	}
}

func zimRes(id, title, desc, url string, sizeMB int) ContentResource {
	return ContentResource{
		ID:          id,
		Title:       title,
		Description: desc,
		KiwixQuery:  id,
		FallbackURL: url,
		SizeMB:      sizeMB,
		Kind:        "zim",
	}
}

func mapRes(stateID, title string, sizeMB int) ContentResource {
	return ContentResource{
		ID:          stateID,
		Title:       title,
		Description: fmt.Sprintf("Topographic and street maps for %s.", title),
		FallbackURL: fmt.Sprintf("https://github.com/Crosstalk-Solutions/project-nomad-maps/raw/refs/heads/master/pmtiles/%s_2025-12.pmtiles", stateID),
		SizeMB:      sizeMB,
		Kind:        "pmtiles",
	}
}

// GetArkCollections returns the 6 curated Ark Additional Content collections and their 3 tiers.
func GetArkCollections() []ArkCollection {
	return []ArkCollection{
		{
			Slug:        "medicine",
			Name:        "Medicine",
			Description: "Medical references, guides, and encyclopedias for healthcare information and emergency preparedness.",
			SizeRange:   "315 MB - 6.8 GB",
			Tiers: []ArkTier{
				{
					Slug:        "medicine-essential",
					Name:        "Essential",
					Description: "Core medical references for first aid, medications, and emergency care. Start here.",
					SizeLabel:   "315 MB",
					Recommended: true,
					Resources: []ContentResource{
						zimRes("zimgit-medicine_en", "Medical Library", "Field and emergency medicine books and guides", "https://download.kiwix.org/zim/other/zimgit-medicine_en_2024-08.zim", 67),
						zimRes("nhs.uk_en_medicines", "NHS Medicines A to Z", "How medicines work, dosages, side effects, and interactions", "https://download.kiwix.org/zim/zimit/nhs.uk_en_medicines_2025-12.zim", 16),
						zimRes("irp.fas.org_en_military-medicine", "Military Medicine", "Tactical and field medicine manuals", "https://download.kiwix.org/zim/zimit/irp.fas.org_en_military-medicine_2026-10.zim", 62),
						zimRes("wwwnc.cdc.gov_en_all", "CDC Health Information", "Disease prevention, travel health, and outbreak information", "https://download.kiwix.org/zim/zimit/wwwnc.cdc.gov_en_all_2024-11.zim", 170),
					},
				},
				{
					Slug:         "medicine-standard",
					Name:         "Standard",
					Description:  "Comprehensive medical encyclopedia with detailed health information. Includes everything in Essential.",
					SizeLabel:    "2.1 GB",
					IncludesTier: "medicine-essential",
					Resources: []ContentResource{
						zimRes("medlineplus.gov_en_all", "MedlinePlus", "NIH consumer health encyclopedia - diseases, conditions, drugs, supplements", "https://download.kiwix.org/zim/zimit/medlineplus.gov_en_all_2025-01.zim", 1800),
					},
				},
				{
					Slug:         "medicine-comprehensive",
					Name:         "Comprehensive",
					Description:  "Professional-level medical references and textbooks. Includes everything in Standard.",
					SizeLabel:    "6.8 GB",
					IncludesTier: "medicine-standard",
					Resources: []ContentResource{
						zimRes("wikipedia_en_medicine_maxi", "Wikipedia Medicine", "Curated medical articles from Wikipedia with images", "https://download.kiwix.org/zim/wikipedia/wikipedia_en_medicine_maxi_2026-01.zim", 2000),
						zimRes("libretexts.org_en_med", "LibreTexts Medicine", "Medical textbooks and clinical references", "https://download.kiwix.org/zim/libretexts/libretexts.org_en_med_2025-01.zim", 1100),
						zimRes("librepathology_en_all_maxi", "LibrePathology", "Open-access pathology reference with histology", "https://download.kiwix.org/zim/other/librepathology_en_all_maxi_2026-09.zim", 95),
					},
				},
			},
		},
		{
			Slug:        "survival",
			Name:        "Survival & Preparedness",
			Description: "Emergency preparedness, bug-out planning, and tactical survival skills for crisis situations.",
			SizeRange:   "2.3 GB - 14.5 GB",
			Tiers: []ArkTier{
				{
					Slug:        "survival-essential",
					Name:        "Essential",
					Description: "Water, food, and knot references plus core survival videos. Start here for emergency basics.",
					SizeLabel:   "2.3 GB",
					Recommended: true,
					Resources: []ContentResource{
						zimRes("zimgit-water_en", "Water Treatment Library", "Purification, filtration, and safe storage", "https://download.kiwix.org/zim/other/zimgit-water_en_2024-08.zim", 20),
						zimRes("zimgit-food-preparation_en", "Food for Preppers", "Food storage, canning, and emergency cooking", "https://download.kiwix.org/zim/other/zimgit-food-preparation_en_2025-04.zim", 97),
						zimRes("zimgit-knots_en", "A Library of Knots", "Step-by-step tying guides for camp and rescue", "https://download.kiwix.org/zim/other/zimgit-knots_en_2024-08.zim", 27),
						zimRes("canadian-prepper_en_winterprepping", "Canadian Prepper: Winter Prepping", "Cold-weather survival and heating videos", "https://download.kiwix.org/zim/videos/canadian-prepper_en_winterprepping_2026-08.zim", 1342),
						zimRes("canadian-prepper_en_bugoutroll", "Canadian Prepper: Bug Out Roll", "Bug-out gear and tactical loadout guides", "https://download.kiwix.org/zim/videos/canadian-prepper_en_bugoutroll_2026-07.zim", 879),
					},
				},
				{
					Slug:         "survival-standard",
					Name:         "Standard",
					Description:  "Official disaster planning, field skills, and radio communications, plus bug-out and urban survival.",
					SizeLabel:    "10.1 GB",
					IncludesTier: "survival-essential",
					Resources: []ContentResource{
						zimRes("www.ready.gov_en", "Ready.gov", "Official disaster preparedness plans and emergency checklists", "https://download.kiwix.org/zim/zimit/www.ready.gov_en_2024-12.zim", 2429),
						zimRes("outdoors.stackexchange.com_en_all", "The Great Outdoors Q&A", "Wilderness navigation, shelter, and backcountry survival Q&A", "https://download.kiwix.org/zim/stack_exchange/outdoors.stackexchange.com_en_all_2026-02.zim", 142),
						zimRes("ham.stackexchange.com_en_all", "Amateur Radio Q&A", "Emergency radio communications and antennas Q&A", "https://download.kiwix.org/zim/stack_exchange/ham.stackexchange.com_en_all_2026-02.zim", 75),
						zimRes("canadian-prepper_en_bugoutconcepts", "Canadian Prepper: Bug Out Concepts", "Evacuation planning and shelter videos", "https://download.kiwix.org/zim/videos/canadian-prepper_en_bugoutconcepts_2026-08.zim", 2886),
						zimRes("urban-prepper_en_all", "Urban Prepper", "Urban disaster readiness and EDC planning videos", "https://download.kiwix.org/zim/videos/urban-prepper_en_all_2026-08.zim", 2245),
					},
				},
				{
					Slug:         "survival-comprehensive",
					Name:         "Comprehensive",
					Description:  "Complete prepper library - post-disaster recovery, growing food, off-grid tech, and classic strategy.",
					SizeLabel:    "14.5 GB",
					IncludesTier: "survival-standard",
					Resources: []ContentResource{
						zimRes("canadian-prepper_en_preppingfood", "Canadian Prepper: Prepping Food", "Long-term food storage and preservation videos", "https://download.kiwix.org/zim/videos/canadian-prepper_en_preppingfood_2026-07.zim", 2160),
						zimRes("zimgit-post-disaster_en", "Post Disaster Resource Library", "Field sanitation, shelter building, and recovery", "https://download.kiwix.org/zim/other/zimgit-post-disaster_en_2024-05.zim", 644),
						zimRes("100r.co_en_all", "Hundred Rabbits", "Off-grid sailing, solar computing, and resilient tooling", "https://download.kiwix.org/zim/zimit/100r.co_en_all_2026-06.zim", 172),
						zimRes("gutenberg_en_lcc-u", "Project Gutenberg: Military Science", "Classic strategy and logistics texts", "https://download.kiwix.org/zim/gutenberg/gutenberg_en_lcc-u_2026-03.zim", 1200),
						zimRes("cd3wdproject.org_en_all", "CD3WD: Appropriate Technology Library", "Low-tech engineering, water, and self-reliance manuals", "https://download.kiwix.org/zim/zimit/cd3wdproject.org_en_all_2025-11.zim", 581),
					},
				},
			},
		},
		{
			Slug:        "education",
			Name:        "Education & Reference",
			Description: "Encyclopedias, textbooks, tutorials, and educational videos for self-directed learning.",
			SizeRange:   "3 GB - 46.4 GB",
			Tiers: []ArkTier{
				{
					Slug:        "education-essential",
					Name:        "Essential",
					Description: "Core reference materials - open textbooks and essential educational content. Lightweight, text-focused.",
					SizeLabel:   "3 GB",
					Recommended: true,
					Resources: []ContentResource{
						zimRes("wikibooks_en_all_nopic", "Wikibooks", "Open-content textbooks across subjects (text-focused)", "https://download.kiwix.org/zim/wikibooks/wikibooks_en_all_nopic_2026-10.zim", 3100),
					},
				},
				{
					Slug:         "education-standard",
					Name:         "Standard",
					Description:  "Adds educational videos, university-level tutorials, and STEM textbooks. Includes Essential.",
					SizeLabel:    "17.2 GB",
					IncludesTier: "education-essential",
					Resources: []ContentResource{
						zimRes("ted_mul_ted-ed", "TED-Ed", "Educational animations and structured lessons", "https://download.kiwix.org/zim/ted/ted_mul_ted-ed_2026-09.zim", 6381),
						zimRes("wikiversity_en_all_maxi", "Wikiversity", "Open learning courses and workshops", "https://download.kiwix.org/zim/wikiversity/wikiversity_en_all_maxi_2026-02.zim", 2370),
						zimRes("libretexts.org_en_math", "LibreTexts Mathematics", "Open college mathematics textbooks", "https://download.kiwix.org/zim/libretexts/libretexts.org_en_math_2026-01.zim", 792),
						zimRes("libretexts.org_en_phys", "LibreTexts Physics", "Open physics textbooks and problem sets", "https://download.kiwix.org/zim/libretexts/libretexts.org_en_phys_2026-01.zim", 534),
						zimRes("libretexts.org_en_chem", "LibreTexts Chemistry", "Open chemistry textbooks and references", "https://download.kiwix.org/zim/libretexts/libretexts.org_en_chem_2025-01.zim", 2180),
						zimRes("libretexts.org_en_bio", "LibreTexts Biology", "Open biology textbooks and references", "https://download.kiwix.org/zim/libretexts/libretexts.org_en_bio_2025-01.zim", 2240),
					},
				},
				{
					Slug:         "education-comprehensive",
					Name:         "Comprehensive",
					Description:  "Complete educational library with enhanced textbooks and TED talks. Includes Standard.",
					SizeLabel:    "46.4 GB",
					IncludesTier: "education-standard",
					Resources: []ContentResource{
						zimRes("wikibooks_en_all_maxi", "Wikibooks (With Images)", "Illustrated open textbooks", "https://download.kiwix.org/zim/wikibooks/wikibooks_en_all_maxi_2026-01.zim", 5400),
						zimRes("ted_mul_ted-conference", "TED Conference", "Full library of TED conference talks", "https://download.kiwix.org/zim/ted/ted_mul_ted-conference_2026-09.zim", 18136),
						zimRes("libretexts.org_en_human", "LibreTexts Humanities", "History, literature, and humanities textbooks", "https://download.kiwix.org/zim/libretexts/libretexts.org_en_human_2025-01.zim", 3730),
						zimRes("libretexts.org_en_geo", "LibreTexts Geosciences", "Earth science, geology, and climate texts", "https://download.kiwix.org/zim/libretexts/libretexts.org_en_geo_2026-01.zim", 1127),
						zimRes("libretexts.org_en_eng", "LibreTexts Engineering", "Applied engineering textbooks", "https://download.kiwix.org/zim/libretexts/libretexts.org_en_eng_2025-01.zim", 678),
						zimRes("libretexts.org_en_biz", "LibreTexts Business", "Accounting, economics, and management texts", "https://download.kiwix.org/zim/libretexts/libretexts.org_en_biz_2026-01.zim", 801),
					},
				},
			},
		},
		{
			Slug:        "diy",
			Name:        "DIY & Repair",
			Description: "Fix it yourself - repair guides, home improvement, woodworking, and vehicle maintenance.",
			SizeRange:   "420 MB - 5.6 GB",
			Tiers: []ArkTier{
				{
					Slug:        "diy-essential",
					Name:        "Essential",
					Description: "Core repair knowledge - woodworking basics and vehicle maintenance Q&A.",
					SizeLabel:   "420 MB",
					Recommended: true,
					Resources: []ContentResource{
						zimRes("woodworking.stackexchange.com_en_all", "Woodworking Q&A", "Joinery, tool tuning, and carpentry Q&A", "https://download.kiwix.org/zim/stack_exchange/woodworking.stackexchange.com_en_all_2026-02.zim", 99),
						zimRes("mechanics.stackexchange.com_en_all", "Motor Vehicle Maintenance Q&A", "Auto and small-engine diagnostics and repair", "https://download.kiwix.org/zim/stack_exchange/mechanics.stackexchange.com_en_all_2026-02.zim", 321),
					},
				},
				{
					Slug:         "diy-standard",
					Name:         "Standard",
					Description:  "Home improvement expertise with thousands of Q&A threads. Includes Essential.",
					SizeLabel:    "2.3 GB",
					IncludesTier: "diy-essential",
					Resources: []ContentResource{
						zimRes("diy.stackexchange.com_en_all", "DIY & Home Improvement Q&A", "Plumbing, electrical, framing, and household repair Q&A", "https://download.kiwix.org/zim/stack_exchange/diy.stackexchange.com_en_all_2026-02.zim", 1900),
					},
				},
				{
					Slug:         "diy-comprehensive",
					Name:         "Comprehensive",
					Description:  "Complete repair library with step-by-step guides for electronics and appliances. Includes Standard.",
					SizeLabel:    "5.6 GB",
					IncludesTier: "diy-standard",
					Resources: []ContentResource{
						zimRes("ifixit_en_all", "iFixit Repair Guides", "Illustrated teardown and repair manuals for electronics, appliances, and tools", "https://download.kiwix.org/zim/ifixit/ifixit_en_all_2025-12.zim", 3380),
					},
				},
			},
		},
		{
			Slug:        "agriculture",
			Name:        "Agriculture & Food",
			Description: "Grow and prepare your own food - gardening, cooking techniques, and food preservation.",
			SizeRange:   "40 MB - 9.2 GB",
			Tiers: []ArkTier{
				{
					Slug:        "agriculture-essential",
					Name:        "Essential",
					Description: "Quick recipes and cooking basics for everyday meal preparation.",
					SizeLabel:   "40 MB",
					Recommended: true,
					Resources: []ContentResource{
						zimRes("foss.cooking_en_all", "FOSS Cooking", "Open-source recipes and pantry meals", "https://download.kiwix.org/zim/zimit/foss.cooking_en_all_2026-02.zim", 24),
						zimRes("based.cooking_en_all", "Based.Cooking", "Minimalist scratch recipes without ads", "https://download.kiwix.org/zim/zimit/based.cooking_en_all_2026-08.zim", 16),
					},
				},
				{
					Slug:         "agriculture-standard",
					Name:         "Standard",
					Description:  "Gardening knowledge and cooking expertise from community Q&A. Includes Essential.",
					SizeLabel:    "1.3 GB",
					IncludesTier: "agriculture-essential",
					Resources: []ContentResource{
						zimRes("gardening.stackexchange.com_en_all", "Gardening Q&A", "Soil, crop rotation, seed saving, and pest control Q&A", "https://download.kiwix.org/zim/stack_exchange/gardening.stackexchange.com_en_all_2026-02.zim", 923),
						zimRes("cooking.stackexchange.com_en_all", "Cooking Q&A", "Culinary techniques and food safety Q&A", "https://download.kiwix.org/zim/stack_exchange/cooking.stackexchange.com_en_all_2026-02.zim", 236),
						zimRes("zimgit-food-preparation_en", "Food for Preppers", "Canning, drying, and staple preservation manuals", "https://download.kiwix.org/zim/other/zimgit-food-preparation_en_2025-04.zim", 98),
					},
				},
				{
					Slug:         "agriculture-comprehensive",
					Name:         "Comprehensive",
					Description:  "Complete self-sufficiency with homesteading videos, classic agricultural texts, and advanced techniques.",
					SizeLabel:    "9.2 GB",
					IncludesTier: "agriculture-standard",
					Resources: []ContentResource{
						zimRes("lrnselfreliance_en_all", "Learning Self-Reliance: Homesteading", "Off-grid homesteading and small-farm video guides", "https://download.kiwix.org/zim/videos/lrnselfreliance_en_all_2026-06.zim", 3790),
						zimRes("gutenberg_en_lcc-s", "Project Gutenberg: Agriculture", "Classic agronomy, animal husbandry, and horticulture texts", "https://download.kiwix.org/zim/gutenberg/gutenberg_en_lcc-s_2026-03.zim", 4300),
					},
				},
			},
		},
		{
			Slug:        "computing",
			Name:        "Computing & Technology",
			Description: "Programming tutorials, electronics projects, and technical documentation for makers and developers.",
			SizeRange:   "22 MB - 4.5 GB",
			Tiers: []ArkTier{
				{
					Slug:        "computing-essential",
					Name:        "Essential",
					Description: "Learn to code with interactive tutorials and core programming documentation.",
					SizeLabel:   "22 MB",
					Recommended: true,
					Resources: []ContentResource{
						zimRes("freecodecamp_en_all", "freeCodeCamp", "Offline software development curriculum", "https://download.kiwix.org/zim/freecodecamp/freecodecamp_en_all_2026-08.zim", 8),
						zimRes("devdocs_en_python", "Python Documentation", "Complete Python offline reference", "https://download.kiwix.org/zim/devdocs/devdocs_en_python_2026-08.zim", 4),
						zimRes("devdocs_en_javascript", "JavaScript Documentation", "MDN/DevDocs JavaScript reference", "https://download.kiwix.org/zim/devdocs/devdocs_en_javascript_2026-07.zim", 3),
						zimRes("devdocs_en_html", "HTML Documentation", "HTML element and API documentation", "https://download.kiwix.org/zim/devdocs/devdocs_en_html_2026-07.zim", 2),
						zimRes("devdocs_en_css", "CSS Documentation", "CSS layout and styling reference", "https://download.kiwix.org/zim/devdocs/devdocs_en_css_2026-07.zim", 5),
					},
				},
				{
					Slug:         "computing-standard",
					Name:         "Standard",
					Description:  "Maker projects with Arduino and Raspberry Pi, plus more programming docs. Includes Essential.",
					SizeLabel:    "560 MB",
					IncludesTier: "computing-essential",
					Resources: []ContentResource{
						zimRes("arduino.stackexchange.com_en_all", "Arduino Q&A", "Microcontroller wiring, sensors, and firmware Q&A", "https://download.kiwix.org/zim/stack_exchange/arduino.stackexchange.com_en_all_2026-02.zim", 247),
						zimRes("raspberrypi.stackexchange.com_en_all", "Raspberry Pi Q&A", "SBC hardware, GPIO, and Linux setup Q&A", "https://download.kiwix.org/zim/stack_exchange/raspberrypi.stackexchange.com_en_all_2026-02.zim", 285),
						zimRes("devdocs_en_node", "Node.js Documentation", "Node.js runtime reference", "https://download.kiwix.org/zim/devdocs/devdocs_en_node_2026-08.zim", 1),
						zimRes("devdocs_en_react", "React Documentation", "React components and hooks reference", "https://download.kiwix.org/zim/devdocs/devdocs_en_react_2026-02.zim", 3),
						zimRes("devdocs_en_git", "Git Documentation", "Git CLI and workflow manual", "https://download.kiwix.org/zim/devdocs/devdocs_en_git_2026-07.zim", 2),
					},
				},
				{
					Slug:         "computing-comprehensive",
					Name:         "Comprehensive",
					Description:  "Deep electronics knowledge and extensive Q&A for hardware projects. Includes Standard.",
					SizeLabel:    "4.5 GB",
					IncludesTier: "computing-standard",
					Resources: []ContentResource{
						zimRes("electronics.stackexchange.com_en_all", "Electronics Q&A", "Circuit design, schematics, power, and RF engineering Q&A", "https://download.kiwix.org/zim/stack_exchange/electronics.stackexchange.com_en_all_2026-02.zim", 3800),
						zimRes("robotics.stackexchange.com_en_all", "Robotics Q&A", "Actuators, control loops, and autonomous robotics Q&A", "https://download.kiwix.org/zim/stack_exchange/robotics.stackexchange.com_en_all_2026-02.zim", 233),
						zimRes("devdocs_en_docker", "Docker Documentation", "Container and Compose reference", "https://download.kiwix.org/zim/devdocs/devdocs_en_docker_2026-07.zim", 2),
						zimRes("devdocs_en_bash", "Linux Documentation", "Bash shell and GNU utilities reference", "https://download.kiwix.org/zim/devdocs/devdocs_en_bash_2026-01.zim", 1),
					},
				},
			},
		},
	}
}

// GetArkMapRegions returns all 9 US Census map regions for Ark.
func GetArkMapRegions() []MapRegion {
	return []MapRegion{
		{
			Slug:        "pacific",
			Name:        "Pacific Region",
			Description: "Map assets for the Pacific region, including Alaska, California, Hawaii, Oregon, and Washington.",
			ItemCount:   5,
			SizeLabel:   "3 GB",
			Resources: []ContentResource{
				mapRes("alaska", "Alaska", 684),
				mapRes("california", "California", 1100),
				mapRes("hawaii", "Hawaii", 28),
				mapRes("oregon", "Oregon", 379),
				mapRes("washington", "Washington", 466),
			},
		},
		{
			Slug:        "mountain",
			Name:        "Mountain Region",
			Description: "Map assets for the Mountain region, including Arizona, Colorado, Idaho, Montana, Nevada, New Mexico, Utah, and Wyoming.",
			ItemCount:   8,
			SizeLabel:   "2 GB",
			Resources: []ContentResource{
				mapRes("arizona", "Arizona", 400),
				mapRes("colorado", "Colorado", 450),
				mapRes("idaho", "Idaho", 220),
				mapRes("montana", "Montana", 270),
				mapRes("nevada", "Nevada", 200),
				mapRes("new_mexico", "New Mexico", 230),
				mapRes("utah", "Utah", 240),
				mapRes("wyoming", "Wyoming", 210),
			},
		},
		{
			Slug:        "west-south-central",
			Name:        "West South Central",
			Description: "Map assets for the West South Central region, including Arkansas, Louisiana, Oklahoma, and Texas.",
			ItemCount:   4,
			SizeLabel:   "2 GB",
			Resources: []ContentResource{
				mapRes("arkansas", "Arkansas", 400),
				mapRes("louisiana", "Louisiana", 400),
				mapRes("oklahoma", "Oklahoma", 400),
				mapRes("texas", "Texas", 400),
			},
		},
		{
			Slug:        "east-south-central",
			Name:        "East South Central",
			Description: "Map assets for the East South Central region, including Alabama, Kentucky, Mississippi, and Tennessee.",
			ItemCount:   4,
			SizeLabel:   "2 GB",
			Resources: []ContentResource{
				mapRes("alabama", "Alabama", 400),
				mapRes("kentucky", "Kentucky", 400),
				mapRes("mississippi", "Mississippi", 400),
				mapRes("tennessee", "Tennessee", 400),
			},
		},
		{
			Slug:        "south-atlantic",
			Name:        "South Atlantic",
			Description: "Map assets for the South Atlantic region, including Delaware, Florida, Georgia, Maryland, North Carolina, South Carolina, Virginia, and West Virginia.",
			ItemCount:   8,
			SizeLabel:   "3 GB",
			Resources: []ContentResource{
				mapRes("delaware", "Delaware", 400),
				mapRes("florida", "Florida", 400),
				mapRes("georgia", "Georgia", 400),
				mapRes("maryland", "Maryland", 400),
				mapRes("north_carolina", "North Carolina", 400),
				mapRes("south_carolina", "South Carolina", 400),
				mapRes("virginia", "Virginia", 400),
				mapRes("west_virginia", "West Virginia", 400),
			},
		},
		{
			Slug:        "west-north-central",
			Name:        "West North Central",
			Description: "Map assets for the West North Central region, including Iowa, Kansas, Minnesota, Missouri, Nebraska, North Dakota, and South Dakota.",
			ItemCount:   7,
			SizeLabel:   "3 GB",
			Resources: []ContentResource{
				mapRes("iowa", "Iowa", 400),
				mapRes("kansas", "Kansas", 400),
				mapRes("minnesota", "Minnesota", 400),
				mapRes("missouri", "Missouri", 400),
				mapRes("nebraska", "Nebraska", 400),
				mapRes("north_dakota", "North Dakota", 400),
				mapRes("south_dakota", "South Dakota", 400),
			},
		},
		{
			Slug:        "east-north-central",
			Name:        "East North Central",
			Description: "Map assets for the East North Central region, including Illinois, Indiana, Michigan, Ohio, and Wisconsin.",
			ItemCount:   5,
			SizeLabel:   "2 GB",
			Resources: []ContentResource{
				mapRes("illinois", "Illinois", 400),
				mapRes("indiana", "Indiana", 400),
				mapRes("michigan", "Michigan", 400),
				mapRes("ohio", "Ohio", 400),
				mapRes("wisconsin", "Wisconsin", 400),
			},
		},
		{
			Slug:        "mid-atlantic",
			Name:        "Mid-Atlantic",
			Description: "Map assets for the Mid-Atlantic region, including New Jersey, New York, and Pennsylvania.",
			ItemCount:   3,
			SizeLabel:   "1 GB",
			Resources: []ContentResource{
				mapRes("new_jersey", "New Jersey", 400),
				mapRes("new_york", "New York", 400),
				mapRes("pennsylvania", "Pennsylvania", 400),
			},
		},
		{
			Slug:        "new-england",
			Name:        "New England",
			Description: "Map assets for the New England region, including Connecticut, Maine, Massachusetts, New Hampshire, Rhode Island, and Vermont.",
			ItemCount:   6,
			SizeLabel:   "2 GB",
			Resources: []ContentResource{
				mapRes("connecticut", "Connecticut", 400),
				mapRes("maine", "Maine", 400),
				mapRes("massachusetts", "Massachusetts", 400),
				mapRes("new_hampshire", "New Hampshire", 400),
				mapRes("rhode_island", "Rhode Island", 400),
				mapRes("vermont", "Vermont", 400),
			},
		},
	}
}

// ResolvePresetResources expands any preset/Ark target slug (Wikipedia option,
// collection tier with inherited lower tiers, Starter Pack, legacy preset ID, or
// US regional map pack) into a deduplicated ordered slice of ContentResource items.
func ResolvePresetResources(target string) []ContentResource {
	norm := strings.TrimSpace(strings.ToLower(target))
	if norm == "" {
		return nil
	}

	// 1. Starter pack bundle
	if norm == "starter_pack" || norm == "prepper-starter-pack" {
		sp := GetStarterPack()
		var out []ContentResource
		for _, item := range sp.Items {
			out = append(out, ContentResource{
				ID:          item.ID,
				Title:       item.Name,
				Description: item.Description,
				KiwixQuery:  item.KiwixQuery,
				SizeMB:      int(item.EstimatedSize / (1024 * 1024)),
				Kind:        "zim",
			})
		}
		return out
	}

	// 2. Wikipedia options & legacy Wikipedia preset aliases
	wikiAliases := map[string]string{
		"wikipedia_en_top100": "top-mini",
		"wikipedia-en-top100": "top-mini",
		"wikipedia-top-mini":  "top-mini",
		"wikipedia-top-nopic": "top-nopic",
		"wikipedia_en_mini":   "all-mini",
		"wikipedia-en-mini":   "all-mini",
		"wikipedia_en_nopic":  "all-nopic",
		"wikipedia-en-nopic":  "all-nopic",
		"wikipedia_en_full":   "all-maxi",
		"wikipedia-en-full":   "all-maxi",
	}
	if alias, ok := wikiAliases[norm]; ok {
		norm = alias
	}
	for _, opt := range GetWikipediaOptions() {
		if norm == opt.ID || norm == opt.PresetTarget {
			return []ContentResource{{
				ID:          opt.ID,
				Title:       opt.Name,
				Description: opt.Description,
				KiwixQuery:  opt.KiwixQuery,
				FallbackURL: opt.FallbackURL,
				Kind:        "zim",
			}}
		}
	}

	// 3. Ark collections & inherited tiers
	tierIndex := make(map[string]ArkTier)
	for _, col := range GetArkCollections() {
		for _, t := range col.Tiers {
			tierIndex[t.Slug] = t
		}
	}
	if _, ok := tierIndex[norm]; ok {
		var collect func(slug string, seen map[string]bool) []ContentResource
		collect = func(slug string, seen map[string]bool) []ContentResource {
			t, ok := tierIndex[slug]
			if !ok {
				return nil
			}
			var res []ContentResource
			if t.IncludesTier != "" {
				res = append(res, collect(t.IncludesTier, seen)...)
			}
			for _, r := range t.Resources {
				if !seen[r.ID] {
					seen[r.ID] = true
					res = append(res, r)
				}
			}
			return res
		}
		return collect(norm, make(map[string]bool))
	}

	// 4. Ark US Regional Map bundles (map-<slug> or maps-<slug> or <slug>)
	mapSlug := strings.TrimPrefix(strings.TrimPrefix(norm, "maps-"), "map-")
	for _, reg := range GetArkMapRegions() {
		if mapSlug == reg.Slug {
			return reg.Resources
		}
	}

	// 5. Open Video & Creator ZIM Packs
	for _, vp := range GetVideoCreatorPacks() {
		if norm == vp.ID || norm == "creator-"+vp.ID {
			return []ContentResource{{
				ID:          vp.ID,
				Title:       vp.Name,
				Description: vp.Description,
				KiwixQuery:  vp.KiwixQuery,
				FallbackURL: vp.FallbackURL,
				SizeMB:      vp.SizeMB,
				Kind:        "zim",
			}}
		}
	}

	// 6. Legacy preset IDs (wikimed, survival_repair, gutenberg, etc.)
	legacyAliases := map[string]string{
		"wikimed":         "wikimed-en",
		"survival_repair": "ifixit-en",
		"gutenberg":       "gutenberg-en",
	}
	if alias, ok := legacyAliases[norm]; ok {
		norm = alias
	}
	if p := FindPresetByID(norm); p != nil {
		return []ContentResource{{
			ID:          p.ID,
			Title:       p.Name,
			Description: p.Description,
			KiwixQuery:  p.KiwixQuery,
			SizeMB:      int(p.EstimatedSize / (1024 * 1024)),
			Kind:        "zim",
		}}
	}

	return nil
}

// VideoCreatorPack represents an open, un-gated offline video/creator ZIM pack.
type VideoCreatorPack struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Creator     string `json:"creator"`
	Description string `json:"description"`
	KiwixQuery  string `json:"kiwix_query"`
	FallbackURL string `json:"fallback_url"`
	SizeLabel   string `json:"size_label"`
	SizeMB      int    `json:"size_mb"`
}

// GetVideoCreatorPacks returns open-access video and homesteading creator ZIM packs hosted on Kiwix.
func GetVideoCreatorPacks() []VideoCreatorPack {
	return []VideoCreatorPack{
		{
			ID:          "urban-prepper",
			Name:        "Urban Prepper",
			Creator:     "The Urban Prepper",
			Description: "Urban disaster readiness, EDC loadouts, power backup, and emergency kits.",
			KiwixQuery:  "urban-prepper_en_all",
			FallbackURL: "https://download.kiwix.org/zim/videos/urban-prepper_en_all_2026-08.zim",
			SizeLabel:   "2.2 GB",
			SizeMB:      2245,
		},
		{
			ID:          "canadian-prepper-winter",
			Name:        "Canadian Prepper: Winter & Bug-Out",
			Creator:     "Canadian Prepper",
			Description: "Cold-weather survival, shelter heating, and bug-out planning video guides.",
			KiwixQuery:  "canadian-prepper_en_winterprepping",
			FallbackURL: "https://download.kiwix.org/zim/videos/canadian-prepper_en_winterprepping_2026-08.zim",
			SizeLabel:   "1.3 GB",
			SizeMB:      1342,
		},
		{
			ID:          "learning-self-reliance",
			Name:        "Learning Self-Reliance",
			Creator:     "Learning Self-Reliance",
			Description: "Homesteading, off-grid water, gardening, and small-farm skills videos.",
			KiwixQuery:  "lrnselfreliance_en_all",
			FallbackURL: "https://download.kiwix.org/zim/videos/lrnselfreliance_en_all_2026-06.zim",
			SizeLabel:   "3.8 GB",
			SizeMB:      3790,
		},
		{
			ID:          "ted-ed",
			Name:        "TED-Ed Educational Animations",
			Creator:     "TED-Ed",
			Description: "Illustrated lessons covering science, medicine, engineering, and history.",
			KiwixQuery:  "ted_mul_ted-ed",
			FallbackURL: "https://download.kiwix.org/zim/ted/ted_mul_ted-ed_2026-09.zim",
			SizeLabel:   "6.4 GB",
			SizeMB:      6381,
		},
	}
}

// CountryMapOption represents an extractable global or country PMTiles bounding box via Protomaps.
type CountryMapOption struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Region      string `json:"region"`
	BBox        string `json:"bbox"` // minLon,minLat,maxLon,maxLat (empty = full world)
	MaxZoom     int    `json:"max_zoom"`
	SizeEstimate string `json:"size_estimate"`
}

// GetCountryMapOptions returns the World Basemap plus popular country extracts compatible with go-pmtiles.
func GetCountryMapOptions() []CountryMapOption {
	return []CountryMapOption{
		{Slug: "world-basemap", Name: "World Overview Basemap (z0-5)", Region: "Global", BBox: "-180,-85,180,85", MaxZoom: 5, SizeEstimate: "~65 MB"},
		{Slug: "united-states", Name: "United States (Full Country, z0-10)", Region: "North America", BBox: "-179.15,18.91,-66.95,71.39", MaxZoom: 10, SizeEstimate: "~3.5 GB"},
		{Slug: "canada", Name: "Canada", Region: "North America", BBox: "-141.0,41.67,-52.6,83.11", MaxZoom: 10, SizeEstimate: "~1.8 GB"},
		{Slug: "mexico", Name: "Mexico", Region: "North America", BBox: "-118.4,14.5,-86.7,32.7", MaxZoom: 11, SizeEstimate: "~900 MB"},
		{Slug: "united-kingdom", Name: "United Kingdom", Region: "Europe", BBox: "-8.65,49.86,1.77,60.86", MaxZoom: 12, SizeEstimate: "~1.1 GB"},
		{Slug: "ireland", Name: "Ireland", Region: "Europe", BBox: "-10.67,51.42,-5.99,55.38", MaxZoom: 12, SizeEstimate: "~280 MB"},
		{Slug: "france", Name: "France", Region: "Europe", BBox: "-5.14,41.33,9.56,51.09", MaxZoom: 11, SizeEstimate: "~1.9 GB"},
		{Slug: "germany", Name: "Germany", Region: "Europe", BBox: "5.87,47.27,15.04,55.06", MaxZoom: 11, SizeEstimate: "~2.1 GB"},
		{Slug: "spain", Name: "Spain", Region: "Europe", BBox: "-9.30,36.00,4.33,43.79", MaxZoom: 11, SizeEstimate: "~1.1 GB"},
		{Slug: "italy", Name: "Italy", Region: "Europe", BBox: "6.63,35.49,18.52,47.09", MaxZoom: 11, SizeEstimate: "~1.1 GB"},
		{Slug: "switzerland", Name: "Switzerland", Region: "Europe", BBox: "5.96,45.82,10.49,47.81", MaxZoom: 13, SizeEstimate: "~450 MB"},
		{Slug: "australia", Name: "Australia", Region: "Oceania", BBox: "112.9,-43.6,153.6,-10.7", MaxZoom: 10, SizeEstimate: "~850 MB"},
		{Slug: "new-zealand", Name: "New Zealand", Region: "Oceania", BBox: "166.4,-47.3,178.6,-34.4", MaxZoom: 12, SizeEstimate: "~390 MB"},
		{Slug: "japan", Name: "Japan", Region: "Asia", BBox: "122.9,24.0,153.9,45.5", MaxZoom: 11, SizeEstimate: "~1.6 GB"},
		{Slug: "south-korea", Name: "South Korea", Region: "Asia", BBox: "124.6,33.1,131.9,38.6", MaxZoom: 12, SizeEstimate: "~620 MB"},
		{Slug: "brazil", Name: "Brazil", Region: "South America", BBox: "-73.99,-33.75,-34.79,5.27", MaxZoom: 10, SizeEstimate: "~1.7 GB"},
		{Slug: "argentina", Name: "Argentina", Region: "South America", BBox: "-73.56,-55.06,-53.64,-21.78", MaxZoom: 10, SizeEstimate: "~700 MB"},
		{Slug: "south-africa", Name: "South Africa", Region: "Africa", BBox: "16.45,-34.83,32.89,-22.13", MaxZoom: 11, SizeEstimate: "~520 MB"},
	}
}

// FindCountryMapBySlug finds a CountryMapOption by slug.
func FindCountryMapBySlug(slug string) *CountryMapOption {
	norm := strings.TrimSpace(strings.ToLower(slug))
	for _, opt := range GetCountryMapOptions() {
		if opt.Slug == norm {
			c := opt
			return &c
		}
	}
	return nil
}

var shortResourceLabels = map[string]string{
	"zimgit-medicine_en":                  "Medical Library",
	"nhs.uk_en_medicines":                 "NHS Medicines",
	"irp.fas.org_en_military-medicine":    "Military Med",
	"wwwnc.cdc.gov_en_all":                "CDC Health",
	"medlineplus.gov_en_all":              "MedlinePlus",
	"wikipedia_en_medicine_maxi":          "WikiMed",
	"libretexts.org_en_med":               "LibreTexts Med",
	"librepathology_en_all_maxi":          "LibrePathology",
	"zimgit-water_en":                     "Water Treatment",
	"zimgit-food-preparation_en":          "Prepper Food",
	"zimgit-knots_en":                     "Knots Guide",
	"canadian-prepper_en_winterprepping":  "Winter Prep",
	"canadian-prepper_en_bugoutroll":      "Bug-Out Roll",
	"www.ready.gov_en":                    "Ready.gov",
	"outdoors.stackexchange.com_en_all":   "Outdoors Q&A",
	"ham.stackexchange.com_en_all":        "Ham Radio",
	"canadian-prepper_en_bugoutconcepts":  "Bug-Out Video",
	"urban-prepper_en_all":                "Urban Prepper",
	"canadian-prepper_en_preppingfood":    "Food Storage",
	"zimgit-post-disaster_en":             "Post Disaster",
	"100r.co_en_all":                      "100 Rabbits",
	"gutenberg_en_lcc-u":                  "Military Texts",
	"cd3wdproject.org_en_all":             "CD3WD Tech",
	"wikibooks_en_all_nopic":              "Wikibooks",
	"ted_mul_ted-ed":                      "TED-Ed",
	"wikiversity_en_all_maxi":             "Wikiversity",
	"libretexts.org_en_math":              "Math Texts",
	"libretexts.org_en_phys":              "Physics Texts",
	"libretexts.org_en_chem":              "Chemistry Texts",
	"libretexts.org_en_bio":               "Biology Texts",
	"wikibooks_en_all_maxi":               "Wikibooks Media",
	"ted_mul_ted-conference":              "TED Talks",
	"libretexts.org_en_human":             "Humanities",
	"libretexts.org_en_geo":               "Geosciences",
	"libretexts.org_en_eng":               "Engineering",
	"libretexts.org_en_biz":               "Business",
	"woodworking.stackexchange.com_en_all":"Woodworking",
	"mechanics.stackexchange.com_en_all":  "Auto Repair",
	"diy.stackexchange.com_en_all":        "Home DIY",
	"ifixit_en_all":                       "iFixit",
	"foss.cooking_en_all":                 "FOSS Cooking",
	"based.cooking_en_all":                "Based Cooking",
	"gardening.stackexchange.com_en_all":  "Gardening",
	"cooking.stackexchange.com_en_all":    "Cooking Q&A",
	"lrnselfreliance_en_all":              "Homesteading",
	"gutenberg_en_lcc-s":                  "Agriculture",
	"freecodecamp_en_all":                 "freeCodeCamp",
	"devdocs_en_python":                   "Python Docs",
	"devdocs_en_javascript":               "JS Docs",
	"devdocs_en_html":                     "HTML Docs",
	"devdocs_en_css":                      "CSS Docs",
	"arduino.stackexchange.com_en_all":    "Arduino Q&A",
	"raspberrypi.stackexchange.com_en_all":"Raspberry Pi",
	"devdocs_en_node":                     "Node Docs",
	"devdocs_en_react":                    "React Docs",
	"devdocs_en_git":                      "Git Docs",
	"electronics.stackexchange.com_en_all":"Electronics",
	"robotics.stackexchange.com_en_all":   "Robotics",
	"devdocs_en_docker":                   "Docker Docs",
	"devdocs_en_bash":                     "Linux Docs",
}

// CumulativeTierLabels returns concise 1-2 word labels for every bundle included in the given tier (including inherited lower tiers).
func CumulativeTierLabels(tierSlug string) []string {
	resources := ResolvePresetResources(tierSlug)
	out := make([]string, 0, len(resources))
	for _, r := range resources {
		if lbl, ok := shortResourceLabels[r.ID]; ok {
			out = append(out, lbl)
		} else {
			out = append(out, r.Title)
		}
	}
	return out
}
