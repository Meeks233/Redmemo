# Archive Control

← [Wiki index](README.md) · Related: [Natural Prefetch](Natural-Prefetch.md) · [Persistence](Persistence.md)

Archive Control is a single filter that decides **which subreddits are allowed into your archive at all**. It sits in front of every archiving path — NP background crawls and on-demand page visits alike. A post whose subreddit fails the filter is still fetched live from Reddit and shown to the viewer; it is simply **never written to the archive**.

Set it on the settings page (Archive Control → **Subs to archive**) or seed it at boot with `REDMEMO_DEFAULT_ARCHIVE_CONTROL`. An **empty value means "archive everything"** — no filter.

## Grammar

Names are separated by `+`, `-`, or whitespace. A leading `-` marks an **exclude**; anything else is an **include**. An `r/` prefix is stripped and names are lowercased.

| Form | Meaning |
|------|---------|
| `cats+dogs` | **Whitelist** — archive ONLY r/cats and r/dogs |
| `-spam-meta` | **Blacklist** — archive everything EXCEPT r/spam and r/meta |
| _(empty)_ | No filter — archive everything |

Two rules resolve ambiguous input:

1. **Any `+` wins the round.** If the string contains even one include, *every* exclude is discarded and the result is a pure whitelist. So `cats+dogs-spam` is just the whitelist `{cats, dogs}` — the `-spam` is ignored. (Whitelist and blacklist never apply at the same time.)
2. **Duplicates are dropped entirely** — not deduped. A name that appears more than once (`cats+cats`, `cats-cats`) is removed completely, yielding neither an include nor an exclude for it.

The stored value is canonicalised on save: includes first (sorted, `+`-joined), then any excludes each prefixed with `-`. The settings box echoes back exactly what the archive layer will honour.

## Examples

```
cats+dogs+aww
```
Whitelist — archive only these three subs; everything else is shown live but not stored.

```
-nsfw-politics
```
Blacklist — archive everything except r/nsfw and r/politics.

```
r/cats+r/dogs
```
Same as `cats+dogs` — the `r/` prefix is stripped automatically.

```
cats+dogs-spam
```
A `+` is present, so the `-spam` is discarded → whitelist `{cats, dogs}`.

```
cats+cats+dogs
```
r/cats appears twice → dropped entirely → whitelist `{dogs}`.

> Archive Control only governs **what is stored**, not what is crawled or browsable. To control *how* and *how deeply* subs are crawled, use the Natural Prefetch field instead — see [Natural Prefetch](Natural-Prefetch.md#driving-np-from-settings--the-prefetch-field).
