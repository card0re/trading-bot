package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// sentMessage — то, что тестовый сервер записал из POST .../sendMessage.
type sentMessage struct {
	chatID string
	text   string
}

// fakeTelegramServer имитирует ровно то подмножество Bot API, которое нужно
// ListenCommands: getUpdates отдаёт одну и ту же пачку обновлений один раз,
// дальше — пусто (как реальный long polling без новых сообщений);
// sendMessage складывает присланное в mu-защищённый срез для проверки.
type fakeTelegramServer struct {
	mu          sync.Mutex
	sent        []sentMessage
	served      bool // updates отданы только один раз — иначе тест зациклит команды повторно
	updatesJSON string
}

func (f *fakeTelegramServer) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/botT/getUpdates":
		f.mu.Lock()
		alreadyServed := f.served
		f.served = true
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if alreadyServed {
			time.Sleep(20 * time.Millisecond) // не даём тесту busy-spin'ить сервер
			w.Write([]byte(`{"ok":true,"result":[]}`))
			return
		}
		w.Write([]byte(f.updatesJSON))

	case r.URL.Path == "/botT/sendMessage":
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.sent = append(f.sent, sentMessage{chatID: r.Form.Get("chat_id"), text: r.Form.Get("text")})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"result":{}}`))

	default:
		http.NotFound(w, r)
	}
}

func (f *fakeTelegramServer) snapshot() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentMessage, len(f.sent))
	copy(out, f.sent)
	return out
}

// TestListenCommands_RespondsOnlyToConfiguredChat проверяет два свойства
// сразу: /help из "своего" чата получает ответ, а команда из чужого чата
// (бота может найти и написать кто угодно — уже случалось на практике)
// молча игнорируется, а не просто не отвечает, а вообще не обрабатывается.
func TestListenCommands_RespondsOnlyToConfiguredChat(t *testing.T) {
	updates := []update{
		{UpdateID: 1, Message: &struct {
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
			Text string `json:"text"`
		}{Chat: struct {
			ID int64 `json:"id"`
		}{ID: 100}, Text: "/help"}},
		{UpdateID: 2, Message: &struct {
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
			Text string `json:"text"`
		}{Chat: struct {
			ID int64 `json:"id"`
		}{ID: 999}, Text: "/help"}},
	}
	body, err := json.Marshal(struct {
		OK     bool     `json:"ok"`
		Result []update `json:"result"`
	}{OK: true, Result: updates})
	if err != nil {
		t.Fatalf("marshal updates: %v", err)
	}

	fake := &fakeTelegramServer{updatesJSON: string(body)}
	server := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer server.Close()

	tg := &Telegram{
		token:   "T",
		chatID:  "100",
		baseURL: server.URL,
		client:  server.Client(),
		poller:  server.Client(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tg.ListenCommands(ctx, func(context.Context) string { return "status" })
	}()

	deadline := time.After(2 * time.Second)
	for {
		if len(fake.snapshot()) >= 1 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("не дождались ответа на /help из разрешённого чата")
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ListenCommands не завершился после отмены контекста")
	}

	sent := fake.snapshot()
	if len(sent) != 1 {
		t.Fatalf("ожидался ровно 1 ответ (только разрешённому чату), получено %d: %+v", len(sent), sent)
	}
	if sent[0].chatID != "100" {
		t.Fatalf("ответ ушёл не в тот чат: %s", sent[0].chatID)
	}
	if sent[0].text != helpText {
		t.Fatalf("ожидался helpText, получено %q", sent[0].text)
	}
}
