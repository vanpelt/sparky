# /// script
# requires-python = ">=3.11"
# dependencies = [
#     "marimo",
#     "duckdb>=1.1",
#     "altair>=5.4",
#     "polars",
#     "pyarrow",
#     "vl-convert-python",
# ]
# ///

import marimo

__generated_with = "0.25.0"
app = marimo.App(width="full", app_title="HiveMind at W&B")


@app.cell(hide_code=True)
def _(mo):
    mo.md(r"""
    # HiveMind at W&B: how the team builds with AI

    Team-wide trends from **anonymized, aggregate-only** pulls of prod `sessions`
    (W&B cohort = members of the `wandb` GitHub org or `@wandb.com` emails).
    User ids are salted SHA-256 prefixes; no names, repos, titles, or prompts
    were ever pulled. Queries live next to the data in `DATA_DIR/*.sql`.

    **Definitions used throughout**

    - *Top-level session*: `parent_session_id IS NULL`. *Subagent*: the rest.
    - *Interactive session*: top-level with **≥ 2 turns**. Single-turn top-level
      sessions are almost all headless automation (`claude -p`, CI, review bots).
    - *Weeks* are Monday-based; the current, partial week is dropped.
    - *Cost* is list-price equivalent computed from tokens, not what W&B was invoiced
      (most seats are subscriptions).
    """)
    return


@app.cell
def _():
    import os
    from datetime import date, timedelta
    from pathlib import Path

    import altair as alt
    import duckdb
    import marimo as mo
    import polars as pl

    DATA_DIR = Path(os.environ.get("HM_PREZO_DATA", "~/.hivemind/exports/prezo-agent-eng")).expanduser()
    DECK_DIR = Path(__file__).resolve().parent.parent
    ME = os.environ.get("HM_PREZO_ME", "c2cbf1607e")
    TODAY = date.today()
    CURRENT_WEEK = TODAY - timedelta(days=TODAY.weekday())
    return CURRENT_WEEK, DATA_DIR, DECK_DIR, ME, alt, duckdb, mo, pl


@app.cell
def _():
    # Validated for CVD on the deck's dark surface (#0d0d0d); gray is the "other" fold.
    HARNESS_ORDER = ["Codex", "Claude Code", "Cursor", "OpenCode & Pi", "Other"]
    HARNESS_COLORS = ["#3987e5", "#f5a400", "#9085e9", "#199e70", "#6b6b6b"]
    PROVIDER_ORDER = ["OpenAI", "Anthropic", "Cursor (Auto/Composer)", "Open weights", "Unknown"]
    PROVIDER_COLORS = HARNESS_COLORS
    INK, PAPER, MUTE, AMBER = "#0d0d0d", "#faf8f5", "#868686", "#f5a400"
    return (
        AMBER,
        HARNESS_COLORS,
        HARNESS_ORDER,
        INK,
        MUTE,
        PAPER,
        PROVIDER_COLORS,
        PROVIDER_ORDER,
    )


@app.cell
def _(CURRENT_WEEK, DATA_DIR, duckdb):
    con = duckdb.connect()
    for _name in ["heat", "tools", "skills"]:
        con.execute(f"CREATE OR REPLACE VIEW raw_{_name} AS SELECT * FROM read_parquet('{DATA_DIR / _name}.parquet')")
    con.execute(f"""
    CREATE OR REPLACE VIEW raw_cube AS SELECT * REPLACE (n::DOUBLE AS n, active_ms::DOUBLE AS active_ms, wall_s::DOUBLE AS wall_s, turns::DOUBLE AS turns, messages::DOUBLE AS messages, tool_calls::DOUBLE AS tool_calls, errors::DOUBLE AS errors, compactions::DOUBLE AS compactions, input_tokens::DOUBLE AS input_tokens, output_tokens::DOUBLE AS output_tokens, cached_read_tokens::DOUBLE AS cached_read_tokens, cached_write_tokens::DOUBLE AS cached_write_tokens, cost_usd::DOUBLE AS cost_usd, additions::DOUBLE AS additions, deletions::DOUBLE AS deletions, with_agent_md::DOUBLE AS with_agent_md, d_lt1m::DOUBLE AS d_lt1m, d_1_5m::DOUBLE AS d_1_5m, d_5_15m::DOUBLE AS d_5_15m, d_15_60m::DOUBLE AS d_15_60m, d_1_3h::DOUBLE AS d_1_3h, d_3h_plus::DOUBLE AS d_3h_plus)
    FROM read_parquet('{DATA_DIR}/cube.parquet')
    """)

    # Accounts whose top-level work is overwhelmingly single-turn are automation, not people.
    con.execute("""
    CREATE OR REPLACE TABLE automation_users AS
    SELECT u FROM raw_cube WHERE NOT is_sub GROUP BY u
    HAVING sum(n) > 5000 AND sum(n) FILTER (WHERE turn_bkt = '0-1') > 0.8 * sum(n)
    """)

    con.execute(f"""
    CREATE OR REPLACE VIEW s AS
    SELECT
      c.*,
      CAST(week AS DATE) AS wk,
      CAST(date_trunc('month', week) AS DATE) AS month,
      NOT is_sub AND turn_bkt <> '0-1' AS interactive,
      u IN (SELECT u FROM automation_users) AS automation_user,
      CASE agent_type
        WHEN 'claude' THEN 'Claude Code' WHEN 'codex' THEN 'Codex' WHEN 'cursor' THEN 'Cursor'
        WHEN 'opencode' THEN 'OpenCode & Pi' WHEN 'pi' THEN 'OpenCode & Pi'
        ELSE 'Other' END AS harness,
      CASE
        WHEN model IS NULL OR model IN ('', '<synthetic>', 'unknown') THEN 'Unknown'
        WHEN regexp_matches(model, 'gpt-oss|glm|kimi|qwen|deepseek|llama|mistral|gemma|minimax') THEN 'Open weights'
        WHEN regexp_matches(model, 'claude|opus|sonnet|haiku|fable|anthropic') THEN 'Anthropic'
        WHEN regexp_matches(model, 'gpt|codex|o[134]-|^o[134]$|openai') THEN 'OpenAI'
        WHEN regexp_matches(model, 'gemini') THEN 'Google'
        WHEN regexp_matches(model, 'cursor|composer|^default$|^auto$') THEN 'Cursor (Auto/Composer)'
        WHEN agent_type = 'claude' THEN 'Anthropic'
        WHEN agent_type = 'codex' THEN 'OpenAI'
        ELSE 'Unknown' END AS provider_raw
    FROM raw_cube c
    WHERE week < DATE '{CURRENT_WEEK}'
    """)
    # Google is too small to carry its own hue; it folds into Unknown/other on charts.
    con.execute("""
    CREATE OR REPLACE VIEW sessions_x AS
    SELECT *, CASE WHEN provider_raw = 'Google' THEN 'Unknown' ELSE provider_raw END AS provider FROM s
    """)
    return (con,)


