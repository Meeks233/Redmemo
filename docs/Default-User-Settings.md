# Default User Settings (`REDMEMO_DEFAULT_*`)

← [Wiki index](README.md) · Related: [Configuration](Configuration.md)

Like Redlib, every per-user setting can be given an **instance-wide default** by setting `REDMEMO_DEFAULT_<COOKIE>=<value>`. Cookie names map 1:1 from Redlib so existing deployments can rename `REDLIB_DEFAULT_*` to `REDMEMO_DEFAULT_*` (or rely on auto-translation).

**The scan is fully dynamic** — `REDMEMO_DEFAULT_<ANY_KEY>` is written to the DB on every startup with the highest-priority `env_override` source, so it **overrides whatever was previously stored** (by the user, by legacy sync, or by an earlier env value). Removing the env var causes its `env_override` row to be demoted on the next boot, letting user changes stick again.

### Managed settings: latest-writer-wins (homepage / Natural-Prefetch / archive-control)

Three keys do **not** follow the plain "env_override always wins" rule above, because the operator and the user legitimately compete to set them: the homepage feed (`FRONT_PAGE_SUBS`), the Natural-Prefetch crawl list (`PREFETCH_SUBS`), and the archive filter (`ARCHIVE_CONTROL`). For these, the env default and the user's `/settings` save are reconciled by **whoever wrote most recently**:

