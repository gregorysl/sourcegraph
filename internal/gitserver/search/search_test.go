package search

import (
	"fmt"
	"regexp"
	"testing"

	git "github.com/libgit2/git2go/v31"
	"github.com/stretchr/testify/require"
)

func TestFormatDiff(t *testing.T) {
	repo, err := git.OpenRepository("/Users/ccheek/src/sourcegraph/sourcegraph")
	require.NoError(t, err)

	pred := And{
		&AuthorMatches{regexp.MustCompile(`camden`)},
		&DiffMatches{regexp.MustCompile(`dec\.ReadAll`)},
	}

	obj, err := repo.RevparseSingle("decddf8f0")
	require.NoError(t, err)

	err = IterCommits(repo, obj.Id(), func(commit *Commit) bool {
		commitMatches, highlights := pred.Match(commit)
		if commitMatches {
			diff, _ := commit.Diff()
			formatted, ranges := FormatDiffWithHighlights(diff, highlights.Diff)
			print(formatted)
			fmt.Printf("%#v\n", ranges)
		}
		return true
	})
	require.NoError(t, err)
}