@app.cell(hide_code=True)
def _(mo):
    start = mo.ui.date(value="2026-01-05", label="Trend start")
    drop_automation = mo.ui.switch(value=True, label="Exclude automation-dominated accounts")
    window_days = mo.ui.slider(28, 120, value=60, step=7, label="'Recent' window (days)")
    mo.hstack([start, drop_automation, window_days], justify="start", gap=2)
    return drop_automation, start, window_days


@app.cell
def _(con, drop_automation, start):
    _auto = "AND NOT automation_user" if drop_automation.value else ""
    con.execute(f"""
    CREATE OR REPLACE VIEW f AS
    SELECT * FROM sessions_x WHERE wk >= DATE '{start.value}' {_auto}
    """)
    filter_ready = True
    return (filter_ready,)


@app.cell(hide_code=True)
def _(mo):
    mo.md("""
    ## 0 · By the numbers
    """)
    return


@app.cell
def _(con, filter_ready, mo):
    assert filter_ready
    _all = con.execute("""
    SELECT sum(n) sessions, count(DISTINCT u) engineers, sum(tool_calls) tool_calls,
      sum(n) FILTER (WHERE pr_state = 'merged' AND NOT is_sub) merged_sessions,
      sum(n) FILTER (WHERE interactive) interactive_sessions,
      sum(active_ms) / 3.6e6 active_hours,
      sum(n) FILTER (WHERE is_sub) subagents,
      min(wk) first_week
    FROM sessions_x
    """).pl().row(0, named=True)
    _auto = con.execute("SELECT count(*) FROM automation_users").fetchone()[0]
    _months = con.execute("""
    SELECT date_diff('month', min(month), max(month)) + 1 FROM sessions_x
    WHERE month >= (SELECT min(month) FROM sessions_x WHERE harness IN ('Claude Code','Codex'))
    """).fetchone()[0]
    headline = {**_all, "automation_accounts": _auto, "months_since_rollout": _months}

    def _fmt(x):
        x = float(x)
        return f"{x/1e6:.1f}M" if x >= 1e6 else f"{x/1e3:.0f}K" if x >= 1e4 else f"{x:,.0f}"

    mo.hstack(
        [
            mo.stat(_fmt(headline["sessions"]), label="sessions captured", caption=f"{_fmt(headline['interactive_sessions'])} interactive"),
            mo.stat(_fmt(headline["engineers"]), label="engineers"),
            mo.stat(_fmt(headline["tool_calls"]), label="tool calls"),
            mo.stat(_fmt(headline["merged_sessions"]), label="sessions → merged PRs"),
            mo.stat(_fmt(headline["active_hours"]), label="active agent hours"),
            mo.stat(_fmt(headline["subagents"]), label="subagent sessions"),
        ],
        justify="start",
    )
    return (headline,)


@app.cell(hide_code=True)
def _(mo):
    mo.md("""
    ## 1 · Adoption: monthly active engineers

    Pre-2026 history is mostly **Cursor**: the daemon backfills whatever is on disk, Cursor keeps
    history forever and Claude Code prunes transcripts after ~30 days. Read the 2025 part of this
    chart as "what backfill could see", not as real adoption.
    """)
    return


@app.cell
def _(HARNESS_COLORS, HARNESS_ORDER, alt, con, drop_automation):
    _auto = "WHERE NOT automation_user" if drop_automation.value else ""
    mau = con.execute(f"""
    SELECT month, harness, count(DISTINCT u) AS users FROM sessions_x {_auto}
    GROUP BY ALL
    UNION ALL
    SELECT month, 'All harnesses', count(DISTINCT u) FROM sessions_x {_auto} GROUP BY ALL
    ORDER BY month
    """).pl()
    _base = alt.Chart(mau).encode(x=alt.X("month:T", title=None))
    mau_chart = (
        _base.transform_filter("datum.harness == 'All harnesses'").mark_area(opacity=0.12, color="#aaaaaa", line={"color": "#aaaaaa"})
        .encode(y=alt.Y("users:Q", title="monthly active engineers"))
        + _base.transform_filter("datum.harness != 'All harnesses'").mark_line(point=True, strokeWidth=2)
        .encode(
            y="users:Q",
            color=alt.Color("harness:N", scale=alt.Scale(domain=HARNESS_ORDER, range=HARNESS_COLORS)),
            tooltip=[alt.Tooltip("month:T", format="%b %Y"), alt.Tooltip("harness:N"), alt.Tooltip("users:Q")],
        )
    ).properties(height=320, width="container")
    mau_chart
    return (mau,)


@app.cell(hide_code=True)
def _(mo):
    mo.md("""
    ## 2 · Harnesses over time

    Share of **interactive** sessions per week, and how many engineers use more than one harness.
    """)
    return


