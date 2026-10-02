package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMarkdownToHTML(t *testing.T) {
	md := "Смотри **важное** и `x < y`:\n```go\nif a < b && c {\n}\n```"
	want := "Смотри <b>важное</b> и <code>x &lt; y</code>:\n" +
		"<pre><code class=\"language-go\">if a &lt; b &amp;&amp; c {\n}</code></pre>"
	if got := markdownToHTML(md); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestMarkdownUnclosedFence(t *testing.T) {
	got := markdownToHTML("```\nоборвалось")
	if got != "<pre>оборвалось</pre>" {
		t.Fatalf("незакрытый блок должен закрыться: %q", got)
	}
}

func TestSplitMarkdownKeepsFences(t *testing.T) {
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, "строка кода номер такая-то")
	}
	md := "Вступление\n```go\n" + strings.Join(lines, "\n") + "\n```\nКонец"
	chunks := splitMarkdown(md, 300)
	if len(chunks) < 3 {
		t.Fatalf("ожидали несколько кусков, получили %d", len(chunks))
	}
	for i, c := range chunks {
		if n := strings.Count(c, "```"); n%2 != 0 {
			t.Fatalf("кусок %d: непарные ``` (%d)\n%s", i, n, c)
		}
		if utf8.RuneCountInString(c) > 300+len("```go\n")+len("```\n") {
			t.Fatalf("кусок %d длиннее лимита: %d", i, utf8.RuneCountInString(c))
		}
	}
	if !strings.HasPrefix(chunks[1], "```go") {
		t.Fatalf("код-блок должен открываться заново с языком: %q", chunks[1][:20])
	}
}

func TestSplitMarkdownLongLine(t *testing.T) {
	chunks := splitMarkdown(strings.Repeat("я", 1000), 300)
	total := 0
	for _, c := range chunks {
		total += utf8.RuneCountInString(c)
	}
	if total != 1000 {
		t.Fatalf("потеряли символы: %d", total)
	}
}
