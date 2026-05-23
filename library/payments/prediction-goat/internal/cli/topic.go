// Copyright 2026 mvanhorn. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/mvanhorn/printing-press-library/library/payments/prediction-goat/internal/store"
)

type topicHit struct {
	Source         string  `json:"source"`
	Kind           string  `json:"kind"`
	ID             string  `json:"id"`
	Title          string  `json:"title"`
	Status         string  `json:"status,omitempty"`
	YesProbability float64 `json:"yesProbability,omitempty"`
	Volume24h      float64 `json:"volume24h,omitempty"`
	EndDate        string  `json:"endDate,omitempty"`
	URL            string  `json:"url,omitempty"`
	// PriceSource is set when the row carries a price/probability that
	// went through the live refresh pipeline ("live" or "stale"). Rows
	// with no price-bearing fields (tags, series) leave this empty so
	// agents can tell a tag-row apart from a price-row whose refresh
	// genuinely failed.
	PriceSource string `json:"price_source,omitempty"`
}

type topicResult struct {
	Topic string         `json:"topic"`
	Count int            `json:"count"`
	Hits  []topicHit     `json:"hits"`
	Meta  *freshnessMeta `json:"meta,omitempty"`
}

func newTopicCmd(flags *rootFlags) *cobra.Command {
	var limit int
	var dbPath string
	var vf venueFlags
	cmd := &cobra.Command{
		Use:   "topic <name>",
		Short: "Cross-venue topic bundle (slim ranked markets/events/tags from Polymarket and Kalshi)",
		Example: `  prediction-goat-pp-cli topic kanye-west --json
  prediction-goat-pp-cli topic 'arizona basketball' --limit 20
  prediction-goat-pp-cli topic 'world cup' --kalshi   # skip Polymarket
  prediction-goat-pp-cli topic 'world cup' --polymarket --agent`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if dryRunOK(flags) {
				return nil
			}
			venue, err := resolveVenue(vf)
			if err != nil {
				return err
			}
			if dbPath == "" {
				dbPath = defaultDBPath("prediction-goat-pp-cli")
			}
			db, err := store.OpenWithContext(cmd.Context(), dbPath)
			if err != nil {
				return fmt.Errorf("topic open database: %w", err)
			}
			defer db.Close()
			topic := strings.Join(args, " ")
			// Run two independent FTS searches per venue so a heavier-corpus
			// venue (Kalshi has events+series+markets, Polymarket has
			// markets+events+tags) cannot crowd the other out via raw rank.
			// Each side gets up to `limit` rows; they are then interleaved
			// round-robin and trimmed to the final `limit`. When the user
			// scopes to a single venue, only that side runs.
			polyTypes := []string{"markets", "events", "tags"}
			kalshiTypes := []string{"kalshi_markets", "kalshi_events", "kalshi_series"}
			var polyHits, kalshiHits []topicHit
			if venue == "all" || venue == "polymarket" {
				polyHits, err = topicSearchByTypes(cmd.Context(), db.DB(), topicFTSQuery(topic), polyTypes, limit)
				if err != nil {
					return fmt.Errorf("topic search polymarket: %w", err)
				}
			}
			if venue == "all" || venue == "kalshi" {
				kalshiHits, err = topicSearchByTypes(cmd.Context(), db.DB(), topicFTSQuery(topic), kalshiTypes, limit)
				if err != nil {
					return fmt.Errorf("topic search kalshi: %w", err)
				}
			}
			results := interleaveTopicHits(polyHits, kalshiHits, limit)
			// Live-on-read freshness: refresh price-bearing fields from
			// the upstream APIs so the cached discovery index never serves
			// stale prices. See freshness.go for the design.
			outcome := refreshTopicHits(cmd.Context(), nil, results)
			meta := buildFreshnessMeta(outcome, indexSyncedAt(db))
			result := topicResult{Topic: topic, Count: len(results), Hits: results, Meta: meta}
			if flags.asJSON || !isTerminal(cmd.OutOrStdout()) {
				if err := printJSONFiltered(cmd.OutOrStdout(), result, flags); err != nil {
					return err
				}
			} else {
				if err := printSimpleTable(cmd.OutOrStdout(), []string{"Source", "Kind", "Title", "%Yes", "Volume24h", "EndDate"}, topicRows(results)); err != nil {
					return err
				}
				if footer := freshnessFooterLine(meta); footer != "" {
					fmt.Fprintln(cmd.OutOrStdout(), footer)
				}
			}
			if len(results) == 0 {
				return notFoundErr(fmt.Errorf("no markets, events, or tags matched topic %q (try a broader query, or run `prediction-goat-pp-cli sync` and `kalshi sync` first)", topic))
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Max results")
	cmd.Flags().StringVar(&dbPath, "db", "", "Database path (default: standard cache location)")
	addVenueFlags(cmd, &vf)
	return cmd
}

// topicSearchByTypes runs an FTS5 search restricted to a fixed set of
// resource types and returns up to `limit` decoded topicHit rows. It is
// the per-venue half of the cross-venue interleave the topic command does.
func topicSearchByTypes(ctx context.Context, db *sql.DB, ftsQuery string, types []string, limit int) ([]topicHit, error) {
	if len(types) == 0 || limit <= 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(types)), ",")
	q := `SELECT r.resource_type, r.id, r.data FROM resources r
JOIN resources_fts f ON r.id = f.id AND r.resource_type = f.resource_type
WHERE resources_fts MATCH ?
AND r.resource_type IN (` + placeholders + `)
ORDER BY rank LIMIT ?`
	args := make([]any, 0, len(types)+2)
	args = append(args, ftsQuery)
	for _, t := range types {
		args = append(args, t)
	}
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hits := make([]topicHit, 0)
	for rows.Next() {
		var typ, id, data sql.NullString
		if err := rows.Scan(&typ, &id, &data); err != nil {
			return nil, err
		}
		if !typ.Valid || !data.Valid {
			continue
		}
		hit, ok := topicHitFromJSON(typ.String, id.String, data.String)
		if ok {
			hits = append(hits, hit)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}

// interleaveTopicHits round-robins two ranked venue slices into one bundle
// of at most `limit` rows, deduplicating by (source,id) and by case-folded
// title so v1/v2 Kalshi series with identical titles fold into one row.
func interleaveTopicHits(a, b []topicHit, limit int) []topicHit {
	if limit <= 0 {
		return nil
	}
	out := make([]topicHit, 0, limit)
	seenKey := make(map[string]struct{}, limit)
	seenTitle := make(map[string]struct{}, limit)
	add := func(h topicHit) bool {
		k := h.Source + "|" + h.ID
		if _, dup := seenKey[k]; dup {
			return false
		}
		tk := strings.ToLower(strings.TrimSpace(h.Title))
		if tk != "" {
			if _, dup := seenTitle[tk]; dup {
				return false
			}
			seenTitle[tk] = struct{}{}
		}
		seenKey[k] = struct{}{}
		out = append(out, h)
		return true
	}
	ai, bi := 0, 0
	for len(out) < limit && (ai < len(a) || bi < len(b)) {
		if ai < len(a) {
			add(a[ai])
			ai++
			if len(out) >= limit {
				break
			}
		}
		if bi < len(b) {
			add(b[bi])
			bi++
		}
	}
	return out
}

func topicHitFromJSON(resourceType, fallbackID, raw string) (topicHit, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return topicHit{}, false
	}
	id := fallbackID
	h := topicHit{ID: id}
	switch resourceType {
	case "markets":
		id = firstNonEmpty(jsonString(obj, "slug"), id)
		h = topicHit{Source: "polymarket", Kind: "market", ID: id, Title: firstNonEmpty(jsonString(obj, "question"), jsonString(obj, "title")), Status: pmStatus(obj), YesProbability: jsonFloat(obj, "lastTradePrice"), Volume24h: firstFloat(obj, "volume24hr", "volumeNum"), EndDate: jsonString(obj, "endDate"), URL: "https://polymarket.com/market/" + id}
	case "events":
		id = firstNonEmpty(jsonString(obj, "slug"), id)
		h = topicHit{Source: "polymarket", Kind: "event", ID: id, Title: jsonString(obj, "title"), Status: pmStatus(obj), Volume24h: jsonFloat(obj, "volume"), EndDate: jsonString(obj, "endDate"), URL: "https://polymarket.com/event/" + id}
	case "tags":
		id = firstNonEmpty(jsonString(obj, "slug"), id)
		h = topicHit{Source: "polymarket", Kind: "tag", ID: id, Title: firstNonEmpty(jsonString(obj, "label"), jsonString(obj, "title")), URL: "https://polymarket.com/tag/" + id}
	case "kalshi_markets":
		id = firstNonEmpty(jsonString(obj, "ticker"), id)
		eventTicker := jsonString(obj, "event_ticker")
		h = topicHit{Source: "kalshi", Kind: "market", ID: id, Title: jsonString(obj, "title"), Status: jsonString(obj, "status"), YesProbability: jsonFloat(obj, "last_price_dollars"), Volume24h: jsonFloat(obj, "volume_24h_fp"), EndDate: jsonString(obj, "expiration_time"), URL: "https://kalshi.com/markets/" + eventTicker + "/" + id}
	case "kalshi_events":
		id = firstNonEmpty(jsonString(obj, "event_ticker"), id)
		h = topicHit{Source: "kalshi", Kind: "event", ID: id, Title: jsonString(obj, "title"), EndDate: jsonString(obj, "strike_period"), URL: "https://kalshi.com/markets/" + id}
	case "kalshi_series":
		id = firstNonEmpty(jsonString(obj, "ticker"), id)
		h = topicHit{Source: "kalshi", Kind: "series", ID: id, Title: jsonString(obj, "title"), URL: "https://kalshi.com/markets?series=" + id}
	}
	return h, h.Source != "" && h.ID != ""
}

func topicRows(items []topicHit) [][]string {
	rows := make([][]string, 0, len(items))
	for _, it := range items {
		rows = append(rows, []string{it.Source, it.Kind, it.Title, formatProb(it.YesProbability), formatNumber(it.Volume24h), it.EndDate})
	}
	return rows
}

func printSimpleTable(w io.Writer, headers []string, rows [][]string) error {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(headers, "\t"))
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

func jsonString(obj map[string]any, key string) string {
	if v, ok := obj[key]; ok && v != nil {
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

func jsonFloat(obj map[string]any, key string) float64 {
	if v, ok := obj[key]; ok && v != nil {
		switch n := v.(type) {
		case float64:
			return n
		case json.Number:
			f, _ := n.Float64()
			return f
		case string:
			var f float64
			_, _ = fmt.Sscanf(n, "%f", &f)
			return f
		}
	}
	return 0
}

func firstFloat(obj map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if f := jsonFloat(obj, k); f != 0 {
			return f
		}
	}
	return 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// refreshTopicHits batches the topic command's hits by venue, fires
// one live API call per venue, and overwrites the cached
// price-bearing fields on the in-memory slice. Returns the per-venue
// outcome so the caller can populate the envelope's price_source.
//
// Hits with Kind=="market" carry prices; other kinds (event/tag/
// series) intentionally have no PriceSource set so an agent can tell
// a tag-row apart from a market-row whose refresh failed.
func refreshTopicHits(ctx context.Context, fc freshnessClient, hits []topicHit) refreshOutcome {
	polySlugs := make([]string, 0, len(hits))
	kalshiTickers := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.Kind != "market" {
			continue
		}
		switch h.Source {
		case "polymarket":
			polySlugs = append(polySlugs, h.ID)
		case "kalshi":
			kalshiTickers = append(kalshiTickers, h.ID)
		}
	}
	outcome := refreshVenues(ctx, fc, polySlugs, kalshiTickers)
	for i := range hits {
		if hits[i].Kind != "market" {
			continue
		}
		switch hits[i].Source {
		case "polymarket":
			if !outcome.PolymarketAsked {
				continue
			}
			if !outcome.PolymarketOK {
				hits[i].PriceSource = priceSourceStale
				continue
			}
			if v, ok := outcome.Polymarket[hits[i].ID]; ok {
				applyLiveValuesIfPresent(v, &hits[i].YesProbability, &hits[i].Volume24h, &hits[i].Status)
			}
			hits[i].PriceSource = priceSourceLive
		case "kalshi":
			if !outcome.KalshiAsked {
				continue
			}
			if !outcome.KalshiOK {
				hits[i].PriceSource = priceSourceStale
				continue
			}
			if v, ok := outcome.Kalshi[hits[i].ID]; ok {
				applyLiveValuesIfPresent(v, &hits[i].YesProbability, &hits[i].Volume24h, &hits[i].Status)
			}
			hits[i].PriceSource = priceSourceLive
		}
	}
	return outcome
}

func pmStatus(obj map[string]any) string {
	if jsonString(obj, "closed") == "true" {
		return "closed"
	}
	if jsonString(obj, "active") == "false" {
		return "inactive"
	}
	return "active"
}
