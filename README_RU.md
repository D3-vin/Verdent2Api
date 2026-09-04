# ⚡ Verdent 2API

OpenAI и Anthropic-совместимый прокси для Verdent.ai — один Go-бинарник.

[![Version](https://img.shields.io/badge/version-1.0.1-blue)](https://github.com/D3-vin/Verdent2Api/releases)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-AS_IS-green)](#license)

[![Telegram](https://img.shields.io/badge/Telegram-@D3_vin-blue?logo=telegram)](https://t.me/D3_vin)
[![Author](https://img.shields.io/badge/Author-@D3vin_dev-blue?logo=telegram)](https://t.me/D3vin_dev)
[![GitHub](https://img.shields.io/badge/GitHub-Repository-black?logo=github)](https://github.com/D3-vin/Verdent2Api)

[Возможности](#возможности) • [Быстрый старт](#быстрый-старт) • [Использование](#использование) • [Конфигурация](#конфигурация) • [Решение проблем](#решение-проблем) • [Контакты](#контакты)

[English](README.md) | [Русский](#)

---

## Возможности

- 🚀 **Один бинарник** — HTTP-сервер, клиент Verdent и дашборд в одном Go-бинарнике; нулевые внешние зависимости
- 🔓 **Мультипротокол** — OpenAI (`/v1/chat/completions`), Anthropic (`/v1/messages`) и Responses API (`/v1/responses`)
- 🧩 **Tool calling** — полная поддержка вызова функций для интеграции с IDE; работает из коробки с текстовым контрактом
- 🔐 **Пул аккаунтов** — ротация нескольких JWT с автоматическим переключением и отслеживанием квот
- 📊 **Встроенный дашборд** — живой HTML UI для управления аккаунтами, выбора моделей и OAuth-авторизации
- 🌐 **Кросс-платформенность** — Windows, macOS, Linux
- 📦 **Автономность** — чистый Go, без CGO зависимостей

> 💡 **Работает с IDE из коробки!** Идеально подходит для AI-разработки с моделями Verdent.

---

## Быстрый старт

### 1. Скачать

Скачайте последний релиз для вашей платформы:
- [Windows (64-bit)](https://github.com/D3-vin/Verdent2Api/releases)
- [Linux (64-bit)](https://github.com/D3-vin/Verdent2Api/releases)
- [macOS Intel](https://github.com/D3-vin/Verdent2Api/releases)
- [macOS Apple Silicon](https://github.com/D3-vin/Verdent2Api/releases)

Или соберите из исходников:

```bash
git clone https://github.com/D3-vin/Verdent2Api.git
cd Verdent2Api
go build -trimpath -ldflags="-s -w" -o verdent ./cmd/server
```

### 2. Настроить

```bash
cp .env.example .env
# Отредактируйте .env — укажите VERDENT_TOKEN (JWT из приложения Verdent)
# Получите токен: DevTools → Network → заголовок authorization
```

### 3. Запустить

```bash
./verdent
```

Дашборд: http://localhost:5084

### 4. Протестировать

```bash
curl -X POST http://localhost:5084/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer change-me" \
  -d '{"model":"deepseek-v4-flash-free","messages":[{"role":"user","content":"Привет!"}],"stream":false}'
```

---

## Использование

### Потоковый режим

```bash
curl -N -X POST http://localhost:5084/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer change-me" \
  -d '{"model":"deepseek-v4-flash-free","stream":true,"messages":[{"role":"user","content":"Скажи привет"}]}'
```

### Подключение из IDE

| Настройка | Значение |
|---|---|
| Base URL | `http://localhost:5084/v1` |
| API Key | `change-me` (значение `AUTH_TOKEN` из .env) |
| Model | `deepseek-v4-flash-free`, `glm-5.3-flash-free` и др. |

> 💡 **Интеграция с IDE**: Этот API отлично работает с AI-помощниками для кодирования! Просто добавьте его как кастомный OpenAI endpoint.

### Модели

Бесплатные модели без ограничений аккаунта:
- `deepseek-v4-flash-free` — DeepSeek V4 Flash (быстрая, эффективная)
- `glm-5.3-flash-free` — GLM-5.3 Flash (сбалансированная)
- И другие через `/v1/models`

Премиум-модели требуют Verdent JWT:
- `deepseek-v4` — DeepSeek V4 (полная версия)
- `glm-4-plus` — GLM-4 Plus
- `claude-3-5-sonnet-20241022` — Claude 3.5 Sonnet
- Полный список: http://localhost:5084/ или `GET /v1/models`

### API Endpoints

| Метод | Путь | Auth | Описание |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | да | OpenAI-совместимый чат (stream/non-stream, tools) |
| `POST` | `/v1/messages` | да | Anthropic-совместимый чат (Claude Desktop) |
| `POST` | `/v1/responses` | да | Responses API (расширенный формат) |
| `GET` | `/v1/models` | да | Список доступных моделей |
| `POST` | `/prompt` | да | Устаревший: один текстовый промпт |
| `GET` | `/status` | нет | Статус сервера |

**Заголовки сессии:**

| Заголовок | Назначение |
|---|---|
| `X-Session-Id` | ID разговора (по умолчанию `default`) |
| `X-Fresh-Session` | `true` — новый разговор, пустая история |

**Дополнительные поля запроса:** `deepThink`, `search`/`webSearch`, `reasoning`.

### Tool Calling

Verdent не имеет встроенной поддержки вызова функций. Прокси реализует адаптер на основе текстового контракта:

- `tools` → переписываются в структурированный промпт → модель выдаёт JSON → парсится в `tool_calls`
- Множественные стратегии парсинга: JSON code blocks, agent markers, прямой JSON
- Поддержка стриминга: перехват вызовов инструментов в реальном времени
- Совместимость с форматами OpenAI и Anthropic

### Управление аккаунтами

Веб-дашборд по адресу http://localhost:5084/:
- Добавление/удаление JWT аккаунтов
- Просмотр использования квот по аккаунтам
- Вход через OAuth (PKCE flow)
- Выбор модели по умолчанию
- Просмотр статуса кулдаунов

---

## Конфигурация

Настройки в `.env` (автозагрузка) или переменные окружения:

| Переменная | По умолчанию | Описание |
|---|---|---|
| `VERDENT_TOKEN` | *(пусто)* | Verdent JWT (сид для первого запуска) |
| `VERDENT_TOKENS_FILE` | `verdent_tokens.txt` | Файл с несколькими аккаунтами (один JWT на строку) |
| `PORT` | `5084` | Порт сервера |
| `HOST` | `0.0.0.0` | Адрес привязки |
| `AUTH_TOKEN` | `change-me` | Bearer токен для доступа к API |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `TIMEOUT` | `300000` | Таймаут запроса (миллисекунды) |
| `VERDENT_PROXY` | *(пусто)* | HTTP прокси для трафика Verdent |
| `PERSIST_HISTORY` | `false` | Сохранять историю разговоров |
| `CLAUDE_DESKTOP` | `false` | Включить Anthropic-совместимый режим |

**Как получить `VERDENT_TOKEN`:**
1. Откройте приложение Verdent
2. DevTools (F12) → вкладка Network
3. Найдите запросы с заголовком `authorization`
4. Скопируйте JWT (начинается с `eyJ...`)
5. Добавьте в `.env`: `VERDENT_TOKEN=eyJ...`

Опциональные флаги командной строки:

```bash
./verdent  # Вся конфигурация из .env
```

---

## Решение проблем

| Симптом | Причина | Решение |
|---|---|---|
| `No accounts available` | Отсутствует или неверный JWT | Добавьте валидный `VERDENT_TOKEN` в `.env` или через дашборд |
| `401 Unauthorized` | Неверный `AUTH_TOKEN` | Проверьте, что Bearer токен совпадает с `AUTH_TOKEN` в `.env` |
| Пустой ответ | Ошибка сети/таймаут | Установите `LOG_LEVEL=debug`, проверьте логи |
| `Model not available` | Модель отсутствует в каталоге | Проверьте `/v1/models` для списка доступных моделей |
| Квота исчерпана | Все аккаунты израсходованы | Дождитесь сброса или добавьте больше аккаунтов |

---

## Структура проекта

```
verdent/
├── cmd/server/          # Точка входа (вызывает app.Main)
├── internal/app/        # Основная реализация
│   ├── verdent.go       # Клиент Verdent (AES-GCM, SSE streaming)
│   ├── verdentmeta.go   # Каталог моделей + отслеживание квот
│   ├── verdentoauth.go  # OAuth PKCE авторизация
│   ├── verdentpool.go   # Пул JWT аккаунтов + config.json
│   ├── models.go        # Каталог моделей + алиасы + кулдауны
│   ├── main.go          # HTTP сервер, OpenAI роуты
│   ├── anthropic.go     # Совместимость с Anthropic API
│   ├── responses.go     # Responses API
│   ├── tools.go         # Адаптер tool calling
│   ├── toolstream.go    # Парсер потоковых вызовов инструментов
│   └── dashboard.go     # Встроенный дашборд + API
├── .env.example         # Шаблон конфигурации
├── go.mod               # Go модуль (без внешних зависимостей)
└── BUILD.md             # Инструкции по сборке
```

---

## Контакты

- **GitHub**: https://github.com/D3-vin/Verdent2Api
- **Telegram**: [@D3_vin](https://t.me/D3_vin)
- **Автор**: [@D3vin_dev](https://t.me/D3vin_dev)

---

## Лицензия

Предоставляется как есть для образовательных целей и обеспечения совместимости. Используйте ответственно и в соответствии с условиями использования Verdent.