@app.cell
def _(HARNESS_COLORS, HARNESS_ORDER, alt, con, filter_ready, mo):
    assert filter_ready
    harness_weekly = con.execute("""
    SELECT wk, harness, sum(n) sessions, count(DISTINCT u) AS users,
      sum(n) / sum(sum(n)) OVER (PARTITION BY wk) AS share
    FROM f WHERE interactive GROUP BY wk, harness ORDER BY wk
    """).pl()
    harness_share_chart = (
        alt.Chart(harness_weekly)
        .mark_area()
        .encode(
            x=alt.X("wk:T", title=None),
            y=alt.Y("share:Q", stack="normalize", axis=alt.Axis(format="%"), title="share of interactive sessions"),
            color=alt.Color("harness:N", scale=alt.Scale(domain=HARNESS_ORDER, range=HARNESS_COLORS), sort=HARNESS_ORDER),
            order=alt.Order("harness_rank:Q"),
            tooltip=[alt.Tooltip("wk:T"), alt.Tooltip("harness:N"), alt.Tooltip("share:Q", format=".0%"), alt.Tooltip("sessions:Q")],
        )
        .transform_calculate(harness_rank=f"indexof({HARNESS_ORDER}, datum.harness)")
        .properties(height=320, width="container")
    )

    multi = con.execute("""
    WITH um AS (SELECT month, u, count(DISTINCT harness) h FROM f WHERE NOT is_sub GROUP BY ALL)
    SELECT month, count(*) AS users, count(*) FILTER (WHERE h >= 2) multi, round(100.0::DOUBLE * multi / users, 1) pct_multi
    FROM um GROUP BY month ORDER BY month
    """).pl()
    mo.vstack([harness_share_chart, mo.ui.table(multi, label="Engineers using ≥ 2 harnesses in a month", selection=None)])
    return harness_weekly, multi


@app.cell(hide_code=True)
def _(mo):
    mo.md("""
    ## 3 · Providers over time

    Provider is derived from the session's primary model. Counting **all** sessions (subagents
    included) measures where the work runs; toggle to interactive-only to measure human choice.
    Cursor's `default`/Auto routing hides the underlying vendor, so it gets its own bucket.
    """)
    return


@app.cell
def _(mo):
    provider_scope = mo.ui.radio(
        options={"All sessions (incl. subagents)": "all", "Interactive only": "interactive"},
        value="All sessions (incl. subagents)",
        label="Scope",
        inline=True,
    )
    provider_measure = mo.ui.radio(
        options={"Sessions": "n", "Output tokens": "output_tokens", "Cost (list price)": "cost_usd"},
        value="Sessions",
        label="Measure",
        inline=True,
    )
    mo.hstack([provider_scope, provider_measure], justify="start", gap=3)
    return provider_measure, provider_scope


@app.cell
def _(
    PROVIDER_COLORS,
    PROVIDER_ORDER,
    alt,
    con,
    filter_ready,
    provider_measure,
    provider_scope,
):
    assert filter_ready
    _where = "WHERE interactive" if provider_scope.value == "interactive" else ""
    provider_weekly = con.execute(f"""
    SELECT wk, provider, sum({provider_measure.value})::DOUBLE v,
      v / sum(v) OVER (PARTITION BY wk) AS share
    FROM f {_where} GROUP BY wk, provider ORDER BY wk
    """).pl()
    provider_chart = (
        alt.Chart(provider_weekly)
        .mark_area()
        .encode(
            x=alt.X("wk:T", title=None),
            y=alt.Y("share:Q", stack="normalize", axis=alt.Axis(format="%"), title="share"),
            color=alt.Color("provider:N", scale=alt.Scale(domain=PROVIDER_ORDER, range=PROVIDER_COLORS), sort=PROVIDER_ORDER),
            order=alt.Order("p_rank:Q"),
            tooltip=[alt.Tooltip("wk:T"), alt.Tooltip("provider:N"), alt.Tooltip("share:Q", format=".0%")],
        )
        .transform_calculate(p_rank=f"indexof({PROVIDER_ORDER}, datum.provider)")
        .properties(height=320, width="container")
    )
    provider_chart
    return


@app.cell
def _(con, filter_ready, mo):
    assert filter_ready
    top_models = con.execute("""
    SELECT strftime(month, '%Y-%m') AS month, model, sum(n) sessions,
      round(100.0::DOUBLE * sessions / sum(sessions) OVER (PARTITION BY month), 1) pct
    FROM f WHERE interactive AND month >= DATE '2026-03-01'
    GROUP BY month, model QUALIFY row_number() OVER (PARTITION BY month ORDER BY sessions DESC) <= 5
    ORDER BY month DESC, sessions DESC
    """).pl()
    mo.accordion({"Top 5 models per month (interactive sessions)": mo.ui.table(top_models, selection=None, page_size=30)})
    return


@app.cell(hide_code=True)
def _(mo):
    mo.md("""
    ## 4 · Session length: is it getting longer, and is it about the person?

    `active_duration_ms` = time the agent was actually working/talking (idle gaps removed).
    """)
    return


@app.cell
def _(alt, con, filter_ready):
    assert filter_ready
    dur_mix = con.execute("""
    UNPIVOT (
      SELECT month, sum(d_lt1m) "< 1 min", sum(d_1_5m) "1–5 min", sum(d_5_15m) "5–15 min",
        sum(d_15_60m) "15–60 min", sum(d_1_3h) "1–3 h", sum(d_3h_plus) "3 h +"
      FROM f WHERE interactive GROUP BY month
    ) ON COLUMNS(* EXCLUDE month) INTO NAME bucket VALUE sessions
    """).pl()
    _buckets = ["< 1 min", "1–5 min", "5–15 min", "15–60 min", "1–3 h", "3 h +"]
    duration_mix_chart = (
        alt.Chart(dur_mix)
        .mark_bar()
        .encode(
            x=alt.X("yearmonth(month):O", title=None),
            y=alt.Y("sessions:Q", stack="normalize", axis=alt.Axis(format="%"), title="share of interactive sessions"),
            color=alt.Color("bucket:N", sort=_buckets, scale=alt.Scale(domain=_buckets, scheme="blues")),
            order=alt.Order("b_rank:Q"),
            tooltip=[alt.Tooltip("bucket:N"), alt.Tooltip("sessions:Q")],
        )
        .transform_calculate(b_rank=f"indexof({_buckets}, datum.bucket)")
        .properties(height=300, width="container")
    )

    dur_trend = con.execute("""
    SELECT month,
      sum(active_ms) / sum(n) / 60000 avg_active_min,
      sum(turns) / sum(n) avg_turns,
      sum(tool_calls) / sum(n) avg_tool_calls,
      (sum(d_15_60m) + sum(d_1_3h) + sum(d_3h_plus)) / sum(n) share_over_15m,
      sum(compactions) / sum(n) compactions_per_session
    FROM f WHERE interactive GROUP BY month ORDER BY month
    """).pl()
    duration_mix_chart
    return (dur_trend,)


