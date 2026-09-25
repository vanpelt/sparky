# /// script
# requires-python = ">=3.11"
# dependencies = [
#     "duckdb>=1.1",
#     "altair>=5.4",
#     "polars",
#     "pyarrow",
#     "vl-convert-python",
# ]
# ///
"""One engineer's year with agents: the speaker's own HiveMind export, charted for the deck.

Reads the Parquet files `hivemind export --format parquet` writes (default
~/.hivemind/exports/me, override with HM_ME_DATA) and writes deck-themed SVGs to
assets/charts/me.js plus the headline numbers to assets/charts/me.json.

    uv run analysis/hivemind_me_analysis.py

Definitions match hivemind_team_analysis.py: a top-level session has no parent, a
subagent session does. Cost is list-price equivalent from tokens, not what was paid.
Everything is 2026 onward; the current month is partial and labelled as such.
"""

import json
import os
from datetime import date
from pathlib import Path

import altair as alt
import duckdb
import vl_convert as vlc

DATA_DIR = Path(os.environ.get("HM_ME_DATA", "~/.hivemind/exports/me")).expanduser()
DECK_DIR = Path(__file__).resolve().parent.parent
START = "2026-01-01"

INK, PAPER, MUTE, AMBER, GRAY = "#0d0d0d", "#faf8f5", "#868686", "#f5a400", "#6b6b6b"

con = duckdb.connect()
con.execute(f"CREATE VIEW s AS SELECT * FROM '{DATA_DIR}/sessions.parquet' WHERE started_at >= '{START}'")
# Tool calls, rolled up to the session a person started so subagents count toward their parent.
con.execute(f"""
CREATE TABLE calls AS
WITH ch AS (
  SELECT session_id, timestamp_ms,
    json_extract_string(event_data, '$.toolCallId') AS id,
    json_extract_string(event_data, '$.toolCallName') AS tool
  FROM '{DATA_DIR}/events.parquet' WHERE event_type = 'TOOL_CALL_CHUNK'
  QUALIFY row_number() OVER (PARTITION BY id ORDER BY timestamp_ms) = 1  -- one row per call, not per chunk
),
rs AS (
  SELECT json_extract_string(event_data, '$.toolCallId') AS id, json_extract(event_data, '$.isError')::BOOL AS err
  FROM '{DATA_DIR}/events.parquet' WHERE event_type = 'TOOL_CALL_RESULT'
  QUALIFY row_number() OVER (PARTITION BY id) = 1
)
SELECT ch.*, rs.err, coalesce(s.parent_session_id, s.id) AS root
FROM ch JOIN s ON ch.session_id = s.id LEFT JOIN rs ON rs.id = ch.id
""")


def one(sql):
    return con.execute(sql).fetchone()


# ── Headline ─────────────────────────────────────────────────────────
sessions, top, subs, cost, adds, commits_proxy, secret_sessions, cache_hit = one("""
SELECT count(*), count(*) FILTER (WHERE parent_session_id IS NULL), count(*) FILTER (WHERE parent_session_id IS NOT NULL),
  sum(cost_usd), sum(total_additions), sum(tool_call_count),
  count(*) FILTER (WHERE secrets_redacted_count > 0),
  sum(cached_read_tokens) / sum(input_tokens + cached_read_tokens + cached_write_tokens)
FROM s
""")
merged, prs = one("""
SELECT count(DISTINCT pr_url) FILTER (WHERE pr_state = 'merged'), count(DISTINCT pr_url) FILTER (WHERE pr_url <> '') FROM s
""")
med_open_min, med_active_min = one("""
SELECT median(epoch(last_activity_at - started_at)) / 60, median(active_duration_ms) / 60000
FROM s WHERE parent_session_id IS NULL AND last_activity_at > started_at
""")

# ── Fan-out: sessions I start vs sessions my agents start ─────────────
fanout = con.execute("""
SELECT date_trunc('month', started_at)::DATE AS month,
  CASE WHEN parent_session_id IS NULL THEN 'Sessions I started' ELSE 'Subagents my agents started' END AS who,
  count(*) AS n
FROM s GROUP BY ALL ORDER BY month
""").pl()
ratio = con.execute("""
SELECT strftime(date_trunc('month', started_at), '%b') AS m,
  round(count(*) FILTER (WHERE parent_session_id IS NOT NULL) / count(*) FILTER (WHERE parent_session_id IS NULL), 1) AS subs_per_session
FROM s GROUP BY date_trunc('month', started_at) ORDER BY date_trunc('month', started_at)
""").pl().to_dicts()

# ── Parallelism: agents actively calling tools in the same 5 minutes ──
buckets = """
SELECT timestamp_ms // 300000 AS bucket, date_trunc('month', to_timestamp(min(timestamp_ms) / 1000))::DATE AS month,
  count(DISTINCT root) AS live
FROM calls GROUP BY 1
"""
parallel = con.execute(f"SELECT month, avg(live) AS avg_live, max(live) AS peak FROM ({buckets}) GROUP BY month ORDER BY month").pl()
pct_parallel, pct_3plus = one(f"SELECT avg((live >= 2)::INT), avg((live >= 3)::INT) FROM ({buckets})")

