# go-address-validator

An address validation API built on the [GeoNames](https://www.geonames.org/) open dataset.
It answers two questions: *is this a real place in this country?* and *does this postal code
actually cover the city that was given with it?*

Go · Gin · GORM · PostgreSQL · Redis · Docker

---

## The problem this is actually about

Address validation looks like string comparison until you try it across borders.

**Administrative hierarchies are not the same shape in every country.** Turkey has
il → ilçe. The US has state → county → city. Germany, for these purposes, has two levels.
Monaco has none at all. A schema with `province` / `district` / `city` columns fits almost
nobody: some countries leave columns empty, others need a fourth, and the column names are
wrong in every language but one.

**Turkish breaks naive string matching.** Go's `strings.ToLower` applies *simple* Unicode
case mapping, which is locale-independent, so it loses the dotless i:

```go
strings.ToLower("IĞDIR")  // => "iğdir", but Turkish requires "ığdır"
```

Input typed as `IĞDIR` then fails to match a record stored as `Iğdır`. A second hazard
comes from upstream: runtimes that apply *full* case mapping — JavaScript and Java among
them — lowercase `İ` to `i` + `U+0307 COMBINING DOT ABOVE`. That extra rune is invisible
when printed and makes equality fail silently.

Neither problem raises an error. Matches just quietly disappear.

## How it is solved

**One table, variable depth.** `locations` is self-referencing with a numeric `level`, and
a separate `admin_level_labels` table says what each level is *called* per country. The
storage shape stays constant; only the labels and the depth change.

```
("TR", 1, "il")     ("TR", 2, "ilçe")
("US", 1, "state")  ("US", 2, "county")
("MC", 1, "commune")
```

The API answers in the caller's own vocabulary instead of leaking `admin1`/`admin2`.

**A Turkish-locale normalizer.** [`internal/normalize`](internal/normalize/normalize.go)
does Turkish-aware lowercasing, folds diacritics, strips combining marks and punctuation,
and produces one canonical key. Every lookup goes through that key, never the raw name.

**Scored matching, not exact matching.** Real input is truncated and misspelled, so results
carry a confidence score rather than a boolean. See
[Scoring](#scoring-why-not-plain-edit-distance).

---

## Quick start

```bash
docker compose up -d
```

That is the whole setup. A small seed dataset (TR, US, DE, MC) loads on first boot, so the
service is queryable immediately — no 350 MB GeoNames download required to see it work.

```bash
curl -s localhost:8080/health
# {"cache":"up","database":"up","status":"ok"}
```

Get a token, then query:

```bash
curl -s -X POST localhost:8080/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"a-password-1"}'

TOKEN=$(curl -s -X POST localhost:8080/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"a-password-1"}' \
  | jq -r .access_token)

curl -s -H "Authorization: Bearer $TOKEN" \
  "localhost:8080/v1/validate-city?country=TR&city=Turgutlu" | jq
```

```json
{
  "valid": true,
  "input": "Turgutlu",
  "country": "TR",
  "match": { "id": 18, "name": "Turgutlu", "level": 2, "level_label": "ilçe", "score": 1 },
  "hierarchy": [ { "level": 1, "level_label": "il", "name": "Manisa" } ]
}
```

### The cases worth trying

```bash
# diacritics dropped by the user — still an exact match
?country=TR&city=Sisli
{"valid":true,"match":{"name":"Şişli","level_label":"ilçe","score":1},
 "hierarchy":[{"level_label":"il","name":"İstanbul"}]}

# the truncation everyone actually types
?country=TR&city=Afyon
{"valid":true,"match":{"name":"Afyonkarahisar","level_label":"il","score":0.9}}

# one typo
?country=TR&city=Ankarra
{"valid":true,"match":{"name":"Ankara","level_label":"il","score":0.86}}

# a country with no subdivisions at all
?country=MC&city=Monaco
{"valid":true,"match":{"name":"Monaco","level_label":"commune","score":1}}

# a postal code that exists, with a city that does not belong to it
?country=TR&code=45400&city=Ankara
{"valid":false,"code":"45400","places":["Turgutlu"],"admin1":"Manisa",
 "city_matches":false,
 "reason":"postal code exists but does not cover \"Ankara\" (best 0.00)"}
```

That last one is the point of the endpoint. A postal code that *exists* is not the same as
a postal code that *matches the rest of the address*, and treating them the same is how bad
addresses get accepted.

---

## API

All `/v1` routes require `Authorization: Bearer <access_token>`.

| Method | Path | Notes |
|---|---|---|
| `GET`  | `/health` | Reports database and cache status separately |
| `POST` | `/auth/register` | `{email, password}`, password min 8 chars |
| `POST` | `/auth/login` | Returns an access/refresh pair |
| `POST` | `/auth/refresh` | Rotates the refresh token |
| `POST` | `/auth/logout` | Revokes the whole token family |
| `GET`  | `/v1/validate-city` | `country`, `city`, optional `region` |
| `GET`  | `/v1/validate-zipcode` | `country`, `code`, optional `city` |
| `GET`  | `/v1/countries` | Countries with data loaded |
| `GET`  | `/v1/countries/:code/levels` | What each admin level is called |

`region` scopes the search to one parent. It is what disambiguates repeated names — Turkey
has a great many districts called *Merkez*, and the only thing separating them is the
province above.

---

## Design decisions

### Two-stage lookup

An exact key lookup runs first: one indexed equality read, which covers most real traffic
because most callers send clean input. Only on a miss does the fuzzy pass run, and it
narrows candidates with an indexed prefix scan before scoring anything in Go.

Scoring every row in the country on every request is simpler to write and unusable in
production — Turkey alone has roughly 50k populated places in GeoNames.

The prefix is 3 runes. Shorter pulls too many rows; longer misses typos near the start of
the word. A first-character typo defeats the prefix scan entirely, so there is one retry on
a 1-rune prefix rather than returning nothing.

### Scoring: why not plain edit distance

Plain Levenshtein gets truncation badly wrong. `afyon` → `afyonkarahisar` is 9 edits out of
14 characters, which scores 0.36 — well below any usable threshold, for the single most
common way Turks write that province. So the scorer special-cases prefixes: if the shorter
string (4+ runes) leads the longer one, the score floors at 0.85 and scales with coverage.

Distance is otherwise normalized by the longer length, because one typo in a 4-letter town
is a much weaker signal than one typo in a 20-letter one.

Two thresholds: at or above **0.85** a match is returned as valid, and between **0.65** and
0.85 it comes back as a suggestion instead. Returning a wrong address confidently is worse
than asking the user to confirm.

The scorer cannot separate `izmir` from `izmit` — one edit apart, different cities. That is
a real limitation, not an oversight; disambiguating needs the parent region. There is a test
pinning the behaviour so it stays visible.

### Indexes

Lookups filter country → level → key, so the composite index is in that order. An index on
`name_key` alone would force a scan across every country.

Prefix scans (`name_key LIKE 'anka%'`) cannot use a default btree index under a non-C
collation, so there is a second index with `text_pattern_ops`. Without it the fuzzy path
falls back to a sequential scan.

### Cache

Cache-aside on Redis, and deliberately non-fatal: if Redis is down the service gets slower,
not broken. A cache that can take the API down with it is worse than no cache.

Only successful validations are cached. Negative results are cheap to recompute, and caching
them lets one bad import poison answers for a full TTL after the data is fixed.

Keys are SHA-1 over the joined parts, so a long free-text place name cannot produce an
unbounded key. Responses carry `X-Cache: HIT|MISS`.

### Refresh token rotation

Each refresh invalidates the presented token and issues a new one. Rotation alone does not
say what to do when an *already-used* token appears again — that can only mean the token
leaked, and the request itself cannot tell you whether it is the attacker or the real user
replaying it.

So every token descended from one login shares a `family_id`, and a replay revokes the whole
family. The legitimate user is logged out once; the stolen token is worthless.

Tokens are stored as SHA-256 hashes, so a database leak yields nothing usable. SHA-256 rather
than bcrypt because these are 256-bit random values, not human-chosen passwords: there is
nothing to brute-force, and a slow KDF on every request buys no security here.

Refresh tokens are opaque random bytes rather than JWTs. They carry no claims, so they cannot
be read client-side, and revocation is a database fact instead of something to work around.

Login runs a bcrypt comparison even when the email does not exist, so a missing account and a
wrong password take about the same time. Otherwise response latency tells an attacker which
emails are registered.

---

## Tests

```bash
go test ./...              # unit tests
bash scripts/smoke.sh      # 31 end-to-end checks against a running stack
```

The unit tests cover normalization and scoring — where the behaviour callers notice lives.
`scripts/smoke.sh` covers the database and HTTP paths against real Postgres and Redis,
including cache hits and the token-reuse revocation flow. The query layer depends on
Postgres-specific index behaviour, so testing it against a stub would prove nothing.

```
31 passed, 0 failed
```

## Loading the real dataset

The seed is for demonstration. For real coverage, download from
[GeoNames](https://download.geonames.org/export/dump/) (CC BY 4.0):

```
data/
  admin1CodesASCII.txt
  admin2Codes.txt
  TR.txt                 # place dump for one country
  postal/TR.txt          # postal codes — separate download, different layout
```

```bash
go run ./cmd/api -import ./data -country TR
```

Rows are inserted in batches of 1000; row-at-a-time is roughly two orders of magnitude
slower on a file this size. Files that are absent are skipped rather than treated as errors.

## Layout

```
cmd/api            entrypoint, graceful shutdown, import CLI
internal/normalize Turkish-aware normalization and scoring   <- the interesting part
internal/validate  matching logic and thresholds
internal/model     schema, including the variable-depth hierarchy
internal/store     connection, migration, indexes
internal/auth      JWT issuing, refresh rotation, family revocation
internal/cache     Redis cache-aside
internal/httpapi   routes, middleware, handlers
internal/geonames  bulk importer and demo seed
scripts/smoke.sh   end-to-end checks
```

## Known limitations

- **No street-level validation.** This validates country → region → city and postal codes.
  Street and building number need a different data source.
- **`izmir` / `izmit` cannot be separated** by score alone; pass `region` to disambiguate.
- **Postal codes are stored flat.** GeoNames postal data does not line up cleanly with the
  locations tree — one code can cover several places and vice versa — so it is denormalized
  rather than joined.
- **No rate limiting.** Fine behind a gateway; it would need adding before public exposure.
- **Seed data is illustrative**, not a complete gazetteer.

## License

MIT. GeoNames data is CC BY 4.0 and must be attributed if you ship it.