@app.cell
def _(dur_trend, mo):
    mo.ui.table(dur_trend, label="Per-session averages (interactive)", selection=None, format_mapping={
        "avg_active_min": "{:.1f}", "avg_turns": "{:.1f}", "avg_tool_calls": "{:.1f}",
        "share_over_15m": "{:.1%}", "compactions_per_session": "{:.2f}",
    })
    return


@app.cell
def _(alt, con, filter_ready, mo, pl, window_days):
    assert filter_ready
    per_user_len = con.execute(f"""
    SELECT u, sum(n) sessions,
      sum(active_ms) / sum(n) / 60000 avg_active_min,
      (sum(d_15_60m) + sum(d_1_3h) + sum(d_3h_plus)) / sum(n) share_over_15m,
      sum(turns) / sum(n) avg_turns
    FROM f WHERE interactive AND wk >= (SELECT max(wk) FROM f) - INTERVAL {window_days.value} DAY
    GROUP BY u HAVING sum(n) >= 20
    """).pl()
    _q = per_user_len.select("share_over_15m").to_series().quantile
    _stats = [per_user_len.select(pl.corr(pl.col("sessions").log(), pl.col("share_over_15m"))).item()]
    length_spread = {
        "users": per_user_len.height,
        "p10_share_over_15m": _q(0.1),
        "p50_share_over_15m": _q(0.5),
        "p90_share_over_15m": _q(0.9),
        "corr_volume_vs_long": _stats[0],
    }
    per_user_len_chart = (
        alt.Chart(per_user_len)
        .mark_circle(size=70, opacity=0.75, color="#3987e5", stroke="#0d0d0d", strokeWidth=1)
        .encode(
            x=alt.X("sessions:Q", scale=alt.Scale(type="log"), title=f"interactive sessions (last {window_days.value}d, log)"),
            y=alt.Y("share_over_15m:Q", axis=alt.Axis(format="%"), title="share of that person's sessions > 15 active min"),
            tooltip=[alt.Tooltip("sessions:Q"), alt.Tooltip("share_over_15m:Q", format=".0%"), alt.Tooltip("avg_turns:Q", format=".1f")],
        )
        .properties(height=340, width="container")
    )
    mo.vstack([
        per_user_len_chart,
        mo.md(
            f"Across **{length_spread['users']}** engineers with ≥ 20 sessions, the share of long (> 15 min) "
            f"sessions ranges from **{length_spread['p10_share_over_15m']:.0%}** (p10) to "
            f"**{length_spread['p90_share_over_15m']:.0%}** (p90), a big per-person effect. "
            f"Correlation with volume (log sessions): **{length_spread['corr_volume_vs_long']:.2f}**."
        ),
    ])
    return (length_spread,)


@app.cell(hide_code=True)
def _(mo):
    mo.md("""
    ## 5 · Subagents: are people delegating more?
    """)
    return


@app.cell
def _(HARNESS_COLORS, HARNESS_ORDER, alt, con, filter_ready, mo):
    assert filter_ready
    sub_adoption = con.execute("""
    WITH um AS (
      SELECT month, u,
        sum(n) FILTER (WHERE NOT is_sub) top,
        coalesce(sum(n) FILTER (WHERE is_sub), 0) subs,
        coalesce(sum(n) FILTER (WHERE in_workflow), 0) wf
      FROM f GROUP BY ALL
    )
    SELECT month, count(*) FILTER (WHERE top > 0) active_users,
      count(*) FILTER (WHERE top > 0 AND subs > 0) users_with_subagents,
      users_with_subagents / active_users pct_users_with_subagents,
      count(*) FILTER (WHERE wf > 0) users_with_workflows,
      sum(subs) / sum(top) subagents_per_top_level,
      median(subs / top) FILTER (WHERE top > 0) median_user_ratio
    FROM um GROUP BY month ORDER BY month
    """).pl()
    sub_adoption_chart = (
        alt.Chart(sub_adoption)
        .mark_line(point=True, strokeWidth=2, color="#f5a400")
        .encode(
            x=alt.X("month:T", title=None),
            y=alt.Y("pct_users_with_subagents:Q", axis=alt.Axis(format="%"), scale=alt.Scale(domain=[0, 1]), title="engineers who ran ≥ 1 subagent that month"),
            tooltip=[alt.Tooltip("month:T", format="%b %Y"), alt.Tooltip("pct_users_with_subagents:Q", format=".0%"), alt.Tooltip("users_with_subagents:Q"), alt.Tooltip("active_users:Q")],
        )
        .properties(height=300, width="container")
    )
    subs_by_harness = con.execute("""
    SELECT month, harness, sum(n) subagents FROM f WHERE is_sub GROUP BY ALL ORDER BY month
    """).pl()
    subs_by_harness_chart = (
        alt.Chart(subs_by_harness)
        .mark_bar()
        .encode(
            x=alt.X("yearmonth(month):O", title=None),
            y=alt.Y("subagents:Q", title="subagent sessions"),
            color=alt.Color("harness:N", scale=alt.Scale(domain=HARNESS_ORDER, range=HARNESS_COLORS), sort=HARNESS_ORDER),
            tooltip=[alt.Tooltip("harness:N"), alt.Tooltip("subagents:Q")],
        )
        .properties(height=260, width="container")
    )
    mo.vstack([
        mo.hstack([sub_adoption_chart, subs_by_harness_chart], widths="equal"),
        mo.ui.table(sub_adoption, selection=None, format_mapping={
            "pct_users_with_subagents": "{:.0%}", "subagents_per_top_level": "{:.2f}", "median_user_ratio": "{:.2f}",
        }),
    ])
    return (sub_adoption,)


