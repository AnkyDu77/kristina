package main

// Скиллы — плейбуки Кристины (skills/<раздел>/<имя>.md, вшиты в бинарь).
// В системном промпте только оглавление: имя и одна строка «о чём»;
// полный текст Кристина берёт инструментом read_skill, когда садится за
// работу. Так контекст не раздувается тем, что сейчас не нужно.
//
// Формат файла: первая строка «# Заголовок», вторая — «> о чём скилл».
// README.md раздела называется по разделу: instagram/README.md → instagram.

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed skills
var skillsFS embed.FS

type skillInfo struct {
	Name, Title, Summary string
}

// skillName — имя скилла по пути файла внутри skills/.
func skillName(p string) string {
	p = strings.TrimSuffix(strings.TrimPrefix(p, "skills/"), ".md")
	return strings.TrimSuffix(p, "/README")
}

func listSkills() []skillInfo {
	var out []skillInfo
	_ = fs.WalkDir(skillsFS, "skills", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".md" {
			return nil
		}
		data, err := skillsFS.ReadFile(p)
		if err != nil {
			return nil
		}
		s := skillInfo{Name: skillName(p)}
		for i, line := range strings.SplitN(string(data), "\n", 3) {
			switch {
			case i == 0 && strings.HasPrefix(line, "# "):
				s.Title = strings.TrimSpace(line[2:])
			case i == 1 && strings.HasPrefix(line, "> "):
				s.Summary = strings.TrimSpace(line[2:])
			}
		}
		out = append(out, s)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func readSkill(name string) (string, error) {
	name = strings.Trim(strings.TrimSpace(name), "/")
	name = strings.TrimSuffix(name, ".md")
	for _, p := range []string{"skills/" + name + ".md", "skills/" + name + "/README.md"} {
		if data, err := skillsFS.ReadFile(p); err == nil {
			return string(data), nil
		}
	}
	var names []string
	for _, s := range listSkills() {
		names = append(names, s.Name)
	}
	return "", fmt.Errorf("скилла %q нет; есть: %s", name, strings.Join(names, ", "))
}

// skillsIndex — оглавление для системного промпта.
func skillsIndex() string {
	var sb strings.Builder
	for _, s := range listSkills() {
		fmt.Fprintf(&sb, "- %s — %s\n", s.Name, s.Summary)
	}
	return sb.String()
}

func (b *bot) skillTools() []*tool {
	return []*tool{{
		spec: toolSpecFunc{
			Name: "read_skill",
			Description: "Прочесть свой плейбук (скилл) целиком: правила, грабли и шаблоны для задачи. Оглавление — в системном промпте. " +
				"Читай нужный скилл ДО начала работы по его теме.",
			Parameters: object(map[string]any{"name": str("имя скилла из оглавления, например instagram/photos")}, "name"),
		},
		modes:  everywhere,
		status: func(a json.RawMessage) string { return "📘 Читаю скилл " + argStr(a, "name") },
		run: func(_ context.Context, _ int64, a json.RawMessage) (string, error) {
			return readSkill(argStr(a, "name"))
		},
	}}
}
