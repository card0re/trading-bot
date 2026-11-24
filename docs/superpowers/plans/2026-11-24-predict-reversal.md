# Predict Reversal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Measure the real reversal probability inside 5m/15m up/down windows and record live Binance Prediction order books, to test "buy the far side at ~$0.02 (≈50x)".

**Architecture:** Pure analysis in `internal/predict` (no network, unit-tested). `cmd/predict` loads spot 1m klines (compact cache), prints probability tables, stability split and bankroll sim; with `-rec` it joins recorder CSV against the same model. `cmd/predictrec` polls public prediction endpoints via a small client in `internal/exchange/binance/prediction.go` and appends CSV.

**Tech Stack:** Go 1.26, `github.com/adshao/go-binance/v2` (spot klines), stdlib `net/http`, `encoding/gob`, `encoding/csv`, `math`.

**Spec:** `docs/superpowers/specs/2026-11-24-predict-reversal-design.md`

## Global Constraints
- Module `trading-bot`; comments and CLI output in Russian, like the rest of the repo.
- Windows: 5 and 15 minutes, aligned to UTC (`T.Unix() % (N*60) == 0`). Target = open of the window's first 1m bar.
- σ = std of 1m log returns over the 60 bars ending at the checkpoint bar (inclusive). No look-ahead.
- z = ln(price/open) / (σ·√m), m = minutes left.
- Tie (final close == open) counts as a LOSS for the reversal bet and is counted separately.
- Fee: 2%, worst case on payout → breakeven odds K* = 1/(0.98·p).
- |z| bucket edges: 0, 0.5, 1, 1.5, 2, 2.5, 3, 3.5, 4, 5, +Inf.
- Stake $1, default odds 50 for the bankroll sim.
- Prediction API base: `https://www.binance.com/bapi/defi/v1/public/wallet-direct/prediction/`; read-only, never call trade/order endpoints.
- No commits unless the user asks.

## Review Focus
- Gaps in 1m history (exchange outages) → windows with a missing bar are skipped, not mis-labelled. Test in Task 1.
- Flat market (σ = 0, all closes equal) → checkpoint skipped, no NaN/Inf in tables. Test in Task 1.
- Price exactly at open at a checkpoint (z = 0) → not a "reversal" setup; skipped. Test in Task 1.
- API field drift / non-`000000` code → recorder logs and keeps running, writes nothing bogus. Test in Task 4 (fixture with `success:false`).
- Empty order-book side → best ask/bid written as empty, not 0 (0 would read as a 100x… bogus price). Test in Task 4.

---

### Task 1: Window/checkpoint labelling (`internal/predict`)

**Files:**
- Create: `internal/predict/samples.go`, `internal/predict/samples_test.go`

**Interfaces:**
- Produces:
  - `type Bar struct { T int64; O, C float64 }` — T = open time, unix seconds.
  - `type Sample struct { T int64; Window, MinLeft int; Z, Sigma float64; Reversal, Tie bool }` — T = checkpoint time (end of minute k).
  - `func Samples(bars []Bar, window int) []Sample`
  - `func Sigma(bars []Bar, i int) (float64, bool)` — std of log returns of the 60 bars ending at i; false if fewer than 61 contiguous bars.

- [ ] **Step 1: Write failing tests** in `samples_test.go`:
  - `TestSamplesReversal`: 60 warm-up bars with alternating ±0.1% closes, then a 5m window open=100 with closes 99, 98, 98, 98, 101 → checkpoints k=1..4 all have Z<0, `Reversal=true`, MinLeft = 4,3,2,1.
  - `TestSamplesNoReversal`: same but last close 97 → `Reversal=false`.
  - `TestSamplesTie`: last close 100 → `Reversal=false, Tie=true`.
  - `TestSamplesGapSkipsWindow`: remove the 3rd bar of the window → 0 samples for that window.
  - `TestSamplesFlatSkipped`: all 65 bars at 100 → 0 samples, no NaN.
  - `TestSamplesUnalignedSkipped`: window starting at T%300 != 0 is never used.
- [ ] **Step 2:** `go test ./internal/predict/ -run Samples` → FAIL (undefined).
- [ ] **Step 3:** Implement. Iterate window starts aligned to `window*60`; require bars at T, T+60, … contiguous (index lookup by `(T-bars[0].T)/60` only if history is gapless, else map[int64]int). Skip checkpoint if `price == open` or σ not ok or σ == 0.
- [ ] **Step 4:** `go test ./internal/predict/` → PASS.

### Task 2: Probability table, stability, bankroll (`internal/predict`)

**Files:**
- Create: `internal/predict/table.go`, `internal/predict/table_test.go`

