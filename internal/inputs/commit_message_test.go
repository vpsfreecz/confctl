package inputs

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func messageChange(name string, old, new, url string) Change {
	return Change{Name: name, Old: Info{Rev: old, ShortRev: old[:8], URL: url}, New: Info{Rev: new, ShortRev: new[:8], URL: url}}
}
func TestCommitMessageGroupingTitleAndDowngrade(t *testing.T) {
	changes := []Change{messageChange("b", "1111111111", "2222222222", "https://repo/"), messageChange("a", "1111111111", "2222222222", "https://repo"), messageChange("b", "1111111111", "2222222222", "https://repo/")}
	var calls [][]string
	lookup := func(_ context.Context, url, from, to string) (string, error) {
		calls = append(calls, []string{url, from, to})
		return "git log for " + from + ".." + to + "\n< old\n> new\n", nil
	}
	text, err := BuildCommitMessage(context.Background(), changes, true, true, lookup)
	want := "inputs: update a, b, b to 22222222\n\nb: 11111111 -> 22222222\na: 11111111 -> 22222222\nb: 11111111 -> 22222222\ngit log for 2222222222..1111111111\n< old\n> new"
	if err != nil || text != want || !reflect.DeepEqual(calls, [][]string{{"https://repo/", "2222222222", "1111111111"}}) {
		t.Fatal(text, err, calls)
	}
	changes[1].New.URL = "https://different"
	calls = nil
	text, err = BuildCommitMessage(context.Background(), changes, false, false, lookup)
	if err != nil || strings.Contains(strings.Split(text, "\n")[0], " to ") || len(calls) != 0 || !strings.Contains(text, "b: 11111111 -> 22222222\na:") {
		t.Fatal(text, err, calls)
	}
}
func TestCommitMessageMissingRevisionsErrorsAndCancel(t *testing.T) {
	c := messageChange("a", "1111111111", "2222222222", "https://repo")
	text, err := BuildCommitMessage(context.Background(), []Change{c}, true, false, func(context.Context, string, string, string) (string, error) { return "", errors.New("clone refused") })
	if err != nil || !strings.HasSuffix(text, "(changelog unavailable: clone refused)") {
		t.Fatal(text, err)
	}
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded} {
		_, err = BuildCommitMessage(context.Background(), []Change{c}, true, false, func(context.Context, string, string, string) (string, error) { return "", failure })
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
	}
	c.Old = Info{}
	text, err = BuildCommitMessage(context.Background(), []Change{c}, true, false, func(context.Context, string, string, string) (string, error) {
		t.Fatal("lookup without old revision")
		return "", nil
	})
	if err != nil || text != "inputs: update a to 22222222\n\na: set -> 22222222" {
		t.Fatal(text, err)
	}
	c.New = Info{}
	text, err = BuildCommitMessage(context.Background(), []Change{c}, false, false, nil)
	if err != nil || text != "inputs: update a\n\na: set -> -" {
		t.Fatal(text, err)
	}
}

func TestCommitMessageFirstSeenRevisionGroupsAndNilSources(t *testing.T) {
	changes := []Change{messageChange("b", "1111111111", "2222222222", "https://repo"), messageChange("c", "3333333333", "4444444444", "https://repo"), messageChange("a", "1111111111", "2222222222", "https://repo/")}
	var calls [][]string
	text, err := BuildCommitMessage(context.Background(), changes, true, false, func(_ context.Context, url, from, to string) (string, error) {
		calls = append(calls, []string{url, from, to})
		return "git log for " + from + ".." + to, nil
	})
	if err != nil || !reflect.DeepEqual(calls, [][]string{{"https://repo", "1111111111", "2222222222"}, {"https://repo", "3333333333", "4444444444"}}) || !strings.Contains(text, "b: 11111111 -> 22222222\na: 11111111 -> 22222222\ngit log for 1111111111..2222222222\n\nc:") {
		t.Fatal(text, err, calls)
	}
	for i := range changes {
		changes[i].Old.URL = nil
		changes[i].New.URL = nil
		changes[i].New.Rev = "2222222222"
		changes[i].New.ShortRev = "22222222"
	}
	text, err = BuildCommitMessage(context.Background(), changes, true, false, func(context.Context, string, string, string) (string, error) {
		t.Fatal("nil source mirror")
		return "", nil
	})
	if err != nil || strings.Contains(strings.Split(text, "\n")[0], " to ") || !strings.Contains(text, "b: 11111111 -> 22222222\n\nc: 33333333 -> 22222222\n\na:") {
		t.Fatal(text, err)
	}
}
