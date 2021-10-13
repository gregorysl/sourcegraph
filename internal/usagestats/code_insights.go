package usagestats

import (
	"context"
	"fmt"
	"time"

	"github.com/sourcegraph/sourcegraph/internal/insights"

	"github.com/inconshreveable/log15"

	"github.com/lib/pq"

	"github.com/sourcegraph/sourcegraph/internal/database/dbutil"
	"github.com/sourcegraph/sourcegraph/internal/types"
)

func GetCodeInsightsUsageStatistics(ctx context.Context, db dbutil.DB) (*types.CodeInsightsUsageStatistics, error) {
	stats := types.CodeInsightsUsageStatistics{}

	const platformQuery = `
	SELECT
		COUNT(*) FILTER (WHERE name = 'ViewInsights')                       AS weekly_insights_page_views,
		COUNT(distinct user_id) FILTER (WHERE name = 'ViewInsights')        AS weekly_insights_unique_page_views,
		COUNT(distinct user_id)
			FILTER (WHERE name = 'InsightAddition')							AS weekly_insight_creators,
		COUNT(*) FILTER (WHERE name = 'InsightConfigureClick') 				AS weekly_insight_configure_click,
		COUNT(*) FILTER (WHERE name = 'InsightAddMoreClick') 				AS weekly_insight_add_more_click
	FROM event_logs
	WHERE name in ('ViewInsights', 'InsightAddition', 'InsightConfigureClick', 'InsightAddMoreClick')
		AND timestamp > DATE_TRUNC('week', $1::timestamp);
	`

	if err := db.QueryRowContext(ctx, platformQuery, timeNow()).Scan(
		&stats.WeeklyInsightsPageViews,
		&stats.WeeklyInsightsUniquePageViews,
		&stats.WeeklyInsightCreators,
		&stats.WeeklyInsightConfigureClick,
		&stats.WeeklyInsightAddMoreClick,
	); err != nil {
		return nil, err
	}

	const metricsByInsightQuery = `
	SELECT argument ->> 'insightType'::text 					AS insight_type,
        COUNT(*) FILTER (WHERE name = 'InsightAddition') 		AS additions,
        COUNT(*) FILTER (WHERE name = 'InsightEdit') 			AS edits,
        COUNT(*) FILTER (WHERE name = 'InsightRemoval') 		AS removals,
		COUNT(*) FILTER (WHERE name = 'InsightHover') 			AS hovers,
		COUNT(*) FILTER (WHERE name = 'InsightUICustomization') AS ui_customizations,
		COUNT(*) FILTER (WHERE name = 'InsightDataPointClick') 	AS data_point_clicks
	FROM event_logs
	WHERE name in ('InsightAddition', 'InsightEdit', 'InsightRemoval', 'InsightHover', 'InsightUICustomization', 'InsightDataPointClick')
		AND timestamp > DATE_TRUNC('week', $1::timestamp)
	GROUP BY insight_type;
	`

	weeklyUsageStatisticsByInsight := []*types.InsightUsageStatistics{}
	rows, err := db.QueryContext(ctx, metricsByInsightQuery, timeNow())

	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		weeklyInsightUsageStatistics := types.InsightUsageStatistics{}

		if err := rows.Scan(
			&weeklyInsightUsageStatistics.InsightType,
			&weeklyInsightUsageStatistics.Additions,
			&weeklyInsightUsageStatistics.Edits,
			&weeklyInsightUsageStatistics.Removals,
			&weeklyInsightUsageStatistics.Hovers,
			&weeklyInsightUsageStatistics.UICustomizations,
			&weeklyInsightUsageStatistics.DataPointClicks,
		); err != nil {
			return nil, err
		}

		weeklyUsageStatisticsByInsight = append(weeklyUsageStatisticsByInsight, &weeklyInsightUsageStatistics)
	}
	stats.WeeklyUsageStatisticsByInsight = weeklyUsageStatisticsByInsight

	if err := rows.Err(); err != nil {
		return nil, err
	}

	const weeklyFirstTimeCreatorsQuery = `
	WITH first_times AS (
		SELECT
			user_id,
			MIN(timestamp) as first_time
		FROM event_logs
		WHERE name = 'InsightAddition'
		GROUP BY user_id
		)
	SELECT
		DATE_TRUNC('week', $1::timestamp) AS week_start,
		COUNT(distinct user_id) as weekly_first_time_insight_creators
	FROM first_times
	WHERE first_time > DATE_TRUNC('week', $1::timestamp);
	`

	if err := db.QueryRowContext(ctx, weeklyFirstTimeCreatorsQuery, timeNow()).Scan(
		&stats.WeekStart,
		&stats.WeeklyFirstTimeInsightCreators,
	); err != nil {
		return nil, err
	}

	weeklyUsage, err := GetCreationViewUsage(ctx, db, timeNow)
	if err != nil {
		return nil, err
	}
	stats.WeeklyAggregatedUsage = weeklyUsage

	// These two pings are slightly more fragile than the others because they deserialize json from settings. They are
	// also less important. So, in the case of any errors here we will not fail the entire ping for code insights.
	timeIntervals, err := GetTimeStepCounts(ctx, db)
	if err != nil {
		log15.Error("code-insights/GetTimeStepCounts", "error", err)
		return nil, nil
	}
	stats.InsightTimeIntervals = timeIntervals

	orgVisible, err := GetOrgInsightCounts(ctx, db)
	if err != nil {
		log15.Error("code-insights/GetOrgInsightCounts", "error", err)
		return nil, nil
	}
	stats.InsightOrgVisible = orgVisible

	return &stats, nil
}

