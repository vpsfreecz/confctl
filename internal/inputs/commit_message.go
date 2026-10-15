package inputs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

func normalizedURL(value any) (string, bool, error) {
	if value == nil {
		return "", false, nil
	}
	url, ok := value.(string)
	if !ok {
		return "", false, errors.New("expected input URL string")
	}
	return strings.TrimRight(url, "/"), true, nil
}

func revision(value any, fallback string) string {
	if value == nil || value == false {
		return fallback
	}
	return value.(string)
}

func changeSummary(c Change) string {
	old := "set"
	if c.Old.Rev != nil && c.Old.Rev != false {
		old = revision(c.Old.ShortRev, "")
	}
	return c.Name + ": " + old + " -> " + revision(c.New.ShortRev, "-") + "\n"
}

type CommitAction uint8

const (
	UpdateAction CommitAction = iota
	SetAction
)

func commitTitle(changes []Change) (string, error) {
	return commitActionTitle(changes, UpdateAction)
}

func commitActionTitle(changes []Change, action CommitAction) (string, error) {
	var verb string
	switch action {
	case UpdateAction:
		verb = "update"
	case SetAction:
		verb = "set"
	default:
		return "", errors.New("invalid commit action")
	}
	names := make([]string, len(changes))
	for i, c := range changes {
		names[i] = c.Name
	}
	sort.Strings(names)
	title := "inputs: " + verb + " " + strings.Join(names, ", ")
	reference := changes[0]
	if reference.New.Rev == nil || reference.New.ShortRev == nil {
		return title, nil
	}
	for _, c := range changes {
		if !reflect.DeepEqual(c.New.Rev, reference.New.Rev) {
			return title, nil
		}
	}
	if len(changes) > 1 {
		url, present, err := normalizedURL(reference.URL())
		if err != nil || !present {
			return title, err
		}
		for _, c := range changes {
			other, present, err := normalizedURL(c.URL())
			if err != nil {
				return "", err
			}
			if !present || other != url {
				return title, nil
			}
		}
	}
	return title + " to " + revision(reference.New.ShortRev, ""), nil
}

// BuildCommitMessage groups in first-seen order. Only ordinary mirror failures
// become changelog text; cancellation remains an invocation failure.
func BuildCommitMessage(ctx context.Context, changes []Change, changelog, downgrade bool, lookup Changelog) (string, error) {
	return buildCommitMessage(ctx, changes, changelog, downgrade, lookup, UpdateAction)
}

func buildCommitMessage(ctx context.Context, changes []Change, changelog, downgrade bool, lookup Changelog, action CommitAction) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return "", errors.New("no changes")
	}
	title, err := commitActionTitle(changes, action)
	if err != nil {
		return "", err
	}
	if !changelog {
		var body strings.Builder
		for _, c := range changes {
			body.WriteString(changeSummary(c))
		}
		return strings.Trim(title+"\n\n"+body.String(), "\x00\t\n\v\f\r "), nil
	}
	groups := [][]Change{}
	for _, c := range changes {
		url, present, err := normalizedURL(c.URL())
		if err != nil {
			return "", err
		}
		found := false
		if present {
			for i, g := range groups {
				other, otherPresent, _ := normalizedURL(g[0].URL())
				if otherPresent && other == url && reflect.DeepEqual(c.Old.Rev, g[0].Old.Rev) && reflect.DeepEqual(c.New.Rev, g[0].New.Rev) {
					groups[i] = append(groups[i], c)
					found = true
					break
				}
			}
		}
		if !found {
			groups = append(groups, []Change{c})
		}
	}
	bodies := make([]string, 0, len(groups))
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var body strings.Builder
		for _, c := range group {
			body.WriteString(changeSummary(c))
		}
		reference := group[0]
		url := reference.URL()
		if url != nil && url != false && reference.Old.Rev != nil && reference.Old.Rev != false && reference.New.Rev != nil && reference.New.Rev != false && !reflect.DeepEqual(reference.Old.Rev, reference.New.Rev) {
			from, to := revision(reference.Old.Rev, ""), revision(reference.New.Rev, "")
			if downgrade {
				from, to = to, from
			}
			text, err := lookup(ctx, url.(string), from, to)
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return "", err
			}
			if err != nil {
				body.WriteString(fmt.Sprintf("(changelog unavailable: %s)", err))
			} else {
				body.WriteString(text)
			}
		}
		bodies = append(bodies, strings.TrimRight(body.String(), "\x00\t\n\v\f\r "))
	}
	return strings.Trim(title+"\n\n"+strings.Join(bodies, "\n\n"), "\x00\t\n\v\f\r "), nil
}
