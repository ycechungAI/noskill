package skills

import (
	"bytes"
	"errors"
	"maps"
	"slices"

	"go.yaml.in/yaml/v3"
)

// SetMeta returns SKILL.md with scalar keys of its `noskill:` block set (creating the block when
// there is none). Other keys and the body are kept; the learning loop uses this to change a learned
// skill's status or version without rewriting the rest (SKILL_FORMAT.md "unknown front-matter is
// preserved on write").
func SetMeta(raw []byte, set map[string]string) ([]byte, error) {
	return editMeta(raw, func(ns *yaml.Node) {
		for _, k := range slices.Sorted(maps.Keys(set)) {
			setKey(ns, k, &yaml.Node{Kind: yaml.ScalarNode, Value: set[k]})
		}
	})
}

// SetMetaList sets a list key of the `noskill:` block (triggers, for a merge).
func SetMetaList(raw []byte, key string, vals []string) ([]byte, error) {
	return editMeta(raw, func(ns *yaml.Node) {
		l := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, v := range vals {
			l.Content = append(l.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: v})
		}
		setKey(ns, key, l)
	})
}

func setKey(m *yaml.Node, k string, v *yaml.Node) {
	if old := mapGet(m, k); old != nil {
		*old = *v
		return
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, v)
}

func editMeta(raw []byte, edit func(ns *yaml.Node)) ([]byte, error) {
	fm, body, err := splitFrontMatter(raw)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(fm, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("front-matter isn't a mapping")
	}
	top := doc.Content[0]
	ns := mapGet(top, "noskill")
	if ns == nil || ns.Kind != yaml.MappingNode {
		if ns == nil {
			top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "noskill"}, &yaml.Node{Kind: yaml.MappingNode})
			ns = top.Content[len(top.Content)-1]
		} else {
			*ns = yaml.Node{Kind: yaml.MappingNode}
		}
	}
	edit(ns)
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	enc.Close()
	return []byte("---\n" + b.String() + "---\n" + body), nil
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
