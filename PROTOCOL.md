# PROTOCOL.md — контракт клиент↔сервер

Это единственный источник правды про формат сообщений между фронтом (three.js)
и бэком (Go). **Если меняешь формат — меняй этот файл в том же коммите** и
обязательно отметь это как `BREAKING` в `UPDATES.md` — иначе напарник чинит
баг, которого на самом деле нет в его коде.

Код, который это реализует: `server/internal/httpapi/handlers.go` (сервер),
`client/src/net.js` + `client/src/main.js` (клиент).

## Авторизация

**[BREAKING]** Сервер теперь только на HTTPS/WSS (самоподписанный сертификат,
см. раздел "HTTPS" ниже) и требует настоящий пароль — просто ника больше не
достаточно. Два отдельных эндпоинта, оба `{ "name": "...", "password": "..." }`:

1. `POST /api/register` — создаёт новый аккаунт. 409, если имя занято.
   **[BREAKING]** Пароль должен быть ≥8 символов и содержать цифру,
   строчную и заглавную букву (иначе 400 с понятным текстом ошибки) —
   проверяется и на клиенте (мгновенная подсказка), и на сервере
   (`validatePasswordPolicy` в `handlers.go`, финальное слово за ним).
   → `{ "playerId": "...", "token": "...", "room": "hub" }`.
   Пароль хранится как bcrypt-хеш (`server/internal/store/store.go`), не в
   открытом виде.
2. `POST /api/login` — проверяет пароль существующего аккаунта (bcrypt
   compare), выдаёт **новый** токен (старый токен того же аккаунта перестаёт
   работать). 401 с одинаковым сообщением и на "нет такого имени", и на
   "неверный пароль" — чтобы по ответу нельзя было угадывать существующие
   имена. Политика пароля НЕ проверяется при входе — иначе аккаунты,
   созданные до её введения, не смогли бы залогиниться.
   → `{ "playerId": "...", "token": "...", "room": "..." }` — `room` берётся
   из БД, это последняя комната игрока (не всегда `"hub"`, если это не
   первый вход).
3. Оба эндпоинта рейт-лимитятся по IP (`server/internal/ratelimit`):
   `/api/login` — 8 попыток/30 сек, `/api/register` — 5 попыток/60 сек.
   При превышении — 429 с `{ "error": "слишком много попыток, попробуй
   снова через N сек" }`. In-memory, без внешнего стора — для LAN-демо
   этого достаточно, задача не "выдержать распределённую атаку", а не
   дать перебирать пароль вручную/скриптом за разумное время.
4. Клиент открывает `wss://<server>/ws?token=<token>`.
   Сервер проверяет токен **до** апгрейда соединения — без валидного токена
   WS не открывается (401). Это и есть требуемая по заданию защита
   "двунаправленный канал работает только после авторизации". Токен живёт
   14 дней с момента выдачи (`store.TokenTTL`) — раньше был бессрочным,
   что стало особенно важным после того, как клиент начал хранить токен в
   `localStorage` ("запомнить меня", см. ниже): так утёкший/забытый токен
   не остаётся рабочим навсегда. Каждый `login` выдаёт новый токен с новым
   сроком; истёкший токен сервер трактует так же, как несуществующий (401).

`playerId` теперь стабилен для аккаунта (генерируется один раз при
регистрации) — раньше при каждом входе создавалась новая строка в БД даже
под тем же именем, и позиция/комната фактически никогда не восстанавливались
между сессиями. Теперь восстанавливаются.

### "Запомнить меня" и выход

Клиент сохраняет `{ token, name }` в `localStorage` (`sharolet.session`,
`client/src/main.js`) после успешного `register`/`login`. При загрузке
страницы, если сессия есть, клиент сразу пробует `net.connect(token)` в обход
формы входа; если токен невалиден/истёк — тихо стирает сессию и показывает
обычный логин. `Esc` во время игры открывает паузу ("Продолжить"/"Выйти");
"Выйти" разрывает WS, стирает `localStorage`-сессию и возвращает на экран
входа — специально без сообщения "соединение потеряно" (`net.disconnect()` /
`intentionalClose` в `net.js`), это не авария, а осознанный выход.

## HTTPS

Сервер слушает только HTTPS (`http.ListenAndServeTLS`,
`server/cmd/server/tls.go`). Настоящий публичный CA-сертификат для LAN-демо
невозможен в принципе (CA валидирует владение доменом через интернет, а у
нас IP и демо офлайн) — поэтому сервер при первом запуске сам генерирует
**свой локальный удостоверяющий центр** и подписывает им серверный
сертификат (цепочка из 2 уровней, не self-signed):

