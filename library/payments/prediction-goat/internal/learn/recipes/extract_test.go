// Copyright 2026 mvanhorn. Licensed under Apache-2.0. See LICENSE.

package recipes_test

import (
	"testing"

	"github.com/mvanhorn/printing-press-library/library/payments/prediction-goat/internal/learn"
	"github.com/mvanhorn/printing-press-library/library/payments/prediction-goat/internal/learn/recipes"
	"github.com/mvanhorn/printing-press-library/library/payments/prediction-goat/internal/store"
)

// teachOne writes one search_learnings row at source="taught" with
// the QueryEntities pre-populated to match what the live teach path
// would do (run normalize.Normalize over the query and pass through
// the entities). This is the same shape the U10 Extract walks.
func teachOne(t *testing.T, s *store.Store, query, resourceType, resourceID string) {
	t.Helper()
	normalized := learn.Normalize(query, learn.DefaultPredictionGoatConfig())
	if _, _, err := s.UpsertLearning(store.UpsertLearningInput{
		Query:         query,
		QueryEntities: normalized.Entities,
		ResourceID:    resourceID,
		ResourceType:  resourceType,
		Source:        store.LearningSourceTaught,
	}); err != nil {
		t.Fatalf("teach %q -> %s: %v", query, resourceID, err)
	}
}

// TestExtract_KalshiCountryTicker_ExactSubstitute is the flagship
// "Portugal + USA generalize to England" story. Two teaches with the
// same query shape and a country-iso2 swap in the ticker should
// produce a substitute-strategy recipe bound to country_iso2.
func TestExtract_KalshiCountryTicker_ExactSubstitute(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	teachOne(t, s, "odds Portugal wins world cup", "kalshi_markets", "KXMENWORLDCUP-26-PT")
	teachOne(t, s, "odds USA wins world cup", "kalshi_markets", "KXMENWORLDCUP-26-US")

	created, err := recipes.Extract(s.DB())
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if created < 1 {
		t.Errorf("Extract created %d recipes, want >= 1", created)
	}

	rows, err := recipes.List(s.DB(), recipes.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) < 1 {
		t.Fatalf("want at least 1 recipe row, got %d", len(rows))
	}

	// Find the country_iso2 substitute recipe.
	var got *recipes.Recipe
	for i := range rows {
		if rows[i].EntityKind == "country_iso2" && rows[i].Strategy == recipes.StrategySubstitute {
			got = &rows[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no country_iso2/substitute recipe found in %+v", rows)
	}
	if got.ResourceTemplate != "KXMENWORLDCUP-26-{entity:country_iso2}" {
		t.Errorf("resource_template = %q, want KXMENWORLDCUP-26-{entity:country_iso2}", got.ResourceTemplate)
	}
	if got.ResourceType != "kalshi_markets" {
		t.Errorf("resource_type = %q, want kalshi_markets", got.ResourceType)
	}
	if got.Source != recipes.SourceInferred {
		t.Errorf("source = %q, want inferred", got.Source)
	}
}

// TestExtract_PolymarketSlug_PrefixSearch covers the second strategy:
// two Polymarket slugs that share a literal core but carry an
// unpredictable trailing numeric segment. Extract should bind the
// lowercase kind and emit substitute-then-search-prefix.
func TestExtract_PolymarketSlug_PrefixSearch(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	teachOne(t, s, "odds Portugal wins world cup", "markets", "will-portugal-win-the-2026-fifa-world-cup-912")
	teachOne(t, s, "odds USA wins world cup", "markets", "will-usa-win-the-2026-fifa-world-cup-467")

	if _, err := recipes.Extract(s.DB()); err != nil {
		t.Fatalf("extract: %v", err)
	}

	rows, err := recipes.List(s.DB(), recipes.ListFilter{Strategy: recipes.StrategySubstituteThenSearchPrefix})
	if err != nil {
		t.Fatalf("list prefix: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 prefix recipe, got %d (rows=%+v)", len(rows), rows)
	}
	got := rows[0]
	// Either the table-backed country_lowercase kind or the
	// computed lowercase kind produces the right substitution
	// value for these countries; the inference engine prefers
	// table-backed when both work because it carries stronger
	// "user meant this specific alias" semantics.
	if got.EntityKind != "lowercase" && got.EntityKind != "country_lowercase" {
		t.Errorf("entity_kind = %q, want lowercase or country_lowercase", got.EntityKind)
	}
	if got.ResourceTemplate == "" || got.ResourceTemplate[len(got.ResourceTemplate)-1] != '*' {
		t.Errorf("resource_template should end with '*'; got %q", got.ResourceTemplate)
	}
}

// TestExtract_NoPattern_NoRecipe asserts that two unrelated teaches
// don't get fused into a bogus recipe. Different query patterns
// AND different resource shapes = no group.
func TestExtract_NoPattern_NoRecipe(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	teachOne(t, s, "odds Portugal wins world cup", "kalshi_markets", "KXMENWORLDCUP-26-PT")
	teachOne(t, s, "trending crypto markets today", "markets", "trending-crypto-2026")

	if _, err := recipes.Extract(s.DB()); err != nil {
		t.Fatalf("extract: %v", err)
	}
	rows, err := recipes.List(s.DB(), recipes.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("unrelated teaches should not yield a recipe; got %+v", rows)
	}
}

// TestExtract_SingleTeach_NoRecipe asserts a lone teach doesn't
// spawn a one-sample recipe.
func TestExtract_SingleTeach_NoRecipe(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	teachOne(t, s, "odds Portugal wins world cup", "kalshi_markets", "KXMENWORLDCUP-26-PT")

	if _, err := recipes.Extract(s.DB()); err != nil {
		t.Fatalf("extract: %v", err)
	}
	rows, err := recipes.List(s.DB(), recipes.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("single teach should not yield a recipe; got %+v", rows)
	}
}

// TestExtract_Idempotent asserts that running Extract twice over the
// same data does not create duplicate rows. The second pass should
// bump confidence on the existing recipe and leave the row count
// unchanged.
func TestExtract_Idempotent(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	teachOne(t, s, "odds Portugal wins world cup", "kalshi_markets", "KXMENWORLDCUP-26-PT")
	teachOne(t, s, "odds USA wins world cup", "kalshi_markets", "KXMENWORLDCUP-26-US")

	if _, err := recipes.Extract(s.DB()); err != nil {
		t.Fatalf("first extract: %v", err)
	}
	firstRows, err := recipes.List(s.DB(), recipes.ListFilter{})
	if err != nil {
		t.Fatalf("list after first extract: %v", err)
	}
	firstCount := len(firstRows)
	if firstCount == 0 {
		t.Fatalf("first extract produced 0 rows; subsequent assertions meaningless")
	}

	if _, err := recipes.Extract(s.DB()); err != nil {
		t.Fatalf("second extract: %v", err)
	}
	secondRows, err := recipes.List(s.DB(), recipes.ListFilter{})
	if err != nil {
		t.Fatalf("list after second extract: %v", err)
	}
	if len(secondRows) != firstCount {
		t.Errorf("recipe count drifted across Extract calls: first=%d second=%d", firstCount, len(secondRows))
	}
	// Confidence should bump on the matching row.
	for _, r := range secondRows {
		if r.Confidence < firstRows[0].Confidence {
			t.Errorf("expected confidence to increase or stay, got %d (first=%d)", r.Confidence, firstRows[0].Confidence)
		}
	}
}
