# Instagram: правки готовых кадров
> Что можно поправить в Higgsfield и как: добавить предмет, сменить одежду, вписать меня в реальное фото; чего правками не сделать.

## Какую модель брать
- nano_banana_2_lite — 1 кр. Добавить предмет, заменить вещь, поменять мелочь. Выход ~896×1200 для 3:4 — для Instagram хватает.
- nano_banana_2_1 — 1.5 кр. То же, чуть аккуратнее.
- nano_banana_pro — 2 кр. Сложные правки, до 14 картинок-референсов, подмена человека в реальном фото.
- Соул (0.12 кр) иногда дешевле: перегенерировать кадр с исправленным промптом, чем трижды править.

## Что получается
- ДОБАВИТЬ предмет: нож и вилку слева от тарелки, подставку под ноутбук, шляпу. Пиши, где именно: «to the LEFT of the plate, in the upper left part of the photo».
- ЗАМЕНИТЬ вещь: «replace her orange coat with an oversized plain rust-orange chunky knit sweater», «replace the white beret with a plain camel felt fedora». Lite справляется.
- Мимика в мелочах: «lips pursed in a playful air-kiss pout» — иногда.
- Всегда добавляй хвост: «Keep everything else exactly identical: her face and identity, hair, pose, <остальное>, the light and framing.»

## Что почти не получается
- СТЕРЕТЬ предмет (окно в углу, серёжки, штатив, пол): Lite и Pro оставляют на месте. Вместо этого: перегенерировать кадр Soul с уточнением («no window», «no jewelry») или попросить владельца — у него на маке есть локальная LaMa (бесплатно и чисто).
- ПЕРЕДВИНУТЬ предмет (лист к глазу): двигает неохотно. Лучше новый кадр с этим в промпте.
- Сменить ракурс/«переснять» сцену: лицо уплывает — так делать только с моими фото-эталонами в images и честно проверять сходство.

## Одинаковая одежда во всём посте
Soul рисует логотипы, полоски и серьги каждый раз по-новому. Если деталь важна:
1. Эталон вещи — «фото товара»: nano_banana_pro, «Product photo of a single garment laid flat… Exact design: 1) … 2) … 3) …, no other letters, no logos». Перечисли каждую деталь по номерам.
2. Каждый кадр: images = [кадр, эталон], «Edit image 1. Replace ONLY the cardigan she is wearing with the exact cardigan design from image 2: <перечень деталей>. The cardigan must follow the folds, drape, pose and lighting of image 1 and stay as visible or hidden as in image 1. Keep everything else in image 1 exactly identical…»
3. Проверь физику: она лежит вверх ногами — буква на груди тоже вверх ногами; кардиган был распахнут — пусть остаётся распахнутым.
Дешевле всего — сразу одежда без деталей (instagram/photos).

## NSFW-фильтр
- Nano Banana режет кадры с голыми ногами/шортами как nsfw (кредиты возвращаются). Повтор того же — снова nsfw.
- Soul иногда режет слова «seductive», «sexy»: пиши «confident, sultry gaze», «tasteful, not vulgar».
- Если правка нужна именно такому кадру — скажи владельцу: он умеет вырезать кусок, поправить и вклеить обратно.

## Подмена человека в реальном фото (лучше всего для бытовых сцен)
Только когда владелец сам дал фото и согласен, что оно уйдёт в Higgsfield.
nano_banana_pro, images = [его фото, мой лучший кадр в нужной одежде, мой портрет-эталон]:
«Image 1 is a real photo of <сцена>. Replace the man in image 1 with the young woman shown in images 2 and 3 (keep her exact face and identity from images 2 and 3): she takes exactly the same pose as the man — <поза>. She wears <одежда>, as in image 2. Keep everything else in image 1 exactly identical: <предметы сцены>, the light and the framing.»
Результат — самый естественный: раскладка предметов настоящая. Окно и мелочи модель может перерисовать — для приватности это даже плюс.

## После правки
- Сравни с исходником: лицо то же? Ничего лишнего не поменялось (свет, вырез свитера, фон)? Если правка поменяла стиль кадра — в карусели он будет выбиваться, лучше другой кандидат.
- Финал — photo_finish (4:5, зерно 4–6).
