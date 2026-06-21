# Search & URL Reference

← [Wiki index](README.md)

The search parser is a single **e621-style** grammar (`internal/searchquery`) shared between the live `/search` and the offline `/archive`. Free-text words become the title/body match; everything else is a `key:value` (or `key<op>value`) constraint. Tokens can appear in any order; the same query targets both back-ends.

## Token table

| Token | Aliases | Meaning | Live `/search` | Archive `/archive` |
|-------|---------|---------|----------------|--------------------|
| `sub:<a>+<b>` | `s:` `sr:` `subreddit:` | Whitelist (only these subs) | `(subreddit:a OR subreddit:b)` | `LOWER(subreddit) = ANY(...)` |
| `sub:-<a>-<b>` | — | Blacklist (exclude these subs) | `-subreddit:a -subreddit:b` | `LOWER(subreddit) != ALL(...)` |
| `rating:nsfw` / `rating:safe` | `r:` | NSFW / SFW only | `nsfw:yes` / `nsfw:no` | `over_18 = true/false` |
| `author:<user>` | `a:` `user:` | Post author | `author:<user>` | `LOWER(author) = ...` |
| `flair:"<text>"` | `f:` | Flair text | `flair_name:"<text>"` | *(ignored — not indexed)* |
| `score<op>N` | `upvote<op>N` `u<op>N` `ups<op>N` | Reddit post score threshold | *(local post-filter)* | `score <op> N` |
| `comments<op>N` | `c<op>N` | Comment count threshold | *(local post-filter)* | `(Comments)::int <op> N` |
| `type:image` | `t:` `media:` | Image / gallery posts | *(local post-filter)* | `PostType IN ('image','gallery')` |
| `type:video` | — | Real video (`is_gif=false`) | *(local post-filter)* | `PostType='video'` |
| `type:gif` | — | GIF upload (`is_gif=true`) | *(local post-filter)* | `PostType='gif'` |
| `after:YYYY-MM-DD` | `since:` | Created on/after | *(local post-filter)* | `created_utc >= date` |
| `before:YYYY-MM-DD` | `until:` | Created on/before | *(local post-filter)* | `created_utc <= date` |

Numeric `<op>` is one of `>`, `<`, `>=`, `<=`, `=` (and `:` as `=`, e.g. `score:100`). Note that `score:` here is the **Reddit post score**, the same quantity as `upvote:` / `ups:` — there is also a distinct **media cache eviction score** filter that exists only on `/archive` and `/random`.

## Examples

```
sub:rust rating:nsfw score:>1000
sub:golang+rust+selfhosted type:image after:2025-01-01
sub:-meta author:spez comments>=100
```

## `/random` endpoint

`GET /random` returns **one random archived post** matching the filters in the
`q` query parameter. It **never contacts Reddit** — it only ever surfaces what is
already stored locally — and 503s when nothing matches. There is **no separate
`/random` grammar**: `q=` takes the exact same unified search box grammar
documented above, so `?q=sub:golang ups>200 after:2025-01-01` filters by sub
scope, score and date just as the search box would.

### Response modes

| Query | Response |
|-------|----------|
| no media type pinned | a **JSON envelope** describing one random post (`url`, `subreddit`, `post_id`, `title`, `author`, `score`, `created_utc`, `nsfw`, `post_type`, `domain`, `media_done`, plus `media{}` / `gallery[]` when present). |
| `type:<kind>` pinned | **302 redirect** to the cached media resource — but only to a post whose bytes are **genuinely resident on local disk** (never a live Reddit fetch). If no resident match exists it falls back to the JSON envelope rather than 503. |
| `mode:raw` (alias `mode:instant` / `mode:ins`) | **raw bytes**: redirects to the first resident media in a fixed `video → image → gif` preference (restricted to the `type:` allow-set if one is given); when no resident media matches, writes the post body (selftext), or its title, as `text/plain`. |

### Filter modifiers (in addition to the full grammar above)

| Modifier | Example | Meaning |
|----------|---------|---------|
| `type:<kind>` | `type:image` | Only posts whose media is of `<kind>`. |
| `type:-<kind>` | `type:-gif` | Exclude `<kind>`. |
| `type:<a>+<b>` | `type:image+video` | Both kinds; combinable with excludes: `type:image+video-gif`. |
| `mode:raw` | `mode:raw` | Return raw media/body instead of a JSON envelope (see table). |
| `cache_score:<op><n>` | `cache_score:>0` | Filter by the **media cache eviction score** (resident-media-only; resolved per-post in Go). `/archive` + `/random` only. |

Supported `<kind>` tokens: `image` (alias `img`), `video` (alias `vid`), `gif`.

### Browser-friendly `&` separator

Because `net/url` splits a query string on `&`, clauses may be separated by `&`
in place of a space: `?q=sub:golang&ups>1000&type:image` is equivalent to
`?q=sub:golang ups>1000 type:image`. A literal `&` inside a value must be
percent-encoded as `%26`; `+` stays a literal `+` (a grammar joiner), so encode a
wanted space as `%20`.

### No-replacement traversal

Successive `/random` calls for the **same** filter do not repeat posts: each
distinct filter keeps its own Redis-backed cursor and walks a shuffled
permutation of the matching subset to exhaustion before reshuffling (golden-ratio
origin rotation). Redis being unavailable degrades gracefully to a fresh random
draw each call.

The downstream proxy understands a `dl_title` query parameter so a redirected
media response gets a human-friendly `Content-Disposition` filename:

```
GET /random?q=type:video
  → 302 /proxy/vreddit/<id>.mp4?dl_title=<post_title>
  → Content-Disposition: attachment; filename="post_title_vreddit_id_format.mp4"
```