**Interfaces:**
- Consumes: `Sample` (Task 1).
- Produces:
  - `var ZEdges = []float64{0, .5, 1, 1.5, 2, 2.5, 3, 3.5, 4, 5, math.Inf(1)}`
  - `type Cell struct { N, Rev, Ties int; GaussP float64 }` (GaussP = mean Φ(−|z|) over samples) with methods `P() float64`, `Wilson() (lo, hi float64)` (95%, z=1.96), `Breakeven() float64` (= 1/(0.98·P), +Inf if P=0).
  - `type Key struct { Window, MinLeft, Bucket int }`
  - `func Tabulate(s []Sample) map[Key]*Cell`
  - `func Bucket(z float64) int`
  - `type BankrollStats struct { RuinProb, MedianFinal float64; MedianMaxLoseStreak, P95MaxLoseStreak int }`
  - `func SimulateBankroll(p, odds float64, bank, bets, runs int, seed int64) BankrollStats` — $1 per bet, win pays `odds*0.98` total (stake included), ruin = bank < 1.

- [ ] **Step 1: Write failing tests:**
  - `TestBucket`: `Bucket(0.2)==0`, `Bucket(-2.2)==4`, `Bucket(7)==9`.
  - `TestWilson`: Cell{N:100, Rev:2} → P=0.02, lo≈0.0055, hi≈0.0700 (±0.001).
  - `TestBreakeven`: P=0.02 → 51.02 (±0.01); P=0 → +Inf.
  - `TestTabulate`: 3 samples in same key, 1 reversal, 1 tie → N=3, Rev=1, Ties=1.
  - `TestSimulateBankrollEdges`: p=1 → RuinProb 0, MedianMaxLoseStreak 0; p=0, bank 10 → RuinProb 1.
- [ ] **Step 2:** `go test ./internal/predict/ -run 'Bucket|Wilson|Breakeven|Tabulate|Bankroll'` → FAIL.
- [ ] **Step 3:** Implement. Φ via `0.5*math.Erfc(x/math.Sqrt2)`; rng `math/rand/v2` with `rand.NewPCG(uint64(seed), 0)`.
- [ ] **Step 4:** `go test ./internal/predict/` → PASS.

### Task 3: `cmd/predict` — load, cache, report

**Files:**
- Create: `cmd/predict/main.go`

**Interfaces:**
- Consumes: `exchange.LoadHistoricalSpotCandles` (`internal/exchange/binance/spot.go`), `predict.Samples`, `predict.Tabulate`, `predict.SimulateBankroll`.
- Produces: `func loadBars(ctx, client *spotapi.Client, cacheDir, symbol string, days int) ([]predict.Bar, error)` (reused by Task 5).

- [ ] **Step 1:** Flags: `-symbols BTCUSDT,ETHUSDT`, `-days 730`, `-cache <TempDir>/trading-bot-predict-cache`, `-odds 50`, `-bank 100`, `-rec ""` (Task 5). Cache = gob of `[]predict.Bar` at `<cache>/<SYM>_1m_<days>d.gob` (shortcut: cache keyed by days, not by date — refresh by deleting the file). Converting `domain.Candle` → `Bar` uses `OpenTime.Unix()`, `Open/Close.InexactFloat64()`.
- [ ] **Step 2:** Report per symbol × window: table rows MinLeft × bucket with columns `n, rev, p, CI95, gaussP, K*`; skip cells with n < 30. Then the stability block: Tabulate on samples with T before/after the 70% time split, print p_first vs p_last for cells where K* ≤ 100. Then bankroll: for the best cell with Wilson-lo ≥ 1/(0.98·odds) (if any) run `SimulateBankroll(p, odds, bank, 1000, 2000, 1)`; otherwise print «ячеек с преимуществом при кэфе X нет».
- [ ] **Step 3:** `go build ./... && go vet ./cmd/predict/` → OK.
- [ ] **Step 4:** Smoke run: `go run ./cmd/predict -symbols BTCUSDT -days 30` → tables printed, no NaN; then full `go run ./cmd/predict` and save output to `docs/superpowers/specs/predict-results-2026-11-24.txt`.

### Task 4: Prediction API client (`internal/exchange/binance/prediction.go`)

**Files:**
- Create: `internal/exchange/binance/prediction.go`, `internal/exchange/binance/prediction_test.go`, `internal/exchange/binance/testdata/prediction_event_detail.json`, `.../prediction_order_book.json`, `.../prediction_error.json`

