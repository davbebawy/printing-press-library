// Copyright 2026 mvanhorn. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

func topicFTSQuery(s string) string {
	replacer := strings.NewReplacer("-", " ", "_", " ", "'", " ", `"`, " ")
	parts := strings.Fields(replacer.Replace(s))
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		quoted = append(quoted, `"`+part+`"`)
	}
	return strings.Join(quoted, " ")
}

func newLiquidCmd(flags *rootFlags) *cobra.Command {
	var minVolume float64
	var limit int
	var dbPath string
	var vf venueFlags
	cmd := &cobra.Command{
		Use:   "liquid",
		Short: "Markets above a volume floor across Polymarket and Kalshi",
		Example: `  prediction-goat-pp-cli liquid --min-volume 100000 --json
  prediction-goat-pp-cli liquid --min-volume 50000 --limit 25
  prediction-goat-pp-cli liquid --kalshi --min-volume 50000`,
		Annotations: map[string]string{"mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRunOK(flags) {
				return nil
			}
			venue, err := resolveVenue(vf)
			if err != nil {
				return err
			}
			items, err := runMarketScreen(cmd, "liquid", dbPath, venue, limit, minVolume, "", "")
			if err != nil {
				return err
			}
			outcome := refreshMarketScreenItems(cmd.Context(), nil, items)
			meta := buildFreshnessMeta(outcome, indexSyncedAtFromPath(cmd.Context(), dbPath))
			if renderErr := renderTrending(cmd, flags, trendingResult{Items: items, Meta: meta}); renderErr != nil {
				return renderErr
			}
			if len(items) == 0 {
				if hint := emptyStoreHint(cmd, dbPath, "liquid", venue); hint != nil {
					return hint
				}
			}
			return nil
		},
	}
	cmd.Flags().Float64Var(&minVolume, "min-volume", 10000, "Minimum volume")
	cmd.Flags().IntVar(&limit, "limit", 20, "Max results")
	cmd.Flags().StringVar(&dbPath, "db", "", "Database path (default: standard cache location)")
	addVenueFlags(cmd, &vf)
	return cmd
}