- `server/ca-cert.pem` / `ca-key.pem` — корневой CA. `ca-cert.pem`
  (публичный) нужно один раз импортировать в доверенные корневые
  сертификаты на каждом клиентском ноутбуке, тогда браузер показывает
  обычный зелёный замок без предупреждений — см. `docs/https-local-ca.md`,
  там же пошаговая инструкция для Windows/macOS. `ca-key.pem` — приватный,
  никогда никуда не передаётся, живёт только на сервере.
- `server/cert.pem` / `key.pem` — серверный сертификат, подписанный этим
  CA, с SAN на `localhost`, `127.0.0.1` и все текущие локальные IPv4
  сервера (валиден для LAN-IP, каким бы он ни был на момент генерации).

Все четыре файла — в `.gitignore`, генерируются один раз и переиспользуются
между перезапусками сервера (если все четыре на месте). Без импорта
`ca-cert.pem` игра всё равно полностью работает — браузер просто один раз
спросит подтверждение ("Дополнительно" → "Перейти на сайт"), трафик
(включая пароль) в любом случае зашифрован.

`client/vite.config.js`: дев-прокси тоже ходит в Go по HTTPS (`secure: false`,
чтобы не ругаться на этот же сертификат при самом проксируемом запросе).

## Формат сообщений

Любое сообщение — JSON-объект с полем `type`. Остальные поля зависят от типа.

### Клиент → Сервер

| type            | поля                              | что делает                                                        |
|-----------------|-----------------------------------|--------------------------------------------------------------------|
| `move`          | `x, y, z` (float)                 | обновляет позицию игрока, транслируется остальным в той же комнате |
| `chunk_request` | `room` (string, id комнаты)       | игрок переходит в другую комнату, запрашивает её данные            |
| `npc_talk`      | `npcId` (string), `message` (string), `requestId` (number) | игрок обращается к NPC в открытом чате |
| `npc_conversation` | `npcId` (string), `active` (boolean) | начать/поддержать или завершить разговор |
| `pong`          | `id, fps, frameMs, memoryMB, drawCalls, triangles, paused, chatOpen, gpu` | ответ на серверный `ping` (метрики для админки) |

```json
{ "type": "move", "x": 1.2, "y": 1.6, "z": -4.0 }
{ "type": "chunk_request", "room": "room_stars" }
{ "type": "npc_conversation", "npcId": "losyash", "active": true }
{ "type": "npc_talk", "npcId": "losyash", "message": "расскажи про чёрные дыры", "requestId": 1 }
```

### Сервер → Клиент

| type             | поля                                                  | когда отправляется                              |
|------------------|--------------------------------------------------------|--------------------------------------------------|
| `welcome`        | `playerId, name, position{x,y,z}, chunk, players, state, settings` | сразу после успешного подключения по WS |
| `chunk_data`     | `chunk, players, state, position?`                     | ответ на `chunk_request`; с `position` — телепорт из админки |
| `player_joined`  | `playerId, name, x, y, z`                              | кто-то зашёл в комнату, где ты сейчас находишься |
| `player_moved`   | `playerId, x, y, z`                                    | кто-то в твоей комнате сдвинулся                 |
| `player_left`    | `playerId`                                              | кто-то вышел из твоей комнаты (или отключился)   |
| `npc_response`   | `npcId, npcName, text, requestId`                       | ответ NPC на `npc_talk`                          |
| `ping`           | `id`                                                    | каждые 2 с; клиент сразу отвечает `pong` с тем же `id` |
| `settings`       | `settings{walkSpeed, sprintSpeed}`                      | администратор изменил скорости (также внутри `welcome`) |
| `announcement`   | `text`                                                  | объявление администратора всем игрокам           |
| `error`          | `code, message, requestId?`                             | невалидный запрос; `npc_out_of_reach` / `npc_busy` — см. "Wandering NPCs", `kicked` — администратор отключил игрока |

**Ping** считает сервер: время от отправки `ping` до получения `pong` с тем
же `id`. Это RTT уровня приложения — сеть плюс задержка главного потока
браузера (если у клиента 1 FPS, ping тоже вырастет). FPS/кадр/память/draw
calls/GPU клиент сообщает сам; сервер их только обрезает до разумных пределов.
`pong` обрабатывается в цикле чтения сокета, а ответы ИИ — нет (см. ниже),
поэтому пока NPC «думает», ping не растёт.

`players` — массив `{playerId, name, x, y, z}`, те, кто **уже стоит** в
комнате на момент входа (себя сервер исключает). Нужен отдельно от
`player_joined`/`player_moved`: те присылаются только за события ПОСЛЕ
твоего подключения — без `players` ты никогда не увидел бы того, кто
зашёл в комнату раньше тебя.

