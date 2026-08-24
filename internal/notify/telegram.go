// Package notify отправляет уведомления о ключевых событиях бота (сделки,
// критические ошибки) во внешние каналы — сейчас только Telegram — и умеет
// слушать входящие команды (/status, /help) через тот же Bot API.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Telegram шлёт сообщения через Bot API. Ошибки отправки не фатальны для
// торговли — только логируются: уведомление важно, но не важнее сделки.
type Telegram struct {
	token   string
	chatID  string
	baseURL string       // https://api.telegram.org в проде; подменяется в тестах на httptest.Server
	client  *http.Client // короткие запросы (Notify)
	poller  *http.Client // long polling getUpdates — таймаут заметно больше
}

// NewTelegram создаёт отправителя. token/chatID — из @BotFather и chat_id
// целевого чата (например, свой личный чат с ботом).
func NewTelegram(token, chatID string) *Telegram {
	return &Telegram{
		token:   token,
		chatID:  chatID,
		baseURL: "https://api.telegram.org",
		client:  &http.Client{Timeout: 10 * time.Second},
		poller:  &http.Client{Timeout: 40 * time.Second}, // > long-poll timeout ниже (30с)
	}
}

func (t *Telegram) Notify(ctx context.Context, message string) {
	body := url.Values{"chat_id": {t.chatID}, "text": {message}}
	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", t.baseURL, t.token)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body.Encode()))
	if err != nil {
		log.Printf("⚠️  Telegram: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.client.Do(req)
	if err != nil {
		log.Printf("⚠️  Telegram недоступен: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("⚠️  Telegram вернул статус %d", resp.StatusCode)
	}
}

// update — минимальный набор полей ответа getUpdates, которых достаточно
// для обработки команд (не весь протокол Bot API).
type update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
		Text string `json:"text"`
	} `json:"message"`
}

const helpText = "🤖 Команды:\n" +
	"/status — текущее состояние по каждому символу и эквити счёта\n" +
	"/help — это сообщение"

// ListenCommands слушает входящие команды через long polling getUpdates и
// отвечает через Notify. Блокирует до отмены ctx — рассчитан на запуск в
// отдельной горутине. statusFn считает сводку по текущему запросу (не
// кэшируется — команда всегда должна показывать свежее состояние).
//
// Обрабатываются только сообщения из чата chatID: бота технически может
// найти и написать ему кто угодно (это уже случалось — см. историю
// настройки), обрабатывать чужие команды или отвечать в чужой чат нельзя.
func (t *Telegram) ListenCommands(ctx context.Context, statusFn func(context.Context) string) {
	var offset int64
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		updates, err := t.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return // отмена во время long-poll — не ошибка, просто выходим
			}
			log.Printf("⚠️  Telegram getUpdates: %v — повтор через 5с", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		for _, u := range updates {
			offset = u.UpdateID + 1
			if u.Message == nil {
				continue
			}
			if strconv.FormatInt(u.Message.Chat.ID, 10) != t.chatID {
				log.Printf("⚠️  Telegram: команда из чужого чата (%d) проигнорирована", u.Message.Chat.ID)
				continue
			}
			switch strings.TrimSpace(u.Message.Text) {
			case "/status":
				t.Notify(ctx, statusFn(ctx))
			case "/help", "/start":
				t.Notify(ctx, helpText)
			}
		}
	}
}

func (t *Telegram) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	// timeout=30 — long polling: сервер Telegram держит соединение открытым
	// до 30с, отдавая ответ раньше, если появилось обновление. Это резко
	// снижает частоту запросов вхолостую по сравнению с обычным пуллингом.
	endpoint := fmt.Sprintf("%s/bot%s/getUpdates?offset=%d&timeout=30", t.baseURL, t.token, offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := t.poller.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out struct {
		OK     bool     `json:"ok"`
		Result []update `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("разбор ответа getUpdates: %w", err)
	}
	if !out.OK {
		return nil, fmt.Errorf("getUpdates вернул ok=false")
	}
	return out.Result, nil
}