@app.cell
def _(con, filter_ready, mo, window_days):
    assert filter_ready
    _w = window_days.value
    sub_then_now = con.execute(f"""
    WITH b AS (SELECT max(wk) + INTERVAL 7 DAY AS e FROM f),
    tagged AS (
      SELECT f.*, CASE WHEN wk >= e - INTERVAL {_w} DAY THEN 'now'
                       WHEN wk >= e - INTERVAL {2 * _w} DAY THEN 'then' END AS period
      FROM f, b
    ),
    um AS (
      SELECT period, u, sum(n) FILTER (WHERE NOT is_sub) top, coalesce(sum(n) FILTER (WHERE is_sub), 0) subs
      FROM tagged WHERE period IS NOT NULL GROUP BY ALL HAVING top >= 5
    ),
    paired AS (
      SELECT u, max(subs / top) FILTER (WHERE period = 'then') r_then, max(subs / top) FILTER (WHERE period = 'now') r_now
      FROM um GROUP BY u HAVING r_then IS NOT NULL AND r_now IS NOT NULL
    )
    SELECT count(*) users_active_both,
      count(*) FILTER (WHERE r_now > r_then) delegating_more,
      count(*) FILTER (WHERE r_now < r_then) delegating_less,
      median(r_then) median_ratio_then, median(r_now) median_ratio_now
    FROM paired
    """).pl().row(0, named=True)
    mo.md(
        f"""
        **Same people, {_w} days vs the {_w} days before** (engineers with ≥ 5 top-level sessions in both):
        {sub_then_now['delegating_more']} of {sub_then_now['users_active_both']} delegate more to subagents,
        {sub_then_now['delegating_less']} delegate less. Median subagents per top-level session went
        **{sub_then_now['median_ratio_then']:.2f} → {sub_then_now['median_ratio_now']:.2f}**.
        """
    )
    return (sub_then_now,)


@app.cell(hide_code=True)
def _(mo):
    mo.md("""
    ## 6 · From session to PR

    `pr_state` comes from GitHub enrichment on the session's branch. Completeness caveats: sessions
    on repos without the GitHub App, or on `main`, never get a PR; several sessions can map to one
    PR; and branch-only matching can over- or under-attribute. Treat these as lower bounds on shipping.
    """)
    return


@app.cell
def _(alt, con, filter_ready, mo):
    assert filter_ready
    pr_funnel = con.execute("""
    SELECT month,
      sum(n) interactive_sessions,
      sum(n) FILTER (WHERE pr_state <> '') with_pr,
      sum(n) FILTER (WHERE pr_state = 'merged') merged,
      sum(n) FILTER (WHERE pr_state = 'open') still_open,
      sum(n) FILTER (WHERE pr_state = 'closed') closed_unmerged,
      sum(n) FILTER (WHERE pr_state = '' AND additions > 0) wrote_code_no_pr_rows,
      with_pr / interactive_sessions pct_with_pr,
      merged / nullif(with_pr, 0) merge_rate
    FROM f WHERE interactive GROUP BY month ORDER BY month
    """).pl()
    _long = con.execute("""
    SELECT month, CASE pr_state WHEN 'merged' THEN 'Merged' WHEN 'open' THEN 'Open' WHEN 'closed' THEN 'Closed, not merged' ELSE 'No PR' END state, sum(n) sessions
    FROM f WHERE interactive GROUP BY ALL
    """).pl()
    _states = ["Merged", "Open", "Closed, not merged", "No PR"]
    pr_chart = (
        alt.Chart(_long)
        .mark_bar()
        .encode(
            x=alt.X("yearmonth(month):O", title=None),
            y=alt.Y("sessions:Q", stack="normalize", axis=alt.Axis(format="%"), title="interactive sessions"),
            color=alt.Color("state:N", sort=_states, scale=alt.Scale(domain=_states, range=["#f5a400", "#3987e5", "#9085e9", "#3a3a3a"])),
            order=alt.Order("s_rank:Q"),
            tooltip=[alt.Tooltip("state:N"), alt.Tooltip("sessions:Q")],
        )
        .transform_calculate(s_rank=f"indexof({_states}, datum.state)")
        .properties(height=300, width="container")
    )
    by_class = con.execute("""
    SELECT coalesce(nullif(session_class, ''), '(unclassified)') session_class, sum(n) sessions,
      sum(n) FILTER (WHERE pr_state <> '') / sum(n) pct_with_pr,
      sum(n) FILTER (WHERE pr_state = 'merged') / nullif(sum(n) FILTER (WHERE pr_state <> ''), 0) merge_rate
    FROM f WHERE interactive GROUP BY ALL ORDER BY sessions DESC
    """).pl()
    mo.vstack([
        pr_chart,
        mo.hstack([
            mo.ui.table(pr_funnel, selection=None, format_mapping={"pct_with_pr": "{:.1%}", "merge_rate": "{:.0%}"}),
            mo.ui.table(by_class, selection=None, format_mapping={"pct_with_pr": "{:.1%}", "merge_rate": "{:.0%}"}),
        ], widths=[3, 2]),
    ])
    return (pr_funnel,)