Клиент показывает число игроков по этому ростеру и событиям входа/выхода.
При занятой точке появления он выбирает свободную рядом и отправляет её
обычным `move`. HTTP/WS используют origin страницы; Vite dev/preview
проксируют `/api` и `/ws` в Go на `127.0.0.1:8080`. Формат сообщений прежний.

`chunk` — это **весь объём данных, который клиент имеет право кешировать**:

```json
{
  "room": {
    "id": "room_stars", "name": "Обсерватория",
    "position": {"x": 0, "y": 0, "z": -30}, "size": {"w": 12, "h": 6, "d": 12},
    "color": "#1e2a5e", "npcId": "losyash", "connectsTo": ["hub"]
  },
  "npcs": [
    { "id": "losyash", "name": "Лосяш", "title": "астрофизик станции", "position": {"x": 0, "y": 1, "z": -30} }
  ]
}
```

`room` может нести необязательное поле `model` (URL `.glb`-сцены, отдаётся
клиентской статикой из `client/public/`). Если оно есть — клиент грузит эту
модель вместо процедурной коробки и вписывает её в комнату автоматически
(центрирует и масштабирует по bounding box, см. `client/src/world.js`).
Если поля нет — рендер как раньше, короб + пол. `size`/`color` для такой
комнаты в `rooms.json` по-прежнему нужны (используются для заглушки на
время загрузки модели **и** для коллизии — см. ниже).

У `room` также могут быть необязательные поля:

- `modelScale` — явный масштаб GLB с сохранением авторского начала координат (вместо автоматической подгонки).
- `radius` — радиус внешней круглой коллизии, в том числе для комнаты с моделью.
- `collision` — необязательное `"walkmesh"`: клиент строит коллизию из геометрии текущей комнаты (пол, ступени, галерея, препятствия и NPC). Без поля остаётся внешний коллайдер.
- `spawn` — локальная позиция появления относительно `room.position`, `y` — высота камеры.
- `doors` — массив `{room, position{x,y,z}, halfWidth, modelDoor?}`: id назначения,
  локальный центр дверного проёма, полуширина триггера и необязательный `destination`
  из extras узла GLB. Переход происходит при пересечении этого проёма; обратный
  вход выбирается по двери, ведущей в предыдущую комнату. `connectsTo` перечисляет
  те же назначения. Триггер учитывает `door.position.y`: высота ног должна быть
  близка к полу проёма, поэтому верхняя галерея не включает нижние двери.
  Формат `chunk_request` не меняется.

`neighbors` — список соседних комнат (`id, name, position, size`, без NPC и
фактов), по одному на каждый id из `room.connectsTo`. Это не для рендера,
а для клиентской физики. При наличии `doors` используются явные проёмы модели;
для старых комнат без `doors` проход определяется направлением на соседа.
Клиент шлёт `chunk_request` сразу при пересечении проёма.

Важно: `chunk` **не содержит** базу фактов NPC — ни одного текста факта.
Факты раздаются по одному, только через `npc_talk` → `npc_response`, и
сервер помнит (в SQLite), что конкретному игроку уже рассказал конкретный
NPC. В памяти и сцене только текущая комната с её NPC. При переходе клиент
отменяет загрузки старого чанка, освобождает ресурсы и дожидается незавершённого
GLB parse, прежде чем начать загрузку следующего. Кеша посещённых моделей нет;
fetch GLB выполняется с `cache: "no-store"`. Небо постоянно и не привязано к чанку.
Одна из десяти комнат не гарантирует ≤10% по байтам: модели разного размера.

## Карта станции

`hub` использует `starling-lobby-9-doors.glb`. Все девять подписанных дверей
ведут в отсеки; у каждого есть обратный вход в lobby:
Medbay → `room_medbay`, Mechanical Bay → `room_rockets`, Bio Lab → `room_greenhouse`,
Chem Lab → `room_chemistry`, Radio Room → `room_radio`, Planetarium → `room_stars`,
Library → `room_library`, Spacesuit Room → `room_gravity`, Cafeteria → `room_dining`.
Room id сохранены. В каждой комнате один NPC; повторяющиеся персонажи имеют
разные id (`losyash_chemistry`, `sovunya_dining`). `factBank` остаётся серверным
полем; выбор факта использует его, а не id экземпляра. Память индивидуальна
для пары игрок/экземпляр NPC. Все NPC, включая `yozhik`, загружаются из GLB через поле `model`. Камера может подняться по лестнице lobby на галерею;
её высота синхронизируется уже существующим полем `move.y`.

