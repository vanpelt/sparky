# Reframe design plan

**Subject.** A pre-editor for Insta360 X6 footage. **Audience:** someone back from a trip with
hours of 360° clips who will finish in Final Cut or Resolve. **Primary job:** find the moments,
aim the camera, and hand off clean log selects.

## Principles

1. **The sphere is the canvas.** Under the viewer, a *horizon scan* shows the whole 360° horizon
   over time: x is time, y is heading. You can see where the people and the action are at every
   moment, and the camera's line of sight is drawn through it. Click anywhere on it to jump there
   *and* aim there. This is the one bold element. Everything else stays quiet.
2. **Footage stays honest.** Log is the truth. A LUT is a "Look" you hold up to preview, and it
   never leaks into exports unless you explicitly bake it.
3. **One bright line.** Color is reserved for the sightline: the look path, playhead, keyframes
   and the primary action. Clip metadata is set in plain ink. Search results follow suit: while
   a search is showing, the horizon scan's white heat is the query's instead of interest (and says
   so), rather than adding a second hue.

## Tokens

| Token | Value | Use |
| --- | --- | --- |
| Sightline | `#33D6C4` | Look path, playhead, keyframes, primary button. Optical cyan; reads against warm desert footage and grey log. |
| Sightline dim | `#33D6C4` at 35% | The look path outside the active shot. |
| Range | white at 12% | Shot in/out spans on the scan. |
| Surround | neutral `rgb(0.085)` | Behind the frame. Neutral, not tinted, so color judgement isn't biased. |
| Ink / Ink 2 | system label / secondaryLabel | All text. |
| Alert | system red | Errors only. |

**Type.** SF Pro for interface text; **SF Pro Rounded** for anything numeric and "instrument-like"
(timecode, durations, degrees, clip titles), always with tabular digits. Sentence case, with no
all-caps labels and no eyebrows.

**Layout.**

```
┌ sidebar ─────┬ toolbar: Flat|360°  Stabilize  Look ▾         Export 3 shots ┬ inspector ┐
│ dated clips  │                                                            │ Shot      │
│ 360 thumbs   │              viewer (16:9 or 2:1), HUD lower-left           │ Framing   │
│ quiet meta   │                                                            │ Stabilize │
│              ├ ▶ 00:05:37 ──────────────────────── 1:28   In  Out  ◆ Key  ┤ Clip      │
│              │ horizon scan (time × heading) + look path + shot spans     │           │
│              │ shake ribbon (gyro speed)                                  │           │
│              ├ shots: in-point frame · name · length ─────────────────────┤           │
└──────────────┴────────────────────────────────────────────────────────────┴───────────┘
```

Left-aligned throughout. The scan and its controls run full width under the viewer, aligned to
the viewer's edges.

## Review against the brief

- *Dark UI with a single accent* is close to the generated-design trope of near-black plus one
  neon. It is kept because video tools must be dark and neutral. To make it ours, the surfaces
  are system materials (vibrant sidebar, translucent bars) instead of a flat custom black, and the
  accent only ever means "where the camera looks".
- The first draft had colored badge chips for profile, fps and shake. They read as a SaaS tag
  kit and competed with the sightline. Replaced with plain secondary text and a five-bar
  shake meter, which encodes a value instead of decorating.
- Shot items were drafted as rounded cards with shadows. Replaced with a filmstrip: in-point
  frame, name, length, and no card chrome.

## Critique log

Reviewed from in-app snapshots (window, Metal view and sheet) after each pass.

- **Light mode rendered by default.** Forced dark: a footage tool needs a neutral dark surround.
- **The horizon scan was muddy.** Averaging the horizon band flattens color, so the scan gets a
  mild contrast and saturation lift. It is a navigation aid, never used for color decisions.
- **Keyframe yaw read 330° and 355°.** Yaw now wraps to ±180° everywhere it's shown.
- **Shot names truncated** ("9:41 AM sho…"). Shots are "Shot 1, 2…" inside a clip. Exports prefix
  the recording time, which also fixed a real collision between same-named shots of different clips.
- **System blue leaked in** (timecode links, sheet toggles). Links are plain text now, and the sheet
  carries the sightline tint.
- **Meta strings joined with "·".** Rewritten as plain phrases ("I-Log, 48p", "HD (1920 × 1080)").
