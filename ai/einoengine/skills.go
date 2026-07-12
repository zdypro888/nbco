package einoengine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	skillmw "github.com/cloudwego/eino/adk/middlewares/skill"

	"github.com/zdypro888/nbco/ai"
)

// turnSkillBackend exposes only the candidates already authorized and recalled
// for this turn. Eino shows their metadata to the model and loads full content
// only after the model selects one through the native skill tool.
type turnSkillBackend struct {
	items map[string]skillmw.Skill
	list  []skillmw.FrontMatter
}

func newTurnSkillBackend(skills []ai.Skill) *turnSkillBackend {
	backend := &turnSkillBackend{items: make(map[string]skillmw.Skill, len(skills))}
	for _, item := range skills {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		if _, exists := backend.items[name]; exists {
			continue
		}
		frontMatter := skillmw.FrontMatter{
			Name:        name,
			Description: strings.TrimSpace(item.Description),
		}
		backend.items[name] = skillmw.Skill{
			FrontMatter: frontMatter,
			Content:     strings.TrimSpace(item.Content),
		}
		backend.list = append(backend.list, frontMatter)
	}
	sort.Slice(backend.list, func(i, j int) bool { return backend.list[i].Name < backend.list[j].Name })
	return backend
}

func (b *turnSkillBackend) List(context.Context) ([]skillmw.FrontMatter, error) {
	if b == nil {
		return nil, nil
	}
	return append([]skillmw.FrontMatter(nil), b.list...), nil
}

func (b *turnSkillBackend) Get(_ context.Context, name string) (skillmw.Skill, error) {
	if b == nil {
		return skillmw.Skill{}, fmt.Errorf("skill %q is unavailable", name)
	}
	item, ok := b.items[strings.TrimSpace(name)]
	if !ok {
		return skillmw.Skill{}, fmt.Errorf("skill %q is unavailable in this turn", name)
	}
	return item, nil
}
