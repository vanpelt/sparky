# Insta360 Cloud (Insta360+) — what we know

Goal: pick shots from footage that lives in Insta360 Cloud without pulling 8K originals to disk.

There is **no official cloud API** (the Insta360 developer program covers camera control and the
stitching Media SDK only). Everything below is the private API that Insta360 Studio uses, from
Studio's binary and caches on this Mac (2026-11-06) plus three open-source clients that use it
for personal archiving. Insta360's app agreements prohibit reverse engineering and automated
collection, so treat this as personal-use access to your own account.

## What the cloud holds per clip

| Asset | Example (6 min X6 clip) | Notes |
| --- | --- | --- |
| Original `.insv` | 8.6 GB | Dual fisheye; gyro lives in the file's trailer |
| `.lrv` proxy | 529 MB (2048×1024 HEVC, 29.97 fps) | Uploaded alongside, free of quota. Reframe already browses and analyses from LRVs |
| DASH proxy (`media.mpd`) | 3264×2176 8 Mbps / 1536×1024 2 Mbps HEVC + AAC | Stitched 360 in a 3×2 equi-angular cubemap, log colour, not stabilised. Studio streams this for cloud previews (`previewer_cache/cloud`) |

## Endpoints

Account (`openapi.insta360.com`, `openapi-g` global, `openapi-c` CN): `account/v2/checkToken`,
`account/v2/refreshToken`, `account/v1/getProfileByToken`, `account/service/getRegionalDomain`.

Media (regional host, e.g. `service-va` (US) / `service-fra` / `service-sg` / `service-hz` `.insta360.com`;
the account's own is `baseInfo.endpoint` from `cloudBenefitInfo`, see Spike results):

- `GET /cloud/service/media/view/list?pageNumber&pageSize` 
- `GET /cloud/service/media/view/searchList?lastUpdatePosition=0` (incremental cursor)
- `GET /cloud/service/media/view/detail?mediaId`
- `GET /cloud/service/media/download?mediaId` → `data.paths[] {index, name, size, url}`
- Also in Studio: `media/edit/{one,multi}`, `media/view/deleteList`, `share/*`, and a whole
  `cloud/service/export/*` family (server-side rendering: `buildExport`, `progress`, `list`).

Files: `https://cloud-media-vaf.insta360.com/cloud/<shard>/<mediaId>/o/<file>?resourceKey=…`
(CloudFront; `.insv` and `.lrv` alike). DASH: `…/<mediaId>/<yyyymmddhhmmss>/m4s/media.mpd`, segments
`init-stream<rep>.m4s`, `c-s<rep>-<nnnnn>.m4s`. Without credentials the CDN answers 401.

## Auth

Sign-in is reCAPTCHA-gated, so the user signs in themselves at
`https://cloud.insta360.com/account/studio_login/?v=2` (Studio embeds this page in CEF). The page
yields a JWT (~30 days, refreshable via `refreshToken`) sent as `Authentication` and `X-User-Token`
headers (some clients also send cookie `insta_access_token`), plus `x-insta360-platform: pc/mac`,
`app_version`, `User-Agent: insta360/studio`. List and download need no request signing; share
endpoints need an HMAC whose key isn't known.

## Spike results (2026-11-06, `reframe-cli cloud …`)

Verified against the real account (76 items, 88 GB of originals, US region):

- **Sign-in**: `reframe-cli cloud login` shows Insta360's page in a WKWebView (throwaway data
  store); the user signs in, the JWT is picked out of page storage/cookies and checked against
  `getProfileByToken`. Saved owner-only to `~/Library/Application Support/Reframe/cloud-session.json`
  (valid 30 days). The window must float (`.floating` + `orderFrontRegardless`) or it opens behind
  whatever launched it.
- **Media host**: `getRegionalDomain` answers 400; `cloud-subscription-service/user/v3/cloudBenefitInfo`
  gives `baseInfo.endpoint` = `https://service-va.insta360.com`. Other regions' hosts answer
  `403 没有权益` ("no entitlement") for this account. The same call reports quota (`total`, `usage` GB).
