// Copyright 2026 mvanhorn. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/mvanhorn/printing-press-library/library/payments/prediction-goat/internal/store"
)

type moversItem struct {
	Source       string  `json:"source"`
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Delta        float64 `json:"delta"`
	CurrentPrice float64 `json:"currentPrice"`
	EndDate      string  `json:"endDate,omitempty"`
	Volume24h    float64 `json:"volume24h,omitempty"`
	// PriceSource is set after the live-on-read refresh fires: "live"
	// or "stale". See freshness.go.
	PriceSource string `json:"price_source,omitempty"`
}

type moversResult struct {
	Items []moversItem   `json:"items"`
	Meta  *freshnessMeta `json:"meta,omitempty"`
}

func newMoversCmd(flags *rootFlags) *cobra.Command {
	var window, dbPath string
	var limit int
	var vf venueFlags
	cmd := &cobra.Command{
		Use:   "movers",
		Short: "Biggest implied-probability deltas across Polymarket and Kalshi",
		Example: `  prediction-goat-pp-cli movers --window 24h --json
  prediction-goat-pp-cli movers --window 7d --kalshi --limit 10`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return nil
			}
			venue, err := resolveVenue(vf)
			if err != nil {
				return err
			}
			items, err := runMovers(cmd, dbPath, venue, window, limit)
			if err != nil {
				return err
			}
			// Live-on-read freshness: refresh CurrentPrice (and Volume24h)
			// from upstream for every row. Delta is window-derived and
			// cannot be refreshed without point-in-time history; the
			// CurrentPrice refresh is what makes "movers --window 24h"
			// no longer report yesterday's price as current.
			outcome := refreshMoversItems(cmd.Context(), nil, items)
			meta := buildFreshnessMeta(outcome, indexSyncedAtFromPath(cmd.Context(), dbPath))
			result := moversResult{Items: items, Meta: meta}
			if flags.asJSON || !isTerminal(cmd.OutOrStdout()) {
				if err := printJSONFiltered(cmd.OutOrStdout(), result, flags); err != nil {
					return err
				}
			} else {
				if err := printSimpleTable(cmd.OutOrStdout(), []string{"Source", "ID", "Title", "Delta", "%Now", "Volume24h", "EndDate"}, moverRows(items)); err != nil {
					return err
				}
				if footer := freshnessFooterLine(meta); footer != "" {
					fmt.Fprintln(cmd.OutOrStdout(), footer)
				}
			}
			if len(items) == 0 {
				if hint := emptyStoreHint(cmd, dbPath, "movers", venue); hint != nil {
					return hint
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&window, "window", "24h", "Window: 24h or 7d")
	cmd.Flags().IntVar(&limit, "limit", 20, "Max results")
	cmd.Flags().StringVar(&dbPath, "db", "", "Database path (default: standard cache location)")
	addVenueFlags(cmd, &vf)
	return cmd
}

func runMovers(cmd *cobra.Command, dbPath, venue, window string, limit int) ([]moversItem, error) {
	if dbPath == "" {
		dbPath = defaultDBPath("prediction-goat-pp-cli")
	}
	if venue != "all" && venue != "polymarket" && venue != "kalshi" {
		return nil, fmt.Errorf("invalid --venue %q: must be all, polymarket, or kalshi", venue)
	}
	if window != "24h" && window != "7d" {
		return nil, fmt.Errorf("invalid --window %q: must be 24h or 7d", window)
	}
	db, err := store.OpenWithContext(cmd.Context(), dbPath)
	if err != nil {
		return nil, fmt.Errorf("movers open database: %w", err)
	}
	defer db.Close()
	sqlText := moversSQL(venue, window)
	rows, err := db.DB().QueryContext(cmd.Context(), sqlText, limit)
	if err != nil {
		return nil, fmt.Errorf("movers query: %w", err)
	}
	defer rows.Close()
	items := make([]moversItem, 0)
	for rows.Next() {
		var source, id, title, endDate sql.NullString
		var delta, current, volume sql.NullFloat64
		if err := rows.Scan(&source, &id, &title, &delta, &current, &endDate, &volume); err != nil {
			return nil, fmt.Errorf("movers scan: %w", err)
		}
		items = append(items, moversItem{Source: source.String, ID: id.String, Title: title.String, Delta: delta.Float64, CurrentPrice: current.Float64, EndDate: endDate.String, Volume24h: volume.Float64})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if venue == "all" || venue == "kalshi" {
		// Rewrite Kalshi multi-leg outcome-CSV titles to the parent
		// event title; harmless if no items need it.
		stub := make([]marketScreenItem, len(items))
		for i, it := range items {
			stub[i] = marketScreenItem{Source: it.Source, ID: it.ID, Title: it.Title}
		}
		enrichKalshiTitles(cmd, db, stub)
		for i := range items {
			items[i].Title = stub[i].Title
		}
	}
	return items, nil
}

// refreshMoversItems batches by venue, fires one live API call per
// venue, and overwrites CurrentPrice / Volume24h on each row. Delta
// is window-derived (oneDayPriceChange / last vs previous on Kalshi)
// and cannot be live-refreshed without a point-in-time history
// service; the refresh focuses on CurrentPrice so the
// "movers --window 24h" answer reports the actual current price
// alongside its window-relative delta.
func refreshMoversItems(ctx context.Context, fc freshnessClient, items []moversItem) refreshOutcome {
	polySlugs := make([]string, 0, len(items))
	kalshiTickers := make([]string, 0, len(items))
	for _, it := range items {
		switch it.Source {
		case "polymarket":
			polySlugs = append(polySlugs, it.ID)
		case "kalshi":
			kalshiTickers = append(kalshiTickers, it.ID)
		}
	}
	outcome := refreshVenues(ctx, fc, polySlugs, kalshiTickers)
	var dummyStatus string
	for i := range items {
		switch items[i].Source {
		case "polymarket":
			if !outcome.PolymarketAsked {
				continue
			}
			if !outcome.PolymarketOK {
				items[i].PriceSource = priceSourceStale
				continue
			}
			if v, ok := outcome.Polymarket[items[i].ID]; ok {
				applyLiveValuesIfPresent(v, &items[i].CurrentPrice, &items[i].Volume24h, &dummyStatus)
			}
			items[i].PriceSource = priceSourceLive
		case "kalshi":
			if !outcome.KalshiAsked {
				continue
			}
			if !outcome.KalshiOK {
				items[i].PriceSource = priceSourceStale
				continue
			}
			if v, ok := outcome.Kalshi[items[i].ID]; ok {
				applyLiveValuesIfPresent(v, &items[i].CurrentPrice, &items[i].Volume24h, &dummyStatus)
			}
			items[i].PriceSource = priceSourceLive
		}
	}
	return outcome
}

func moversSQL(venue, window string) string {
	pmDelta := "json_extract(data,'$.oneDayPriceChange')"
	if window == "7d" {
		pmDelta = "COALESCE(json_extract(data,'$.oneWeekPriceChange'), json_extract(data,'$.oneMonthPriceChange'), json_extract(data,'$.oneDayPriceChange'))"
	}
	pm := fmt.Sprintf(`SELECT 'polymarket' source, id, COALESCE(json_extract(data,'$.question'), json_extract(data,'$.title'), id) title,
CAST(COALESCE(%s,0) AS REAL) delta, CAST(COALESCE(json_extract(data,'$.lastTradePrice'),0) AS REAL) current_price,
COALESCE(json_extract(data,'$.endDate'), '') end_date,
CAST(COALESCE(json_extract(data,'$.volume24hr'), json_extract(data,'$.volumeNum'),0) AS REAL) volume_24h FROM resources WHERE resource_type='markets'`, pmDelta)
	ks := `SELECT 'kalshi' source, id, COALESCE(json_extract(data,'$.title'), id) title,
CAST(COALESCE(json_extract(data,'$.last_price_dollars'),0) - COALESCE(json_extract(data,'$.previous_price_dollars'),0) AS REAL) delta,
CAST(COALESCE(json_extract(data,'$.last_price_dollars'),0) AS REAL) current_price,
COALESCE(json_extract(data,'$.expiration_time'), json_extract(data,'$.close_time'), '') end_date,
CAST(COALESCE(json_extract(data,'$.volume_24h_fp'),0) AS REAL) volume_24h FROM resources WHERE resource_type='kalshi_markets'`
	switch venue {
	case "polymarket":
		return "SELECT * FROM (" + pm + ") ORDER BY ABS(delta) DESC LIMIT ?"
	case "kalshi":
		return "SELECT * FROM (" + ks + ") ORDER BY ABS(delta) DESC LIMIT ?"
	default:
		return "SELECT * FROM (" + pm + " UNION ALL " + ks + ") ORDER BY ABS(delta) DESC LIMIT ?"
	}
}

func moverRows(items []moversItem) [][]string {
	rows := make([][]string, 0, len(items))
	for _, it := range items {
		rows = append(rows, []string{it.Source, it.ID, it.Title, fmt.Sprintf("%+.1f%%", it.Delta*100), formatProb(it.CurrentPrice), formatNumber(it.Volume24h), it.EndDate})
	}
	return rows
}
