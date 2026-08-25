# trading-bot

Multi-coin crypto futures trading bot for Binance USDT-M Futures (testnet by
default). Go, no external services beyond Binance + optional Telegram.

## ⚠️ Current status (25.08.2026)

The main directional strategy (`cmd/bot`) is **not proven profitable**. A
rigorous walk-forward validation over 3 years of history (holdout: last 6
months, split into 3 independent periods, real fees + slippage) shows a
negative result on every out-of-sample period, even with every improvement
tried (ADX trend-strength filter, funding-rate carry entry, volatility-
targeted sizing, order-flow confirmation — see below). This looks like a
market-regime problem (range-bound market), not a parameter or symbol
problem — see comments in `.env.example` and `internal/strategy/brain.go`
for the full trail.

The bot keeps running on testnet to collect more data. **Do not point it at
mainnet funds based on the current backtest results.**

A second, unrelated strategy class — cross-sectional momentum rotation
(`cmd/rotationbot`) — runs alongside it as a **paper-only shadow bot** (no
real orders, virtual portfolio marked to real prices) to see if it holds up
live. See `cmd/rotation/main.go` for the rationale.

**Adopted** (real, walk-forward-confirmed improvement — not a fix on its
own): order-flow confirmation (`OrderFlowMinRatio` / `ORDER_FLOW_MIN_RATIO`
in `internal/strategy.Params`). Breakout entries now also require the
breakout candle's taker-buy ratio to actually confirm direction, not just
exceed average volume — a free proxy for order-book imbalance from data
Binance already provides in every candle (real L2 depth has no historical
REST endpoint on Binance at all, so it isn't backtestable the same way; see
`domain.Candle.TakerBuyVolume`). Isolated test (`cmd/tune -mode orderflow`,
same 3yr/6mo/3-fold methodology): walk-forward score improved from -5.72 to
-5.30 — same character as the ADX filter and vol-targeting: a real,
consistent improvement, still not enough to make the strategy profitable
on this holdout window by itself.

Tried and **rejected**: a regime-switching allocator (`cmd/regime`) that
would hand the whole account to whichever of Breakout/Rotation suits the
basket's current daily ADX. Walk-forward validated the same way as
everything else — it doesn't hold up out-of-sample; see the doc comment at
the top of `cmd/regime/main.go` for the numbers.

