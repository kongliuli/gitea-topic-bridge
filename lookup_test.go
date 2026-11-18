package bridge

import (
	"context"
	"errors"
	"testing"
)

type fixtureSource struct {
	permissionError bool
	reads           []int64
}

func (source *fixtureSource) CanReadPullRequests(_ context.Context, repo Repository) (bool, error) {
	if source.permissionError && repo.ID == 4 {
		return false, errors.New("private repository details")
	}
	return repo.ID != 4, nil
}

func (source *fixtureSource) PullRequests(_ context.Context, repo Repository, pageNumber int) (Page, error) {
	source.reads = append(source.reads, repo.ID)
	switch repo.ID {
	case 1:
		return Page{PullRequests: []PullRequest{{Number: 1, Title: "[Topic: CAD-01]", Lifecycle: "merged"}, {Number: 1, Title: "[Topic: CAD-01]"}, {Number: 2, Title: "[Topic: CAD-010]"}}}, nil
	case 2:
		return Page{PullRequests: []PullRequest{{Number: 1, Body: "[Topic: CAD-01]", Lifecycle: "open"}}, HasMore: pageNumber == 1}, nil
	case 3:
		return Page{}, nil
	default:
		return Page{}, errors.New("upstream unavailable")
	}
}

func TestLookupFixtures(t *testing.T) {
	source := &fixtureSource{}
	repos := []Repository{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}, {ID: 5}}
	results, err := Lookup(context.Background(), source, repos, "CAD-01", 1)
	if err != nil || len(results) != 4 {
		t.Fatalf("got %#v, %v", results, err)
	}
	wantStatuses := []string{"complete", "incomplete", "complete", "failed"}
	wantCounts := []int{1, 1, 0, 0}
	for index, result := range results {
		if result.Status != wantStatuses[index] || len(result.PullRequests) != wantCounts[index] {
			t.Errorf("unexpected result: %#v", result)
		}
	}
	for _, read := range source.reads {
		if read == 4 {
			t.Fatal("queried PRs in a hidden repository")
		}
	}
	if results[1].PullRequests[0].Body != "" {
		t.Fatal("returned PR body")
	}
	results, err = Lookup(context.Background(), source, repos[1:2], "CAD-01", 2)
	if err != nil || len(results) != 1 || results[0].Status != "complete" || len(results[0].PullRequests) != 1 {
		t.Fatalf("pagination: got %#v, %v", results, err)
	}
}

func TestLookupPermissionFailure(t *testing.T) {
	source := &fixtureSource{permissionError: true}
	results, err := Lookup(context.Background(), source, []Repository{{ID: 1}, {ID: 4}}, "CAD-01", 1)
	if results != nil || err == nil || err.Error() != "cannot verify repository permissions" {
		t.Fatalf("got %#v, %v", results, err)
	}
}