func GetTimeStepCounts(ctx context.Context, db dbutil.DB) ([]types.InsightTimeIntervalPing, error) {
	definedInsights, err := insights.GetSearchInsights(ctx, db, insights.All)
	if err != nil {
		return []types.InsightTimeIntervalPing{}, err
	}

	daysCounts := make(map[int]int)
	for _, insight := range definedInsights {
		days := convertStepToDays(insight)
		daysCounts[days] += 1
	}

	results := make([]types.InsightTimeIntervalPing, 0)
	for interval, count := range daysCounts {
		results = append(results, types.InsightTimeIntervalPing{
			IntervalDays: interval,
			TotalCount:   count,
		})
	}

	return results, nil
}

// convertStepToDays converts the step interval defined in the insight settings to days, rounded down
func convertStepToDays(insight insights.SearchInsight) int {
	if insight.Step.Days != nil {
		return *insight.Step.Days
	} else if insight.Step.Hours != nil {
		return 0
	} else if insight.Step.Weeks != nil {
		return *insight.Step.Weeks * 7
	} else if insight.Step.Months != nil {
		return *insight.Step.Months * 30
	} else if insight.Step.Years != nil {
		return *insight.Step.Years * 365
	}

	return 0
}

func GetOrgInsightCounts(ctx context.Context, db dbutil.DB) ([]types.OrgVisibleInsightPing, error) {

	definedInsights, err := insights.GetSearchInsights(ctx, db, insights.Org)
	if err != nil {
		return []types.OrgVisibleInsightPing{}, err
	}

	search := types.OrgVisibleInsightPing{Type: "search"}
	search.TotalCount = len(definedInsights)

	langStatsInsights, err := insights.GetLangStatsInsights(ctx, db, insights.Org)
	if err != nil {
		return []types.OrgVisibleInsightPing{}, err
	}
	lang := types.OrgVisibleInsightPing{Type: "lang-stats"}
	lang.TotalCount = len(langStatsInsights)

	return []types.OrgVisibleInsightPing{search, lang}, nil
}

func GetCreationViewUsage(ctx context.Context, db dbutil.DB, timeSupplier func() time.Time) ([]types.AggregatedPingStats, error) {
	builder := creationPagesPingBuilder(timeSupplier)

	results, err := builder.Sample(ctx, db)
	if err != nil {
		return []types.AggregatedPingStats{}, err
	}

	return results, nil
}

// WithAll adds multiple pings by name to this builder
func (b *PingQueryBuilder) WithAll(pings []types.PingName) *PingQueryBuilder {
	for _, p := range pings {
		b.With(p)
	}
	return b
}

// With add a single ping by name to this builder
func (b *PingQueryBuilder) With(name types.PingName) *PingQueryBuilder {
	b.pings = append(b.pings, string(name))
	return b
}

// Sample executes the derived query generated by this builder and returns a sample at the current time
func (b *PingQueryBuilder) Sample(ctx context.Context, db dbutil.DB) ([]types.AggregatedPingStats, error) {

	query := fmt.Sprintf(templatePingQueryStr, b.timeWindow)

	rows, err := db.QueryContext(ctx, query, b.getTime(), pq.Array(b.pings))
	if err != nil {
		return []types.AggregatedPingStats{}, err
	}
	defer rows.Close()

	results := make([]types.AggregatedPingStats, 0)

	for rows.Next() {
		stats := types.AggregatedPingStats{}
		if err := rows.Scan(&stats.Name, &stats.TotalCount, &stats.UniqueCount); err != nil {
			return []types.AggregatedPingStats{}, err
		}
		results = append(results, stats)
	}

	return results, nil
}

func creationPagesPingBuilder(timeSupplier func() time.Time) PingQueryBuilder {
	names := []types.PingName{
		"ViewCodeInsightsCreationPage",
		"ViewCodeInsightsSearchBasedCreationPage",
		"ViewCodeInsightsCodeStatsCreationPage",

		"CodeInsightsCreateSearchBasedInsightClick",
		"CodeInsightsCreateCodeStatsInsightClick",
		"CodeInsightsExploreInsightExtensionsClick",

		"CodeInsightsSearchBasedCreationPageSubmitClick",
		"CodeInsightsSearchBasedCreationPageCancelClick",

		"CodeInsightsCodeStatsCreationPageSubmitClick",
		"CodeInsightsCodeStatsCreationPageCancelClick",
	}

	builder := NewPingBuilder(Week, timeSupplier)
	builder.WithAll(names)

	return builder
}

func NewPingBuilder(timeWindow TimeWindow, timeSupplier func() time.Time) PingQueryBuilder {
	return PingQueryBuilder{timeWindow: timeWindow, getTime: timeSupplier}
}

type PingQueryBuilder struct {
	pings      []string
	timeWindow TimeWindow
	getTime    func() time.Time
}

type TimeWindow string

const (
	Hour  TimeWindow = "hour"
	Day   TimeWindow = "day"
	Week  TimeWindow = "week"
	Month TimeWindow = "month"
	Year  TimeWindow = "year"
)

const templatePingQueryStr = `
-- source:internal/usagestats/code_insights.go:Sample
SELECT name, COUNT(*) AS total_count, COUNT(DISTINCT user_id) AS unique_count
FROM event_logs
WHERE name = ANY($2)
AND timestamp > DATE_TRUNC('%v', $1::TIMESTAMP)
GROUP BY name;
`
