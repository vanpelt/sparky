# stateus

Determine what days of the year you spent in different U.S. states, using Google Maps location history data.

## Overview

**stateus** reads your Google Maps Timeline export (Google Takeout), extracts daily locations, and maps each day to a U.S. state. The output is a per-day breakdown of which state(s) you were in throughout the year — useful for tax residency tracking, travel logging, or curiosity.

## Design

### Inputs

- **Location history JSON** — exported from [Google Takeout](https://takeout.google.com/) under "Location History (Timeline)". The primary file is `Records.json` (or the semantic location history files).
- **Year** — the calendar year to analyze (default: current year).

### Processing

1. Parse location history records for the target year.
2. For each day, collect all recorded lat/lng coordinates.
3. Reverse-geocode coordinates to U.S. states using the `reverse_geocoder` library (offline, fast, no API key needed).
4. Assign each day to the state where the user spent the most time (or flag multi-state days).

### Outputs

- **CSV** — one row per day: `date, state, state_code, confidence`
- **Summary** — total days per state for the year.
- **Console** — a compact table printed to stdout.

## Quickstart

```bash
# Install dependencies
uv pip install -r requirements.txt

# Run
uv run stateus.py --input ~/takeout/location-history/Records.json --year 2025
```

## Requirements

- Python 3.10+
- [uv](https://docs.astral.sh/uv/)
- See `requirements.txt`