@app.cell
def _(con, filter_ready, mo, window_days):
    assert filter_ready
    cost_per_pr = con.execute(f"""
    WITH w AS (SELECT * FROM f WHERE wk >= (SELECT max(wk) FROM f) - INTERVAL {window_days.value} DAY)
    SELECT
      (SELECT sum(cost_usd) FROM w) total_cost,
      (SELECT sum(n) FROM w WHERE NOT is_sub AND pr_state = 'merged') merged_sessions,
      (SELECT sum(cost_usd) FROM w WHERE NOT is_sub AND pr_state = 'merged') cost_in_merged_sessions,
      (SELECT sum(n) FROM w WHERE interactive AND pr_state = '' AND session_class = 'coding') coding_sessions_no_pr,
      (SELECT sum(n) FROM w WHERE interactive AND pr_state = 'open') open_pr_sessions
    """).pl().row(0, named=True)
    mo.md(
        f"""
        **Last {window_days.value} days:** ${cost_per_pr['total_cost']:,.0f} list-price spend,
        {cost_per_pr['merged_sessions']:,} top-level sessions tied to a merged PR
        (≈ **${cost_per_pr['total_cost'] / max(cost_per_pr['merged_sessions'], 1):,.0f}** of total spend per merged session,
        or ${cost_per_pr['cost_in_merged_sessions'] / max(cost_per_pr['merged_sessions'], 1):,.0f} counting only those sessions' own cost).
        {cost_per_pr['coding_sessions_no_pr']:,} interactive *coding* sessions never reached a PR;
        {cost_per_pr['open_pr_sessions']:,} are sitting on a PR that's still open.
        """
    )
    return (cost_per_pr,)


@app.cell(hide_code=True)
def _(mo):
    mo.md("""
    ## 7 · How usage is distributed across the team
    """)
    return


@app.cell
def _(alt, con, filter_ready, mo, pl, window_days):
    assert filter_ready
    _per_user = con.execute(f"""
    SELECT u, sum(n) FILTER (WHERE interactive) sessions, sum(active_ms) / 3.6e6 active_hours, sum(cost_usd) AS cost
    FROM f WHERE wk >= (SELECT max(wk) FROM f) - INTERVAL {window_days.value} DAY GROUP BY u
    """).pl().fill_null(0)

    def _lorenz(col):
        v = _per_user.select(col).to_series().sort()
        n, tot = v.len(), v.sum()
        cum = (v.cum_sum() / tot).to_list()
        gini = 1 - 2 * sum(cum) / n + 1 / n
        top10 = 1 - cum[int(n * 0.9) - 1]
        return pl.DataFrame({"pct_users": [(i + 1) / n for i in range(n)], "pct_total": cum, "measure": col}), gini, top10

    _frames, dist_stats = [], {}
    for _c in ["sessions", "active_hours", "cost"]:
        _df, _g, _t = _lorenz(_c)
        _frames.append(_df)
        dist_stats[_c] = {"gini": _g, "top10_share": _t}
    lorenz = pl.concat(_frames)
    lorenz_chart = (
        alt.Chart(lorenz)
        .mark_line(strokeWidth=2)
        .encode(
            x=alt.X("pct_users:Q", axis=alt.Axis(format="%"), title="engineers, lightest → heaviest"),
            y=alt.Y("pct_total:Q", axis=alt.Axis(format="%"), title="cumulative share"),
            color=alt.Color("measure:N", scale=alt.Scale(domain=["sessions", "active_hours", "cost"], range=["#3987e5", "#f5a400", "#9085e9"])),
        )
        + alt.Chart(pl.DataFrame({"x": [0, 1], "y": [0, 1]})).mark_line(strokeDash=[4, 4], color="#868686").encode(x="x", y="y")
    ).properties(height=340, width=420)
    mo.hstack([
        lorenz_chart,
        mo.md("\n".join(
            [f"Last **{window_days.value} days**, {_per_user.height} engineers:\n"]
            + [f"- **{k.replace('_', ' ')}**: top 10% of engineers = **{v['top10_share']:.0%}** (Gini {v['gini']:.2f})" for k, v in dist_stats.items()]
        )),
    ], justify="start", gap=3)
    return (dist_stats,)


@app.cell
def _(alt, con, filter_ready, window_days):
    assert filter_ready
    tiers = con.execute(f"""
    WITH pu AS (
      SELECT u, sum(n) FILTER (WHERE interactive) / ({window_days.value} / 7.0) per_week
      FROM f WHERE wk >= (SELECT max(wk) FROM f) - INTERVAL {window_days.value} DAY GROUP BY u
    )
    SELECT CASE WHEN per_week < 1 THEN '< 1' WHEN per_week < 5 THEN '1–5' WHEN per_week < 15 THEN '5–15'
                WHEN per_week < 40 THEN '15–40' ELSE '40 +' END tier,
      count(*) engineers
    FROM pu GROUP BY tier
    """).pl()
    _tiers = ["< 1", "1–5", "5–15", "15–40", "40 +"]
    alt.Chart(tiers).mark_bar(color="#3987e5", cornerRadiusEnd=4).encode(
        x=alt.X("tier:N", sort=_tiers, title="interactive sessions per week"),
        y=alt.Y("engineers:Q"),
        tooltip=[alt.Tooltip("tier:N"), alt.Tooltip("engineers:Q")],
    ).properties(height=260, width=480, title="How many sessions a week does a typical engineer run?")
    return


@app.cell(hide_code=True)
def _(mo):
    mo.md("""
    ## 8 · Extras: rhythm, tools, skills
    """)
    return


@app.cell
def _(alt, con, drop_automation):
    heat = con.execute(f"""
    SELECT dow, hour_pt, sum(n)::DOUBLE sessions FROM raw_heat
    WHERE NOT is_sub AND {'interactive' if drop_automation.value else 'TRUE'}
    GROUP BY ALL
    """).pl()
    _days = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"]
    heat_chart = (
        alt.Chart(heat)
        .transform_calculate(day=f"{_days}[datum.dow - 1]")
        .mark_rect(cornerRadius=2)
        .encode(
            x=alt.X("hour_pt:O", title="hour (Pacific)"),
            y=alt.Y("day:N", sort=_days, title=None),
            color=alt.Color("sessions:Q", scale=alt.Scale(scheme="blues"), legend=None),
            tooltip=[alt.Tooltip("day:N"), alt.Tooltip("hour_pt:O"), alt.Tooltip("sessions:Q")],
        )
        .properties(height=220, width="container", title="When interactive sessions start (2026, Pacific time)")
    )
    heat_chart
    return


