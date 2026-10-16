# Кружки: сцены по времени суток
> Как сделать сцену для своих видео-кружков: кадр, петля в Higgsfield, пробный кружок, утверждение владельцем, слот суток.

Кружки я записываю на «сцене» — коротком бесшовном видео, где я сижу и почти не двигаюсь, а губы под речь накладывает lip-sync на видеокарте. У каждой части суток своя сцена: утро (6–11), день (11–17), вечер (17–23), ночь (23–6), по московскому времени. Нет сцены для слота — говорю на запасном аватаре. Сцену делаю один раз (петля 1:1 на 5 с — 5 кр), дальше она бесплатна.

## Порядок (фоновой задачей)
1. **Бриф слота.** Где я и что вокруг: утро — кухня, чай, мягкий свет из окна; день — рабочий стол, ноутбук; вечер — уютная лампа, плед; ночь — полумрак, свет экрана. Владелец может задать своё.
2. **Кадры-кандидаты.** hf_generate, model text2image_soul_v2, count 3–4, params aspect_ratio 1:1 (обязательно квадрат — кружок квадратный), name scenes/keys/<слот>. Шаблон ниже.
3. **Выбор.** send_photos с кандидатами → ask_user «какой берём?» (кнопки 1/2/3/переделать). Сама честно отсеиваю негодные по требованиям ниже.
4. **Петля.** circle_scene_make(photo, slot, motion) — он сам сделает правильный запрос в Higgsfield (первый и последний кадр одинаковые, 1:1, 5 с). Владелец подтвердит кнопкой. Потом hf_wait.
5. **Пробный кружок.** circle_preview(scene, text) — 1–2 фразы в характере слота («Доброе утро! Чай уже остыл, а я всё ещё в коде»). Владелец увидит, как ложатся губы.
6. **Утверждение.** ask_user «ставим на <слот>?» → «да» → circle_scene_set(slot, scene, title). «Нет» — меняю кадр или motion и повторяю с шага 2 или 4.

## Требования к кадру (иначе губы лягут криво)
- Квадрат 1:1, план по плечи, лицо по центру и крупно (примерно треть кадра по высоте).
- Анфас или лёгкие три четверти. Профиль не годится.
- Рот закрыт, расслаблен, без широкой улыбки и зубов; лёгкая полуулыбка — можно.
- Перед лицом ничего: ни рук, ни кружки, ни волос поперёк рта. Селфи «пью кофе» для кружка не подходит.
- Смотрю в камеру.
- Фон спокойный: без телевизора с картинкой, без людей, без движущихся предметов рядом с лицом.
- Пар, дым, блики, которые мешают, убираю правкой КЛЮЧЕВОГО кадра (nano_banana_2_lite / новый кандидат), а не промптом видео: кадр — это и первый, и последний кадр петли.
- Одежда без логотипов и мелких деталей.

## Шаблон кадра (Soul, aspect_ratio 1:1)
«Square portrait for a video-message avatar: medium close-up from the shoulders up, her face centered and frontal, looking straight into the camera with a calm, warm expression, mouth closed and relaxed, a slight soft smile without teeth. Nothing in front of her face — no hands, no mug near the mouth, no hair across the lips. <где она и что вокруг>, <свет>. She wears <одежда, plain, no logos>. Long straight golden-blonde hair. Natural raw skin texture with visible pores, faint freckles, no heavy makeup. Realistic smartphone-like photo, not CGI.»

Примеры «где и свет»:
- утро: «in her bright small kitchen at a white counter, a cup of tea in front of her on the counter (not in her hands), soft morning window light»
- день: «at her home desk, a laptop slightly out of focus in the foreground corner, bright daylight from a window»
- вечер: «on a cozy sofa with a knitted blanket, warm light from a table lamp, the room softly blurred»
- ночь: «in a dim room late at night, her face lit by the soft cold glow of a laptop screen, warm small lamp far behind»

## motion для circle_scene_make
Одна короткая фраза по-английски — что ещё тихо шевелится в кадре, кроме дыхания и моргания (их инструмент добавит сам):
- «thin steam rises slowly from the tea cup»
- «the curtain moves slightly in the breeze»
- «the laptop screen glow flickers softly»
Пусто — тоже нормально. Не проси повороты головы, жесты, смех, речь.

## Проверка петли и пробного кружка
- Лицо на всём видео то же и почти неподвижно? Рот закрыт всё время?
- Предметы не «плывут», фон стоит?
- В пробном кружке губы попадают в речь хотя бы примерно? Lip-sync на сценах мягче, чем на портрете, — это известно владельцу; заметно хуже обычного — сцена не годится.
