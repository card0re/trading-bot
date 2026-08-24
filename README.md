# trading-bot

Multi-coin crypto futures trading bot for Binance USDT-M Futures (testnet by
default). Go, no external services beyond Binance + optional Telegram.

## ⚠️ Current status (25.08.2026)

The main directional strategy (`cmd/bot`) is **not proven profitable**. A
rigorous walk-forward validation over 3 years of history (holdout: last 6
months, split into 3 independent periods, real fees + slippage) shows a
negative result on every out-of-sample period, even with every improvement
tried (ADX trend-strength filter, funding-rate carry entry, volatility-
targeted sizing). This looks like a market-regime problem (range-bound
market), not a parameter or symbol problem — see comments in
`.env.example` and `internal/strategy/brain.go` for the full trail.

The bot keeps running on testnet to collect more data. **Do not point it at
mainnet funds based on the current backtest results.**

A second, unrelated strategy class — cross-sectional momentum rotation
(`cmd/rotationbot`) — runs alongside it as a **paper-only shadow bot** (no
real orders, virtual portfolio marked to real prices) to see if it holds up
live. See `cmd/rotation/main.go` for the rationale.

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
  backtest/     — replays internal/strategy.Breakout over history, no network trades
  tune/         — grid search + walk-forward validation for strategy params
  rotation/     — grid search + walk-forward validation for the rotation strategy

internal/
  strategy/   — Breakout state machine (the actual trading logic)
  risk/       — portfolio-level risk gate (per-trade, portfolio cap, daily loss, drawdown)
  rotation/   — shared state (Load/Save) for the rotation shadow-bot, read by app.go for /status
  backtest/   — SimExecutor (fee+slippage-aware fill simulation) and stats
  indicator/  — EMA, ATR, ADX, volume average
  exchange/binance/ — Binance REST/WebSocket wrapper, order placement, funding rate
  notify/     — Telegram notifications + inbound /status command listener
  app/        — wires everything together for cmd/bot; also serves /status for both bots
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

## Running

```bash
cp .env.example .env    # fill in testnet API keys at minimum
go run ./cmd/bot         # live/testnet directional bot
go run ./cmd/rotationbot # paper rotation shadow-bot (public endpoints only, no keys needed)
```

Backtesting / parameter search (no network trades, public market-data
endpoints only):

```bash
go run ./cmd/backtest -days 180 -mode full     # quick sanity check
go run ./cmd/tune -folds 3                     # walk-forward grid search, directional strategy
go run ./cmd/rotation -days 270 -holdout 60     # walk-forward grid search, rotation strategy
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
  notifications plus a `/status` command covering both bots.
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

Runs as two systemd services on a small GCE VM: `trading-bot` (the
directional bot) and `rotation-bot` (the shadow bot). Deploy by
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