**Interfaces:**
- Produces:
  - `type PredWindow struct { Slug string; Start, End int64; MarketID int64; UpToken, DownToken string; Status string; Winner string /* "Up"/"Down"/"" */ }`
  - `type PredBook struct { BestBid, BestAsk float64; HasBid, HasAsk bool; AskDepthTo3c float64 /* USDT on asks priced ≤ 0.03 */ }`
  - `func FetchPredWindow(ctx context.Context, hc *http.Client, slug string, startMs int64) (PredWindow, error)` → POST `web/market/event/detail` `{"eventSlug","startDate"}`.
  - `func FetchPredBook(ctx context.Context, hc *http.Client, marketID int64, tokenID string) (PredBook, error)` → POST `msite-web/order-book` `{"marketId","tokenId","vendor":"PREDICT_FUN"}`.
  - Both return an error when `code != "000000"` or `success != true`.

- [ ] **Step 1:** Capture fixtures with a throwaway `go run ./cmd/predictrec -dump` mode (Task 5 flag; write just this mode first, it prints raw JSON of both calls): current `btc-up-or-down-5m` detail + one order book → testdata. Confirm real field names for marketId, tokenId per outcome, winner, and book levels; confirm the 15m and ETH slugs exist (`btc-up-or-down-15m`, `eth-up-or-down-5m`, `eth-up-or-down-15m`). Write the error fixture by hand: `{"code":"000002","message":"illegal parameter","data":null,"success":false}`.
- [ ] **Step 2: Failing tests** (parse functions split out as `parsePredWindow([]byte)`, `parsePredBook([]byte)` so tests need no network): window fixture → non-zero MarketID, both tokens set; book fixture → BestAsk in (0,1); empty-asks book → `HasAsk=false`; error fixture → error.
- [ ] **Step 3:** `go test ./internal/exchange/binance/ -run Pred` → FAIL, implement, → PASS.

### Task 5: Recorder `cmd/predictrec`

**Files:**
- Create: `cmd/predictrec/main.go`

**Interfaces:**
- Consumes: Task 4 functions; spot last price via `spotapi.NewClient("","").NewListPricesService()`.
- Produces CSV `<out>/predict_<YYYY-MM-DD>.csv`, header
  `ts_ms,slug,window_start_ms,side,best_bid,best_ask,ask_depth_le_3c,spot,winner`;
  side ∈ `UP`/`DOWN` (book rows, winner empty) or `RESULT` (only winner set).

- [ ] **Step 1:** Flags: `-slugs btc-up-or-down-5m,btc-up-or-down-15m,eth-up-or-down-5m,eth-up-or-down-15m`, `-every 2s`, `-out ./data/predict`, `-dump` (Task 4 Step 1). Window length parsed from slug suffix (`5m`/`15m`); current start = `now.Truncate(N)` in UTC ms.
- [ ] **Step 2:** Loop: on new window fetch `PredWindow` (retry next tick on error); each tick fetch both books, write 2 rows; after window end + 30s re-fetch previous window until `Status=="RESOLVED"` and write RESULT row. Errors → `log.Printf` and continue. Ctrl+C flushes and exits.
- [ ] **Step 3:** `go vet ./cmd/predictrec/` → OK; run `go run ./cmd/predictrec -every 2s` for ~6 min → CSV has rows for ≥1 full 5m window and its RESULT row.
- [ ] **Step 4:** Add `data/` to `.gitignore`.

### Task 6: Join recorded odds with the model (`cmd/predict -rec`)

**Files:**
- Modify: `cmd/predict/main.go`
- Create: `internal/predict/join.go`, `internal/predict/join_test.go`

**Interfaces:**
- Consumes: `Bar`, `Sigma`, `Tabulate` cells, Task 5 CSV.
- Produces: `type Opp struct { T int64; Window, MinLeft int; Z, Ask, ImpliedP, ModelP, EV float64; Won bool }` and `func Opportunities(rows []RecRow, bars []Bar, table map[Key]*Cell) []Opp` where `RecRow` mirrors the CSV; an Opp is any row whose side is the far side (Up when price<open, Down when price>open), `Ask ≤ 0.05`, `ask_depth_le_3c ≥ 1`. EV = ModelP·(0.98/Ask) − 1.

- [ ] **Step 1: Failing test** `TestOpportunities`: synthetic bars + 2 rows (one far-side ask 0.02, one near-side) + table cell P=0.03 → 1 Opp, ImpliedP=0.02, EV≈0.47.
- [ ] **Step 2:** FAIL → implement → PASS.
- [ ] **Step 3:** `-rec <dir>`: load all CSVs, load bars for the covered period (`-days` covering it), print: count of Opps per window/MinLeft bucket, mean EV by model, realised win-rate from RESULT rows, and outcome agreement between spot-candle label and the platform winner (%).
- [ ] **Step 4:** `go test ./... && go build ./...` → PASS; `graphify update .`.
