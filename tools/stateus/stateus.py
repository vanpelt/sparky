#!/usr/bin/env python3
"""stateus — figure out which U.S. states you were in on each day of the year."""

import argparse
import json
import sys
from collections import Counter, defaultdict
from datetime import datetime, timezone
from pathlib import Path

import pandas as pd
import reverse_geocoder as rg


def load_records(path: Path, year: int) -> list[dict]:
    """Load location records from a Google Takeout Records.json file."""
    with open(path) as f:
        data = json.load(f)

    records = []
    for entry in data.get("locations", []):
        ts_us = int(entry.get("timestampMs", entry.get("timestamp", "0").rstrip("Z")))
        # Handle both millisecond timestamps and ISO strings
        if "timestamp" in entry and isinstance(entry["timestamp"], str):
            try:
                dt = datetime.fromisoformat(entry["timestamp"].replace("Z", "+00:00"))
            except ValueError:
                continue
        else:
            dt = datetime.fromtimestamp(ts_us / 1000, tz=timezone.utc)

        if dt.year != year:
            continue

        lat = entry.get("latitudeE7", 0) / 1e7
        lng = entry.get("longitudeE7", 0) / 1e7
        if lat == 0 and lng == 0:
            continue

        records.append({"date": dt.date(), "lat": lat, "lng": lng})

    return records


def geocode_to_states(records: list[dict]) -> pd.DataFrame:
    """Reverse-geocode records and return a DataFrame with date and state."""
    if not records:
        return pd.DataFrame(columns=["date", "state", "state_code"])

    coords = [(r["lat"], r["lng"]) for r in records]
    results = rg.search(coords)

    rows = []
    for record, geo in zip(records, results):
        if geo.get("cc") != "US":
            continue
        rows.append({
            "date": record["date"],
            "state": geo.get("admin1", "Unknown"),
            "state_code": geo.get("admin2", ""),
        })

    return pd.DataFrame(rows)


def summarize(df: pd.DataFrame) -> pd.DataFrame:
    """One row per day with the primary state."""
    if df.empty:
        return df

    def primary_state(group):
        counts = Counter(group["state"])
        state = counts.most_common(1)[0][0]
        total = len(group)
        top = counts.most_common(1)[0][1]
        return pd.Series({
            "state": state,
            "confidence": round(top / total, 2) if total else 0,
        })

    daily = df.groupby("date").apply(primary_state, include_groups=False).reset_index()
    return daily.sort_values("date")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", "-i", required=True, type=Path, help="Path to Records.json")
    parser.add_argument("--year", "-y", type=int, default=datetime.now().year, help="Year to analyze")
    parser.add_argument("--output", "-o", type=Path, help="Output CSV path (optional)")
    args = parser.parse_args()

    if not args.input.exists():
        print(f"Error: {args.input} not found", file=sys.stderr)
        sys.exit(1)

    print(f"Loading records for {args.year}...")
    records = load_records(args.input, args.year)
    print(f"Found {len(records)} location points.")

    if not records:
        print("No records found for this year.")
        sys.exit(0)

    print("Reverse-geocoding...")
    df = geocode_to_states(records)
    daily = summarize(df)

    # Print summary
    print(f"\n{'='*40}")
    print(f"  State residency summary for {args.year}")
    print(f"{'='*40}")
    state_days = daily["state"].value_counts()
    for state, days in state_days.items():
        print(f"  {state:<25} {days:>3} days")
    print(f"{'='*40}")
    print(f"  Total days tracked: {len(daily)}")

    if args.output:
        daily.to_csv(args.output, index=False)
        print(f"\nSaved to {args.output}")


if __name__ == "__main__":
    main()
