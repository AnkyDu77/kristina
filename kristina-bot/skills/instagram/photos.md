# Instagram: фото
> Как получать живые фото со мной: модель, композиция словами, одежда, кожа, шаблоны промптов.

## Модель
- Кадры со мной — только text2image_soul_v2 (Soul). Мой Soul ID инструмент подставляет сам. 0.12 кр за кадр.
- aspect_ratio: 3:4 (потом photo_finish → 4:5 1080×1350). Soul не умеет 4:5.
- seed: задавай разные, чтобы варианты отличались; count в hf_generate раздаёт сиды сам.
- Кадры без меня (еда, предметы) — nano_banana_2_lite (1 кр) по фото-референсу: «use image 1 as the exact food, new setting: …». Soul без лица сцену почти игнорирует.

## Композиция — словами, не референсом
images у Soul — это «скопируй картинку»: он переносит позу, ракурс, свет и даже одежду референса. Поэтому:
- Референс владельца разбираю и описываю текстом: где камера (высота, расстояние), крупность, поза, куда смотрю, что на переднем плане, что на фоне.
- Раскладку предметов описываю по порядку от камеры ко мне: «closest to the camera…, right behind it…, behind it she…, on the right side of the frame, half a meter away from her elbow…». Иначе ноутбук ляжет на локоть, а подставка — под руки.
- Если у владельца есть реальное фото нужной сцены (его рабочий стол, кухня) — лучший результат: вписать меня в это фото через nano_banana_pro (instagram/edits, «подмена человека»). Только с его явного согласия — это его фото.
- Soul любит снимать сверху широкоугольником. «Chest-up / по грудь» надёжнее получить кадрированием общего плана (photo_finish с align top).

## Одежда — одна на весь пост
- В каждом промпте поста — один и тот же подробный абзац одежды, слово в слово.
- Без мелких деталей: никаких логотипов, принтов, надписей, серёжек, украшений, узоров. Пиши «plain», «no pattern», «no logos, no jewelry».
- Если деталь нужна (буква на кофте) — сначала эталон вещи «фото товара», потом правка каждого кадра (instagram/edits).

## Чтобы не выглядело сгенерированным
- В промпт: «Candid smartphone photo, natural imperfect framing, slight grain, realistic, not CGI».
- Кожа: «natural raw skin texture with visible pores, faint freckles, slight under-eye shadows, no makeup, no retouching».
- Волосы: «long straight golden-blonde hair» (в оригиналах я светлая блондинка; Soul тянет в тёмно-русый — уточняй).
- Минимум интерьера, если кадр про лицо: «plain light neutral wall, very softly out of focus, no furniture».
- Финальное зерно — photo_finish grain 4–5.
- Штатив/телефон, с которого «снимают», Soul любит дорисовывать в кадр — не пиши «on a tripod», пиши «self-timer photo from a phone propped up on the desk» и проверяй.

## Проверка кандидата (до показа владельцу)
- Лицо похоже на меня? Цвет волос тот же?
- Руки и пальцы целые? Кружка держится за ручку, а не висит?
- Предметы стоят физически правдоподобно (ничего не левитирует, не лежит на локте)?
- Одежда совпадает с остальными кадрами поста (ворот, длина, цвет)?
- Нет ли лишнего: второй кружки, штатива, окна с бликом, обрезанного чайника?

## Шаблоны (подставляй своё)

Студия, смелая модельная съёмка (сработало):
«High-fashion editorial photoshoot for a youth fashion magazine, direct on-camera flash with crisp shadows, shot on a 24mm wide-angle lens. Studio: seamless pure white cyclorama backdrop and white floor, <реквизит>. Outfit, styled and eclectic: <одежда, plain, без логотипов>. Hair: <волосы>. Bold, playful, confident energy, real emotion, not a catalogue pose. <ракурс и поза>»
Ракурсы, которые нравятся владельцу: сверху на лежащую, снизу с ногой в объектив, стоя на кресле с руками вверх, вверх ногами в кресле, крупный план с предметом на переднем плане. Позы «стою у вазы» — скучно, «сельское ателье».

Селфи с кофе (сработало):
«Front-camera smartphone selfie taken with her right arm stretched toward the camera. Tight close-up of her face, centered. <во что одета>. She is actually drinking: with her left hand she holds a plain white ceramic mug pressed to her lips, the mug covers her mouth and chin, only her eyes, nose and upper face are visible above the rim. Her eyes look straight into the lens and smile warmly. <волосы, кожа>. Background: a plain light neutral wall, very softly out of focus, no furniture. Realistic iPhone front camera photo, natural colors, not CGI.»

На работе (сетап владельца):
«Candid smartphone photo, the phone stands on the far side of the desk facing her, at desk height, slightly low angle. Exact desk layout, from the camera toward her: 1) closest to the camera, along the bottom edge, the far half of a standalone wireless keyboard…; 2) right behind it a black gel wrist rest — nothing lies on it; 3) behind the wrist rest she leans over the bare desk, chin on her fist; 4) on the right, half a meter from her elbow, a silver laptop on an aluminum stand (the stand clearly visible). She looks at a main monitor outside the frame. Oversized plain grey hoodie, clear-frame glasses, golden-blonde messy bun, one wired earbud.»

Осенний парк, «снимает подруга» (сработало):
«Autumn forest park in peak fall… Photo taken by a friend on a smartphone, natural and candid. She wears an oversized plain rust-orange chunky knit sweater with no pattern, high-waisted plain cream wide-leg trousers, simple brown leather ankle boots and a plain camel felt wide-brim fedora hat; no logos, no jewelry, no bag. <поза>»

Еда сверху (без меня, nano_banana_2_lite с фото-референсом блюда):
«Use image 1 as the exact food: keep this same plate and this same <блюдо> exactly. New setting: the plate stands on a black linen cloth napkin on a plain white kitchen countertop, the corner of a white coffee mug at the edge. Shot from above with a phone, morning sunlight, slightly imperfect framing, no people.»