- The env value is a **seed**. Its first observation is timestamped as "oldest", so it only applies until the user picks their own value.
- A `/settings` change is timestamped "now" and **wins over an unchanged env seed** — so a manual setting persists across rebuilds instead of being clobbered by the compose default on every boot.
- If the operator later **changes** the compose value and rebuilds, that edit is timestamped "now" and **wins back** (it is newer than the user's last save).
- Removing the env var withdraws its vote entirely; the user's value (or, if none, the disabled/default state) stands.

Both sides' values and timestamps are kept under hidden `_user_*` / `_env_*` rows in the existing `site_settings` table (no new table; excluded from `GetAll`, the settings form, and orphan-demotion). There is no on/off toggle — a blank resolved value simply disables the homepage / NP.

## Normalization

A few keys are format-canonicalised at env-application time, so the YAML/env path and the `/settings` UI produce the same stored value:

- `PREFETCH_SUBS=golang` → stored as `sub:golang` (the `sub:` prefix is auto-added; bare lists work).
- `FRONT_PAGE_SUBS=sub:Cats+Dogs` → stored as canonical `sub:cats+dogs`.
- `VIDEO_QUALITY=garbage`, `PREFETCH_THRESHOLD=200`, `SCROLL_INTERVAL=abc`, `SETTINGS_TOKEN_TTL=999`, `AUTO_THEME_DAY=auto` and friends are **rejected at startup with a log line** rather than silently persisted — typos surface immediately instead of poisoning the DB.

## Per-user keys (inherited from Redlib)

| Name | Possible values | Default |
|------|-----------------|---------|
| `THEME` | `system`, `light`, `dark`, `black`, `dracula`, `nord`, `laserwave`, `violet`, `gold`, `rosebox`, `gruvboxdark`, `gruvboxlight`, `tokyoNight`, `icebergDark`, `doomone`, `libredditBlack`, `libredditDark`, `libredditLight` | `system` |
| `FRONT_PAGE` | `default`, `popular`, `all` | `default` |
| `LAYOUT` | `card`, `clean`, `compact` | `card` |
| `WIDE` | `on`, `off` | `off` |
| `POST_SORT` | `hot`, `new`, `top`, `rising`, `controversial` | `new` |
| `COMMENT_SORT` | `confidence`, `top`, `new`, `controversial`, `old` | `confidence` |
| `BLUR_SPOILER` | `on`, `off` | `on` |
| `SHOW_NSFW` | `on`, `off` | `on` |
| `BLUR_NSFW` | `on`, `off` | `on` |
| `AUTOPLAY_VIDEOS` | `on`, `off` | `on` |
| `SUBSCRIPTIONS` | `+`-separated list (`sub1+sub2+sub3`) | _(none)_ |
| `HIDE_AWARDS` | `on`, `off` | `off` |
| `DISABLE_VISIT_REDDIT_CONFIRMATION` | `on`, `off` | `off` |
| `HIDE_SCORE` | `on`, `off` | `off` |
| `HIDE_SIDEBAR_AND_SUMMARY` | `on`, `off` | `off` |
| `FIXED_NAVBAR` | `on`, `off` | `on` |

## Instance-only toggles (no per-user equivalent)

| Name | Possible values | Default | Description |
|------|-----------------|---------|-------------|
| `REDMEMO_DEFAULT_BANNER` | string | (empty) | Banner string for the instance info page. |
| `REDMEMO_DEFAULT_PUSHSHIFT_FRONTEND` | string | `undelete.pullpush.io` | Where "removed" links point. |
| `REDMEMO_DEFAULT_ENABLE_RSS` | `on`, `off` | `off` | Toggle RSS feeds. |
| `REDMEMO_DEFAULT_FULL_URL` | string | (empty) | Public URL — needed by RSS for absolute links. |

## RedMemo-specific defaults

All overridable, all auto-translated from `REDLIB_DEFAULT_*`.

| Name | Possible values | Default | Description |
|------|-----------------|---------|-------------|
| `REDMEMO_DEFAULT_FRONT_PAGE_SUBS` | unified search grammar | _(empty → disabled)_ | Home-page feed query, e.g. `sub:golang+rust score>10`. The query is the switch and the homepage is **disabled by default**: with no query set (env unset or a cleared /settings box), `/` redirects to `/archive`. Set `all` to render a feed of every archived post, or any query (real subreddit/term) to filter it. Pure-punctuation/whitespace input also counts as empty. |
| `REDMEMO_DEFAULT_DISABLE_INITIATIVE_UPSTREAM_ACCESS` | `on`, `off` | `on` | Block outbound Reddit calls outside the NP-scheduled budget. |
| `REDMEMO_DEFAULT_ENABLE_DEBUG` | `on`, `off` | `off` | Expose `/debug` to all visitors of this instance. |
| `REDMEMO_DEFAULT_PREFETCH_SUBS` | unified search grammar | (empty) | NP crawl list, e.g. `sub:golang+rust`. A non-empty list enables the background prefetch loops; empty leaves them idle (no separate toggle). |
| `REDMEMO_DEFAULT_PREFETCH_THRESHOLD` | `1..99` | `50` | Per-sub freshness threshold (%). |
| `REDMEMO_DEFAULT_PREFETCH_L3_MIN_COMMENTS` | `0..100000` | `0` (compose presets ship `50`) | L3 noise floor — posts with fewer comments are frozen out of bind + standalone L3. Invalid value aborts startup. |
| `REDMEMO_DEFAULT_ARCHIVE_CONTROL` | `+`/`-` sub list | (empty) | Archive whitelist/blacklist, e.g. `cats+dogs` (only those) or `-spam-meta` (everything except). Any `+` discards all `-` entries; duplicate names are dropped entirely. Empty = archive everything. Full grammar: [Archive Control](Archive-Control.md). |
| `REDMEMO_DEFAULT_SHOW_LOCAL_NSFW_SUBS` | `on`, `off` | `off` | Show NSFW subs in the local archive navigation. |
| `REDMEMO_DEFAULT_FETCH_SUB_ABOUT` | `on`, `off` | `off` | Allow background `/r/<sub>/about.json` refresh. |
| `REDMEMO_DEFAULT_LAZY_MEDIA` | `on`, `off` | `on` | Lazy-load images in feed cards. |
| `REDMEMO_DEFAULT_VIDEO_QUALITY` | `source`, `1080`, `720`, `480`, `360`, `240` | `source` | DASH rendition picked for video posts. |
| `REDMEMO_DEFAULT_MUTE_ALL_VIDEOS` | `on`, `off` | `off` | Start every video muted. |
| `REDMEMO_DEFAULT_MUTE_NSFW_VIDEOS` | `on`, `off` | `on` | Start NSFW videos muted. |
| `REDMEMO_DEFAULT_SCROLL_INTERVAL` | integer (seconds) | `2` | Auto-scroll cadence for infinite feeds. |
| `REDMEMO_DEFAULT_AUTO_THEME_DAY` | any selectable theme | `light` | Day-side theme when `THEME=system` resolves to light. |
| `REDMEMO_DEFAULT_AUTO_THEME_NIGHT` | any selectable theme | `black` | Night-side theme when `THEME=system` resolves to dark. |
| `REDMEMO_DEFAULT_SETTINGS_TOKEN_TTL` | `5`, `10`, `15`, `30`, `60` (minutes) | `10` | `/settings` auth-cookie lifetime. Capped at 60 by design. |
| `REDMEMO_DEFAULT_LONG_VIDEO_THRESHOLD` | `0..99` (minutes) | `3` | Videos longer than this render a click-to-download placeholder instead of a live `<video>`. `0` disables the gate entirely. |
| `REDMEMO_DEFAULT_SHOW_ALL_GALLERY_MEDIA` | `on`, `off` | `off` | When on, listing pages render all gallery items inline with left/right navigation instead of only the first image. |
| `REDMEMO_DEFAULT_PREFETCH_SORT` | `hot`, `new`, `top`, `rising`, `controversial` | `hot` | Global default sort for NP L1 listing fetch. |
| `REDMEMO_DEFAULT_PREFETCH_TIMEFRAME` | `hour`, `day`, `week`, `month`, `year`, `all` | `day` | Global default timeframe for NP L1 (only honoured by `top`/`controversial`). |
| `REDMEMO_DEFAULT_LANG` | supported language code | (auto) | Force a UI language; otherwise resolved from `Accept-Language`. |
