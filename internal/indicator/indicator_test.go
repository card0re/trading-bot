package indicator

import (
	"math/rand"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// assertClose сравнивает decimal-значения с небольшим допуском — нужен из-за
// накопления погрешности округления decimal.Div (16 знаков) на нескольких
// шагах подряд, когда ожидаемое значение посчитано вручную через дроби.
func assertClose(t *testing.T, name string, got, want decimal.Decimal, tolerance string) {
	t.Helper()
	diff := got.Sub(want).Abs()
	tol, _ := decimal.NewFromString(tolerance)
	if diff.GreaterThan(tol) {
		t.Errorf("%s: got %s, want %s (diff %s > tolerance %s)", name, got, want, diff, tolerance)
	}
}

func d(s string) decimal.Decimal {
	v, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return v
}

func TestEMA_KnownSequence(t *testing.T) {
	// period=3 => alpha=0.5; ema_0=1, затем ema_t = 0.5*price + 0.5*ema_{t-1}.
	e := NewEMA(3)
	if e.Ready() {
		t.Fatal("EMA не должна быть Ready до первого Update")
	}

	inputs := []string{"1", "2", "3", "4", "5"}
	want := []string{"1", "1.5", "2.25", "3.125", "4.0625"}

	for i, in := range inputs {
		got := e.Update(d(in))
		if !e.Ready() {
			t.Fatalf("после %d-го Update EMA должна быть Ready", i+1)
		}
		assertClose(t, "EMA", got, d(want[i]), "0.0000000001")
		assertClose(t, "EMA.Value", e.Value(), d(want[i]), "0.0000000001")
	}
}

// TestEMA_ManyIterationsStayFast ловит регресс на неограниченный рост
// точности decimal.Decimal: Mul не округляет результат, и без явного
// Round внутри EMA.Update экспонента накопленного значения росла бы
// линейно с числом баров — на десятках тысяч свечей (например, полгода
// 5-минуток) это превращает каждое обновление в операцию над числом с
// сотнями тысяч цифр. Если этот тест вдруг стал заметно медленнее — ищите
// пропавший Round.
func TestEMA_ManyIterationsStayFast(t *testing.T) {
	e := NewEMA(50)
	price := d("63451.23") // реалистичная цена с 2 знаками после запятой

	start := time.Now()
	for i := 0; i < 100_000; i++ {
		// Небольшое колебание цены, чтобы значение не было тривиально константным.
		price = price.Add(decimal.NewFromInt(int64(i%7 - 3)))
		e.Update(price)
	}
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("100 000 обновлений EMA заняли %s — похоже на неограниченный рост точности decimal", elapsed)
	}
	// Экспонента внутреннего значения не должна разъезжаться далеко за
	// roundPrecision, иначе каждое последующее Update будет всё дороже.
	if exp := -e.Value().Exponent(); exp > roundPrecision+2 {
		t.Fatalf("экспонента EMA.Value() разъехалась до %d знаков — точность не ограничивается", exp)
	}
}

func TestATR_WilderSeedAndSmooth(t *testing.T) {
	// period=3. Первые 3 бара имеют TR=2 каждый (затравка = 2).
	// 4-й бар даёт TR=4, что смещает ATR по формуле Уайлдера.
	a := NewATR(3)

	type bar struct{ h, l, c string }
	bars := []bar{
		{"10", "8", "9"},   // TR1 = 10-8 = 2 (нет prevClose)
		{"11", "9", "10"},  // TR2 = max(2, |11-9|=2, |9-9|=0) = 2
		{"12", "10", "11"}, // TR3 = max(2, |12-10|=2, |10-10|=0) = 2
		{"13", "9", "12"},  // TR4 = max(4, |13-11|=2, |9-11|=2) = 4
	}

	var got decimal.Decimal
	for i, b := range bars {
		got = a.Update(d(b.h), d(b.l), d(b.c))
		readyExpected := i >= 2 // готова после 3-го бара (period=3)
		if a.Ready() != readyExpected {
			t.Fatalf("бар %d: Ready()=%v, ожидалось %v", i+1, a.Ready(), readyExpected)
		}
	}

	// Затравка: (2+2+2)/3 = 2. Затем: 2 + (4-2)/3 = 2.6666666666666667.
	assertClose(t, "ATR после 4 баров", got, d("2.6666666666666667"), "0.0000000001")
}