Tried and **inconclusive** (not "doesn't work" — genuinely can't tell yet):
funding-carry as its own isolated strategy (`cmd/tune -mode carry`, breakout
entry made unreachable so only the funding-rate signal can fire). It traded
plenty in-sample (2023-2025) with mixed results, but on the last 180 days —
210 (symbol × fold × threshold) cells checked — 198 had zero trades and none
had more than one. Extreme funding rate essentially stopped happening on
this basket recently; there isn't enough out-of-sample data to validate this
idea right now, positively or negatively. See the doc comment on
`buildCarryGrid` in `cmd/tune/main.go`.

Tried and **the cleanest result of the session so far — not deployed, needs
live execution first, not more history**: delta-neutral funding-rate
arbitrage / cash-and-carry (`cmd/carry`, `internal/carry`) — buy spot, short
the equal-notional perp, price risk cancels by construction, income is pure
funding. This is a genuinely different mechanism from the already-rejected
`strategy.Params.FundingCarryMinRate` (`cmd/tune -mode carry`), which was the
same entry signal but with NO hedge — a naked directional perp position with
an ATR stop. In-sample (2023-2025, ~2.5yr): consistent gains on all 7 symbols
at once (+15-22%), drawdowns 0.15-2.7% — an order of magnitude calmer than
any directional strategy tried, because price risk is hedged away by the
trade's construction rather than managed after the fact. Walk-forward on the
last 180 days: score near zero (-0.01 to -0.08), but that's *idle*, not
*losing* — the best candidates found almost no qualifying trades at all.
Funding rates on this basket have been too thin the last ~6 months to clear
the round-trip fee (~0.15% of notional, both legs, both directions) — the
same quiet-market window already found by `cmd/tune -mode carry` and
`cmd/regime`. This reads as "the trigger condition is rare right now," not
"the strategy is broken" — a different diagnosis than breakout or rotation,
which lose even when they do find trades. Not deployed with real capital for
an unrelated reason: this backtest doesn't model live execution — holding
spot + short perp needs capital on two markets at once (not just futures
margin like `cmd/bot`). **Now running as a paper shadow bot** (`cmd/carrybot`,
25.08.2026) — same reasoning as `cmd/rotationbot`: see real funding/price
data catch (or not catch) the next rate spike before risking real capital on
an execution engine that's never traded live. Shares the exact entry/exit
decision code with the backtest (`carry.ApplyEvent`) so the live version
can't quietly drift from what was walk-forward-validated. See the doc
comment at the top of `cmd/carry/main.go` for the backtest numbers and
`cmd/carrybot/main.go` for the live version.

Tried and **not enough evidence yet** — direction encouraging, sample too
thin to trust: a Fear & Greed Index filter (`cmd/tune -mode sentiment`,
`internal/exchange/sentiment`, `strategy.Params.SentimentExtremeFilter`)
blocking breakout entries at sentiment extremes (buying into extreme greed,
shorting into extreme fear). Tested on the longer 5.5-year window below
(`-days 2000 -holdout 720 -folds 6`): walk-forward score moved the right way
and monotonically with filter strength (off/weak: -2.30 → strong: -2.06,
best), then reversed once the filter got too aggressive (-4.80) — a coherent
curve, not noise. But at the winning threshold, several symbol×fold cells
have 0-2 trades — the same statistical-power problem as the funding-carry
result above, not enough observations yet to trust it over luck on these
specific 6 folds. Not deployed; code kept for when more holdout history
accumulates. See the doc comment on `buildSentimentGrid` in `cmd/tune/main.go`.

Also worth flagging: re-ran the main strategy's walk-forward on this same
longer window (5.5 years total, 2-year holdout in 6 folds, vs. the usual
3yr/6mo/3fold) specifically to test whether the 3-year window was just an
unlucky slice of history. It wasn't — best score (-5.56) landed in the same
range as the short-window result. Same re-test on Rotation, previously the
one bright spot, was more consequential: it **fails** this longer, harder
holdout (best score -15.12, wildly inconsistent fold-to-fold: +43-58% one
fold, -25% the next). Rotation's earlier promising numbers came from a
shorter, less rigorous test than the one applied to everything else here —
it should no longer be read as "the strategy that's working," even as a
paper-only shadow bot.

Tried and **rejected** — with a methodological lesson worth flagging:
pairs trading / stat-arb (`cmd/pairs`, `internal/pairs`) — z-score
mean-reversion on the log-spread between correlated coins, dollar-neutral.
A coarse walk-forward (180d holdout, 3 folds) looked genuinely promising:
the best candidate was profitable on all 3 folds. A finer one (365d
holdout, 6 folds) broke it — a real losing stretch (Oct 2025-Feb 2026,
-5% to -14% across the top candidates) had been hiding inside a single
60-day fold boundary in the coarse pass. Not deployed. Takeaway for any
future idea tested here: one fold split isn't enough to trust
"profitable on every fold" — recheck on a different split before
believing it.

## Architecture

```
cmd/
  bot/          — live/testnet directional breakout bot (real orders)
  rotationbot/  — live shadow rotation bot (virtual portfolio, no orders)
  carrybot/     — live shadow delta-neutral carry bot (virtual portfolio, no orders)
  backtest/     — replays internal/strategy.Breakout over history, no network trades
  tune/         — grid search + walk-forward validation for strategy params
  rotation/     — grid search + walk-forward validation for the rotation strategy
  carry/        — grid search + walk-forward validation for delta-neutral carry
  pairs/        — grid search + walk-forward validation for pairs trading (rejected)
  regime/       — grid search + walk-forward validation for regime-switching (rejected)

internal/
  strategy/   — Breakout state machine (the actual trading logic)
  risk/       — portfolio-level risk gate (per-trade, portfolio cap, daily loss, drawdown)
  rotation/   — shared state (Load/Save) for the rotation shadow-bot, read by app.go for /status
  carry/      — delta-neutral carry: FundingEvent/ApplyEvent (shared by backtest and cmd/carrybot) + state (Load/Save)
  backtest/   — SimExecutor (fee+slippage-aware fill simulation) and stats
  indicator/  — EMA, ATR, ADX, volume average
  exchange/binance/ — Binance REST/WebSocket wrapper, order placement, funding rate, spot candle history
  exchange/sentiment/ — Fear & Greed Index (alternative.me), untested filter
  notify/     — Telegram notifications + inbound /status command listener
  app/        — wires everything together for cmd/bot; also serves /status for all shadow bots
  config/     — env var loading and validation
```

Strategy logic (`internal/strategy.Breakout`) is a single mutex-guarded state
machine per symbol (`idle` → `opening` → `in position`). Discipline: mutate
state under the lock, release the lock before any network I/O. Multiple
`Breakout` instances (one per symbol) run independently.

## Strategy (directional bot, `cmd/bot`)

Symmetric long/short breakout: enter long on a breakout above a lookback
window with an uptrend (EMA) and above-average volume; enter short on the
mirror condition. Two independent extra entries: funding-rate carry (bet
against whichever side is paying funding) and volatility-targeted position
sizing (shrinks size, never grows it, when ATR is above its own average).
ADX gates all entries by trend strength. All parameters are tunable via env
vars — see `.env.example` for the full list and the reasoning behind each
default.

## Strategy (rotation shadow-bot, `cmd/rotationbot`)

Cross-sectional momentum: rank symbols by trailing return, long the top-K,
short the bottom-K, dollar-neutral, rebalance periodically. Bets on the
*spread* between coins rather than market direction. Deliberately kept
paper-only and separate from the main bot — running both live on the same
symbols would fight over the same exchange position book.

## Strategy (carry shadow-bot, `cmd/carrybot`)

Delta-neutral funding-rate arbitrage: buy spot, short the equal-notional
perp, price risk cancels by construction, income is pure funding rate.
Independent virtual account per symbol (matches how `cmd/carry` backtests
it — 7 independent slices, not one pooled portfolio). Shares its entry/exit
decision logic (`carry.ApplyEvent`) with the backtest directly, so the live
version can't drift from what was walk-forward-validated. Paper-only, same
reasoning as the rotation shadow-bot: real funding/price data, no real
capital, until it's actually caught a rate spike live.

## Running

```bash
cp .env.example .env    # fill in testnet API keys at minimum
go run ./cmd/bot         # live/testnet directional bot
go run ./cmd/rotationbot # paper rotation shadow-bot (public endpoints only, no keys needed)
go run ./cmd/carrybot    # paper carry shadow-bot (public endpoints only, no keys needed)
```

Backtesting / parameter search (no network trades, public market-data
endpoints only):

```bash
go run ./cmd/backtest -days 180 -mode full     # quick sanity check
go run ./cmd/tune -folds 3                     # walk-forward grid search, directional strategy
go run ./cmd/rotation -days 270 -holdout 60     # walk-forward grid search, rotation strategy
go run ./cmd/carry -days 1095 -holdout 180 -folds 3 # walk-forward grid search, delta-neutral carry
```

## Configuration

All configuration is env vars (see `.env.example` for the full annotated
list, including the walk-forward evidence behind each default). Highlights:

- `BINANCE_TESTNET` — `true` (default) uses testnet keys/endpoint, `false`
  switches to mainnet with separate `BINANCE_API_KEY`/`BINANCE_SECRET_KEY`
  vars — deliberately separate names so testnet and mainnet keys can never
  be mixed up by a stale env var.
- `SYMBOLS`, `INTERVAL` — what to trade and on what candle timeframe.
- Risk: `RISK_PER_TRADE_PCT`, `PORTFOLIO_RISK_CAP_PCT`, `DAILY_LOSS_LIMIT_PCT`,
  `MAX_DRAWDOWN_PCT` — layered limits, see comments in `.env.example` for
  why each one exists (they catch different failure modes).
- `TELEGRAM_BOT_TOKEN`/`TELEGRAM_CHAT_ID` — optional; enables trade/error
  notifications plus a `/status` command covering all bots.
- `ROTATION_STATE_PATH`/`CARRY_STATE_PATH` — optional; point `/status` at a
  shadow bot's state file if it runs on the same machine.
- `METRICS_ADDR` — optional; if set, serves Prometheus metrics (see below).

`internal/config.Load()` validates everything and fails fast on startup
with a clear error rather than trading with a nonsensical config.

## Logging

Uses `log/slog` with a text handler (`level=INFO msg="..."`), level
controlled by `LOG_LEVEL` (`debug`/`info`/`warn`/`error`, default `info`).
This buys two things: `journalctl -u trading-bot | grep level=ERROR` to
grep out real problems, and `LOG_LEVEL=warn` to cut volume at the source
when the default is too chatty. It does *not* give per-line journalctl `-p`
priority filtering — that needs journald's native protocol, which plain
stdout logging doesn't speak; text-grepping is the honest capability here.
Startup-fatal config errors still use the stdlib `log.Fatal` — nothing
downstream needs those level-filtered, they always abort the process.

## Metrics

Set `METRICS_ADDR` (e.g. `:9090`) to expose a `/metrics` endpoint in
Prometheus text format: equity, open positions, and drawdown per bot. Not
enabled by default — the bot doesn't listen on any port unless asked to.

## Deployment

Runs as systemd services on a small GCE VM: `trading-bot` (the directional
bot), `rotation-bot` and `carry-bot` (paper shadow bots). Deploy by
cross-compiling and copying the binary over:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bot ./cmd/bot
gcloud compute scp bot my-vm:/tmp/ --zone=<zone>
# on the VM: stop the service first (systemd keeps the binary open —
# `cp` over a running binary fails with "text file busy"), copy, restart.
```

A daily summary and a 15-minute health check run as cron jobs under the
service account, both notifying via Telegram.

## Testing

```bash
go build ./... && go vet ./... && go test ./... && go test -race ./...
```

CI (`.github/workflows/ci.yml`) runs this on every push/PR, plus `gofmt -l`
and `govulncheck`.

## Design notes / known tradeoffs

- Money math uses `shopspring/decimal` everywhere — never `float64` for
  prices, quantities, or PnL.
- `placeConditional` (stop/take placement) re-fetches the current position
  from the exchange via REST before placing an order, even though the
  caller already knows the side. Deliberate: the exchange position is kept
  as the single source of truth, so this stays correct across a bot
  restart. See the comment at the call site before "optimizing" it away.
- The rotation shadow-bot's state file (`rotation_state.json` by default)
  is written via write-temp-then-rename for atomicity — a crash mid-write
  can't leave a truncated/corrupt state file on disk.