## Астронавты игроков (ветка character)

`move` и `player_moved` сохраняют формат x/y/z. Клиент интерполирует позиции
других игроков, вычисляет направление/скорость и анимирует скелет астронавта.
Отдельные флаги walking/running и yaw не передаются. Shift увеличивает
локальную скорость с 4 до 7 м/с; сервер пересылает полученные координаты как прежде.
Аватар размещает ноги на y - 1.6 (сетевой y — высота глаз), над шлемом остаётся имя.

## Интерактивные двери

По каждому room.doors клиент создаёт закрытую раздвижную дверь. E открывает/
закрывает её; chunk_request отправляется при пересечении только после полного
открытия. При уходе сервер закрывает двери для всех после освобождения проёма; новый
клиент получает актуальное состояние при входе.
Состояние общее на сервере через door_set/room_state (см. ниже). Дверь рядом имеет приоритет E перед NPC-чатом.

За каждым проёмом клиент создаёт коридор глубиной 3 м в составе текущего
чанка. При открытой двери chunk_request отправляется в центре коридора, на глубине 1.5 м,
перед торцевой стеной; GLB соседней комнаты до перехода не загружается.
Размер створок увеличен с перекрытием рамы, контракт room.doors прежний.

## Shared facility state (server authoritative)

[BREAKING] Updated client and server must be deployed together.
Client -> server: door_set {room: current room ID, door: destination room ID, open: boolean}.
Server -> client: room_state {state:{room, revision, doors:[{room,open}], roombas:[{id,x,y,z,heading}], npcs:[{id,x,y,z,heading,talkingTo}]}}.
welcome and chunk_data include state:{room,revision,doors,roombas,npcs}.
Only current-room actors are sent. Revisions increase monotonically within a server run;
clients ignore stale snapshots and retain only the snapshot for the room being entered.
Doors are shared by the two connected rooms. Server validates proximity and rejects
closing on any player. A successful room transition requests closure of the old room's
doors; occupied openings close once clear. Disconnect requests the same closure.
chunk_request requires a connected open door and a player at the corridor center.
Roombas are simulated at 10 Hz by the server on world-data/roomba-nav.json, prepared
offline from GLB floor/obstacle geometry. Clients interpolate poses, with no local RNG.
No room geometry/navigation grid is sent to clients. Door and cleaner state is in memory
and resets on server restart.

### Wandering NPCs and conversations

`room_state`, `welcome.state` and `chunk_data.state` also contain
`npcs:[{id,x,y,z,heading,talkingTo}]`. Coordinates are the NPC holder/body
anchor (feet + 1 m); heading 0 faces world +Z, increasing toward +X.
NPC movement, random pauses and conversation facing are server-owned at 10 Hz.
Only the current room's poses are sent; the NPC navigation graph stays on Go.

Client -> server: `npc_conversation {npcId,active:boolean}`. Send active=true
when opening chat and every 5 seconds while open, active=false when closing.
The server checks room, floor and distance (3.4 m), releases sessions on room
change/disconnect/timeout (15 s), and keeps an NPC still while any valid
conversation is active. If multiple players talk to one NPC, it faces its
current interlocutor until that conversation ends, then another active player.
`talkingTo` is that player's ID, or an empty string while wandering.
`npc_talk` requires the same room/proximity and an active conversation.
AI replies run separately from the socket read loop (one pending request per
connection), allowing chat closure/heartbeats/movement during generation.
Response delivery is restricted to the originating live connection.
`npc_talk` includes a numeric `requestId`, echoed in `npc_response` and
request errors; the client ignores replies from a closed/replaced chat.
Restart Go and refresh all clients together for NPC movement support.

## Восстановление входа и повторные сессии

Невалидная сохранённая room восстанавливается сервером в hub до welcome.
Для одного playerId допускается одна активная WS-сессия; новая получает
error {code:"session_in_use",message} и закрывается. Клиент ждёт welcome с
непустой chunk.room перед входом в игру; ошибки до welcome показываются на
форме входа, таймаут ожидания — 15 секунд.

## Админ-панель (`/admin.html`, REST `/api/admin/*`)

Отдельная страница, не часть 3D-клиента. Доступ — только аккаунтам с ролью
администратора: колонка `players.is_admin` в SQLite. Роль выдаётся
переменной окружения сервера `ADMIN_USERS=имя1,имя2` (при старте — уже
существующим аккаунтам, остальным — в момент регистрации).