func TestATR_NotReadyBeforeSeed(t *testing.T) {
	a := NewATR(14)
	for i := 0; i < 13; i++ {
		a.Update(d("10"), d("8"), d("9"))
		if a.Ready() {
			t.Fatalf("бар %d: ATR(14) не должна быть Ready раньше 14 баров", i+1)
		}
	}
	a.Update(d("10"), d("8"), d("9"))
	if !a.Ready() {
		t.Fatal("после 14-го бара ATR(14) должна быть Ready")
	}
}

func TestRollingAverage_SlidingWindow(t *testing.T) {
	r := NewRollingAverage(3)

	steps := []struct {
		in        string
		wantAvg   string
		wantReady bool
	}{
		{"10", "10", false},
		{"20", "15", false},
		{"30", "20", true},
		{"40", "30", true},                  // окно теперь [20,30,40]
		{"60", "43.3333333333333333", true}, // окно [30,40,60]
	}

	for i, s := range steps {
		got := r.Update(d(s.in))
		if r.Ready() != s.wantReady {
			t.Fatalf("шаг %d: Ready()=%v, ожидалось %v", i+1, r.Ready(), s.wantReady)
		}
		assertClose(t, "RollingAverage", got, d(s.wantAvg), "0.0000000001")
	}
}

// TestRollingAverage_MatchesNaiveRecompute сверяет инкрементальный O(1)
// расчёт с наивным пересчётом суммы по всему окну на каждом шаге —
// на случайном синтетическом ряду. Ловит ошибки дрейфа в инкрементальной
// арифметике, которые не всегда видны на маленьких ручных примерах.
func TestRollingAverage_MatchesNaiveRecompute(t *testing.T) {
	const period = 7
	rng := rand.New(rand.NewSource(42))

	r := NewRollingAverage(period)
	var history []decimal.Decimal

	for i := 0; i < 500; i++ {
		v := decimal.NewFromFloat(rng.Float64() * 1000).Round(8)
		history = append(history, v)
		got := r.Update(v)

		window := history
		if len(window) > period {
			window = window[len(window)-period:]
		}
		naiveSum := decimal.Zero
		for _, x := range window {
			naiveSum = naiveSum.Add(x)
		}
		naiveAvg := naiveSum.Div(decimal.NewFromInt(int64(len(window))))

		assertClose(t, "RollingAverage vs naive", got, naiveAvg, "0.00000001")
	}
}

func TestADX_NotReadyBeforeSeed(t *testing.T) {
	a := NewADX(3)
	for i := 0; i < 5; i++ {
		a.Update(d("100"), d("99"), d("99.5"))
		if a.Ready() {
			t.Fatalf("ADX не должна быть Ready на баре %d (period=3 требует ~2*period)", i)
		}
	}
}

func TestADX_FlatSeriesDoesNotPanic(t *testing.T) {
	// Постоянная цена без движения — TR=0, +DM=-DM=0. Проверяем, что деление
	// на ноль внутри dx() не паникует (смысловое значение тут не важно).
	a := NewADX(3)
	for i := 0; i < 20; i++ {
		a.Update(d("100"), d("100"), d("100"))
	}
	if !a.Ready() {
		t.Fatal("ADX должна стать Ready после достаточного числа баров даже на плоской серии")
	}
	if !a.Value().IsZero() {
		t.Fatalf("на полностью плоской серии ADX должен быть 0, получено %s", a.Value())
	}
}

func TestADX_TrendingHigherThanChoppy(t *testing.T) {
	trend := NewADX(14)
	price := 100.0
	for i := 0; i < 60; i++ {
		price += 1 // устойчивый рост каждую свечу
		trend.Update(decimal.NewFromFloat(price+0.5), decimal.NewFromFloat(price-0.5), decimal.NewFromFloat(price))
	}

	chop := NewADX(14)
	price = 100.0
	for i := 0; i < 60; i++ {
		if i%2 == 0 {
			price += 1
		} else {
			price -= 1
		}
		chop.Update(decimal.NewFromFloat(price+0.5), decimal.NewFromFloat(price-0.5), decimal.NewFromFloat(price))
	}

	if !trend.Ready() || !chop.Ready() {
		t.Fatal("оба индикатора должны быть Ready после 60 баров при period=14")
	}
	if !trend.Value().GreaterThan(chop.Value()) {
		t.Fatalf("ADX устойчивого тренда (%s) должен быть выше ADX боковика (%s) — иначе фильтр бесполезен",
			trend.Value(), chop.Value())
	}
}
