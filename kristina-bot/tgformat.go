package main

// Markdown модели → HTML телеграма. Полный Markdown не нужен: модель
// пишет код-блоки, инлайн-код и жирный — этого и хватает. Всё прочее
// просто экранируется и уходит как есть.

import (
	"html"
	"regexp"
	"strings"
	"unicode/utf8"
)

// tgChunkRunes — с запасом от лимита сообщения в 4096 символов: разметка
// добавляет теги, а считает телеграм уже размеченный текст.
const tgChunkRunes = 3500

var (
	inlineCodeRe = regexp.MustCompile("`([^`\n]+)`")
	boldRe       = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
)

// splitMarkdown режет текст на куски по строкам. Если граница пришлась на
// код-блок, блок закрывается в одном куске и открывается заново в
// следующем — иначе телеграм покажет полсообщения кодом, полсообщения
// кашей.
func splitMarkdown(text string, limit int) []string {
	var chunks []string
	var cur strings.Builder
	curLen := 0
	fenceOpen, fenceHeader := false, ""

	flush := func() {
		if cur.Len() == 0 {
			return
		}
		if fenceOpen {
			cur.WriteString("```\n")
		}
		chunks = append(chunks, strings.TrimRight(cur.String(), "\n"))
		cur.Reset()
		curLen = 0
		if fenceOpen {
			cur.WriteString(fenceHeader + "\n")
			curLen = utf8.RuneCountInString(fenceHeader) + 1
		}
	}

	for _, line := range strings.Split(text, "\n") {
		n := utf8.RuneCountInString(line) + 1
		if curLen+n > limit && curLen > 0 {
			flush()
		}
		// Строка длиннее лимита целиком — режем по рунам
		for n > limit {
			r := []rune(line)
			cur.WriteString(string(r[:limit]) + "\n")
			curLen += limit
			flush()
			line = string(r[limit:])
			n = utf8.RuneCountInString(line) + 1
		}
		cur.WriteString(line + "\n")
		curLen += n
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") {
			if fenceOpen {
				fenceOpen = false
			} else {
				fenceOpen, fenceHeader = true, t
			}
		}
	}
	flush()
	return chunks
}

// markdownToHTML размечает один кусок (после splitMarkdown).
func markdownToHTML(md string) string {
	var out strings.Builder
	var code []string
	inFence, lang := false, ""

	for _, line := range strings.Split(md, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			if !inFence {
				inFence, lang = true, strings.TrimSpace(strings.TrimPrefix(t, "```"))
				code = code[:0]
				continue
			}
			inFence = false
			writeCode(&out, lang, code)
			continue
		}
		if inFence {
			code = append(code, line)
			continue
		}
		out.WriteString(inlineHTML(line) + "\n")
	}
	if inFence { // незакрытый блок — модель оборвалась на полуслове
		writeCode(&out, lang, code)
	}
	return strings.TrimSpace(out.String())
}

func writeCode(out *strings.Builder, lang string, lines []string) {
	body := html.EscapeString(strings.Join(lines, "\n"))
	if lang != "" {
		out.WriteString(`<pre><code class="language-` + html.EscapeString(lang) + `">` + body + "</code></pre>\n")
	} else {
		out.WriteString("<pre>" + body + "</pre>\n")
	}
}

func inlineHTML(line string) string {
	s := html.EscapeString(line)
	s = inlineCodeRe.ReplaceAllString(s, "<code>$1</code>")
	s = boldRe.ReplaceAllString(s, "<b>$1</b>")
	return s
}
