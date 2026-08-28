# Снапшот сессии 28.08 (полдень) — everything работает

HEAD `2b7079d`, всё запушено, рабочий каталог чистый. **Оба клиента (Flicker веб + нативка 126.21) работают одновременно на одном инстансе.**

## Сделано сегодня (коммиты поверх 0abfd68)

- `0931c5f` cherry-pick 78642a2 — полные user_settings в READY + GET/PATCH
- `abee51a` cherry-pick c12ecc3 — guild_folders-нормализация
- `e5d6fa9` cherry-pick 198cefc — member.id в op14
- `994f756` cherry-pick 23fd68b — self-seed в member list + member_count/online_count
- `f9b3466` ceil-секунды: `model.NowTimestamp()` — RFC3339 без дроби, строго новее клиентского ghost (ghost-дубль Flicker починен)
- `6aad499` UseNumber round-trip снежинок (PATCH ingress + стор + нормализация точными строками)
- `6cd59b1` тема-санитайзер: наружу только dark|light (пересечение поколений) — КОРЕНЬ крашей
- `2b7079d` README: бета, веб-клиент работает, приглашение тестировать

## Следствие «крашей телефона» — ЗАКРЫТО

- Процессных крашей НЕ БЫЛО никогда: crash-буфер пуст, `dumpsys activity exit-info` без crash-причин (только USER_REQUESTED и один CPU-kill от моего даунтайма).
- «Вылеты» = навигационная петля: `Navigation [AppActivity] > [WidgetTabsHost]` + self-START каждые ~1.6с.
- Яд: `theme: "darker"` в user_settings (запатчил Flicker, zod разрешает), 126.21 знает только dark/light/pureEvil → тема не резолвится → петля. Вайпы/откаты были плацебо.
- Дискриминатор: nav-breadcrumbs после READY (1 = норма, десятки = петля). Урок: бисект-бинарь верифицировать хэшем (один «базлайн» собрался не из того workdir).
- Папка «test» в guild_folders — моя тест-контаминация, вычищена; при свежем деплое не появится.

## Полигон

- voidbar: `D:\voidbar\voidbar.exe serve`, `[::]:18084`, storage `%TEMP%\opencode\voidbar-live`, env = start-live-lan.ps1 + VOIDBAR_CLIENT_* (клиентское зеркало mirror-2022-06)
- eris (VoidTest): `%TEMP%\opencode\eris-fork-src`, 127.0.0.1:6670 — жив
- adb: `C:\platform-tools\adb.exe`, телефон 192.168.1.228:5555; пакет com.voidbar
- Flicker: исходники `D:\Client` (zod-схема user_settings: src/types/userSettings.ts — требует status!)
- Токен: `%TEMP%\opencode\tok.txt` (doesnm); Halloy use_tls=false

## Правило сессии

**Каждое изменение — тест на ОБОИХ клиентах** (Flicker веб + нативка 126.21). Flicker = веб, нативка = Discord Android 126.21.

## Ghost-автопсия Flicker (закрыто: клиентский кэш)

Симптом: свои (отправленные из Flicker) сообщения после удаления возвращаются
«белое+серое»; чужие удаляются чисто; reload лечит. Double delete = повторное
удаление воскресшей копии (сервер честно 404).

Механика (mainContent.tsx):
- localCacheRef (742-750) копит строки рендера, дедуп по id, ghost с temp-id
  кэшируется навсегда; выселяет только смена канала
- скролл у дна (708-735) дозаливает из кэша: ts > последней && id ∉ активных
- MESSAGE_DELETE фильтрует messages-стейт, кэш не трогает → resurrection
- у своих две копии в кэше (temp-ghost серый + снежинка белая) → «белое и серое»
- ceil-таймстемп закрывал старую вариацию того же механизма (усечение секунд
  делало серверную копию старее ghost'а в фильтре ts >)

Сервер проверен вживую по всем ногам: nonce в JSON-эхо, multipart REST,
multipart GATEWAY-эхо (flicker-888), форма MESSAGE_DELETE, лишних кадров нет.
Пин-тесты: TestSendMessageMultipartPayloadJSON. Серверсайд чинить нечего.

Кросс-подтверждение: тот же баг (белые+гост) репродуснится на Oldcord
Staging — независимый сервер → причина 100% клиентская.

E2E-репро (playwright): %TEMP%\opencode\pw\flicker-resurrect.js —
25 филлеров+жертва REST-ом → Flicker (localhost:3000, custom instance
127.0.0.1:18084) → DELETE → 0 строк → скролл верх/низ → 1 строка
(воскрешение). Регрессионный пин: заорёт «no resurrection» в день,
когда клиент починит выселение кэша.

Серверсайд-фикс невозможен (анализ всех ~16 обрабатываемых событий
Flicker): кэш чистят только смена канала и кнопка jumpToPresent;
MESSAGE_UPDATE-tombstone возвращает скорлупу (append-ветка),
бамп ts предыдущего = фальсификация данных во всех клиентах,
CHANNEL_DELETE/CREATE выкидывает из канала, DELETE_BULK не обрабатывается.
Задокументировано в README known-issues.

Фиксы Flicker (не применяем — клиенты не патчим):
- handleDeleteMessage: localCacheRef.current = localCacheRef.current.filter(
  (c) => c.id !== deletedMessage.id)
- reconcile: выселять temp-строки с тем же nonce из кэша

## Хвосты (not implemented, по приоритету)

- chathistory-prefill: СДЕЛАНО (6f5a459) — префилл 50 на JOIN + бэк-пагинация скролла (77e5357):
  msgid-якорь (timestamp fallback с обязательными .000ms), NewBelow-ceiling (burst в мс якоря
  минтится строго ниже), тихий флеш без MESSAGE_CREATE/вотермарка, липкий histCap из CAP ACK
  (карта капов girc отстаёт от JOIN-эхо), ms-точные таймстемпы строк (секундное усечение
  инвертировало порядок burst-ов). Живой полигон: #deeptest3 (80 строк eris: page1=50, page2=30)
- op24: ЗАКРЫТО без реализации — терпим молчанием (server.go OpGuildMembersApps):
  любой ответ валил 126.21 в launch-loop (logcat-доказательство); обоим клиентам
  хватает op14 (Flicker шлёт только его, нативка тоже получает сайдбар через op14)
- инвайт-коды: НЕ НУЖНЫ (юзер) — из списка вон

Комментарии юзера по будущим фичам:
- /nick: в дискорде был слэш-командой; нативка дёргает application commands v1 и всё —
  упирается в инфраструктуру апп-команд; пока не берём
- topic: тривиально (TOPIC ↔ CHANNEL_UPDATE); rename: IRCv3 channel-rename (eris умеет)
- away: кнопка есть; маппинг online/away ясен, что делать с dnd/invisible — вопрос
- SASL: не знаем как — отложено

1. ~~`status` в user_settings~~ — НЕ НУЖЕН: StatusEnumSchema = union(enum, z.undefined()), отсутствие ключа валидно; ReadyEventSchema.parse не бросает
2. ~~edit/delete сообщений~~ — delete сделан (b2875b6); edit невозможен upstream
3. ~~бэк-пагинация~~ — СДЕЛАНО (77e5357)
4. topic/rename каналов, /nick, away-toggle, SASL