@app.cell
def _(con, mo):
    mcp = con.execute("""
    SELECT CAST(date_trunc('month', week) AS DATE) AS month, split_part(tool, '__', 2) mcp_server,
      sum(sessions)::DOUBLE sessions, max(users)::DOUBLE peak_weekly_users
    FROM raw_tools WHERE tool LIKE 'mcp\\_\\_%' ESCAPE '\\' AND week >= DATE '2026-03-01'
    GROUP BY 1, 2 QUALIFY row_number() OVER (PARTITION BY month ORDER BY peak_weekly_users DESC) <= 8
    ORDER BY month DESC, peak_weekly_users DESC
    """).pl()
    orchestration = con.execute("""
    SELECT CAST(date_trunc('month', week) AS DATE) AS month,
      max(users) FILTER (WHERE tool IN ('Agent', 'Task', 'spawn_agent', 'task_v2')) peak_weekly_users_spawning,
      max(users) FILTER (WHERE tool = 'Skill') peak_weekly_users_skills,
      max(users) FILTER (WHERE tool IN ('AskUserQuestion', 'request_user_input_async')) peak_weekly_users_asking,
      max(users) FILTER (WHERE tool = 'ToolSearch') peak_weekly_users_toolsearch
    FROM raw_tools WHERE week >= DATE '2026-01-01' GROUP BY ALL ORDER BY month
    """).pl()
    top_skills = con.execute("""
    SELECT skill, sum(activations)::DOUBLE activations, max(max_weekly_users)::DOUBLE peak_weekly_users
    FROM raw_skills WHERE skill <> '(long tail)' AND week >= DATE '2026-06-01'
    GROUP BY skill ORDER BY peak_weekly_users DESC, activations DESC LIMIT 20
    """).pl()
    mo.ui.tabs({
        "Agentic tool adoption": mo.ui.table(orchestration, selection=None),
        "MCP servers": mo.ui.table(mcp, selection=None, page_size=40),
        "Skills used by ≥ 5 people": mo.ui.table(top_skills, selection=None),
    })
    return


@app.cell(hide_code=True)
def _(mo):
    mo.md("""
    ## 9 · Me vs the team

    Where one power user (the speaker) sits against everyone else. Only the speaker's own id
    is de-anonymized, via `HM_PREZO_ME`.
    """)
    return


@app.cell
def _(ME, con, filter_ready, mo, window_days):
    assert filter_ready
    me_vs_team = con.execute(f"""
    WITH pu AS (
      SELECT u,
        sum(n) FILTER (WHERE interactive) sessions,
        coalesce(sum(n) FILTER (WHERE is_sub), 0) / nullif(sum(n) FILTER (WHERE NOT is_sub), 0) subagent_ratio,
        count(DISTINCT harness) harnesses,
        sum(active_ms) / 3.6e6 active_hours,
        sum(cost_usd) AS cost
      FROM f WHERE wk >= (SELECT max(wk) FROM f) - INTERVAL {window_days.value} DAY GROUP BY u
    )
    SELECT m.metric, m.me, m.team_median, m.pct_rank FROM (
      UNPIVOT (
        SELECT
          max(sessions) FILTER (WHERE u = '{ME}') sessions_me, median(sessions) sessions_med, max(percent_rank_sessions) FILTER (WHERE u = '{ME}') sessions_pr,
          max(subagent_ratio) FILTER (WHERE u = '{ME}') subagent_ratio_me, median(subagent_ratio) subagent_ratio_med, max(percent_rank_sub) FILTER (WHERE u = '{ME}') subagent_ratio_pr,
          max(active_hours) FILTER (WHERE u = '{ME}') active_hours_me, median(active_hours) active_hours_med, max(percent_rank_hours) FILTER (WHERE u = '{ME}') active_hours_pr,
          max(harnesses) FILTER (WHERE u = '{ME}') harnesses_me, median(harnesses) harnesses_med, max(percent_rank_h) FILTER (WHERE u = '{ME}') harnesses_pr
        FROM (SELECT *,
          percent_rank() OVER (ORDER BY sessions) percent_rank_sessions,
          percent_rank() OVER (ORDER BY subagent_ratio) percent_rank_sub,
          percent_rank() OVER (ORDER BY active_hours) percent_rank_hours,
          percent_rank() OVER (ORDER BY harnesses) percent_rank_h
        FROM pu)
      ) ON (sessions_me, sessions_med, sessions_pr) AS sessions,
           (subagent_ratio_me, subagent_ratio_med, subagent_ratio_pr) AS subagent_ratio,
           (active_hours_me, active_hours_med, active_hours_pr) AS active_hours,
           (harnesses_me, harnesses_med, harnesses_pr) AS harnesses
      INTO NAME metric VALUE me, team_median, pct_rank
    ) m
    """).pl()
    mo.ui.table(me_vs_team, selection=None, format_mapping={"me": "{:.2f}", "team_median": "{:.2f}", "pct_rank": "{:.0%}"})
    return


@app.cell(hide_code=True)
def _(mo):
    mo.md("""
    ## 10 · Export for the deck

    Renders deck-themed SVGs (dark `--ink` surface, Inter/JetBrains Mono) into
    `assets/charts/charts.js`, which the slides inline via `data-chart="…"`, and the headline
    numbers into `assets/charts/stats.json`. The deck charts always use the default filters
    (2026 onward, automation excluded, 60-day window) regardless of the controls above.
    """)
    return


@app.cell
def _(mo):
    export_btn = mo.ui.run_button(label="Write deck charts")
    export_btn
    return (export_btn,)


