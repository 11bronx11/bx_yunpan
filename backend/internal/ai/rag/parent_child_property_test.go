package rag

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
)

// E03: five fixed seeds, 100 bounded mixed-script documents per seed.
func TestParentChildBoundedProperties(t *testing.T) {
	alphabet := []rune("中文测试回滚。！？\n\t abcXYZ012_🙂🚀é\r")
	for _, seed := range []int64{7, 29, 113, 2026, 17017} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			random := rand.New(rand.NewSource(seed))
			for n := range 100 {
				runes := make([]rune, random.Intn(3500))
				for i := range runes {
					runes[i] = alphabet[random.Intn(len(alphabet))]
				}
				blocks := []Section{{Text: string(runes), HeadingPath: "重复章节"}, {Text: "末尾唯一标记🙂", HeadingPath: "重复章节"}}
				version := uuid.NewSHA1(uuid.Nil, []byte(fmt.Sprintf("%d/%d", seed, n)))
				got, err := BuildParentChild(version, blocks)
				if err != nil {
					t.Fatal(err)
				}
				again, _ := BuildParentChild(version, blocks)
				if !reflect.DeepEqual(got, again) {
					t.Fatalf("n=%d nondeterministic", n)
				}
				ids := map[uuid.UUID]bool{}
				windows := map[[2]int][]Parent{}
				for _, parent := range got.Parents {
					if ids[parent.ID] || parent.TokenCount > 1600 || !utf8.ValidString(parent.Content) {
						t.Fatal("invalid parent")
					}
					ids[parent.ID] = true
					span := [2]int{parent.BlockStart, parent.BlockEnd}
					windows[span] = append(windows[span], parent)
					covered := make([]bool, len([]rune(parent.Content)))
					for _, child := range got.Children {
						if child.ParentID != parent.ID {
							continue
						}
						if ids[child.ID] || child.TokenCount > 400 || child.ParentStart < 0 || child.ParentEnd > len(covered) || child.ParentStart >= child.ParentEnd {
							t.Fatal("invalid child")
						}
						ids[child.ID] = true
						if child.Content != string([]rune(parent.Content)[child.ParentStart:child.ParentEnd]) {
							t.Fatal("range mismatch")
						}
						for i := child.ParentStart; i < child.ParentEnd; i++ {
							covered[i] = true
						}
					}
					for _, yes := range covered {
						if !yes {
							t.Fatal("coverage gap")
						}
					}
				}
				seenBlocks := map[int]bool{}
				for span, parents := range windows {
					var expected []string
					for i := span[0]; i <= span[1]; i++ {
						if i < 0 || i >= len(blocks) || seenBlocks[i] {
							t.Fatal("invalid/duplicate source block range")
						}
						seenBlocks[i] = true
						part := strings.TrimSpace(strings.ReplaceAll(blocks[i].Text, "\r\n", "\n"))
						if part != "" {
							expected = append(expected, part)
						}
					}
					assertParentWindowsCover(t, strings.Join(expected, "\n\n"), parents)
				}
				for i, b := range blocks {
					if strings.TrimSpace(b.Text) != "" && !seenBlocks[i] {
						t.Fatal("uncovered source block")
					}
				}
			}
		})
	}
}