# ── Tools: what the agent reaches for, and what fails ────────────────
con.execute("""
CREATE VIEW tools AS SELECT CASE
  WHEN tool IN ('Bash', 'exec', 'exec_command', 'write_stdin', 'shell') THEN 'Shell'
  WHEN tool IN ('Read') THEN 'Read'
  WHEN tool IN ('Edit', 'MultiEdit', 'apply_patch') THEN 'Edit'
  WHEN tool IN ('Grep', 'Glob') THEN 'Search'
  WHEN tool IN ('Write') THEN 'Write'
  WHEN tool IN ('WebFetch', 'WebSearch') OR tool LIKE 'mcp__claude-in-chrome%' THEN 'Web & browser'
  ELSE 'Everything else' END AS kind, tool, err
FROM calls
""")
tool_share = con.execute("""
SELECT kind, count(*) / sum(count(*)) OVER () AS share FROM tools GROUP BY kind ORDER BY share DESC
""").pl()
calls_total, err_rate, bash_err, bash_share = one("""
SELECT count(*), avg(err::INT), avg(err::INT) FILTER (WHERE tool = 'Bash'), avg((kind = 'Shell')::INT) FROM tools
""")

# ── Render ───────────────────────────────────────────────────────────
def theme(chart, w=1000, h=520):
    return (
        chart.properties(width=w, height=h, background=INK)
        .configure(font="Inter")
        .configure_view(stroke=None)
        .configure_axis(
            labelColor=MUTE, titleColor=MUTE, labelFontSize=22, titleFontSize=20, titleFontWeight=400,
            gridColor="#262626", domainColor="#333333", tickColor="#333333", labelFont="JetBrains Mono",
            titlePadding=16, labelPadding=8,
        )
        .configure_legend(
            labelColor=PAPER, labelFontSize=22, titleColor=MUTE, symbolSize=260, orient="top",
            direction="horizontal", title=None, symbolType="square", columnPadding=36, labelLimit=0,
        )
    )


month_axis = alt.Axis(format="%b", grid=False, labelAngle=0)
who_order = ["Sessions I started", "Subagents my agents started"]
charts = {
    "me-fanout": theme(
        alt.Chart(fanout).mark_bar(cornerRadiusEnd=4, stroke=INK, strokeWidth=2).encode(
            x=alt.X("month:O", timeUnit="yearmonth", title=None, axis=alt.Axis(format="%b", labelAngle=0)),
            y=alt.Y("n:Q", title=None, axis=alt.Axis(tickCount=4)),
            color=alt.Color("who:N", scale=alt.Scale(domain=who_order, range=[GRAY, AMBER]), sort=who_order),
            order=alt.Order("r:Q"),
        ).transform_calculate(r=f"indexof({who_order}, datum.who)")
    ),
    "me-parallel": theme(
        alt.Chart(parallel).mark_line(
            strokeWidth=5, color=AMBER, point=alt.OverlayMarkDef(size=180, filled=True, color=AMBER, stroke=INK, strokeWidth=3)
        ).encode(
            x=alt.X("month:T", title=None, axis=alt.Axis(format="%b", grid=False, tickCount={"interval": "month", "step": 1})),
            y=alt.Y("avg_live:Q", scale=alt.Scale(domain=[1, 2]), axis=alt.Axis(tickCount=4, format=".1f"),
                    title="agents working at once (avg)"),
        )
    ),
    "me-tools": theme(
        alt.Chart(tool_share).mark_bar(cornerRadiusEnd=4, height=44).encode(
            y=alt.Y("kind:N", sort=list(tool_share.filter(tool_share["kind"] != "Everything else")["kind"]) + ["Everything else"], title=None, axis=alt.Axis(labelFont="Inter", labelColor=PAPER, labelFontSize=26, labelLimit=0, domain=False, ticks=False)),
            x=alt.X("share:Q", title=None, axis=alt.Axis(format="%", tickCount=4)),
            color=alt.condition(alt.datum.kind == "Shell", alt.value(AMBER), alt.value(GRAY)),
        ),
        h=480,
    ),
}
svgs = {k: vlc.vegalite_to_svg(v.to_json()) for k, v in charts.items()}

out = DECK_DIR / "assets" / "charts"
out.mkdir(parents=True, exist_ok=True)
(out / "me.js").write_text(
    "// Generated by analysis/hivemind_me_analysis.py. Do not edit by hand.\n"
    f"window.HM_CHARTS = Object.assign(window.HM_CHARTS || {{}}, {json.dumps(svgs)});\n"
    "document.querySelectorAll('[data-chart]').forEach((el) => { el.innerHTML = window.HM_CHARTS[el.dataset.chart] || ''; });\n"
)
stats = {
    "generated": date.today().isoformat(),
    "since": START,
    "sessions": sessions, "top_level": top, "subagents": subs,
    "merged_prs": merged, "prs": prs,
    "cost_usd": round(cost), "usd_per_merged_pr": round(cost / merged, 2),
    "lines_added": adds, "tool_calls": calls_total,
    "sessions_with_secret_scrubbed": secret_sessions, "pct_sessions_with_secret": round(secret_sessions / sessions, 3),
    "cache_hit": round(cache_hit, 3),
    "median_open_min": round(med_open_min), "median_active_min": round(med_active_min),
    "subagents_per_session_by_month": ratio,
    "parallel_by_month": parallel.to_dicts(),
    "pct_working_time_2plus_agents": round(pct_parallel, 3), "pct_working_time_3plus_agents": round(pct_3plus, 3),
    "tool_share": tool_share.to_dicts(), "shell_share": round(bash_share, 3),
    "tool_error_rate": round(err_rate, 4), "bash_error_rate": round(bash_err, 4),
}
(out / "me.json").write_text(json.dumps(stats, indent=2, default=str))
print(json.dumps(stats, indent=2, default=str))
