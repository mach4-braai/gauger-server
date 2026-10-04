# 2. Serve the dashboard with templ and Datastar

Date: 2026-10-04

## Status

Accepted

## Context

The report pages are `html/template` pages with a full reload per filter change. We want one dashboard at `/stats/` with the page structure and look of omp's stats dashboard ([can1357/oh-my-pi `packages/stats`](https://github.com/can1357/oh-my-pi/tree/main/packages/stats)), filters that update in place, hover tooltips, and later live updates. `docs/stats-page.md` has the analysis of what the pages show.

omp's dashboard is a React client built with Bun, fed by a JSON API over a SQLite rollup database. gauger-server is one Go binary with its rows already in Postgres, built by one Dockerfile with no Node toolchain.

## Decision

- **templ for pages and components.** Handlers pass Go structs to typed templ components, so renaming a field breaks the build. There is no JSON API and no generated TypeScript. The generated `*_templ.go` files are committed, so `go build` needs only Go. templ is a Go tool dependency (`go get -tool github.com/a-h/templ/cmd/templ`), and `mise run check` fails when `go tool templ generate` changes a file.
- **Datastar for interaction.** `internal/ui/static/datastar.js` is the official v1.0.4 build (MIT), `bundles/datastar.js` at the `v1.0.4` tag, vendored unchanged: 33,553 bytes raw and 13,377 bytes gzipped. Its sha256 is `727844adfc825ee651fb93c544a2a739986f9a21820a94524b35f0cac470cf91`, and a test checks the served file against it. The server uses `github.com/starfederation/datastar-go` v1.2.2.
- **Free attributes only.** `data-query-string`, `data-replace-url`, `data-persist`, `data-on-resize`, `data-match-media`, `data-animate`, `data-on-raf`, `data-scroll-into-view`, `data-custom-validity` and `data-view-transition` are [Pro](https://data-star.dev/reference/attributes#pro-attributes) and need a commercial licence. We don't use them. Where a Pro attribute would help, the server does the work instead:
  - The address bar follows a filter change through a script the server sends with the patch, `history.replaceState(...)`, in place of `data-replace-url`.
  - Charts have a fixed `viewBox` and `width: 100%`, in place of measuring with `data-on-resize`.
- **The URL is the state.** Pages live at `/stats/<page>` with filters as query parameters, and no hash routes. The filter bar is a GET form, so it works with JavaScript off. With Datastar, a change in the form or a click on an `a[data-nav]` link sends `@get` for the new URL. The server answers with patches for `#page` and `#nav` and the `replaceState` script. A full load of the same URL renders the same view. Page state beyond range, repository and event, such as hidden chart series, is more query parameters, held by inputs that join the form with `form="filters"` so a filter change keeps them.
- **Charts are SVG drawn in Go.** omp has no chart library. Its charts are its own SVG in `src/client/charts/Chart.tsx`, and that file is where the look comes from. `internal/ui/chart` ports its geometry: categorical slots, stacked or grouped bars, stacked areas, lines, a right axis, reference lines, `niceScale`, and the empty state. The server renders each slot's tooltip, and `data-show` shows it on hover. Hover state lives in signals under `_chart`, which Datastar doesn't send to the server. A legend toggle is a link with the series added to or removed from the `hide` parameter, so the server recomputes the stack.
- **Copy omp's look, not its code.** `internal/ui/static/stats.css` is omp's `src/client/styles.css` (MIT, Can Bölük and Stencil Labs, Inc.) with its notice, plus rules for our server-rendered markup. Icons are [Lucide](https://lucide.dev)'s (ISC), inlined as SVG.
- **Embedded, hashed assets.** `/static/` serves the embedded files under names that hold a hash of their content, with `Cache-Control: immutable`. Pages and assets are gzipped.
- **No Bun, no npm, one Dockerfile.** Nothing in the build needs Node or Bun, and the Dockerfile doesn't change.
- **Postgres, no rollups.** omp keeps rollups because it parses log files into SQLite first. Our rows are already in Postgres, and the aggregates measured in milliseconds at 2,700 jobs and 25,000 steps. Pages query `runs`, `jobs`, `steps` and `samples` directly with `percentile_cont`, `date_trunc` and `generate_series`. We add a rollup table only when a page is measurably slow.
- **UTC everywhere.** Buckets, hours and weekdays are UTC.
- **Setup stays on `html/template`.** `/setup`, the manifest flow and backfill don't move.

## Rejected

- **omp's React client.** It needs Bun in CI and in the Docker build, and a type boundary between Go and TypeScript that only a generator or discipline keeps in sync.
- **Observable Plot.** Its published `plot.umd.min.js` needs the full `d3.min.js`, together 477 KB raw and 160 KB gzipped, against 33 KB and 13 KB for Datastar. It also doesn't look like omp.

## Consequences

- A page is a query in `internal/store/stats_<page>.go`, a handler and template in `internal/ui/stats/<page>.go` and `<page>.templ`, one line in `routes.go` and one in `nav.go`. Every page renders its body through one function, so a live stream can re-render any page without knowing it.
- `cmd/dashboard-dev` serves the dashboard on a loopback address with `GAUGER_DATABASE_URL` and nothing else, since the real binary only serves the UI on the tailnet.
- Upgrading Datastar means replacing the file, updating its sha256 here and in the test, and checking the release notes for attribute changes.
- Chart text scales with the chart, so it gets small in a narrow card.