`POST /api/admin/login {name, password}` → `{token, name}` (403, если у
аккаунта нет роли). Это **отдельная сессия панели** в памяти сервера (12 ч):
игровой токен не меняется, тот же аккаунт может одновременно играть.
Все остальные запросы — с заголовком `Authorization: Bearer <token>`, без
него 401. После перезапуска сервера в панель нужно войти заново.

| метод и путь | тело / ответ | что делает |
|---|---|---|
| `GET /api/admin/metrics` | `{server, clients[], doors[]}` | панель опрашивает раз в секунду: метрики сервера (аптайм, Go heap, горутины, LLM: запросы/ошибки/средняя задержка/в работе/ответы по шаблону), клиенты с `stats{pingMs, fps, frameMs, memoryMB, drawCalls, triangles, paused, chatOpen, gpu, droppedMessages}`, двери |
| `GET /api/admin/config` | `{settings, defaults, npcs[], rooms[], factBanks[]}` | всё редактируемое; у NPC — текущие поля, `profile` (правки), `defaults` (встроенные промпт/приветствие/оффтоп), `base` (из npcs.json) |
| `PUT /api/admin/settings` | `{walkSpeed, sprintSpeed, llmEnabled, model, cleanersEnabled, cleanerSpeed}` | глобальные настройки, сразу рассылает `settings` игрокам |
| `DELETE /api/admin/settings` | → настройки | сброс к исходным |
| `POST /api/admin/doors` | `{a, b, open}` | открыть/закрыть дверь между комнатами без проверки расстояния |
| `POST /api/admin/announce` | `{text}` → `{recipients}` | объявление всем (`announcement`), до 300 символов |
| `POST /api/admin/players/{id}/teleport` | `{room}` | переносит игрока в точку появления комнаты (`chunk_data` с `position`) |
| `POST /api/admin/players/{id}/kick` | — | `error kicked`, затем сокет закрывается |
| `PUT /api/admin/npcs/{id}` | `{name, title, personality, profile{systemPrompt, greeting, offtopic, mode, temperature?, maxTokens}}` | поведение персонажа; пустые строки = встроенный текст. `mode`: `llm` / `fsm` (только шаблоны) / `silent` |
| `DELETE /api/admin/npcs/{id}` | — | вернуть персонажа к исходному |
| `GET /api/admin/npcs/{id}/memory` | `[{playerId, name, toldFacts, lines[{role, content, at}]}]` | диалоги NPC с каждым игроком (последние 40 реплик, в памяти) |
| `DELETE /api/admin/npcs/{id}/memory?player=` | — | забыть одного игрока или всех (контекст LLM, состояние FSM, рассказанные факты) |
| `GET/PUT/DELETE /api/admin/facts/{bank}` | `[{id, topic, difficulty, text}]` | банк фактов целиком; у новых фактов пустой `id` — сервер выдаст сам; DELETE — вернуть исходные |

Правки сохраняются в SQLite (таблица `admin_config`, `kind` = `settings` /
`npc` / `facts`) **поверх** `world-data/*.json` — сами JSON-файлы в git не
меняются, поэтому у тиммейтов при `git pull` ничего не конфликтует. При
старте сервер применяет сохранённые правки (`LoadAdminOverrides`).

## NPC world actions (branch llm_to_world)

NPC dialogue can propose one whitelisted action per request: `give_apple`
(Sovunya), `guide_to_door {room}` (Krosh, connected hub door), or
`set_suit_color {color}` (Nyusha, named palette). Ollama function calls and
offline intent matching use the same action executor. Clients never submit
arbitrary tools. Execution rechecks the originating live connection, active
conversation generation, room and distance after model generation.

`welcome` includes `playerState:{playerId,apples,suitColor,revision}`.
Server -> client `player_state {state:...}` goes to players in that room;
rosters and `player_joined` include `apples,suitColor` for late joins.
Apples (max 5, grant cooldown 5 s) and suit color are persisted per account
in SQLite `player_world_state`. Client -> server `use_item {item:"apple"}`
consumes one owned apple; quantities cannot go negative.

Server -> client `npc_action {room,npcId,playerId,action,from:{x,y,z},to:{x,y,z}}`
is a transient cosmetic event (apple handoff); persistent state comes from
player_state. Room snapshots extend each NPC pose with `guidePlayer` and
`guideRoom` (empty strings when inactive), plus `guideArrived` boolean.
Guidance starts after closing chat, stays inside the room on checked nav
edges, pauses if the owner is >3.5 m away, and ends on arrival + exit,
disconnect, room change or a 180 s timeout. Other players cannot steal a guide.
Doors retain their existing E interaction. Updated client/server deploy together.