@app.cell
def _(
    AMBER,
    DECK_DIR,
    HARNESS_COLORS,
    HARNESS_ORDER,
    INK,
    MUTE,
    PAPER,
    PROVIDER_COLORS,
    PROVIDER_ORDER,
    alt,
    con,
    cost_per_pr,
    dist_stats,
    export_btn,
    headline,
    length_spread,
    mo,
    multi,
    pr_funnel,
    sub_adoption,
    sub_then_now,
):
    import json
    from os import environ as _env

    mo.stop(not (export_btn.value or _env.get("HM_PREZO_EXPORT")))

    import vl_convert as vlc

    def _theme(chart, w=1560, h=560):
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

    _defaults = """
    CREATE OR REPLACE VIEW d AS SELECT * FROM sessions_x WHERE wk >= DATE '2026-01-05' AND NOT automation_user
    """
    con.execute(_defaults)
    _hs = con.execute("""
    SELECT wk, harness, sum(n) / sum(sum(n)) OVER (PARTITION BY wk) AS share FROM d WHERE interactive GROUP BY wk, harness
    """).pl()
    _ps = con.execute("""
    SELECT wk, provider, sum(n) / sum(sum(n)) OVER (PARTITION BY wk) AS share FROM d GROUP BY wk, provider
    """).pl()
    _mau = con.execute("""
    SELECT month, harness, count(DISTINCT u) AS users FROM d
    WHERE NOT is_sub AND harness IN ('Claude Code','Codex','Cursor') AND month < (SELECT max(month) FROM d)
    GROUP BY ALL
    """).pl()
    _full_months = sub_adoption.filter(sub_adoption["month"] < sub_adoption["month"].max())

    def _start_end(dim, where):
        return con.execute(f"""
        WITH w AS (SELECT min(wk) a, max(wk) b FROM d),
        t AS (
          SELECT CASE WHEN wk < a + INTERVAL 28 DAY THEN 'start' WHEN wk > b - INTERVAL 28 DAY THEN 'end' END AS period, {dim} AS k, sum(n) v
          FROM d, w WHERE {where} GROUP BY ALL
        )
        SELECT k, sum(v) FILTER (WHERE period = 'start') / (SELECT sum(v) FROM t WHERE period = 'start') AS start_share,
                  sum(v) FILTER (WHERE period = 'end') / (SELECT sum(v) FROM t WHERE period = 'end') AS end_share
        FROM t WHERE period IS NOT NULL GROUP BY k ORDER BY end_share DESC
        """).pl().to_dicts()

    def _stack(df, field, order, colors):
        return alt.Chart(df).mark_area(stroke=INK, strokeWidth=2).encode(
            x=alt.X("wk:T", title=None, axis=alt.Axis(format="%b", tickCount="month", grid=False)),
            y=alt.Y("share:Q", stack="normalize", axis=alt.Axis(format="%", tickCount=4), title=None),
            color=alt.Color(f"{field}:N", scale=alt.Scale(domain=order, range=colors), sort=order),
            order=alt.Order("r:Q"),
        ).transform_calculate(r=f"indexof({order}, datum.{field})")

    charts = {
        "harness-share": _theme(_stack(_hs, "harness", HARNESS_ORDER, HARNESS_COLORS), w=1000, h=520),
        "provider-share": _theme(_stack(_ps, "provider", PROVIDER_ORDER, PROVIDER_COLORS), w=1000, h=520),
        "mau-by-harness": _theme(
            alt.Chart(_mau).mark_line(strokeWidth=4, point=alt.OverlayMarkDef(size=140, filled=True, stroke=INK, strokeWidth=2)).encode(
                x=alt.X("month:T", title=None, axis=alt.Axis(format="%b", grid=False, tickCount={"interval": "month", "step": 1})),
                y=alt.Y("users:Q", title=None, axis=alt.Axis(tickCount=4)),
                color=alt.Color("harness:N", scale=alt.Scale(domain=HARNESS_ORDER[:3], range=HARNESS_COLORS[:3])),
            ),
            h=520,
        ),
        "subagent-adoption": _theme(
            alt.Chart(_full_months).mark_line(
                strokeWidth=5, color=AMBER, point=alt.OverlayMarkDef(size=180, filled=True, color=AMBER, stroke=INK, strokeWidth=3)
            ).encode(
                x=alt.X("month:T", title=None, axis=alt.Axis(format="%b", grid=False, tickCount={"interval": "month", "step": 1})),
                y=alt.Y("pct_users_with_subagents:Q", scale=alt.Scale(domain=[0, 1]), axis=alt.Axis(format="%", tickCount=4), title=None),
            ),
            w=1000, h=520,
        ),
    }
    _svgs = {k: vlc.vegalite_to_svg(v.to_json()) for k, v in charts.items()}
    _out = DECK_DIR / "assets" / "charts"
    _out.mkdir(parents=True, exist_ok=True)
    (_out / "charts.js").write_text(
        "// Generated by analysis/hivemind_team_analysis.py (section 10). Do not edit by hand.\n"
        f"window.HM_CHARTS = {json.dumps(_svgs)};\n"
        "document.querySelectorAll('[data-chart]').forEach((el) => { el.innerHTML = window.HM_CHARTS[el.dataset.chart] || ''; });\n"
    )
    _stats = {
        "headline": headline,
        "multi_harness": multi.tail(2).to_dicts(),
        "harness_share": _start_end("harness", "interactive"),
        "provider_share": _start_end("provider", "TRUE"),
        "subagents": {"then_now": sub_then_now, "by_month": sub_adoption.to_dicts()},
        "length_spread": length_spread,
        "pr": {"window": cost_per_pr, "by_month": pr_funnel.to_dicts()},
        "distribution": dist_stats,
    }
    (_out / "stats.json").write_text(json.dumps(_stats, indent=2, default=str))
    mo.md(f"Wrote {len(_svgs)} charts + stats to `{_out}`")
    return


@app.cell(hide_code=True)
def _(mo):
    mo.md("""
    ## Ask your own question

    Views: `f` (filtered, derived columns), `sessions_x` (unfiltered), `raw_cube`, `raw_heat`,
    `raw_tools`, `raw_skills`, `automation_users`.
    """)
    return


@app.cell
def _(con, filter_ready, mo):
    assert filter_ready
    _df = mo.sql(
        f"""
        SELECT harness, provider, sum(n) sessions, round(sum(cost_usd)) AS cost
        FROM f WHERE interactive GROUP BY ALL ORDER BY sessions DESC
        """,
        engine=con,
    )
    return


if __name__ == "__main__":
    app.run()
