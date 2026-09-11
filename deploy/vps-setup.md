# 🚀 Руководство по выкатке Metacrawler на VPS и проксированию GPU-ресурсов (LLM + Whisper)

Это руководство описывает процесс развертывания **Metacrawler** на VPS с безопасным проксированием ресурсоемких задач (**локальные LLM** и **Whisper STT**) на домашнюю рабочую станцию без открытия входящих портов домашней сети.

---

## 🏗 Архитектура схемы

```
[ Домашний ПК (Intel Arc A770 GPU) ]
      │
      │  (Исходящее защищенное подключение к VPS)
      ▼
[ VPS Сервер ]
  ├── 127.0.0.1:8666 (Local Bind)
  │     ├── /v1/chat/completions ────▶ llama-server (через туннель)
  │     └── /v1/audio/transcriptions ─▶ whisper-cli Vulkan (через туннель)
  │
  └── Metacrawler Docker Container
        ├── LLM_BASE_URL=http://localhost:8666/v1
        └── WHISPER_URL=http://localhost:8666/v1/audio/transcriptions
```

---

## Шаг 1. Запуск туннельного сервера на VPS

На VPS необходимо запустить легковесный туннельный сервер `llmctl tunnel-server`. Он слушает внешний порт управления (по умолчанию `:8443`) и открывает локальный прокси-порт `127.0.0.1:8666`, доступный только внутри сервера.

### Вариант A: Бинарник llmctl (рекомендуемый)
Скомпилируйте `llmctl` и скопируйте его на VPS:
```bash
# Сборка на локальной машине:
cd ../llmcontrol
go build -o llmctl ./cmd/llmctl
scp llmctl user@vps_ip:/usr/local/bin/

# На VPS:
llmctl tunnel-server --listen :8443 --bind 127.0.0.1:8666 --token "МОЙ_СЕКРЕТНЫЙ_ТОКЕН"
```

Для постоянного автозапуска создайте systemd-сервис на VPS (`/etc/systemd/system/llmcontrol-tunnel.service`):
```ini
[Unit]
Description=LLMControl Reverse Tunnel Server
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/llmctl tunnel-server --listen :8443 --bind 127.0.0.1:8666 --token "МОЙ_СЕКРЕТНЫЙ_ТОКЕН"
Restart=always
RestartSec=5s

[Install]
WantedBy=multi-user.target
```
Активируйте службу:
```bash
sudo systemctl daemon-reload
sudo systemctl enable --now llmcontrol-tunnel
```

### Вариант Б: SSH Reverse Tunnel (альтернатива без доп. бинарников)
Если вы предпочитаете стандартный SSH, туннель можно поднять с домашней машины одной командой:
```bash
ssh -N -R 8666:127.0.0.1:8666 user@vps_ip
```

---

## Шаг 2. Активация туннеля через веб-интерфейс LLM Control

На домашнем компьютере запустите демон `llmcontrol`:
```bash
cd ../llmcontrol
./llmctl daemon
```
Откройте в браузере **`http://localhost:8666`**:

1. В шапке нажмите на бейдж **`☁️ VPS: Отключен`**.
2. В появившемся модальном окне введите:
   - **VPS Host**: IP или домен вашего VPS (например, `vps.my-site.com`).
   - **Control Port**: `8443`.
   - **Remote Bind Port**: `8666`.
   - **Секретный токен**: тот же токен, который был указан при запуске сервера на VPS.
3. Нажмите кнопку **«Сохранить»**.
4. На карточке любой желаемой модели (например, `gemma4`) нажмите кнопку **`☁️ VPS`**:
   - Туннель мгновенно установит исходящую связь с VPS.
   - Бейдж в шапке изменится на: **`🟢 VPS (gemma4)`**.
   - На карточке модели появится отметка **`☁️ VPS ✓`**.

### ⚡ Режим On-Demand (Wake-on-Request и автоотключение)
Нажмите на бейдж **`⚡ On-Demand`** в шапке:
- **Wake-on-Request**: когда Metacrawler на VPS отправляет запрос на анализ отзывов, `llmcontrol` автоматически запускает модель на домашней видеокарте Intel Arc A770 и обрабатывает запрос.
- **Idle Timeout**: если запросов не поступает в течение 5 минут (300 сек), модель автоматически выгружается из памяти, освобождая GPU для Whisper и других задач.

---

## Шаг 3. Настройка и запуск Metacrawler на VPS

На сервере VPS в каталоге проекта настройте файл `.env`:

```ini
PORT=8079
DB_PATH=data/metacrawler.db
CRON_SCHEDULE=0 * * * *
CRAWL_DELAY_MIN_MS=2000
CRAWL_DELAY_MAX_MS=4000

# Авторизация админ-панели (обязательны сильные значения)
ADMIN_USERNAME=admin
ADMIN_PASSWORD=REPLACE_WITH_STRONG_PASSWORD
SESSION_SECRET=REPLACE_WITH_RANDOM_SECRET_AT_LEAST_32_CHARS

# Проксирование на домашний llmcontrol через локальный туннельный сокет
LLM_BASE_URL=http://localhost:8666/v1
LLM_API_KEY=
LLM_MODEL=auto

# Whisper STT через домашний GPU
WHISPER_URL=http://localhost:8666/v1/audio/transcriptions

# Векторный поиск (встроенный чистый Go-векторизатор)
EMBEDDING_ENGINE=local
EMBEDDING_MODEL=local
```

### Запуск через Docker Compose
```bash
docker compose up --build -d
```

Проверьте логи:
```bash
docker compose logs -f metacrawler
```

---

## 🧪 Проверка работы на VPS

1. **Проверка связи с LLM через туннель**:
   ```bash
   curl -s http://127.0.0.1:8666/v1/models
   ```
   Должен вернуться список моделей из домашней базы SQLite.

2. **Проверка тестовой генерации LLM (с автоматическим Wake-on-Request)**:
   ```bash
   curl -s http://127.0.0.1:8666/v1/chat/completions \
     -H "Content-Type: application/json" \
     -d '{"model":"auto","messages":[{"role":"user","content":"Ping!"}]}'
   ```

3. **Проверка удаленной транскрипции Whisper**:
   ```bash
   curl -s http://127.0.0.1:8666/v1/audio/transcriptions \
     -F "file=@test.mp3" \
     -F "language=auto"
   ```
   Должен вернуться JSON: `{"text":"..."}`.
