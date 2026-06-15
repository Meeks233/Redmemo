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

`/random` selects a random archived post and 302-redirects to its media. It uses the same e621 grammar above — there is no separate /random grammar:

| Modifier | Example | Meaning |
|----------|---------|---------|
| `type:<kind>` | `type:image` | Include only posts whose media is of `<kind>`. |
| `type:-<kind>` | `type:-gif` | Exclude posts of `<kind>`. |
| `type:<a>+<b>` | `type:image+video` | Include both `<a>` and `<b>`. Combinable with excludes: `type:image+video-gif`. |
| `mode:raw` / `mode:instant` | `mode:raw` | Return the raw cached media (redirect) or post body as `text/plain` instead of a JSON envelope. |

Supported `<kind>` tokens: `image` (alias `img`), `video` (alias `vid`), `gif`.

The downstream proxy understands a `dl_title` query parameter so the resulting `Content-Disposition` filename is human-friendly:

```
GET /random?q=type:video
  → 302 /proxy/vreddit/<id>.mp4?dl_title=<post_title>
  → Content-Disposition: attachment; filename="post_title_vreddit_id_format.mp4"
```
