-- "How long would this have taken without AI?" vs how long the agent actually worked.
-- Source: prod ClickHouse `effort_reports` (the in-product survey, migration 0054 in
-- wandb/agentstream), joined to `sessions`. Aggregates only; no user ids leave the query.
-- Cohort matches hivemind_team_analysis.py: GitHub `wandb` org members or @wandb.com emails.
-- Agent time = the surveyed top-level session's active time + its subagents' active time.
-- Run from an agentstream-py checkout:
--   uv run python .claude/skills/clickhouse-query/scripts/clickhouse_query.py "$(cat effort_reports.sql)" -f json
WITH
wb AS (
  SELECT user_id FROM users FINAL
  WHERE has(github_orgs, 'wandb') OR endsWith(lower(email), '@wandb.com')
),
r AS (
  SELECT session_id, user_id, reported_hours FROM effort_reports FINAL
  WHERE response = 'answered' AND reported_hours > 0 AND user_id IN (SELECT user_id FROM wb)
),
top AS (
  SELECT toString(id) AS sid, active_duration_ms, dateDiff('second', started_at, last_activity_at) AS wall_s, cost_usd
  FROM sessions FINAL WHERE toString(id) IN (SELECT session_id FROM r)
),
kids AS (
  SELECT toString(parent_session_id) AS sid, sum(active_duration_ms) AS sub_ms, sum(cost_usd) AS sub_cost
  FROM sessions FINAL WHERE toString(parent_session_id) IN (SELECT session_id FROM r) GROUP BY sid
),
j AS (
  SELECT r.user_id, r.reported_hours AS said_h,
    top.active_duration_ms / 3.6e6 AS agent_h,
    (top.active_duration_ms + ifNull(kids.sub_ms, 0)) / 3.6e6 AS agent_incl_sub_h,
    top.wall_s / 3600 AS wall_h,
    top.cost_usd + ifNull(kids.sub_cost, 0) AS cost
  FROM r JOIN top ON top.sid = r.session_id LEFT JOIN kids ON kids.sid = r.session_id
  WHERE top.active_duration_ms > 0
)
SELECT count() AS n, uniqExact(user_id) AS users,
  round(sum(said_h)) AS said_h_total, round(sum(agent_h), 1) AS agent_h_total, round(sum(agent_incl_sub_h), 1) AS agent_sub_h_total, round(sum(wall_h)) AS wall_h_total,
  round(quantileExact(0.5)(said_h), 2) AS med_said_h, round(quantileExact(0.5)(agent_h) * 60) AS med_agent_min, round(quantileExact(0.5)(wall_h), 2) AS med_wall_h,
  round(quantileExact(0.5)(said_h / agent_h), 1) AS med_ratio_active, round(quantileExact(0.5)(said_h / agent_incl_sub_h), 1) AS med_ratio_incl_sub, round(quantileExact(0.5)(said_h / greatest(wall_h, 0.01)), 1) AS med_ratio_wall,
  round(sum(said_h) / sum(agent_incl_sub_h), 1) AS pooled_ratio_incl_sub,
  round(avg(said_h > agent_incl_sub_h) * 100) AS pct_said_longer_than_agent,
  round(avg(said_h > wall_h) * 100) AS pct_said_longer_than_wall,
  round(quantileExact(0.25)(said_h / agent_incl_sub_h), 1) AS p25_ratio, round(quantileExact(0.75)(said_h / agent_incl_sub_h), 1) AS p75_ratio,
  round(sum(cost)) AS cost_usd, round(sum(cost) / sum(said_h), 2) AS usd_per_said_hour
FROM j
