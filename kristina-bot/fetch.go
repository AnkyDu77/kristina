package main

// read_page сначала читает страницу сам — с VPS бота, никому не сообщая
// адрес, — и только если не вышло (JS-сайт, PDF, защита от ботов), идёт
// в Parallel web_fetch.
//
// Адрес выбирает модель, а модель читает чужие страницы — значит, адрес
// может подсунуть кто угодно. Поэтому ходим только в публичный интернет:
// никаких localhost, приватных сетей и метаданных облака (169.254.169.254).
// На VPS рядом лежат terraform state и ключи GPU-машины. Проверка — в
// Control дозвонщика, то есть по реальному IP после DNS, на каждом
// редиректе: подмена DNS или редирект на 127.0.0.1 её не обойдут.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const (
	fetchTimeout  = 20 * time.Second
	fetchMaxBytes = 3 << 20
	// Меньше — скорее всего, страница рисуется скриптом, и мы видим пустой
	// каркас: пусть её прочтёт Parallel
	fetchMinText = 400
)

var errNotPublic = errors.New("адрес не в публичном интернете")

// cgnat — 100.64.0.0/10: «серые» адреса провайдеров, IsPrivate их не знает.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsValid() && a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() &&
		!a.IsLinkLocalUnicast() && !cgnat.Contains(a)
}

func newFetchClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !publicAddr(ap.Addr()) {
				return fmt.Errorf("%w: %s", errNotPublic, address)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			// Прокси из окружения обошёл бы проверку IP
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConns:        4,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("слишком много редиректов")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("редирект на %s:// не поддерживается", req.URL.Scheme)
			}
			return nil
		},
	}
}

type page struct {
	URL   string // итоговый, после редиректов
	Title string
	Text  string
}

func checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("нужен адрес вида https://…, а не %q", raw)
	}
	if u.User != nil {
		return nil, errors.New("адреса с логином и паролем не открываю")
	}
	return u, nil
}

func fetchPage(ctx context.Context, hc *http.Client, raw string) (page, error) {
	u, err := checkURL(raw)
	if err != nil {
		return page{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return page{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; KristinaBot/1.0; personal assistant)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", "ru,en;q=0.8")
	resp, err := hc.Do(req)
	if err != nil {
		return page{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return page{}, fmt.Errorf("сайт ответил %s", resp.Status)
	}

	p := page{URL: resp.Request.URL.String()}
	ct := resp.Header.Get("Content-Type")
	mt, _, _ := mime.ParseMediaType(ct)
	body := io.LimitReader(resp.Body, fetchMaxBytes)
	switch {
	case mt == "text/html" || mt == "application/xhtml+xml" || mt == "":
		r, err := charset.NewReader(body, ct)
		if err != nil {
			return page{}, err
		}
		p.Title, p.Text = htmlText(r)
	case strings.HasPrefix(mt, "text/") || mt == "application/json" || strings.HasSuffix(mt, "+json"):
		r, err := charset.NewReader(body, ct)
		if err != nil {
			return page{}, err
		}
		data, err := io.ReadAll(r)
		if err != nil {
			return page{}, err
		}
		p.Text = strings.TrimSpace(string(data))
	default:
		return page{}, fmt.Errorf("сама такое не читаю: %s", mt)
	}
	return p, nil
}

// skipTags — содержимое, которое модели не нужно: код, стили, меню.
// form сюда не входит: ASP.NET-сайты заворачивают в неё всю страницу.
var skipTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true, "template": true,
	"iframe": true, "nav": true, "footer": true, "button": true, "select": true,
}

// blockTags — после них перенос строки, иначе абзацы слипнутся в кашу.
var blockTags = map[string]bool{
	"p": true, "div": true, "li": true, "tr": true, "section": true, "article": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "pre": true,
	"blockquote": true, "table": true, "ul": true, "ol": true, "header": true, "main": true,
	"dt": true, "dd": true, "figcaption": true,
}

// inlineGap — после них пробел: ссылки меню и ячейки таблицы иначе
// слипаются в «ДокиБлогСкачать». span сюда не входит — подсветка кода
// режет им слова.
var inlineGap = map[string]bool{"a": true, "td": true, "th": true, "label": true, "img": true}

// htmlText — заголовок и читаемый текст страницы. Не Readability, но для
// документации, статей и README хватает; остальное дочитает Parallel.
//
// Разбор — html.Parse, а не токенайзер: живые страницы полны незакрытых
// тегов (у go.dev пять <nav> на два </nav>), и только дерево, собранное
// по правилам браузера, говорит, где на самом деле кончается меню.
func htmlText(r io.Reader) (title, text string) {
	doc, err := html.Parse(r)
	if err != nil {
		return "", ""
	}
	var sb strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
			return
		}
		if n.Type == html.ElementNode {
			switch tag := n.Data; {
			case tag == "title":
				if title == "" && n.FirstChild != nil {
					title = strings.TrimSpace(n.FirstChild.Data)
				}
				return
			case skipTags[tag]:
				return
			case tag == "br":
				sb.WriteString("\n")
				return
			case blockTags[tag]:
				sb.WriteString("\n")
				if len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6' {
					sb.WriteString(strings.Repeat("#", int(tag[1]-'0')) + " ")
				}
				if tag == "li" {
					sb.WriteString("- ")
				}
				defer sb.WriteString("\n")
			case inlineGap[tag]:
				defer sb.WriteString(" ")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return title, tidyText(sb.String())
}

// tidyText схлопывает пробелы внутри строк и пустые строки между ними.
func tidyText(s string) string {
	var out []string
	blank := false
	for _, line := range strings.Split(s, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" || line == "-" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		out = append(out, line)
		blank = false
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
