package git

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
)

type CapturedRunner func(context.Context, string, []string) (string, error)
type Mirror struct {
	Root, Cache, URL string
	Run              CapturedRunner
}

// URL spelling is part of the persisted cache identity. Group normalization
// belongs to commit messages, not to this mirror's path or clone argument.
func (m Mirror) Path() string {
	return strings.TrimRight(m.Cache, "/") + "/git-mirrors/" + fmt.Sprintf("%x", sha256.Sum256([]byte(m.URL)))
}

func (m Mirror) Setup(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := m.Path()
	local := path
	if !strings.HasPrefix(path, "/") {
		local = strings.TrimRight(m.Root, "/") + "/" + path
	}
	_, err := os.Stat(local)
	var argv []string
	if os.IsNotExist(err) {
		if err = os.MkdirAll(local, 0777); err != nil {
			return err
		}
		argv = []string{"git", "clone", "--quiet", "--mirror", m.URL, path}
	} else if err != nil {
		return err
	} else {
		argv = []string{"git", "-C", path, "fetch", "--no-show-forced-updates", "--quiet"}
	}
	_, err = m.Run(ctx, m.Root, argv)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (m Mirror) Log(ctx context.Context, from, to string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	span := from + ".." + to
	text, err := m.Run(ctx, m.Root, []string{"git", "-C", m.Path(), "log", "--no-decorate", "--left-right", "--cherry-mark", "--oneline", span})
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	return "git log for " + span + "\n" + strings.Trim(text, "\x00\t\n\v\f\r "), nil
}
