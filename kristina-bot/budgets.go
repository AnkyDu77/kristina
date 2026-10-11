package main

// Бюджеты и продолжение работы.
//
// Сколько выдачи инструментов модель видит за раз, зависит от её
// контекста (llm_context_length → KRISTINA_CONTEXT_TOKENS). Руны, а не
// токены: токенизатора у нас нет, а на русском тексте и коде у Qwen
// выходит ~3 символа на токен — берём с запасом.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
)

const runesPerToken = 3

// chatToolBudget — выдача инструментов за один ответ в чате: ~35% контекста.
func (b *bot) chatToolBudget() int {
	return max(minChatToolBudget, b.cfg.contextTokens*runesPerToken*35/100)
}

// taskBudget — порог, после которого старая выдача в ходе задачи
// сокращается: ~половина контекста (вторая — промпт, инструменты, ответ).
func (b *bot) taskBudget() int {
	return max(minTaskBudget, b.cfg.contextTokens*runesPerToken/2)
}

// ── Чат упёрся в лимит → фоновая задача ───────────────────────────────────

type continuation struct {
	Question string `json:"q"`
	Answer   string `json:"a"`
}

func contKey(chatID int64) string { return "cont:" + strconv.FormatInt(chatID, 10) }

// offerContinue — ответ в чате оборвал лимит шагов. Предлагаем доделать в
// фоне: задача стартует с просьбы и итога этого прогона, а не с нуля.
func (b *bot) offerContinue(ctx context.Context, chatID int64, question, answer string) {
	raw, _ := json.Marshal(continuation{Question: question, Answer: answer})
	if err := b.store.setKV(contKey(chatID), string(raw)); err != nil {
		log.Printf("bot: продолжение: %v", err)
		return
	}
	_, _ = b.tg.sendButtons(ctx, chatID, "За один заход не успела. Доделать фоновой задачей — с того места, где остановилась? "+
		"Она работает раундами и сама спросит, продолжать ли, если затянется.",
		[][]tgButton{{{Text: "▶️ Продолжить в фоне", Data: "cont"}}})
}

func (b *bot) onContinue(ctx context.Context, cq *tgCallback) {
	chatID := cq.Message.Chat.ID
	raw := b.store.getKV(contKey(chatID))
	var c continuation
	if raw == "" || json.Unmarshal([]byte(raw), &c) != nil {
		b.tg.answerCallback(ctx, cq.ID, "Уже неактуально")
		_ = b.tg.editText(ctx, chatID, cq.Message.MessageID, cq.Message.Text, nil)
		return
	}
	_ = b.store.setKV(contKey(chatID), "") // одна кнопка — одна задача
	b.tg.answerCallback(ctx, cq.ID, "")
	_ = b.tg.editText(ctx, chatID, cq.Message.MessageID, "▶️ Продолжаю в фоне.", nil)
	goal := fmt.Sprintf("Доделай работу, начатую в чате: в один заход она не уместилась.\n\n"+
		"Просьба владельца:\n%s\n\nЧем закончился прошлый заход (что сделано и чего не успела):\n%s\n\n"+
		"Файлы прошлого захода — в /workspace: сначала посмотри, что уже есть, потом доводи до цели и проверь результат.",
		truncRunes(c.Question, 3000), truncRunes(c.Answer, 4000))
	if _, err := b.startTask(ctx, chatID, goal); err != nil {
		_ = b.tg.sendPlain(ctx, chatID, "Ошибка: "+err.Error())
	}
}