- **List** (`media/view/list`, 100 per page): `mediaId`, `mediaName`, `mediaType` (`pano_video`,
  photos too), `size`, `videoDuration` (ms), `shootTime`, `width/height`, `framerate`, `fileItems`,
  `thumbPath` (640×320) / `planeThumbPath` (signed CDN JPEGs), and `metadata`: the base64 .insv
  metadata protobuf (camera, serial, firmware, colour profile), so camera details need no file read.
- **Download** (`media/download`): signed `resourceKey` URLs for the original and the LRV.
  **Arbitrary byte ranges work** (206 with `Content-Range`) at ~130 Mbit/s from here.
- **Gyro without the video**: `RemoteFile` (1 MB cached blocks) feeds the existing trailer reader.
  For clip 070 (2.21 GB, cloud-only): metadata, calibration and 98,200 gyro samples in **4.8 MB,
  3 requests, 0.5 s**; `Motion` solves from it unchanged.
- **LRV**: 135.8 MB for that clip (6%), ~8 s to fetch whole.
- **DASH proxy**: `detail` includes a signed `…/m4s/media.mpd?…` URL (3264×2176 8 Mbit/s and
  1536×1024 2 Mbit/s EAC HEVC + AAC); fetchable without the API headers.

Debug commands: `cloud status`, `cloud list [--raw]`, `cloud probe <name|id>`,
`cloud get <host> <path> [k=v…]` (redacted dump), `cloud trailer <url> <size>`, `cloud logout`.

## In the app (built 2026-11-06)

- One library: cloud recordings join the open folder's on the same day sections, badged; the same
  file name locally hides the cloud entry. Covers come from `thumbPath` (needs the API headers;
  CloudFront answers 403 without them).
- Selecting one runs `CloudCache.fetch`: the original's tail from `size - (extra + 64 KB)` saved
  under the original's name (a "stub": the trailer reader counts offsets from the end, so
  `Clip(url:)` works unchanged; 5–50 MB, the trailer holds more than the gyro), then the whole LRV,
  then `cloud.json` (marks it ready). Typical: under 10 s per clip here.
- Stub-aware spots: playback uses the LRV as master (and its audio), audio analysis reads the LRV,
  the exporter refuses a stub (`ExportError.originalInCloud`), the export sheet downloads originals
  first and disables Export / Send to Studio until then. A downloaded original replaces the stub in
  place, so the clip keeps its path and shots.
- Cloud clips' shots: `ShotStore(folder: CloudCache.root)`; shots move to a local copy's path when
  the folder holds the same file name.

- **Send to Studio without originals**: a shot on a clip still in the cloud is written the way
  Studio saves a cloud clip it hasn't downloaded (sample: the user's "Cloud Test" project): clip
  `url` and `resourceId` = the media ID, `online: true`; album node/snapshot keyed by the media ID
  with `"source": "cloud"` and `create_time` (ISO 8601). Studio's own sample also had
  `stab_setting.enableDirectionalLock` (the media's `directionLock`) and stitching/LUT support flags
  that Studio presumably derives on load; we leave the template's. Not yet opened in Studio.
- Frame rates: the listing's `framerate`/`width` match the trailer (4K 120 = 1920² per lens, 8K 48 =
  3840²); 120 fps clips' LRVs are 29.97 fps, 48 fps clips' 24 fps.

- **Migration** (2026-11-06, `reframe-cli cloud migrate`): the 8 shots saved against
  `~/Downloads/Insta360 Download` now reference their cloud recordings; the cache was seeded from
  the local copies (trailer cut, LRV cloned). Reframe no longer opens a folder by default, and the
  cloud's entry wins over a local duplicate (a local copy feeds fetches and originals instead).

Not done: expired signed URLs mid-download aren't retried; nothing evicts old LRVs; the cloud
album `create_time` uses this Mac's time zone (Studio's used the camera's).

Leads: github.com/sunsided/instapi360 (Rust, most thorough), alfari16/insta360-cloud-mcp,
colorfullife123/insta360-cloud-sync.
