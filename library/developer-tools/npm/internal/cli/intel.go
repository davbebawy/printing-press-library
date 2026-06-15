package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"npm-pp-cli/internal/client"
	"npm-pp-cli/internal/config"

	"github.com/spf13/cobra"
)

type packageSummary struct {
	Name               string   `json:"name"`
	LatestVersion      string   `json:"latest_version"`
	Description        string   `json:"description,omitempty"`
	License            string   `json:"license,omitempty"`
	Keywords           []string `json:"keywords,omitempty"`
	MaintainerCount    int      `json:"maintainer_count"`
	DependencyCount    int      `json:"dependency_count"`
	LastPublishTime    string   `json:"last_publish_time,omitempty"`
	LastMonthDownloads int      `json:"last_month_downloads"`
	URL                string   `json:"url"`
}

type packageRisk struct {
	Name    string         `json:"name"`
	Score   int            `json:"score"`
	Level   string         `json:"level"`
	Signals []string       `json:"signals"`
	Summary packageSummary `json:"summary"`
}

type npmPackageDocument struct {
	Name        string                       `json:"name"`
	Description string                       `json:"description"`
	DistTags    map[string]string            `json:"dist-tags"`
	License     string                       `json:"license"`
	Keywords    []string                     `json:"keywords"`
	Maintainers []map[string]any             `json:"maintainers"`
	Versions    map[string]npmPackageVersion `json:"versions"`
	Time        map[string]string            `json:"time"`
	Deprecated  any                          `json:"deprecated"`
}

type npmPackageVersion struct {
	License      string            `json:"license"`
	Dependencies map[string]string `json:"dependencies"`
	Deprecated   any               `json:"deprecated"`
}

type npmDownloadsPoint struct {
	Downloads int `json:"downloads"`
}

func newPackageCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "package <name>",
		Short:       "Summarize an npm package for agent research",
		Example:     "  npm-pp-cli package react --json\n  npm-pp-cli package @types/node --select name,latest_version,last_month_downloads",
		Annotations: map[string]string{"pp:handwritten": "true", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if dryRunOK(flags) {
				return nil
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			summary, err := fetchPackageSummary(c, args[0])
			if err != nil {
				return classifyAPIError(err, flags)
			}
			return printJSONFiltered(cmd.OutOrStdout(), summary, flags)
		},
	}
	return cmd
}

func newCompareCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "compare <package> [package...]",
		Short:       "Compare npm packages by freshness, maintainers, dependencies, and downloads",
		Example:     "  npm-pp-cli compare react vue svelte --json\n  npm-pp-cli compare express fastify koa --select name,latest_version,last_month_downloads,dependency_count",
		Annotations: map[string]string{"pp:handwritten": "true", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if dryRunOK(flags) {
				return nil
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			summaries := make([]packageSummary, 0, len(args))
			for _, name := range args {
				summary, err := fetchPackageSummary(c, name)
				if err != nil {
					return classifyAPIError(err, flags)
				}
				summaries = append(summaries, summary)
			}
			sort.SliceStable(summaries, func(i, j int) bool {
				return summaries[i].LastMonthDownloads > summaries[j].LastMonthDownloads
			})
			return printJSONFiltered(cmd.OutOrStdout(), summaries, flags)
		},
	}
	return cmd
}

func newRiskCmd(flags *rootFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "risk <package>",
		Short:       "Score npm package maintenance and adoption risk",
		Example:     "  npm-pp-cli risk left-pad --json",
		Annotations: map[string]string{"pp:handwritten": "true", "mcp:read-only": "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			if dryRunOK(flags) {
				return nil
			}
			c, err := flags.newClient()
			if err != nil {
				return err
			}
			summary, err := fetchPackageSummary(c, args[0])
			if err != nil {
				return classifyAPIError(err, flags)
			}
			return printJSONFiltered(cmd.OutOrStdout(), scorePackageRisk(summary), flags)
		},
	}
	return cmd
}

func fetchPackageSummary(c *client.Client, name string) (packageSummary, error) {
	doc, err := fetchPackageDocument(c, name)
	if err != nil {
		return packageSummary{}, err
	}
	latest := doc.DistTags["latest"]
	latestVersion := doc.Versions[latest]
	license := firstNonEmpty(doc.License, latestVersion.License)
	lastPublishTime := doc.Time[latest]
	downloads, _ := fetchLastMonthDownloads(c, name)
	return packageSummary{
		Name:               firstNonEmpty(doc.Name, name),
		LatestVersion:      latest,
		Description:        doc.Description,
		License:            license,
		Keywords:           doc.Keywords,
		MaintainerCount:    len(doc.Maintainers),
		DependencyCount:    len(latestVersion.Dependencies),
		LastPublishTime:    lastPublishTime,
		LastMonthDownloads: downloads,
		URL:                "https://www.npmjs.com/package/" + name,
	}, nil
}

func fetchPackageDocument(c *client.Client, name string) (npmPackageDocument, error) {
	path := "/" + escapePackageName(name)
	data, err := c.Get(path, nil)
	if err != nil {
		return npmPackageDocument{}, err
	}
	var doc npmPackageDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return npmPackageDocument{}, fmt.Errorf("decoding npm package metadata for %s: %w", name, err)
	}
	return doc, nil
}

func fetchLastMonthDownloads(c *client.Client, name string) (int, error) {
	downloadClient := c
	if strings.TrimRight(c.BaseURL, "/") == "https://registry.npmjs.org" {
		cfg := config.Config{BaseURL: "https://api.npmjs.org"}
		if c.Config != nil {
			cfg = *c.Config
			cfg.BaseURL = "https://api.npmjs.org"
		}
		downloadClient = client.New(&cfg, c.ConfiguredTimeout(), c.RateLimit())
		downloadClient.DryRun = c.DryRun
		downloadClient.NoCache = c.NoCache
	}
	data, err := downloadClient.Get("/downloads/point/last-month/"+escapePackageName(name), nil)
	if err != nil {
		return 0, err
	}
	var point npmDownloadsPoint
	if err := json.Unmarshal(data, &point); err != nil {
		return 0, fmt.Errorf("decoding npm downloads for %s: %w", name, err)
	}
	return point.Downloads, nil
}

func scorePackageRisk(summary packageSummary) packageRisk {
	score := 0
	signals := []string{}
	if strings.TrimSpace(summary.License) == "" {
		score += 25
		signals = append(signals, "missing license")
	}
	if summary.MaintainerCount == 0 {
		score += 25
		signals = append(signals, "no listed maintainers")
	} else if summary.MaintainerCount == 1 {
		score += 10
		signals = append(signals, "single listed maintainer")
	}
	if summary.DependencyCount > 40 {
		score += 15
		signals = append(signals, "large dependency surface")
	}
	if summary.LastMonthDownloads < 100 {
		score += 15
		signals = append(signals, "low last-month downloads")
	}
	if summary.LastPublishTime != "" {
		if t, err := time.Parse(time.RFC3339Nano, summary.LastPublishTime); err == nil && time.Since(t) > 730*24*time.Hour {
			score += 20
			signals = append(signals, "stale release")
		}
	}
	level := "low"
	switch {
	case score >= 60:
		level = "high"
	case score >= 30:
		level = "medium"
	}
	return packageRisk{Name: summary.Name, Score: score, Level: level, Signals: signals, Summary: summary}
}

func escapePackageName(name string) string {
	return strings.ReplaceAll(url.PathEscape(name), "%2F", "%2f")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
