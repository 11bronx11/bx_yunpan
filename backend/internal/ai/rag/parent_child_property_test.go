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
				joined := map[int]string{}
				for _, parent := range got.Parents {
					if ids[parent.ID] || parent.TokenCount > 2400 || !utf8.ValidString(parent.Content) {
						t.Fatal("invalid parent")
					}
					ids[parent.ID] = true
					joined[parent.BlockStart] += parent.Content
					covered := make([]bool, len([]rune(parent.Content)))
					for _, child := range got.Children {
						if child.ParentID != parent.ID {
							continue
						}
						if ids[child.ID] || child.TokenCount > 600 || child.ParentStart < 0 || child.ParentEnd > len(covered) || child.ParentStart >= child.ParentEnd {
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
				for i, block := range blocks {
					if joined[i] != strings.TrimSpace(strings.ReplaceAll(block.Text, "\r\n", "\n")) {
						t.Fatalf("n=%d dropped source runes", n)
					}
				}
			}
		})
	}
}
