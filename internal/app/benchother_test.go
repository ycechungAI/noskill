package app

import "testing"

func TestRankOfAndCosine(t *testing.T) {
	docs := [][]float32{{1, 0}, {0, 1}, {1, 1}}
	if r := rankOf([]float32{1, 0.1}, docs, 0); r != 1 {
		t.Errorf("closest doc ranked %d", r)
	}
	if r := rankOf([]float32{1, 0.1}, docs, 1); r != 3 {
		t.Errorf("farthest doc ranked %d", r)
	}
	if c := cosine([]float32{1, 2}, []float32{2, 4}); c < 0.999 {
		t.Errorf("parallel vectors: %v", c)
	}
	if c := cosine([]float32{0, 0}, []float32{1, 1}); c != 0 {
		t.Errorf("zero vector: %v", c)
	}
}

// The shuffled-label probe must move every topic to a different letter each turn.
func TestRotatedLabelsMoveEveryTopic(t *testing.T) {
	seen := map[string]map[string]bool{}
	for turn := 0; turn < 3; turn++ {
		choices, label := rotated(turn)
		for topic, l := range label {
			if choices[l] != topicChoices[topic] {
				t.Fatalf("turn %d: label %s doesn't describe %s", turn, l, topic)
			}
			if seen[topic] == nil {
				seen[topic] = map[string]bool{}
			}
			if seen[topic][l] {
				t.Errorf("%s got label %s twice", topic, l)
			}
			seen[topic][l] = true
		}
	}
}

func TestStaplesNotInstalledAreOffered(t *testing.T) {
	have := []ModelStatus{{Name: "granite4:3b"}, {Name: "nomic-embed-text:latest"}}
	var names []string
	for _, s := range missingStaples(have) {
		names = append(names, s.Name)
	}
	if len(names) != 2 || names[0] != "gemma3:1b" || names[1] != "tev1:0.8b" {
		t.Errorf("missing = %v", names)
	}
}
